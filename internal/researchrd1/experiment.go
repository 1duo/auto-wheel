package researchrd1

import (
	"errors"
	"slices"
	"time"

	"autowheel/internal/researchcampaign"
	"autowheel/internal/wheel"
)

// ExperimentCosts are fixed research assumptions, never historical broker fees.
// Scale 2 is a separate doubled-cost replay with the same policy choices.
type ExperimentCosts struct {
	PlanSHA    string    `json:"plan_sha256"`
	DesignedAt time.Time `json:"designed_at"`
	Scale      int64     `json:"scale"`
}

func (m ExperimentCosts) valid() bool {
	return sourceHash(m.PlanSHA) && m.PlanSHA != ProtocolSHA256 && m.DesignedAt.Year() == 2026 && (m.Scale == 1 || m.Scale == 2)
}

func (m ExperimentCosts) tariff(at time.Time) Tariff {
	zero := researchcampaign.SaleFeeRule{NotionalDenominator: 1, ShareDenominator: 1}
	sale := zero
	sale.FlatFee = wheel.Cents(100 * m.Scale)
	return Tariff{Fees: researchcampaign.DatedFees{
		Date: at.In(wheel.Eastern).Format(time.DateOnly), KnownAt: m.DesignedAt, SourceSHA: m.PlanSHA,
		OptionOpen: wheel.Cents(65 * m.Scale), OptionClose: wheel.Cents(65 * m.Scale),
		PhysicalExercise: wheel.Cents(100 * m.Scale), StockSale: sale,
	}, StockBuyFee: 100 * m.Scale, PhysicalStockSale: zero, model: new(m)}
}

func (m ExperimentCosts) matches(f Tariff) bool {
	if !m.valid() || !f.Fees.KnownAt.Equal(m.DesignedAt) || f.Fees.SourceSHA != m.PlanSHA {
		return false
	}
	want := m.tariff(time.Time{})
	return f.Fees.OptionOpen == want.Fees.OptionOpen && f.Fees.OptionSale == nil &&
		f.Fees.OptionClose == want.Fees.OptionClose && f.Fees.PhysicalExercise == want.Fees.PhysicalExercise &&
		f.Fees.StockSale == want.Fees.StockSale && f.StockBuyFee == want.StockBuyFee &&
		f.PhysicalStockSale == want.PhysicalStockSale
}

// ExperimentPacket keeps modeled costs and new rules out of frozen RD1 packets.
// Price, identity, action, dividend and clearing review remains required.
type ExperimentPacket struct {
	Schema             string            `json:"schema"`
	Costs              ExperimentCosts   `json:"modeled_costs"`
	ContractAssumption string            `json:"contract_assumption,omitempty"`
	CallEligibility    string            `json:"call_eligibility,omitempty"`
	EntryRiskRule      string            `json:"entry_risk_rule,omitempty"`
	TargetedControls   *TargetedControls `json:"targeted_controls,omitzero"`
	Data               Packet            `json:"sampled_data"`
}

// TargetedControls are predeclared offline ablations, never live policy knobs.
type TargetedControls struct {
	MaxActiveSymbols int  `json:"max_active_symbols,omitzero"`
	LossReentryDays  int  `json:"loss_reentry_calendar_days,omitzero"`
	DecemberOnly     bool `json:"loss_reentry_december_only,omitzero"`
	GrowthCalls      bool `json:"growth_call_target,omitzero"`
	TaxReserve       bool `json:"reserve_highest_ST_tax,omitzero"`
	ExpandedCapital  bool `json:"account_200k_no_symbol_allocation_cap,omitzero"`
}

const PilotStandardContractModel = "exploratory_pilot_standard_100_shares_actions_not_fully_qualified"
const ConditionalFitContractModel = "conditional_2021_2022_standard_100_shares_pending_actual_path_review"
const ConditionalPathContractModel = "conditional_fixed_library_standard_100_shares_pending_actual_path_review"
const StockCostCallFloor = "stock_cost_floor_v1"

type ExperimentSource struct {
	*PacketSource
	costs            ExperimentCosts
	assumedContracts bool
	conditionalFit   bool
	conditionalPath  bool
	callEligibility  string
	entryRiskRule    string
	riskPeak         int64
	targetedControls TargetedControls
	stockCosts       map[string]int64
	reentryAfter     map[string]time.Time
	taxReserveBound  int64
	taxPositions     map[string]*targetedTaxPosition
}

type targetedTaxPosition struct{ basis, shares, credit int64 }

func DecodeExperimentPacket(raw []byte) (ExperimentPacket, error) {
	return decodePacket[ExperimentPacket](raw)
}

func NewExperimentSource(p ExperimentPacket) (*ExperimentSource, *researchcampaign.SessionCalendar, *researchcampaign.SettlementCalendar, []researchcampaign.ExchangeDay, error) {
	pilot := p.ContractAssumption == PilotStandardContractModel
	conditional := p.ContractAssumption == ConditionalFitContractModel
	pathReview := p.ContractAssumption == ConditionalPathContractModel
	assumed := pilot || conditional || pathReview
	controls := TargetedControls{}
	if p.TargetedControls != nil {
		controls = *p.TargetedControls
		allowed := slices.Contains([]TargetedControls{{MaxActiveSymbols: 2}, {LossReentryDays: 31},
			{GrowthCalls: true}, {LossReentryDays: 31, DecemberOnly: true}, {TaxReserve: true},
			{TaxReserve: true, ExpandedCapital: true}}, controls)
		if !pathReview || !allowed {
			return nil, nil, nil, nil, errors.New("invalid targeted offline controls")
		}
		for _, policy := range p.Data.Policies {
			if controls.TaxReserve && policy.Profile.Mode != "conservative" && policy.Profile.Mode != "balanced" && policy.Profile.Mode != "aggressive" {
				return nil, nil, nil, nil, errors.New("tax reserve requires a declared three-mode profile")
			}
			if (controls.MaxActiveSymbols != 0 || controls.GrowthCalls) && policy.Profile.Mode != "balanced" ||
				controls.LossReentryDays != 0 && policy.Profile.Mode != "aggressive" {
				return nil, nil, nil, nil, errors.New("targeted control outside its declared mode")
			}
		}
	}
	if p.EntryRiskRule != "" && (p.EntryRiskRule != OriginalDDMinus40 || !pathReview || p.CallEligibility != StockCostCallFloor) {
		return nil, nil, nil, nil, errors.New("entry risk rule requires its declared conditional floor experiment")
	}
	if p.CallEligibility != "" && (p.CallEligibility != StockCostCallFloor || !pathReview) {
		return nil, nil, nil, nil, errors.New("experimental call eligibility requires its declared conditional path rule")
	}
	status := "reviewed_sources_under_explicit_modeled_costs"
	if pilot {
		status = "observed_pilot_prices_with_explicit_contract_assumption"
		if p.Data.First != "2021-01-04" || p.Data.Last != "2021-04-07" || len(p.Data.Terms) != 0 {
			return nil, nil, nil, nil, errors.New("exploratory contract assumption is restricted to the fixed pilot and cannot manufacture reviewed terms")
		}
	}
	if conditional {
		status = "conditional_observed_prices_pending_contract_action_review"
		if p.Data.First != "2021-01-04" || p.Data.Last != "2022-12-30" || len(p.Data.Terms) != 0 {
			return nil, nil, nil, nil, errors.New("conditional fit is restricted to the declared two-year path review")
		}
	}
	if pathReview {
		status = "conditional_observed_prices_pending_contract_action_review"
		if p.Data.First < "2021-01-04" || p.Data.Last > "2026-10-09" || len(p.Data.Terms) != 0 {
			return nil, nil, nil, nil, errors.New("conditional path review outside its frozen history or contains manufactured terms")
		}
	}
	if p.Schema != "AutoWheelSimpleResearchPacketV1" || !p.Costs.valid() ||
		p.Data.Schema != "AutoWheelSimpleResearchDataV1" || p.Data.ProtocolSHA != p.Costs.PlanSHA ||
		p.Data.AdmissionStatus != status || p.ContractAssumption != "" && !assumed {
		return nil, nil, nil, nil, errors.New("unreviewed experimental packet or cost model")
	}
	source, sessions, clearing, days, err := newPacketSource(p.Data, true)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	for _, policy := range p.Data.Policies {
		if p.EntryRiskRule != "" && policy.Profile.Mode != "conservative" {
			return nil, nil, nil, nil, errors.New("entry risk experiment is conservative only")
		}
		for _, symbol := range policy.Symbols {
			if slices.Contains([]string{"QCOM", "META", "FB"}, symbol) {
				return nil, nil, nil, nil, errors.New("excluded experimental symbol")
			}
			if pilot && !slices.Contains([]string{"AAPL", "MSFT"}, symbol) {
				return nil, nil, nil, nil, errors.New("exploratory contract assumption outside fixed pilot symbols")
			}
			if conditional && !conditionalFitSymbol(symbol) {
				return nil, nil, nil, nil, errors.New("conditional fit outside its frozen source cohort")
			}
			if pathReview && !conditionalPathSymbol(symbol) {
				return nil, nil, nil, nil, errors.New("conditional path review outside its frozen source cohort")
			}
			if !assumed && !slices.ContainsFunc(p.Data.Terms, func(term Terms) bool {
				return term.Symbol == symbol && term.From.In(wheel.Eastern).Format(time.DateOnly) <= policy.Through &&
					term.Through.In(wheel.Eastern).Format(time.DateOnly) >= policy.From
			}) {
				return nil, nil, nil, nil, errors.New("experimental symbol lacks reviewed deliverable terms")
			}
		}
	}
	return &ExperimentSource{PacketSource: source, costs: p.Costs, assumedContracts: assumed, conditionalFit: conditional, conditionalPath: pathReview, callEligibility: p.CallEligibility, entryRiskRule: p.EntryRiskRule,
		targetedControls: controls, stockCosts: map[string]int64{}, reentryAfter: map[string]time.Time{}}, sessions, clearing, days, nil
}

// The cooldown follows already persisted acquisitions and stock deliveries.
// Its trigger is gross stock loss, without future tax information or premiums.
func (s *ExperimentSource) observeTargetedRecord(r Record) error {
	if s.targetedControls.TaxReserve {
		return s.observeTaxReserve(r)
	}
	if s.targetedControls.LossReentryDays == 0 {
		return nil
	}
	e := r.Effect
	if e.Action == "buy_stock" || e.Action == "resolve" && e.Outcome == "assigned" {
		price := e.Price
		if e.Action == "resolve" {
			price = e.Contract.Strike
		}
		cost, err := sum(100*price, e.Fee)
		if err != nil {
			return err
		}
		s.stockCosts[e.Symbol] = cost
	} else if e.Action == "resolve" && e.Outcome == "called_away" {
		cost, known := s.stockCosts[e.Symbol]
		if !known {
			return errors.New("targeted cooldown lacks its own stock acquisition")
		}
		if 100*e.Contract.Strike-e.Fee < cost {
			local := e.At.In(wheel.Eastern)
			if !s.targetedControls.DecemberOnly || local.Month() == time.December {
				s.reentryAfter[e.Symbol] = time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, wheel.Eastern).AddDate(0, 0, 31)
			}
		}
		delete(s.stockCosts, e.Symbol)
	}
	return nil
}

// This causal upper bound grants no capital-loss credits. It is cash earmarking,
// not a tax return: actual tax accounting is independently reconstructed later.
func (s *ExperimentSource) observeTaxReserve(r Record) error {
	if s.taxPositions == nil {
		s.taxPositions = map[string]*targetedTaxPosition{}
	}
	e := r.Effect
	if e.Action == "mark" || e.Action == "dividend" {
		return nil
	}
	p := s.taxPositions[e.Symbol]
	income := int64(0)
	var err error
	switch e.Action {
	case "buy_stock":
		if p != nil {
			return errors.New("tax reserve overlaps a stock acquisition")
		}
		cost, err := sum(100*e.Price, e.Fee)
		if err != nil {
			return err
		}
		s.taxPositions[e.Symbol] = &targetedTaxPosition{basis: cost, shares: 100}
	case "sell_put", "sell_call":
		if e.Action == "sell_put" {
			if p != nil {
				return errors.New("tax reserve overlaps a put")
			}
			p = &targetedTaxPosition{}
			s.taxPositions[e.Symbol] = p
		}
		if p == nil || p.credit != 0 {
			return errors.New("tax reserve short inventory unknown")
		}
		p.credit, err = sum(100*e.Price, -e.Fee)
	case "close_short":
		if p == nil || p.credit <= 0 {
			return errors.New("tax reserve lacks short credit")
		}
		income, err = sum(p.credit, -100*e.Price)
		if err == nil {
			income, err = sum(income, -e.Fee)
		}
		p.credit = 0
		if p.shares == 0 {
			delete(s.taxPositions, e.Symbol)
		}
	case "resolve":
		if p == nil || p.credit <= 0 {
			return errors.New("tax reserve lacks resolved short")
		}
		switch e.Outcome {
		case "assigned":
			p.basis, err = sum(100*e.Contract.Strike, e.Fee)
			if err == nil {
				p.basis, err = sum(p.basis, -p.credit)
			}
			p.shares = 100
		case "called_away":
			income, err = sum(100*e.Contract.Strike, -e.Fee)
			if err == nil {
				income, err = sum(income, p.credit)
			}
			if err == nil {
				income, err = sum(income, -p.basis)
			}
			p.shares = 0
		case "expired":
			income, err = sum(p.credit, -e.Fee)
		default:
			return errors.New("unknown tax reserve resolution")
		}
		p.credit = 0
		if p.shares == 0 {
			delete(s.taxPositions, e.Symbol)
		}
	case "pay_dividend":
		income = r.CashDelta
	default:
		return errors.New("unknown tax reserve event")
	}
	if err != nil {
		return err
	}
	if income > 0 {
		// One extra cent/event bounds separate component/year rounding as well.
		charge := (income*5410+9999)/10000 + 1
		s.taxReserveBound, err = sum(s.taxReserveBound, charge)
	}
	return err
}

// CallAllowed leaves the selected strike unchanged. A rejected call leaves the
// stock held and marked; it does not erase its loss or spend earlier premiums.
func (s *ExperimentSource) CallAllowed(p Position, c Contract, at time.Time) (bool, error) {
	if s.callEligibility == "" {
		return true, nil
	}
	if s.callEligibility != StockCostCallFloor || !s.costs.valid() || p.Shares != 100 || p.Short != nil ||
		p.StockCost <= 0 || p.StockCost > MaxMoney || p.AvailableAt.After(at) ||
		c.Kind != "call" || c.Strike <= 0 || c.Strike > MaxMoney/100 {
		return false, errors.New("stock-cost call gate requires a settled covered lot and bounded acquisition cost")
	}
	required, err := sum(p.StockCost, int64(s.costs.tariff(at).Fees.PhysicalExercise))
	if err != nil {
		return false, err
	}
	return 100*c.Strike >= required, nil
}

func callAllowed(source ReplaySource, p Position, c Contract, at time.Time) (bool, error) {
	if policy, ok := source.(interface {
		CallAllowed(Position, Contract, time.Time) (bool, error)
	}); ok {
		return policy.CallAllowed(p, c, at)
	}
	return true, nil
}

func conditionalFitSymbol(symbol string) bool {
	return slices.Contains([]string{"AAPL", "MSFT", "BA", "AMD", "JPM", "V", "INTC", "PYPL", "SQ", "XOM", "MRNA", "MU"}, symbol)
}

func conditionalPathSymbol(symbol string) bool {
	return conditionalFitSymbol(symbol) || slices.Contains([]string{"BAC", "CVX", "JNJ", "CRM", "ORCL", "WMT"}, symbol)
}

func (s *ExperimentSource) reviewPilotSymbol(symbol string, at time.Time) error {
	day := at.In(wheel.Eastern).Format(time.DateOnly)
	allowed := slices.Contains([]string{"AAPL", "MSFT"}, symbol)
	if s.conditionalFit {
		allowed = conditionalFitSymbol(symbol)
	}
	if s.conditionalPath {
		allowed = conditionalPathSymbol(symbol)
	}
	if !allowed || day < s.packet.First || day > s.packet.Last {
		return errors.New("exploratory contract assumption outside declared scope")
	}
	for _, action := range s.packet.Unsupported {
		if action.Symbol == symbol && !at.Before(action.At) {
			return errors.New("known unsupported action blocks exploratory position")
		}
	}
	return nil
}

func (s *ExperimentSource) Standard(c Contract, at time.Time) error {
	if !s.assumedContracts {
		return s.PacketSource.Standard(c, at)
	}
	if !validContract(c, c.Symbol, at.In(wheel.Eastern).Format(time.DateOnly)) {
		return errors.New("unknown exploratory contract identity")
	}
	return s.reviewPilotSymbol(c.Symbol, at)
}

func (s *ExperimentSource) ReviewHoldings(state State, at time.Time) error {
	if !s.assumedContracts {
		return s.PacketSource.ReviewHoldings(state, at)
	}
	for symbol := range state.Positions {
		if err := s.reviewPilotSymbol(symbol, at); err != nil {
			return err
		}
	}
	return nil
}

func (s *ExperimentSource) BeforeSession(day researchcampaign.ExchangeDay, state State) ([]Effect, error) {
	if err := s.ReviewHoldings(state, day.OpenAt); err != nil {
		return nil, err
	}
	return dividendEffects(s.packet.Dividends, day, state)
}

func (s *ExperimentSource) Tariff(at time.Time) (Tariff, error) {
	if !s.costs.valid() {
		return Tariff{}, errors.New("missing experimental costs")
	}
	return s.costs.tariff(at), nil
}

// Aggressive begins with stock and writes calls at least 5% above current spot,
// leaving more room for stock growth than RD1's 1% call target. This reuses the
// conservative call-strike selector; the account's profile remains aggressive.
func (s *ExperimentSource) Choose(p Profile, kind, day, symbol string, stock Book, menu []Contract) (*Contract, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	if (p.Mode == "aggressive" || s.targetedControls.GrowthCalls) && kind == "call" {
		p.Mode = "conservative"
	}
	return Choose(p, kind, day, symbol, stock, menu)
}
