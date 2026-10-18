package wheel

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"

	"autowheel/internal/contractpolicy"
)

const FixedConservativePolicy = "conservative-rd1-floor-hold-v1"

type FixedPolicyConfig struct {
	Spec                 FixedPolicySpec `json:"spec"`
	Roster               FixedRoster     `json:"roster"`
	ImportLegacyStateSHA string          `json:"import_legacy_state_sha256,omitempty"`
}

func (f FixedPolicyConfig) validate(c Config) error {
	if err := f.Spec.Validate(); err != nil {
		return err
	}
	if f.ImportLegacyStateSHA != "" && !fixedSHA(f.ImportLegacyStateSHA) {
		return errors.New("invalid fixed legacy-state binding")
	}
	if _, err := fixedOrder(nil, f.Roster.Symbols); err != nil {
		return err
	}
	from, err := time.Parse(time.DateOnly, f.Roster.From)
	through, endErr := time.Parse(time.DateOnly, f.Roster.Through)
	if err != nil || endErr != nil || from.After(through) || f.Roster.KnownAt.IsZero() ||
		!fixedSHA(f.Roster.SourceSHA) || !slices.Equal(c.Symbols, f.Roster.Symbols) ||
		c.Account != f.Spec.Account || c.OrderCoveragePolicy != "exclusive_standard" ||
		c.CoordinatorSocket != "" || c.PriorityProfitClose != nil || c.StrategyPreset != "" || c.SelectionMode != "" ||
		c.MaxSymbols != 3 || c.SymbolLimitScope != "managed" || c.MaxCollateral != Cents(f.Spec.Limits.Account) ||
		c.Reserve != Cents(f.Spec.Limits.Reserve) || c.Fee != f.Spec.OptionFeeBudget || c.MaxQuoteAgeSeconds != f.Spec.MaxQuoteAge {
		return errors.New("invalid fixed-policy roster or native financial/configuration binding")
	}
	return nil
}

// PublicCopy redacts both copies of the account without mutating live config.
func (c Config) PublicCopy() Config {
	c.Account = ""
	c.Symbols = slices.Clone(c.Symbols)
	if c.FixedPolicy != nil {
		f := *c.FixedPolicy
		f.Spec.Account = ""
		f.Roster.Symbols = slices.Clone(f.Roster.Symbols)
		c.FixedPolicy = &f
	}
	return c
}

// FixedPolicySpec describes the financial policy. Source evidence may be
// refreshed without changing its financial digest; native release/config
// activation and actual broker review are still required outside this type.
type FixedPolicySpec struct {
	ID                string                       `json:"id"`
	Account           string                       `json:"account_number"`
	Limits            contractpolicy.CapitalLimits `json:"capital_limits"`
	OptionFeeBudget   Cents                        `json:"option_fee_budget_cents"`
	PhysicalFeeBudget Cents                        `json:"physical_fee_budget_cents"`
	MaxQuoteAge       int                          `json:"max_quote_age_seconds"`
	FeeSourceSHA      string                       `json:"fee_source_sha256"`
	FeeKnownAt        time.Time                    `json:"fee_evidence_known_at"`
}

func (p FixedPolicySpec) Validate() error {
	if p.ID != FixedConservativePolicy || len(p.Account) < 4 || len(p.Account) > 32 ||
		strings.Trim(p.Account, "0123456789") != "" || !fixedSHA(p.FeeSourceSHA) || p.FeeKnownAt.IsZero() ||
		p.OptionFeeBudget < 0 || p.OptionFeeBudget > 10000 || p.PhysicalFeeBudget < 0 || p.PhysicalFeeBudget > 10000 ||
		p.MaxQuoteAge < 1 || p.MaxQuoteAge > 60 || p.Limits.Account <= 0 || p.Limits.Account > int64(MaxMoney) ||
		p.Limits.Symbol <= 0 || p.Limits.Symbol > p.Limits.Account || p.Limits.Reserve < 0 || p.Limits.Reserve >= int64(MaxMoney) {
		return errors.New("invalid or unsupported fixed financial policy")
	}
	return nil
}

func (p FixedPolicySpec) Digest() string {
	// Evidence timestamps/hashes and opening rosters are not financial changes.
	// A roster is separately frozen and source-bound in every weekly batch.
	p.FeeSourceSHA, p.FeeKnownAt = "", time.Time{}
	return fixedDigest(p)
}

type FixedRoster struct {
	Symbols   []string  `json:"symbols"`
	SourceSHA string    `json:"source_sha256"`
	KnownAt   time.Time `json:"known_at"`
	From      string    `json:"from"`
	Through   string    `json:"through"`
}

type FixedSignal struct {
	Symbol             string                   `json:"symbol"`
	Kind               string                   `json:"kind"`
	Phase              string                   `json:"phase"`
	Reason             string                   `json:"reason,omitempty"`
	Contract           *contractpolicy.Contract `json:"contract,omitzero"`
	ObservedAt         time.Time                `json:"observed_at,omitzero"`
	Stock              contractpolicy.Book      `json:"stock,omitzero"`
	Option             contractpolicy.Book      `json:"option,omitzero"`
	BidAt              time.Time                `json:"stock_bid_at,omitzero"`
	AskAt              time.Time                `json:"stock_ask_at,omitzero"`
	OptionAt           time.Time                `json:"option_at,omitzero"`
	BidSize            int                      `json:"bid_size,omitzero"`
	Tick               Cents                    `json:"tick_cents,omitzero"`
	MenuSHA            string                   `json:"menu_sha256,omitempty"`
	DataSHA            string                   `json:"data_sha256,omitempty"`
	AcquisitionCeiling Cents                    `json:"acquisition_ceiling_cents,omitzero"`
	BasisSHA           string                   `json:"declared_basis_sha256,omitempty"`
	TransitionAt       time.Time                `json:"transition_at,omitzero"`
}

type FixedBatch struct {
	PolicySHA    string        `json:"policy_sha256"`
	PreviousSHA  string        `json:"previous_batch_sha256,omitempty"`
	Day          string        `json:"day"`
	SignalAt     time.Time     `json:"signal_at"`
	ExecutionAt  time.Time     `json:"execution_at"`
	LatestAt     time.Time     `json:"latest_at"`
	StartedAt    time.Time     `json:"started_at"`
	SealedAt     time.Time     `json:"sealed_at,omitzero"`
	RecoveredAt  time.Time     `json:"recovered_at,omitzero"`
	Phase        string        `json:"phase"`
	Managed      []string      `json:"managed_at_signal"`
	Roster       FixedRoster   `json:"opening_roster"`
	FeeSourceSHA string        `json:"fee_source_sha256"`
	FeeKnownAt   time.Time     `json:"fee_evidence_known_at"`
	Signals      []FixedSignal `json:"signals"`
}

type FixedPolicyState struct {
	Version   int                `json:"version"`
	Spec      FixedPolicySpec    `json:"spec"`
	PolicySHA string             `json:"policy_sha256"`
	BoundAt   time.Time          `json:"bound_at"`
	Batch     *FixedBatch        `json:"batch,omitzero"`
	Contracts map[string]Option  `json:"open_contracts,omitempty"`
	Legacy    *FixedLegacyImport `json:"legacy_import,omitzero"`
}

// FixedSignalEvidence is input to signal selection, not an order capability.
// The menu hash is a digest of decoded fields, not raw provider publication proof.
type FixedSignalEvidence struct {
	Contract contractpolicy.Contract
	Menu     []contractpolicy.Contract
	Stock    contractpolicy.Book
	Option   contractpolicy.Book
	BidAt    time.Time
	AskAt    time.Time
	OptionAt time.Time
	BidSize  int
	Tick     Cents
}

func fixedSHA(s string) bool {
	_, err := hex.DecodeString(s)
	return err == nil && len(s) == 64 && s == strings.ToLower(s)
}

func fixedDigest(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "" // Callers reject an empty hash; no marshal error becomes authority.
	}
	d := sha256.Sum256(b)
	return hex.EncodeToString(d[:])
}

func fixedSymbol(s string) bool {
	return len(s) >= 1 && len(s) <= 5 && strings.Trim(s, "ABCDEFGHIJKLMNOPQRSTUVWXYZ") == "" &&
		!slices.Contains([]string{"QCOM", "META", "FB"}, s)
}

func fixedSignalDigest(s FixedSignal) string {
	s.Phase, s.Reason, s.DataSHA, s.TransitionAt = "", "", "", time.Time{}
	return fixedDigest(s)
}

func fixedOrder(managed, roster []string) ([]string, error) {
	if len(managed) > 3 || len(roster) < 1 || len(roster) > 10 || !slices.IsSorted(managed) {
		return nil, errors.New("fixed signal roster is invalid")
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(managed)+len(roster))
	for _, group := range [][]string{managed, roster} {
		within := map[string]bool{}
		for _, symbol := range group {
			if !fixedSymbol(symbol) || within[symbol] {
				return nil, errors.New("fixed signal roster contains duplicate or excluded identity")
			}
			within[symbol] = true
			if !seen[symbol] {
				out = append(out, symbol)
				seen[symbol] = true
			}
		}
	}
	return out, nil
}

func (f FixedPolicyState) validate(s State) error {
	if f.Version != 1 {
		return errors.New("unsupported fixed policy journal version")
	}
	if err := f.Spec.Validate(); err != nil {
		return err
	}
	if f.Spec.Account != s.Account || f.PolicySHA != f.Spec.Digest() || !fixedSHA(f.PolicySHA) ||
		f.BoundAt.IsZero() || f.Spec.FeeKnownAt.After(f.BoundAt) {
		return errors.New("fixed policy journal binding is invalid")
	}
	if err := f.validateLegacy(); err != nil {
		return err
	}
	for symbol, p := range s.Positions {
		_, entry, latest, known := FixedEntryTimes(p.PutOpenedAt)
		inherited := f.inherited(symbol, p)
		if !fixedSymbol(symbol) || p.PutID == "" && p.OwnedShares != 100 || p.CallID != "" && p.OwnedShares != 100 ||
			p.PutOpenedAt.IsZero() || !strings.HasPrefix(p.Provenance, "autowheel put fill ") ||
			!inherited && (p.PutOpenedAt.Before(f.BoundAt) || !known || p.PutOpenedAt.Before(entry) || p.PutOpenedAt.After(latest)) ||
			p.OwnedShares == 100 && (p.InventoryAt.IsZero() || p.InventoryAt.Before(p.PutOpenedAt)) {
			return errors.New("fixed policy contains unsupported inventory")
		}
		if id := p.PutID + p.CallID; id != "" {
			op, found := f.Contracts[symbol]
			if !found || op.ID != id || op.Symbol != symbol || op.Quantity != 1 || op.ChainID == "" ||
				op.Strike <= 0 || op.Strike > MaxMoney/100 ||
				p.PutID != "" && (op.Kind != "put" || op.Strike != p.PutStrike || op.Expiry != p.PutExpiry) ||
				p.CallID != "" && (op.Kind != "call" || op.Expiry != p.CallExpiry) {
				return errors.New("fixed owned option lacks exact contract metadata")
			}
		}
	}
	for symbol, op := range f.Contracts {
		p, found := s.Positions[symbol]
		if !found || op.ID == "" || op.ID != p.PutID && op.ID != p.CallID {
			return errors.New("fixed contract metadata outlived its owned position")
		}
	}
	if f.Batch == nil {
		if len(s.Intents) != 0 {
			return errors.New("fixed inventory or submission intent has no signal batch")
		}
		for symbol, p := range s.Positions {
			if !f.inherited(symbol, p) {
				return errors.New("fixed inventory has no signal batch or exact legacy import")
			}
		}
		return nil
	}
	b := f.Batch
	day, err := time.ParseInLocation(time.DateOnly, b.Day, Eastern)
	signal, execution, latest, ok := FixedEntryTimes(day)
	if err != nil || !ok || b.PolicySHA != f.PolicySHA || !b.SignalAt.Equal(signal) || !b.ExecutionAt.Equal(execution) ||
		!b.LatestAt.Equal(latest) || b.StartedAt.Before(signal) || !b.StartedAt.Before(execution) || b.StartedAt.Before(f.BoundAt) ||
		b.PreviousSHA != "" && !fixedSHA(b.PreviousSHA) || !fixedSHA(b.Roster.SourceSHA) || b.Roster.KnownAt.IsZero() ||
		b.Roster.KnownAt.After(signal) || b.Roster.From > b.Day || b.Roster.Through < b.Day ||
		!fixedSHA(b.FeeSourceSHA) || b.FeeKnownAt.IsZero() || b.FeeKnownAt.After(signal) {
		return errors.New("fixed signal batch clock or source binding is invalid")
	}
	for _, value := range []string{b.Roster.From, b.Roster.Through} {
		at, err := time.Parse(time.DateOnly, value)
		if err != nil || at.Format(time.DateOnly) != value {
			return errors.New("fixed opening roster interval is invalid")
		}
	}
	order, err := fixedOrder(b.Managed, b.Roster.Symbols)
	if err != nil || len(order) != len(b.Signals) {
		return errors.New("fixed signal batch is incomplete")
	}
	if b.Phase == "capturing" {
		if !b.SealedAt.IsZero() || !b.RecoveredAt.IsZero() {
			return errors.New("capturing batch contains a completed clock")
		}
	} else if b.Phase != "sealed" || b.SealedAt.Before(b.StartedAt) || b.SealedAt.IsZero() ||
		b.SealedAt.After(execution) && b.RecoveredAt.IsZero() || !b.RecoveredAt.IsZero() && !b.RecoveredAt.Equal(b.SealedAt) {
		return errors.New("invalid fixed signal batch phase")
	}
	for i, entry := range b.Signals {
		if entry.Symbol != order[i] || entry.Kind != "put" && entry.Kind != "call" {
			return errors.New("fixed signal ordering or kind is invalid")
		}
		switch entry.Phase {
		case "unobserved", "abstained":
			if entry.Contract != nil || entry.DataSHA != "" || entry.MenuSHA != "" || !entry.ObservedAt.IsZero() ||
				entry.AcquisitionCeiling != 0 || entry.BasisSHA != "" ||
				entry.Stock != (contractpolicy.Book{}) || entry.Option != (contractpolicy.Book{}) ||
				!entry.BidAt.IsZero() || !entry.AskAt.IsZero() || !entry.OptionAt.IsZero() || entry.Tick != 0 || entry.BidSize != 0 ||
				entry.Phase == "unobserved" && (b.Phase != "capturing" || entry.Reason != "" || !entry.TransitionAt.IsZero()) ||
				entry.Phase == "abstained" && (!fixedAbstention(entry.Reason) || entry.TransitionAt.Before(b.StartedAt)) {
				return errors.New("invalid empty or abstained signal")
			}
		case "ready", "claimed", "expired":
			if entry.Contract == nil || entry.Contract.Symbol != entry.Symbol || entry.Contract.Kind != entry.Kind ||
				entry.Contract.ID == "" || len(entry.Contract.ID) > 128 || !fixedSHA(entry.MenuSHA) ||
				!fixedSHA(entry.DataSHA) || entry.DataSHA != fixedSignalDigest(entry) || entry.Reason != "" ||
				entry.ObservedAt.Before(b.StartedAt) || !entry.ObservedAt.Before(execution) ||
				!contractpolicy.EntryBookUsable(entry.Stock) || !contractpolicy.EntryBookUsable(entry.Option) ||
				entry.Option.Bid*100 <= int64(f.Spec.OptionFeeBudget) ||
				entry.BidSize < 1 || entry.Tick < 1 || entry.Option.Bid%int64(entry.Tick) != 0 ||
				!fresh(entry.BidAt, entry.ObservedAt, f.Spec.MaxQuoteAge) || !fresh(entry.AskAt, entry.ObservedAt, f.Spec.MaxQuoteAge) ||
				!fresh(entry.OptionAt, entry.ObservedAt, f.Spec.MaxQuoteAge) ||
				b.Phase == "sealed" && b.SealedAt.Before(entry.ObservedAt) ||
				entry.Phase != "ready" && b.Phase != "sealed" ||
				entry.Phase == "ready" && !entry.TransitionAt.Equal(entry.ObservedAt) ||
				entry.Phase == "claimed" && (entry.TransitionAt.Before(execution) || entry.TransitionAt.After(latest) || entry.TransitionAt.Before(b.SealedAt)) ||
				entry.Phase == "expired" && !entry.TransitionAt.After(latest) {
				return errors.New("fixed signal data or terminal phase is invalid")
			}
			chosen, err := contractpolicy.Choose("conservative", entry.Kind, b.Day, entry.Symbol, entry.Stock, []contractpolicy.Contract{*entry.Contract})
			if err != nil || chosen == nil || *chosen != *entry.Contract {
				return errors.New("fixed signal contract is outside its original target")
			}
			if entry.Kind == "call" {
				allowed, err := contractpolicy.StockCostCallAllowed(entry.Contract.Strike, int64(entry.AcquisitionCeiling), int64(f.Spec.PhysicalFeeBudget))
				if err != nil || !allowed || !fixedSHA(entry.BasisSHA) {
					return errors.New("fixed covered-call signal lacks its declared acquisition ceiling")
				}
			} else if entry.AcquisitionCeiling != 0 || entry.BasisSHA != "" {
				return errors.New("fixed put signal contains stock basis")
			}
		default:
			return errors.New("unknown fixed signal phase")
		}
	}
	for symbol, in := range s.Intents {
		q := in.Candidate
		i := slices.IndexFunc(b.Signals, func(v FixedSignal) bool { return v.Symbol == symbol })
		if !fixedSymbol(symbol) || q.Closing || in.At.Before(f.BoundAt) || i < 0 || b.Signals[i].Phase != "claimed" ||
			in.At.Before(b.Signals[i].TransitionAt) || in.At.After(b.LatestAt) ||
			b.Signals[i].Contract == nil || q.Quantity != 1 ||
			(contractpolicy.Contract{ID: q.ID, Symbol: q.Symbol, Kind: q.Kind, Expiry: q.Expiry, Strike: int64(q.Strike)}) != *b.Signals[i].Contract {
			return errors.New("fixed submission intent lacks its one claimed signal")
		}
	}
	return nil
}

func fixedAbstention(s string) bool {
	return slices.Contains([]string{"source_unavailable", "no_target", "invalid_book", "basis_unavailable", "cost_floor", "lifecycle_busy", "capture_interrupted"}, s)
}

// This is an operational CEILING from a reconciled own put assignment and its
// policy-bound fee allowance. It is never an actual paid-fee or realized-PnL
// record. A native fixed executor must recheck the broker's current shares.
func fixedPositionBasis(spec FixedPolicySpec, policySHA string, p Position) (Cents, string, error) {
	if p.OwnedShares != 100 || p.PutID != "" || p.CallID != "" || !p.MissingSince.IsZero() ||
		p.Cost <= 0 || p.Cost > MaxMoney/100 || p.PutOpenedAt.IsZero() || p.InventoryAt.Before(p.PutOpenedAt) ||
		!strings.HasPrefix(p.Provenance, "autowheel put fill ") || p.Cost*100 > MaxMoney-spec.PhysicalFeeBudget {
		return 0, "", errors.New("declared basis needs reconciled own assignment inventory")
	}
	ceiling := p.Cost*100 + spec.PhysicalFeeBudget
	proof := struct {
		PolicySHA                string
		Price                    Cents
		PutOpenedAt, InventoryAt time.Time
		Provenance               string
		FeeBudget                Cents
	}{policySHA, p.Cost, p.PutOpenedAt, p.InventoryAt, p.Provenance, spec.PhysicalFeeBudget}
	return ceiling, fixedDigest(proof), nil
}
