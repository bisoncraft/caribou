# Migrating to the Caribou name

The project was renamed from Bison Wallet (interim codename "Boss Wallet") to
**Caribou**. The rename is mechanical — no behavior changed — but it moves the
Go module path, the GitHub repository, several directories, and the installed
application identifiers. This guide covers the steps a contributor needs to take
to adapt an existing clone, fork, or in-flight branch.

## What changed

| Aspect | Before | After |
| --- | --- | --- |
| Go module path | `github.com/bisoncraft/bosswallet` | `github.com/bisoncraft/caribou` |
| GitHub repository | `bisoncraft/bosswallet` | `bisoncraft/caribou` |
| CLI command / short name | `bossw` | `caribou` |
| Desktop command dir | `wallet/cmd/bossw-desktop` | `wallet/cmd/caribou-desktop` |
| Server command dir | `wallet/cmd/bossw` | `wallet/cmd/caribou` |
| Website package dir | `wallet/bworg` | `wallet/website` |
| Config file | `bossw.conf` | `caribou.conf` |
| Application data dir | `~/.bossw` (or platform equivalent) | `~/.caribou` |
| Log / database files | `bossw.log` / `bossw.db` | `caribou.log` / `caribou.db` |
| SPV saved-peers file | `dexc-peers.json` | `caribou-peers.json` |
| npm workspaces | `dcrdex-ui`, `@bossw/*` | `@caribou/newui`, `@caribou/*` |

Application and packaging identifiers:

- Android / iOS bundle ID: `com.bisoncraft.caribou`
- Electron bundle ID: `com.caribou.desktop`
- Linux desktop entry: `org.bisoncraft.caribou.desktop`
- Debian package name: `caribou-wallet` (the binary itself is still `caribou`;
  the package name is deliberately not plain `caribou` because the Debian and
  Ubuntu archives already carry a dead GNOME package by that name)
- Copyright holder in source headers: `The Caribou Developers`

Names that intentionally did **not** change: the `bisonwire` package, the
"Bison Wallet" reference in `README.md` and `wallet/utxowallet/DERIVATION.md`
where it names the ancestor project, and the BIP-39 wordlist and HD key-version
constants (functional values, not branding).

The rename did not touch **artwork**. Icon files were renamed to `caribou-*`
but still contain the Bison Wallet bison, so a filename search looks clean while
the shipped icons are off-brand. See [`branding/README.md`](branding/README.md)
for the inventory of images still needing replacement.

## Updating remotes

A same-owner GitHub rename installs redirects, so existing remotes keep working
against the old URL. Point them at the new name anyway, since a redirect breaks
if the old name is ever reused.

Only the canonical repository was renamed. Update whichever remote tracks it —
the remote's name depends on the workflow:

```sh
# Direct clone: the canonical repo is `origin`.
git remote set-url origin https://github.com/bisoncraft/caribou.git

# Fork workflow: the canonical repo is `upstream`; a personal fork is `origin`.
git remote set-url upstream https://github.com/bisoncraft/caribou.git
```

A personal fork is a separate repository; renaming the canonical repo does not
rename it. To keep the names aligned, rename the fork on GitHub (for example to
`<user>/caribou`) and update its remote as well:

```sh
git remote set-url origin https://github.com/<user>/caribou.git
```

Then fetch to pick up the renamed refs:

```sh
git fetch --all
```

A local application data directory is not migrated automatically. To carry
settings and wallet state across the rename, move it once:

```sh
mv ~/.bossw ~/.caribou      # adjust for the platform's app-data location
```

## Updating Go imports

Because the module path changed, `go get github.com/bisoncraft/bosswallet/...`
no longer resolves — Go verifies the path in `go.mod` against the request.
Depend on the new path instead:

```go
import "github.com/bisoncraft/caribou/wallet/asset/btc"
```

Downstream code that imported the old path must be updated in bulk; the
substitution is a literal `github.com/bisoncraft/bosswallet` →
`github.com/bisoncraft/caribou` across all source, followed by
`go mod tidy`.

## Rebasing an in-flight branch or fork

The rename touches roughly a thousand files, so any branch opened before it will
conflict on merge. Rebase over the rename mechanically rather than resolving
conflicts by hand:

1. Rebase the branch onto the post-rename `master`. Conflicts fall into two
   kinds. Where a file was deleted on `master`, keep the deletion. Where content
   conflicts, take the `master` side.
2. Re-run the identifier substitution over the whole tree so any old names the
   branch introduced (imports, copyright headers on new files) are converted
   too, then `go mod tidy` in both modules (repository root and
   `wallet/cmd/caribou-desktop`).
3. Verify: `go build ./...`, `golangci-lint run ./...`,
   `go test -race -short ./...`, and in `wallet/appserver`,
   `npm run build` and `npm test`.

## Clearing stale build artifacts

Compiled binaries left in the old command directories (`wallet/cmd/bossw`,
`wallet/cmd/bossw-desktop`) are not covered by `.gitignore` under their new
paths and can be picked up by a `git add -A` during a rebase. Remove them before
staging:

```sh
rm -rf wallet/cmd/bossw wallet/cmd/bossw-desktop
```

Confirm no build output is tracked before committing:

```sh
git status --porcelain | grep '^A'
```
