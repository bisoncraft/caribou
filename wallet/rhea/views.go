// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package rhea

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strconv"
)

// ViewResult is a view call's raw return value and the block it was
// evaluated at.
type ViewResult struct {
	Result      []byte
	BlockHeight uint64
	BlockHash   string
}

// Viewer runs contract view calls. blockHash names the block to read at;
// empty reads at the latest final block. A contract failure or transport
// error is an error, never a result.
type Viewer interface {
	CallFunction(ctx context.Context, contractID, method string, args []byte, blockHash string) (*ViewResult, error)
}

// At names the block a view was read at. Callers that draw conclusions
// from views read them at one named final block and take its time from
// that block's header.
type At struct {
	Height uint64
	Hash   string
}

// Reader decodes the deployment's contract views.
type Reader struct {
	viewer Viewer
	dep    *Deployment
}

// NewReader creates a Reader.
func NewReader(v Viewer, dep *Deployment) *Reader {
	return &Reader{viewer: v, dep: dep}
}

// view runs a view call. It returns the raw result and whether it is the
// JSON value null.
func (r *Reader) view(ctx context.Context, contract, method string, args any, blockHash string) ([]byte, bool, At, error) {
	argsJSON := []byte("{}")
	if args != nil {
		var err error
		if argsJSON, err = json.Marshal(args); err != nil {
			return nil, false, At{}, fmt.Errorf("error encoding %s arguments: %w", method, err)
		}
	}
	res, err := r.viewer.CallFunction(ctx, contract, method, argsJSON, blockHash)
	if err != nil {
		return nil, false, At{}, fmt.Errorf("%s.%s: %w", contract, method, err)
	}
	at := At{Height: res.BlockHeight, Hash: res.BlockHash}
	if at.Hash == "" {
		return nil, false, At{}, fmt.Errorf("%s.%s result carries no block reference", contract, method)
	}
	if blockHash != "" && at.Hash != blockHash {
		return nil, false, At{}, fmt.Errorf("%s.%s read at block %s, requested %s", contract, method, at.Hash, blockHash)
	}
	raw := bytes.TrimSpace(res.Result)
	if len(raw) == 0 || !json.Valid(raw) {
		return nil, false, At{}, fmt.Errorf("%s.%s returned no JSON value", contract, method)
	}
	return raw, bytes.Equal(raw, []byte("null")), at, nil
}

// decodeView runs a view call and decodes a non-null result into out.
func (r *Reader) decodeView(ctx context.Context, contract, method string, args any, blockHash string, out any) (At, error) {
	raw, null, at, err := r.view(ctx, contract, method, args, blockHash)
	if err != nil {
		return At{}, err
	}
	if null {
		return At{}, fmt.Errorf("%s.%s returned null", contract, method)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return At{}, fmt.Errorf("error decoding %s.%s result: %w", contract, method, err)
	}
	return at, nil
}

// parseAmount parses an unsigned decimal integer string.
func parseAmount(s string) (*big.Int, error) {
	if s == "" {
		return nil, errors.New("empty amount")
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return nil, fmt.Errorf("bad amount %q", s)
		}
	}
	v, _ := new(big.Int).SetString(s, 10)
	return v, nil
}

// amountView runs a view returning a decimal string amount.
func (r *Reader) amountView(ctx context.Context, contract, method string, args any, blockHash string) (*big.Int, At, error) {
	var s string
	at, err := r.decodeView(ctx, contract, method, args, blockHash, &s)
	if err != nil {
		return nil, At{}, err
	}
	v, err := parseAmount(s)
	if err != nil {
		return nil, At{}, fmt.Errorf("%s.%s: %w", contract, method, err)
	}
	return v, at, nil
}

// MCAByWallet returns the MCA bound to id, or "" when the account manager
// returned null at the read block. Any failure to get an answer is an
// error: absence is concluded only from an explicit null.
func (r *Reader) MCAByWallet(ctx context.Context, id Identity, blockHash string) (string, At, error) {
	args := map[string]Wallet{"wallet": id.Wallet()}
	raw, null, at, err := r.view(ctx, r.dep.AccountManager, "get_mca_by_wallet", args, blockHash)
	if err != nil {
		return "", At{}, err
	}
	if null {
		return "", at, nil
	}
	var mca string
	if err := json.Unmarshal(raw, &mca); err != nil {
		return "", At{}, fmt.Errorf("error decoding get_mca_by_wallet result: %w", err)
	}
	if !r.dep.IsMCA(mca) {
		return "", At{}, fmt.Errorf("get_mca_by_wallet returned %q, not an MCA of this deployment", mca)
	}
	return mca, at, nil
}

// WalletsByMCA lists the wallets bound to an MCA.
func (r *Reader) WalletsByMCA(ctx context.Context, mca, blockHash string) ([]Wallet, At, error) {
	var ws []Wallet
	at, err := r.decodeView(ctx, r.dep.AccountManager, "list_wallets_by_mca", map[string]string{"mca_id": mca}, blockHash, &ws)
	return ws, at, err
}

// Nonce returns the nonce the MCA's next business must carry.
func (r *Reader) Nonce(ctx context.Context, mca, blockHash string) (uint64, At, error) {
	var s string
	at, err := r.decodeView(ctx, mca, "get_nonce", nil, blockHash, &s)
	if err != nil {
		return 0, At{}, err
	}
	n, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return 0, At{}, fmt.Errorf("bad nonce %q: %w", s, err)
	}
	return n, at, nil
}

// CreateMCAFee is the account manager's MCA creation fee in token units.
// The full cost of creating an MCA from a supply is this plus the token
// value of the MCA's initial NEAR balance (see NearValue).
func (r *Reader) CreateMCAFee(ctx context.Context, tokenID, blockHash string) (*big.Int, At, error) {
	return r.amountView(ctx, r.dep.AccountManager, "get_create_mca_fee", map[string]string{"token_id": tokenID}, blockHash)
}

// NearValue is the account manager's configured price of 1 NEAR in the
// token's base units. It is set by account-manager governance, not a
// market feed.
func (r *Reader) NearValue(ctx context.Context, tokenID, blockHash string) (*big.Int, At, error) {
	return r.amountView(ctx, r.dep.AccountManager, "get_near_value", map[string]string{"token_id": tokenID}, blockHash)
}

// MCAInitBalance is the NEAR balance, in yoctoNEAR, the account manager
// funds a new MCA with out of the creation supply.
func (r *Reader) MCAInitBalance(ctx context.Context, blockHash string) (*big.Int, At, error) {
	return r.amountView(ctx, r.dep.AccountManager, "get_mca_init_balance", nil, blockHash)
}

// LatestMCACodeHash is the global contract code hash the account manager
// deploys for new MCAs (and upgrades to on UpgradeToLatest).
func (r *Reader) LatestMCACodeHash(ctx context.Context, blockHash string) (string, At, error) {
	var h string
	at, err := r.decodeView(ctx, r.dep.AccountManager, "get_latest_mca_wasm_code_hash", nil, blockHash, &h)
	return h, at, err
}

// CheckSupplyTemplates reads the account manager's templates and verifies
// the supply templates against the pinned ones (see
// Deployment.CheckTemplates).
func (r *Reader) CheckSupplyTemplates(ctx context.Context, blockHash string) (At, error) {
	args := map[string]int{"from_index": 0, "limit": 100}
	raw, null, at, err := r.view(ctx, r.dep.AccountManager, "get_reliable_business_template_paged", args, blockHash)
	if err != nil {
		return At{}, err
	}
	if null {
		return At{}, errors.New("no supply templates")
	}
	if err := r.dep.CheckTemplates(raw); err != nil {
		return At{}, err
	}
	return at, nil
}

// Pool is a Burrow share pool.
type Pool struct {
	Shares  *big.Int
	Balance *big.Int
}

func (p *Pool) UnmarshalJSON(b []byte) error {
	var v struct {
		Shares  string `json:"shares"`
		Balance string `json:"balance"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	var err error
	if p.Shares, err = parseAmount(v.Shares); err != nil {
		return err
	}
	p.Balance, err = parseAmount(v.Balance)
	return err
}

// AssetConfig is the subset of a Burrow asset's configuration lending
// needs.
type AssetConfig struct {
	ExtraDecimals uint8
	CanDeposit    bool
	CanWithdraw   bool
	// SuppliedLimit is the cap on total supply in internal units, nil when
	// unlimited.
	SuppliedLimit *big.Int
}

// Asset is a Burrow market. Balances are in internal units.
type Asset struct {
	TokenID           string
	Supplied          Pool
	Borrowed          Pool
	MarginDebt        Pool
	MarginPendingDebt *big.Int
	Reserved          *big.Int
	ProtFee           *big.Int
	// SupplyAPR is the base supply APR as a decimal fraction string,
	// excluding farm rewards. Display only.
	SupplyAPR string
	Config    AssetConfig
}

func (a *Asset) UnmarshalJSON(b []byte) error {
	var v struct {
		TokenID           string `json:"token_id"`
		Supplied          Pool   `json:"supplied"`
		Borrowed          Pool   `json:"borrowed"`
		MarginDebt        Pool   `json:"margin_debt"`
		MarginPendingDebt string `json:"margin_pending_debt"`
		Reserved          string `json:"reserved"`
		ProtFee           string `json:"prot_fee"`
		SupplyAPR         string `json:"supply_apr"`
		Config            *struct {
			ExtraDecimals *uint8  `json:"extra_decimals"`
			CanDeposit    *bool   `json:"can_deposit"`
			CanWithdraw   *bool   `json:"can_withdraw"`
			SuppliedLimit *string `json:"supplied_limit"`
		} `json:"config"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	if v.TokenID == "" || v.Supplied.Balance == nil || v.Borrowed.Balance == nil {
		return errors.New("asset lacks a token ID or pools")
	}
	// A missing scale or flag must not read as zero or false.
	c := v.Config
	if c == nil || c.ExtraDecimals == nil || c.CanDeposit == nil || c.CanWithdraw == nil {
		return errors.New("asset lacks its config, extra decimals, or deposit and withdraw flags")
	}
	*a = Asset{
		TokenID:   v.TokenID,
		Supplied:  v.Supplied,
		Borrowed:  v.Borrowed,
		SupplyAPR: v.SupplyAPR,
		Config: AssetConfig{
			ExtraDecimals: *c.ExtraDecimals,
			CanDeposit:    *c.CanDeposit,
			CanWithdraw:   *c.CanWithdraw,
		},
	}
	// Margin pools are absent from assets that predate margin trading.
	a.MarginDebt = v.MarginDebt
	if a.MarginDebt.Balance == nil {
		a.MarginDebt = Pool{Shares: new(big.Int), Balance: new(big.Int)}
	}
	var err error
	for _, f := range []struct {
		dst **big.Int
		s   string
	}{{&a.MarginPendingDebt, v.MarginPendingDebt}, {&a.Reserved, v.Reserved}, {&a.ProtFee, v.ProtFee}} {
		if f.s == "" {
			*f.dst = new(big.Int)
			continue
		}
		if *f.dst, err = parseAmount(f.s); err != nil {
			return err
		}
	}
	if c.SuppliedLimit != nil {
		if a.Config.SuppliedLimit, err = parseAmount(*c.SuppliedLimit); err != nil {
			return err
		}
	}
	return nil
}

// AvailableLiquidity estimates what Burrow can pay out of the market now,
// in internal units: supplied + reserved + protocol fees − borrowed −
// margin debt − pending margin debt (Burrow's available_amount), floored
// at zero. It is an estimate: interest accrues between the read and
// execution.
func (a *Asset) AvailableLiquidity() *big.Int {
	v := new(big.Int).Add(a.Supplied.Balance, a.Reserved)
	v.Add(v, a.ProtFee)
	v.Sub(v, a.Borrowed.Balance)
	v.Sub(v, a.MarginDebt.Balance)
	v.Sub(v, a.MarginPendingDebt)
	if v.Sign() < 0 {
		v.SetInt64(0)
	}
	return v
}

// SupplyHeadroom is how much more can be supplied before the supply limit,
// in internal units, or nil when the market has no limit.
func (a *Asset) SupplyHeadroom() *big.Int {
	if a.Config.SuppliedLimit == nil {
		return nil
	}
	v := new(big.Int).Sub(a.Config.SuppliedLimit, a.Supplied.Balance)
	if v.Sign() < 0 {
		v.SetInt64(0)
	}
	return v
}

// Asset reads a Burrow market.
func (r *Reader) Asset(ctx context.Context, tokenID, blockHash string) (*Asset, At, error) {
	a := new(Asset)
	at, err := r.decodeView(ctx, r.dep.Burrow, "get_asset", map[string]string{"token_id": tokenID}, blockHash, a)
	if err != nil {
		return nil, At{}, err
	}
	if a.TokenID != tokenID {
		return nil, At{}, fmt.Errorf("get_asset returned %q for %q", a.TokenID, tokenID)
	}
	return a, at, nil
}

// AssetBalance is an account's balance of one asset, in internal units.
type AssetBalance struct {
	TokenID string
	Balance *big.Int
	Shares  *big.Int
}

func (ab *AssetBalance) UnmarshalJSON(b []byte) error {
	var v struct {
		TokenID string `json:"token_id"`
		Balance string `json:"balance"`
		Shares  string `json:"shares"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	ab.TokenID = v.TokenID
	var err error
	if ab.Balance, err = parseAmount(v.Balance); err != nil {
		return err
	}
	ab.Shares, err = parseAmount(v.Shares)
	return err
}

// Position is a Burrow borrowing position: collateral and debt.
type Position struct {
	Collateral []*AssetBalance `json:"collateral"`
	Borrowed   []*AssetBalance `json:"borrowed"`
}

// Positions is a Burrow account. Supplied is the plain supplied balance
// (withdrawable without price checks); collateral and debt live in
// Positions, keyed by position name ("REGULAR" for the default).
type Positions struct {
	AccountID string
	Supplied  []*AssetBalance
	Positions map[string]*Position
	IsLocked  bool
}

func noNilBalances(bals []*AssetBalance, where string) error {
	if bals == nil {
		return fmt.Errorf("%s is missing or null", where)
	}
	for i, b := range bals {
		if b == nil {
			return fmt.Errorf("%s entry %d is null", where, i)
		}
	}
	return nil
}

// UnmarshalJSON decodes a Burrow account. Missing or null lists, positions,
// and entries are errors: they must never read as no supply or no debt.
func (p *Positions) UnmarshalJSON(b []byte) error {
	var v struct {
		AccountID string               `json:"account_id"`
		Supplied  []*AssetBalance      `json:"supplied"`
		Positions map[string]*Position `json:"positions"`
		IsLocked  bool                 `json:"is_locked"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	if err := noNilBalances(v.Supplied, "supplied"); err != nil {
		return err
	}
	if v.Positions == nil {
		return errors.New("positions are missing or null")
	}
	for name, pos := range v.Positions {
		if pos == nil {
			return fmt.Errorf("position %s is null", name)
		}
		if err := noNilBalances(pos.Collateral, "position "+name+" collateral"); err != nil {
			return err
		}
		if err := noNilBalances(pos.Borrowed, "position "+name+" borrowed"); err != nil {
			return err
		}
	}
	*p = Positions{AccountID: v.AccountID, Supplied: v.Supplied, Positions: v.Positions, IsLocked: v.IsLocked}
	return nil
}

func sumToken(bals []*AssetBalance, tokenID string) *big.Int {
	v := new(big.Int)
	for _, b := range bals {
		if b.TokenID == tokenID {
			v.Add(v, b.Balance)
		}
	}
	return v
}

// SuppliedBalance is the plain supplied balance of a token.
func (p *Positions) SuppliedBalance(tokenID string) *big.Int {
	return sumToken(p.Supplied, tokenID)
}

// CollateralBalance is the token's collateral across all positions.
func (p *Positions) CollateralBalance(tokenID string) *big.Int {
	v := new(big.Int)
	for _, pos := range p.Positions {
		v.Add(v, sumToken(pos.Collateral, tokenID))
	}
	return v
}

// BorrowedBalance is the token's debt across all positions.
func (p *Positions) BorrowedBalance(tokenID string) *big.Int {
	v := new(big.Int)
	for _, pos := range p.Positions {
		v.Add(v, sumToken(pos.Borrowed, tokenID))
	}
	return v
}

// HasDebt reports whether any position carries debt.
func (p *Positions) HasDebt() bool {
	for _, pos := range p.Positions {
		for _, b := range pos.Borrowed {
			if b.Balance.Sign() > 0 {
				return true
			}
		}
	}
	return false
}

// AccountPositions reads a Burrow account, or nil when the account is not
// registered with Burrow at the read block.
func (r *Reader) AccountPositions(ctx context.Context, accountID, blockHash string) (*Positions, At, error) {
	raw, null, at, err := r.view(ctx, r.dep.Burrow, "get_account_all_positions", map[string]string{"account_id": accountID}, blockHash)
	if err != nil {
		return nil, At{}, err
	}
	if null {
		return nil, at, nil
	}
	p := new(Positions)
	if err := json.Unmarshal(raw, p); err != nil {
		return nil, At{}, fmt.Errorf("error decoding positions: %w", err)
	}
	if p.AccountID != accountID {
		return nil, At{}, fmt.Errorf("get_account_all_positions returned %q for %q", p.AccountID, accountID)
	}
	return p, at, nil
}

// FTMetadata is a NEP-148 token's metadata subset.
type FTMetadata struct {
	Spec     string
	Name     string
	Symbol   string
	Decimals uint8
}

// UnmarshalJSON decodes token metadata. Missing decimals are an error, not
// zero.
func (m *FTMetadata) UnmarshalJSON(b []byte) error {
	var v struct {
		Spec     string `json:"spec"`
		Name     string `json:"name"`
		Symbol   string `json:"symbol"`
		Decimals *uint8 `json:"decimals"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	if v.Decimals == nil {
		return errors.New("token metadata lacks decimals")
	}
	*m = FTMetadata{Spec: v.Spec, Name: v.Name, Symbol: v.Symbol, Decimals: *v.Decimals}
	return nil
}

// FTMetadata reads a token's metadata.
func (r *Reader) FTMetadata(ctx context.Context, tokenID, blockHash string) (*FTMetadata, At, error) {
	m := new(FTMetadata)
	at, err := r.decodeView(ctx, tokenID, "ft_metadata", nil, blockHash, m)
	if err != nil {
		return nil, At{}, err
	}
	return m, at, nil
}

// FTBalance reads an account's token balance in token units.
func (r *Reader) FTBalance(ctx context.Context, tokenID, accountID, blockHash string) (*big.Int, At, error) {
	return r.amountView(ctx, tokenID, "ft_balance_of", map[string]string{"account_id": accountID}, blockHash)
}

// StorageBalance is a NEP-145 storage balance in yoctoNEAR.
type StorageBalance struct {
	Total     *big.Int
	Available *big.Int
}

// StorageBalanceOf reads an account's storage registration with a token,
// or nil when the account is not registered at the read block.
func (r *Reader) StorageBalanceOf(ctx context.Context, tokenID, accountID, blockHash string) (*StorageBalance, At, error) {
	raw, null, at, err := r.view(ctx, tokenID, "storage_balance_of", map[string]string{"account_id": accountID}, blockHash)
	if err != nil {
		return nil, At{}, err
	}
	if null {
		return nil, at, nil
	}
	var v struct {
		Total     string `json:"total"`
		Available string `json:"available"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, At{}, fmt.Errorf("error decoding storage balance: %w", err)
	}
	sb := new(StorageBalance)
	if sb.Total, err = parseAmount(v.Total); err != nil {
		return nil, At{}, err
	}
	if sb.Available, err = parseAmount(v.Available); err != nil {
		return nil, At{}, err
	}
	return sb, at, nil
}
