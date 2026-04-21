// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package appserver

import (
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"sort"
	"strconv"

	"github.com/bisoncraft/meshwallet/wallet/core"
	"github.com/bisoncraft/meshwallet/util"
	pi "github.com/bisoncraft/meshwallet/util/politeia"
	tv1 "github.com/decred/politeia/politeiawww/api/ticketvote/v1"
	qrcode "github.com/skip2/go-qrcode"
)

const (
	homeRoute      = "/"
	initRoute      = "/init"
	loginRoute     = "/login"
	walletsRoute   = "/wallets"
	walletLogRoute = "/wallets/logfile"
	settingsRoute  = "/settings"
	proposalsRoute = "/proposals"
)

// sendTemplate processes the template and sends the result.
func (s *AppServer) sendTemplate(w http.ResponseWriter, tmplID string, data any) {
	if newUI {
		s.serveSPAIndex(w)
		return
	}
	html := s.html.Load().(*templates)
	page, err := html.exec(tmplID, data)
	if err != nil {
		log.Errorf("template exec error for %s: %v", tmplID, err)
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html;charset=UTF-8")
	w.WriteHeader(http.StatusOK)
	io.WriteString(w, page)
}

// CommonArguments are common page arguments that must be supplied to every
// page to populate the <title> and <header> elements.
type CommonArguments struct {
	UserInfo               *userInfo
	Title                  string
	UseDEXBranding         bool
	Version                string
	NewAppVersionAvailable bool
}

// Create the CommonArguments for the request.
func (s *AppServer) commonArgs(r *http.Request, title string) *CommonArguments {
	return &CommonArguments{
		UserInfo:               extractUserInfo(r),
		Title:                  title,
		UseDEXBranding:         s.useDEXBranding,
		Version:                s.appVersion,
		NewAppVersionAvailable: s.newAppVersionAvailable,
	}
}

// handleHome is the handler for the '/' page request. It redirects the
// requester to the wallets page.
func (s *AppServer) handleHome(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, walletsRoute, http.StatusSeeOther)
}

// handleLogin is the handler for the '/login' page request.
func (s *AppServer) handleLogin(w http.ResponseWriter, r *http.Request) {
	cArgs := s.commonArgs(r, "Login | Bison Wallet")
	if cArgs.UserInfo.Authed {
		http.Redirect(w, r, walletsRoute, http.StatusSeeOther)
		return
	}
	s.sendTemplate(w, "login", cArgs)
}

// handleWallets is the handler for the '/wallets' page request.
func (s *AppServer) handleWallets(w http.ResponseWriter, r *http.Request) {
	assetMap := s.core.SupportedAssets()
	// Sort assets by 1. wallet vs no wallet, and 2) alphabetically.
	assets := make([]*core.SupportedAsset, 0, len(assetMap))
	// over-allocating, but assuming user will not have set up most wallets.
	nowallets := make([]*core.SupportedAsset, 0, len(assetMap))
	for _, asset := range assetMap {
		if asset.Wallet == nil {
			nowallets = append(nowallets, asset)
		} else {
			assets = append(assets, asset)
		}
	}

	sort.Slice(assets, func(i, j int) bool {
		return assets[i].Name < assets[j].Name
	})
	sort.Slice(nowallets, func(i, j int) bool {
		return nowallets[i].Name < nowallets[j].Name
	})
	s.sendTemplate(w, "wallets", s.commonArgs(r, "Wallets | Bison Wallet"))
}

// handleWalletLogFile is the handler for the '/wallets/logfile' page request.
func (s *AppServer) handleWalletLogFile(w http.ResponseWriter, r *http.Request) {
	err := r.ParseForm()
	if err != nil {
		log.Errorf("error parsing form for wallet log file: %v", err)
		http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		return
	}

	assetIDQueryString := r.Form["assetid"]
	if len(assetIDQueryString) != 1 || len(assetIDQueryString[0]) == 0 {
		log.Error("could not find asset id in query string")
		http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		return
	}

	assetID, err := strconv.ParseUint(assetIDQueryString[0], 10, 32)
	if err != nil {
		log.Errorf("failed to parse asset id query string %v", err)
		http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		return
	}

	logFilePath, err := s.core.WalletLogFilePath(uint32(assetID))
	if err != nil {
		log.Errorf("failed to get log file path %v", err)
		http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		return
	}

	logFile, err := os.Open(logFilePath)
	if err != nil {
		log.Errorf("error opening log file: %v", err)
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}
	defer logFile.Close()

	assetName := util.BipIDSymbol(uint32(assetID))
	logFileName := fmt.Sprintf("bison-%s-wallet.log", assetName)
	w.Header().Set("Content-Disposition", "attachment; filename="+logFileName)
	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(http.StatusOK)

	_, err = io.Copy(w, logFile)
	if err != nil {
		log.Errorf("error copying log file: %v", err)
	}
}

// handleGenerateQRCode is the handler for the '/generateqrcode' page request
func (s *AppServer) handleGenerateQRCode(w http.ResponseWriter, r *http.Request) {
	err := r.ParseForm()
	if err != nil {
		log.Errorf("error parsing form for generate qr code: %v", err)
		http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		return
	}

	address := r.Form["address"]
	if len(address) != 1 || len(address[0]) == 0 {
		log.Error("form for generating qr code does not contain address")
		http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		return
	}

	png, err := qrcode.Encode(address[0], qrcode.Medium, 200)
	if err != nil {
		log.Error("error generating qr code: %v", err)
		http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		return
	}

	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Content-Length", strconv.Itoa(len(png)))
	w.WriteHeader(http.StatusOK)

	_, err = w.Write(png)
	if err != nil {
		log.Errorf("error writing qr code image: %v", err)
	}
}

// handleGenerateCompanionAppQRCode is the handler for the
// '/generatecompanionappqrcode' page request.
func (s *AppServer) handleGenerateCompanionAppQRCode(w http.ResponseWriter, r *http.Request) {
	if s.onion == "" {
		http.Error(w, "Tor must be enabled to pair a companion app", http.StatusBadRequest)
		return
	}

	authToken := s.authorizeCompanion()
	url := fmt.Sprintf("%s?%s=%s", s.onion, authCK, authToken)

	png, err := qrcode.Encode(url, qrcode.Medium, 200)
	if err != nil {
		log.Error("error generating qr code: %v", err)
		http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		return
	}

	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Content-Length", strconv.Itoa(len(png)))
	w.WriteHeader(http.StatusOK)

	_, err = w.Write(png)
	if err != nil {
		log.Errorf("error writing qr code image: %v", err)
	}
}

// handleInit is the handler for the '/init' page request
func (s *AppServer) handleInit(w http.ResponseWriter, r *http.Request) {
	s.sendTemplate(w, "init", s.commonArgs(r, "Welcome | Bison Wallet"))
}

// handleSettings is the handler for the '/settings' page request.
func (s *AppServer) handleSettings(w http.ResponseWriter, r *http.Request) {
	common := s.commonArgs(r, "Settings | Bison Wallet")
	data := &struct {
		CommonArguments
		FiatRateSources map[string]bool
		FiatCurrency    string
		IsInitialized   bool
	}{
		CommonArguments: *common,
		FiatCurrency:    core.DefaultFiatCurrency,
		FiatRateSources: s.core.FiatRateSources(),
		IsInitialized:   s.core.IsInitialized(),
	}
	s.sendTemplate(w, "settings", data)
}

type proposalsTmplData struct {
	CommonArguments
	Proposals     []*pi.Proposal
	VoteStatuses  map[tv1.VoteStatusT]string
	Pagination    Pagination
	VStatusFilter *uint64
	Query         string
	LastPropSync  uint64
	IsSyncing     bool
	ErrorMsg      string
}

type Pagination struct {
	CurrentPage uint64
	HasPrev     bool
	HasNext     bool
	PrevPage    uint64
	NextPage    uint64
	Pages       []PageItem
}

// handleProposals is the handler for the /proposals page request.
func (s *AppServer) handleProposals(w http.ResponseWriter, r *http.Request) {
	const pageSize = 10

	query := r.URL.Query().Get("query")

	currentPage := uint64(1)
	if p := r.URL.Query().Get("page"); p != "" {
		if parsed, err := strconv.ParseUint(p, 10, 64); err == nil && parsed > 0 {
			currentPage = parsed
		}
	}

	offset := (currentPage - 1) * pageSize

	var filterBy *uint64
	if filterByStr := r.URL.Query().Get("status"); filterByStr != "" && filterByStr != "all" {
		val, err := strconv.ParseUint(filterByStr, 10, 64)
		if err != nil {
			s.sendProposalsPageWithError(r, w, fmt.Sprintf("invalid proposal status filter: %v", err))
			return
		}
		filterBy = &val
	}

	var err error
	var totalCount int
	var proposals []*pi.Proposal

	if filterBy != nil {
		proposals, totalCount, err = s.core.ProposalsAll(int(offset),
			int(pageSize), query, int(*filterBy))
	} else {
		proposals, totalCount, err = s.core.ProposalsAll(int(offset),
			int(pageSize), query)
	}
	if err != nil {
		s.sendProposalsPageWithError(r, w, fmt.Sprintf("error retrieving proposals: %v", err))
		return
	}

	totalPages := int(math.Ceil(float64(totalCount) / float64(pageSize)))
	pages := buildPagination(int(currentPage), totalPages)

	_, isSyncing, lastSyncTimestamp := s.core.PoliteiaDetails()

	s.sendTemplate(w, "proposals", &proposalsTmplData{
		CommonArguments: *s.commonArgs(r, "Proposals | Bison Wallet"),
		Proposals:       proposals,
		VoteStatuses:    pi.VotesStatuses,
		Pagination: Pagination{
			CurrentPage: currentPage,
			HasPrev:     currentPage > 1,
			HasNext:     currentPage < uint64(totalPages),
			PrevPage:    currentPage - 1,
			NextPage:    currentPage + 1,
			Pages:       pages,
		},
		VStatusFilter: filterBy,
		Query:         query,
		LastPropSync:  uint64(lastSyncTimestamp),
		IsSyncing:     isSyncing,
	})
}

type proposalTmplData struct {
	CommonArguments
	Proposal     *pi.Proposal
	VoteStatuses map[tv1.VoteStatusT]string
	PoliteiaURL  string
	ShortToken   string
	ErrorMsg     string
}

// handleProposal is the handler for the /proposal/{token} page request.
func (s *AppServer) handleProposal(w http.ResponseWriter, r *http.Request) {
	token, err := getProposalTokenCtx(r)
	if err != nil {
		s.sendProposalPageWithError(r, w, fmt.Sprintf("error retrieving proposal token from request context: %v", err))
		return
	}

	assetStrID := r.URL.Query().Get("assetID")
	if assetStrID == "" {
		s.sendProposalPageWithError(r, w, "request to fetch proposal is missing url query assetID")
		return
	}

	assetID, err := strconv.ParseUint(assetStrID, 10, 32)
	if err != nil {
		s.sendProposalPageWithError(r, w, fmt.Sprintf("error parsing asset id: %v", err))
		return
	}

	proposal, err := s.core.Proposal(uint32(assetID), token)
	if err != nil {
		s.sendProposalPageWithError(r, w, fmt.Sprintf("error retrieving proposal: %v", err))
		return
	}

	piURL, _, _ := s.core.PoliteiaDetails()

	s.sendTemplate(w, "proposal", &proposalTmplData{
		CommonArguments: *s.commonArgs(r, "Proposal | Bison Wallet"),
		Proposal:        proposal,
		VoteStatuses:    pi.VotesStatuses,
		PoliteiaURL:     piURL,
		ShortToken:      proposal.Token[:7],
	})
}

func (s *AppServer) sendProposalPageWithError(r *http.Request, w http.ResponseWriter, errMsg string) {
	s.sendTemplate(w, "proposal", &proposalTmplData{
		CommonArguments: *s.commonArgs(r, "Proposal | Bison Wallet"),
		ErrorMsg:        errMsg,
	})
}

func (s *AppServer) sendProposalsPageWithError(r *http.Request, w http.ResponseWriter, errMsg string) {
	s.sendTemplate(w, "proposals", &proposalsTmplData{
		CommonArguments: *s.commonArgs(r, "Proposals | Bison Wallet"),
		ErrorMsg:        errMsg,
	})
}

type PageItem struct {
	Num      int
	Active   bool
	Ellipsis bool
}

func buildPagination(current, total int) []PageItem {
	const window = 2
	items := []PageItem{}

	add := func(num int) {
		items = append(items, PageItem{
			Num:    num,
			Active: num == current,
		})
	}

	addEllipsis := func() {
		items = append(items, PageItem{Ellipsis: true})
	}

	add(1)

	if current > window+2 {
		addEllipsis()
	}

	for i := max(2, current-window); i <= min(total-1, current+window); i++ {
		add(i)
	}

	if current < total-(window+1) {
		addEllipsis()
	}

	if total > 1 {
		add(total)
	}

	return items
}
