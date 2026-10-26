package dexnet

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// bypassFuncs are the stdlib and dependency functions and variables that open
// a network connection (or resolve a name) without consulting the --torproxy
// setting. Keyed by import path. Any reference to one is flagged.
var bypassFuncs = map[string][]string{
	"net/http": {"Get", "Post", "PostForm", "Head", "DefaultClient",
		"DefaultTransport"},
	"net": {"Dial", "DialTimeout", "DialTCP", "DialUDP", "DialIP", "DialUnix",
		"LookupHost", "LookupIP", "LookupAddr", "LookupCNAME", "LookupSRV",
		"LookupTXT", "LookupNS", "LookupMX", "LookupPort", "DefaultResolver"},
	"crypto/tls":                   {"Dial", "DialWithDialer"},
	"github.com/gorilla/websocket": {"DefaultDialer"},
	"github.com/ethereum/go-ethereum/rpc": {"Dial", "DialContext",
		"DialHTTP", "DialHTTPWithClient", "DialWebsocket", "DialOptions"},
	"github.com/ethereum/go-ethereum/ethclient": {"Dial", "DialContext"},
}

// bypassTypes are types whose zero value dials directly. Only constructing
// one (a composite literal, new(T) or a zero-value var) is flagged: holding or
// passing a client that was built elsewhere is fine.
var bypassTypes = map[string][]string{
	"net/http":                     {"Client", "Transport"},
	"net":                          {"Dialer", "Resolver"},
	"crypto/tls":                   {"Dialer"},
	"github.com/gorilla/websocket": {"Dialer"},
}

// proxyDirective marks a use of a bypass API as deliberate: it either
// installs the proxy itself or connects somewhere --torproxy must not reach
// (loopback). It must carry a reason.
const proxyDirective = "torproxy:"

// exemptDirs are not shipped wallet code: test harnesses and one-off tools.
var exemptDirs = []string{
	"util/testing",
	"wallet/internal/cmd",
	"wallet/cmd/treasurysweep",
	"wallet/cmd/treasurykey",
	"docs/examples",
}

var skipDirNames = map[string]bool{
	".git": true, ".claude": true, ".worktrees": true,
	"node_modules": true, "vendor": true,
}

// TestOutboundConnectionsHonorProxy fails when production code reaches the
// network through an API that ignores --torproxy. Every such use must be
// justified with a "// torproxy: <reason>" comment on the same or the
// preceding line, which makes a new leak a visible, reviewable decision
// instead of a silent default.
func TestOutboundConnectionsHonorProxy(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}

	// Walk the tracked files rather than the filesystem: untracked build
	// trees (such as .monero_c-ios) are left out, and tracked nested modules
	// (the desktop binary) are covered.
	out, err := exec.Command("git", "-C", root, "ls-files", "-z", "*.go").Output()
	if err != nil {
		t.Fatalf("git ls-files: %v", err)
	}
	var violations []string
	for _, rel := range strings.Split(string(out), "\x00") {
		if rel == "" || strings.HasSuffix(rel, "_test.go") ||
			slices.ContainsFunc(exemptDirs, func(d string) bool {
				return rel == d || strings.HasPrefix(rel, d+"/")
			}) ||
			slices.ContainsFunc(strings.Split(rel, "/"), func(e string) bool {
				return skipDirNames[e]
			}) {
			continue
		}
		path := filepath.Join(root, rel)
		if _, err := os.Stat(path); err != nil {
			continue // tracked but deleted in the working tree
		}
		violations = append(violations, scanFile(t, path, rel)...)
	}
	if len(violations) > 0 {
		sort.Strings(violations)
		t.Fatalf("%d outbound connection(s) bypass --torproxy. Route them "+
			"through util/dexnet (ProxyHTTPClient, ProxyTransport, "+
			"ProxyDialContext) or annotate with \"// %s <reason>\":\n  %s",
			len(violations), proxyDirective, strings.Join(violations, "\n  "))
	}
}

// standalone reports whether only whitespace precedes the comment at pos on
// its line.
func standalone(src []byte, pos token.Position) bool {
	return len(bytes.TrimSpace(src[pos.Offset-(pos.Column-1):pos.Offset])) == 0
}

var majorVersion = regexp.MustCompile(`^v[0-9]+$`)

// scanFile returns the unannotated bypass API uses in one file.
func scanFile(t *testing.T, path, rel string) []string {
	t.Helper()
	fset := token.NewFileSet()
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	f, err := parser.ParseFile(fset, path, src, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse %s: %v", rel, err)
	}

	// Local import name -> banned selectors for that package.
	funcs := make(map[string]map[string]bool)
	types := make(map[string]map[string]bool)
	var out []string
	for _, imp := range f.Imports {
		p, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			t.Fatalf("%s: bad import path %s", rel, imp.Path.Value)
		}
		_, isFunc := bypassFuncs[p]
		_, isType := bypassTypes[p]
		if !isFunc && !isType {
			// Only the paths above are matched, so the package name is never
			// guessed for any other import.
			continue
		}
		local := filepath.Base(p)
		// A major version suffix is not the package name.
		if majorVersion.MatchString(local) {
			local = filepath.Base(filepath.Dir(p))
		}
		if imp.Name != nil {
			local = imp.Name.Name
		}
		if local == "." && (isFunc || isType) {
			// Uses would be unqualified, so none could be matched below.
			out = append(out, rel+": dot-import of "+p+" hides its uses from this check")
			continue
		}
		if names, ok := bypassFuncs[p]; ok {
			funcs[local] = toSet(names)
		}
		if names, ok := bypassTypes[p]; ok {
			types[local] = toSet(names)
		}
	}
	if len(funcs) == 0 && len(types) == 0 {
		return out
	}

	annotated := make(map[int]bool)
	for _, cg := range f.Comments {
		for _, c := range cg.List {
			// The directive leads the comment, so a comment that merely
			// mentions --torproxy cannot hide the line it sits above.
			text := strings.TrimSpace(strings.TrimPrefix(c.Text, "//"))
			if !strings.HasPrefix(text, proxyDirective) ||
				strings.TrimSpace(strings.TrimPrefix(text, proxyDirective)) == "" {
				continue
			}
			start := fset.Position(c.Pos()).Line
			end := fset.Position(c.End()).Line
			// A comment covers its own lines, and the line after it only
			// when it stands alone on its line: a trailing comment must not
			// exempt the next statement.
			last := end
			if standalone(src, fset.Position(c.Pos())) {
				last = end + 1
			}
			for l := start; l <= last; l++ {
				annotated[l] = true
			}
		}
	}

	// bannedType reports whether e names a bypass type, such as http.Client.
	bannedType := func(e ast.Expr) (*ast.SelectorExpr, *ast.Ident, bool) {
		sel, ok := e.(*ast.SelectorExpr)
		if !ok {
			return nil, nil, false
		}
		id, ok := sel.X.(*ast.Ident)
		return sel, id, ok && types[id.Name][sel.Sel.Name]
	}
	report := func(sel *ast.SelectorExpr, id *ast.Ident) {
		pos := fset.Position(sel.Pos())
		if annotated[pos.Line] {
			return
		}
		out = append(out, rel+":"+strconv.Itoa(pos.Line)+": "+id.Name+"."+sel.Sel.Name)
	}
	ast.Inspect(f, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.CompositeLit:
			if sel, id, ok := bannedType(n.Type); ok {
				report(sel, id)
			}
		case *ast.CallExpr:
			if fn, ok := n.Fun.(*ast.Ident); ok && fn.Name == "new" && len(n.Args) == 1 {
				if sel, id, ok := bannedType(n.Args[0]); ok {
					report(sel, id)
				}
			}
		case *ast.ValueSpec:
			if len(n.Values) == 0 && n.Type != nil {
				if sel, id, ok := bannedType(n.Type); ok {
					report(sel, id)
				}
			}
		case *ast.SelectorExpr:
			if id, ok := n.X.(*ast.Ident); ok && funcs[id.Name][n.Sel.Name] {
				report(n, id)
			}
		}
		return true
	})
	return out
}

func toSet(names []string) map[string]bool {
	set := make(map[string]bool, len(names))
	for _, n := range names {
		set[n] = true
	}
	return set
}
