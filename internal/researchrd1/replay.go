package researchrd1

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"autowheel/internal/researchcampaign"
	"autowheel/internal/wheel"
)

// Observation is one admitted sampled bucket, not a broker receipt. Its source
// packet binds exact book coordinates, units and the disclosed bucket assumption.
type Observation struct {
	At        time.Time             `json:"at"`
	SourceSHA string                `json:"source_packet_sha256"`
	Stocks    map[string]Book       `json:"stock_books"`
	Options   map[string]Book       `json:"option_books"`
	Menus     map[string][]Contract `json:"observed_menus"`
}

type Tariff struct {
	Fees        researchcampaign.DatedFees `json:"dated_fees"`
	StockBuyFee int64                      `json:"stock_buy_fee_cents"`
	// Exercise delivery and ordinary stock trades can use different dates
	// for regulatory fee changes. Never reuse the ordinary T+2/T+1 sale rule.
	PhysicalStockSale researchcampaign.SaleFeeRule `json:"physical_stock_sale"`
	// Only the separate experiment source can set this marker. It retains the
	// actual design clock rather than inventing a historical publication time.
	model *ExperimentCosts
}

type TradeReference struct {
	At time.Time `json:"at"`
	// Reported stock trades can be sub-cent. This reference is not a cash
	// transaction: preserve millionths of one dollar, rather than round it
	// across the modeled one-cent ITM threshold (one cent is 10,000 micros).
	PriceMicros int64  `json:"price_micros"`
	SourceSHA   string `json:"source_packet_sha256"`
}

// ReplaySource supplies independently admitted originals. Missing entry books
// may be represented by absent map keys. An error means the packet or a required
// accounting source is unknown; it is never converted to a zero or a risk reject.
// BeforeSession returns only due dividend effects, and must reject unsupported
// held actions. It includes intervening payment days and reviews held identities
// and deliverables. EntryUniverse is the already frozen stock-only rank order.
type ReplaySource interface {
	Sample(time.Time) (Observation, error)
	EntryUniverse(time.Time) ([]string, Profile, error)
	Tariff(time.Time) (Tariff, error)
	Standard(Contract, time.Time) error
	Reference(string, researchcampaign.ExchangeDay) (TradeReference, error)
	ReviewHoldings(State, time.Time) error
	BeforeSession(researchcampaign.ExchangeDay, State) ([]Effect, error)
}

type Diagnostic struct {
	At        time.Time `json:"at"`
	Symbol    string    `json:"symbol"`
	Stage     string    `json:"stage"`
	Reason    string    `json:"reason"`
	Contract  *Contract `json:"contract,omitzero"`
	SourceSHA string    `json:"source_packet_sha256"`
}

type ReplayResult struct {
	ResearchModel      string            `json:"research_model,omitempty"`
	ModeledCosts       *ExperimentCosts  `json:"modeled_costs,omitzero"`
	ContractAssumption string            `json:"contract_assumption,omitempty"`
	CallEligibility    string            `json:"call_eligibility,omitempty"`
	EntryRiskRule      string            `json:"entry_risk_rule,omitempty"`
	TargetedControls   *TargetedControls `json:"targeted_controls,omitzero"`
	TaxReserveBound    *int64            `json:"reserved_highest_ST_tax_cents,omitzero"`
	EntryRiskPeak      int64             `json:"entry_risk_peak_NAV_cents,omitzero"`
	Scenario           Scenario          `json:"sensitivity"`
	Control            string            `json:"control,omitempty"`
	Status             string            `json:"status"`
	Reason             string            `json:"reason"`
	LastNAV            *int64            `json:"last_near_close_NAV_cents,omitzero"`
	LastNAVAt          time.Time         `json:"last_near_close_at,omitzero"`
	NetProfit          *int64            `json:"net_profit_cents,omitzero"`
	State              State             `json:"checkpoint_state"`
	Counts             map[string]int    `json:"diagnostic_counts"`
	Pending            []Effect          `json:"pending_attempts"`
	Independent        bool              `json:"independent_financial_verification_complete"`
	SessionsMarked     int               `json:"sessions_marked"`
}

func sourceHash(value string) bool {
	return len(value) == 64 && strings.Trim(value, "0123456789abcdef") == ""
}

func reviewedTariff(source ReplaySource, at time.Time) (Tariff, error) {
	f, err := source.Tariff(at)
	if err != nil {
		return Tariff{}, err
	}
	if f.Fees.Date != at.In(wheel.Eastern).Format(time.DateOnly) || f.Fees.KnownAt.IsZero() || f.model == nil && f.Fees.KnownAt.After(at) || !sourceHash(f.Fees.SourceSHA) || f.StockBuyFee < 0 || f.StockBuyFee > 10000 || f.Fees.OptionClose < 0 || f.Fees.OptionClose > 10000 || f.Fees.PhysicalExercise < 0 || f.Fees.PhysicalExercise > 10000 {
		return Tariff{}, errors.New("RD1 tariff date/publication/fee binding unknown")
	}
	if f.model != nil && !f.model.matches(f) {
		return Tariff{}, errors.New("experimental costs differ from their explicit model")
	}
	if err := f.Fees.StockSale.Validate(); err != nil {
		return Tariff{}, err
	}
	if err := f.PhysicalStockSale.Validate(); err != nil {
		return Tariff{}, err
	}
	if _, err := f.Fees.OpeningFee(1); err != nil {
		return Tariff{}, err
	}
	return f, nil
}

// RunPrimary preserves the primary model; sensitivities require explicit Run
// selection and their own charged case. Admission/reconciliation stay external.
func RunPrimary(a *Account, source ReplaySource, days []researchcampaign.ExchangeDay, diagnostic func(Diagnostic) error) (ReplayResult, error) {
	return Run(a, source, days, diagnostic, Primary)
}

// Run changes one declared timing/cost/exercise assumption at a time. Financial
// journal failures are terminal; diagnostics must also be durable. This driver
// does not authorize dispatch before independent scenario audits qualify it.
func Run(a *Account, source ReplaySource, days []researchcampaign.ExchangeDay, diagnostic func(Diagnostic) error, scenario Scenario) (ReplayResult, error) {
	result := ReplayResult{Scenario: scenario, Status: "incomplete", Counts: map[string]int{}, Independent: false}
	if a == nil || source == nil || diagnostic == nil || len(days) == 0 || len(days) > 2000 || scenario.Validate() != nil || a.model != scenario.AccountModel() {
		return result, errors.New("RD1 replay requires a matching declared model, account, source, sessions and durable diagnostics")
	}
	if s, ok := source.(*ExperimentSource); ok && s.targetedControls != (TargetedControls{}) {
		if a.state.Sequence != 0 {
			return result, errors.New("targeted controls require a fresh account")
		}
		result.TargetedControls = new(s.targetedControls)
		if s.targetedControls.ExpandedCapital {
			originalAccountCap, originalSymbolCap := a.accountCap, a.symbolCap
			a.accountCap, a.symbolCap = 20_000_000, 20_000_000
			defer func() { a.accountCap, a.symbolCap = originalAccountCap, originalSymbolCap }()
		}
		original := a.journal
		a.journal = func(r Record) error {
			if err := original(r); err != nil {
				return err
			}
			return s.observeTargetedRecord(r)
		}
		defer func() { a.journal = original }()
	}
	var pending []Effect
	var riskSource *ExperimentSource
	if s, ok := source.(*ExperimentSource); ok && s.entryRiskRule != "" {
		if a.state.Sequence != 0 {
			return result, errors.New("entry-risk experiment requires a fresh sampled peak origin")
		}
		riskSource, s.riskPeak = s, a.state.Seed
	}
	var lastDay researchcampaign.ExchangeDay
	finish := func(err error) (ReplayResult, error) {
		result.State, result.Pending = a.View(), slices.Clone(pending)
		if s, ok := source.(*ExperimentSource); ok && s.targetedControls.TaxReserve {
			result.TaxReserveBound = new(s.taxReserveBound)
		}
		if riskSource != nil {
			result.EntryRiskPeak = riskSource.riskPeak
		}
		if err != nil {
			result.Reason, result.NetProfit = err.Error(), nil
		} else {
			result.Status, result.Reason = "complete", string(scenario)+" path complete; financial verification pending"
			if result.LastNAV != nil {
				result.NetProfit = new(*result.LastNAV - result.State.Seed)
			}
		}
		return result, err
	}
	log := func(at time.Time, symbol, stage, reason string, c *Contract, sourceSHA string) error {
		if err := diagnostic(Diagnostic{At: at, Symbol: symbol, Stage: stage, Reason: reason, Contract: c, SourceSHA: sourceSHA}); err != nil {
			return fmt.Errorf("RD1 diagnostic append failed: %w", err)
		}
		result.Counts[stage+":"+reason]++
		return nil
	}
	mark := func(ref string, obs Observation) (int64, error) {
		state := a.View()
		if err := source.ReviewHoldings(state, obs.At); err != nil {
			return 0, err
		}
		// Keep journals proportional to financial inventory. The source packet
		// retains the full menu/books; only held sides belong to each NAV record.
		stocks, options := map[string]Book{}, map[string]Book{}
		for symbol, p := range state.Positions {
			if p.Shares > 0 {
				if b, known := obs.Stocks[symbol]; known {
					stocks[symbol] = b
				}
			}
			if p.Short != nil {
				if b, known := obs.Options[p.Short.Contract.ID]; known {
					options[p.Short.Contract.ID] = b
				}
			}
		}
		nav, err := a.Mark(ref, obs.At, obs.SourceSHA, stocks, options)
		if err == nil && riskSource != nil {
			riskSource.riskPeak = max(riskSource.riskPeak, nav)
		}
		return nav, err
	}
	// Reject an incomplete/reordered declared session vector before executing it.
	for i, day := range days {
		known, _, err := a.sessions.Session(day.OpenAt)
		if err != nil || known.Date != day.Date || !known.OpenAt.Equal(day.OpenAt) || !known.CloseAt.Equal(day.CloseAt) {
			return finish(errors.New("RD1 declared session differs from reviewed calendar"))
		}
		if i > 0 {
			next, _, err := a.sessions.NextSession(days[i-1].OpenAt)
			if err != nil || next.Date != day.Date {
				return finish(errors.New("RD1 replay omitted or reordered a session"))
			}
		}
	}
	for index, day := range days {
		lastDay = day
		events, err := source.BeforeSession(day, a.View())
		if err != nil {
			return finish(err)
		}
		for _, event := range events {
			if !slices.Contains([]string{"dividend", "pay_dividend"}, event.Action) || event.At.After(day.OpenAt) {
				return finish(errors.New("RD1 unsupported or future compulsory pre-session effect"))
			}
			if err := a.Apply(event); err != nil {
				return finish(err)
			}
		}
		signalAt, executionAt, navAt := day.CloseAt.Add(-31*time.Minute), day.CloseAt.Add(-30*time.Minute), day.CloseAt.Add(-time.Minute)
		if scenario == ExecutionFive {
			executionAt = signalAt.Add(5 * time.Minute)
		}
		observations := map[string]Observation{}
		for _, sample := range []struct {
			label string
			at    time.Time
		}{{"signal", signalAt}, {"execution", executionAt}, {"NAV", navAt}} {
			obs, err := source.Sample(sample.at)
			if err != nil || !obs.At.Equal(sample.at) || !sourceHash(obs.SourceSHA) {
				return finish(fmt.Errorf("RD1 %s source bucket unknown: %w", sample.label, errors.Join(err, errors.New("sample binding required"))))
			}
			observations[sample.label] = obs
		}
		signal := observations["signal"]
		if _, err := mark("signal-"+day.Date, signal); err != nil {
			return finish(err)
		}
		universe, current, err := source.EntryUniverse(signalAt)
		if err != nil || current.Validate() != nil || len(universe) > 10 {
			return finish(errors.New("RD1 frozen universe/new-opening policy unknown"))
		}
		seen := map[string]bool{}
		for _, symbol := range universe {
			if symbol == "" || seen[symbol] {
				return finish(errors.New("RD1 duplicate/empty universe identity"))
			}
			seen[symbol] = true
		}
		state := a.View()
		order := slices.Sorted(maps.Keys(state.Positions))
		for _, symbol := range universe {
			if !slices.Contains(order, symbol) {
				order = append(order, symbol)
			}
		}
		weekly, err := a.sessions.FirstSessionOfWeek(signalAt)
		if err != nil {
			return finish(err)
		}
		for _, symbol := range order {
			position, held := state.Positions[symbol]
			profile := current
			if held {
				profile = position.Profile
			}
			if slices.ContainsFunc(pending, func(e Effect) bool { return e.Symbol == symbol }) {
				continue
			}
			e := Effect{Ref: "policy-" + day.Date + "-" + symbol, Symbol: symbol, At: executionAt, SignalAt: signalAt, Profile: profile, SourceSHA: signal.SourceSHA}
			fee, err := reviewedTariff(source, signalAt)
			if err != nil {
				return finish(err)
			}
			if position.Short != nil {
				short := position.Short
				b, known := signal.Options[short.Contract.ID]
				if !known {
					return finish(errors.New("RD1 held signal option missing"))
				}
				ask := b.Ask
				managed := profitProfile(profile, short.Contract.Kind)
				if managed.Exit != "hold" {
					ask, err = scenario.policyPrice(b, true)
					if err != nil {
						return finish(err)
					}
				}
				trigger, err := TakeProfit(managed, short.Net, ask, int64(fee.Fees.OptionClose))
				if err != nil {
					return finish(err)
				}
				if trigger {
					e.At, err = scenario.executionAt(a, day.CloseAt, signalAt)
					if err != nil {
						return finish(err)
					}
					e.Action, e.Contract = "close_short", short.Contract
					pending = append(pending, e)
				}
				continue
			}
			if !weekly || !held && !seen[symbol] {
				continue
			}
			_, sessionIndex, _ := a.sessions.Session(signalAt)
			if fence, known := state.Fences[symbol]; known && (sessionIndex < fence.Session || signalAt.Before(fence.At)) {
				if err := log(signalAt, symbol, "signal", "reentry_fence", nil, signal.SourceSHA); err != nil {
					return finish(err)
				}
				continue
			}
			if held && signalAt.Before(position.AvailableAt) {
				if err := log(signalAt, symbol, "signal", "shares_unavailable", nil, signal.SourceSHA); err != nil {
					return finish(err)
				}
				continue
			}
			if s, ok := source.(*ExperimentSource); ok && !held && signalAt.Before(s.reentryAfter[symbol]) {
				if err := log(signalAt, symbol, "policy_rejection", "stock_loss_reentry_cooldown", nil, signal.SourceSHA); err != nil {
					return finish(err)
				}
				continue
			}
			stock, known := signal.Stocks[symbol]
			menu, observed := signal.Menus[symbol]
			if !known || !observed {
				if err := log(signalAt, symbol, "data_abstention", "missing_stock_or_menu", nil, signal.SourceSHA); err != nil {
					return finish(err)
				}
				continue
			}
			if !stock.EntryValid() {
				if err := log(signalAt, symbol, "invalid_book", "signal_stock", nil, signal.SourceSHA); err != nil {
					return finish(err)
				}
				continue
			}
			kind := "put"
			if held || profile.Mode == "aggressive" {
				kind = "call"
			}
			var chosen *Contract
			if experiment, ok := source.(interface {
				Choose(Profile, string, string, string, Book, []Contract) (*Contract, error)
			}); ok {
				chosen, err = experiment.Choose(profile, kind, day.Date, symbol, stock, menu)
			} else {
				chosen, err = Choose(profile, kind, day.Date, symbol, stock, menu)
			}
			if err != nil {
				return finish(err)
			}
			if chosen == nil {
				if err := log(signalAt, symbol, "data_abstention", "no_observed_contract_at_target", nil, signal.SourceSHA); err != nil {
					return finish(err)
				}
				continue
			}
			b, known := signal.Options[chosen.ID]
			if !known || source.Standard(*chosen, signalAt) != nil {
				if err := log(signalAt, symbol, "data_abstention", "chosen_quote_or_deliverable_unknown", chosen, signal.SourceSHA); err != nil {
					return finish(err)
				}
				continue
			}
			if !b.EntryValid() {
				if err := log(signalAt, symbol, "invalid_book", "chosen_signal_option_or_credit", chosen, signal.SourceSHA); err != nil {
					return finish(err)
				}
				continue
			}
			price, err := scenario.policyPrice(b, false)
			if err != nil {
				if err := log(signalAt, symbol, "invalid_book", "undefined_signal_cost_shock", chosen, signal.SourceSHA); err != nil {
					return finish(err)
				}
				continue
			}
			openFee, err := fee.Fees.OpeningFee(wheel.Cents(100 * max(price, 0)))
			if err != nil || 100*price <= int64(openFee) {
				if err := log(signalAt, symbol, "invalid_book", "chosen_signal_option_or_credit", chosen, signal.SourceSHA); err != nil {
					return finish(err)
				}
				continue
			}
			if held && kind == "call" {
				allowed, err := callAllowed(source, position, *chosen, signalAt)
				if err != nil {
					return finish(err)
				}
				if !allowed {
					if err := log(signalAt, symbol, "policy_rejection", "call_below_stock_acquisition_cost", chosen, signal.SourceSHA); err != nil {
						return finish(err)
					}
					continue
				}
			}
			e.Action, e.Contract = "sell_"+kind, *chosen
			if !held && profile.Mode == "aggressive" {
				e.Action = "buy_stock"
			}
			e.At, err = scenario.executionAt(a, day.CloseAt, signalAt)
			if err != nil {
				return finish(err)
			}
			pending = append(pending, e)
		}
		execution := observations["execution"]
		if _, err := mark("execution-"+day.Date, execution); err != nil {
			return finish(err)
		}
		var remaining []Effect
		for _, e := range pending {
			if e.At.After(executionAt) {
				remaining = append(remaining, e)
				continue
			}
			if err := executeScenario(a, source, execution, e, log, scenario); err != nil {
				return finish(err)
			}
		}
		pending = remaining
		nav := observations["NAV"]
		value, err := mark("NAV-"+day.Date, nav)
		if err != nil {
			return finish(err)
		}
		result.LastNAV, result.LastNAVAt = new(value), navAt
		result.SessionsMarked++
		// The declared path ends at its final near-close mark. Its later close,
		// expiration, notices and settlements remain censored, never backdated.
		if index == len(days)-1 {
			break
		}
		state = a.View()
		if err := source.ReviewHoldings(state, day.CloseAt); err != nil {
			return finish(err)
		}
		for _, symbol := range slices.Sorted(maps.Keys(state.Positions)) {
			p := state.Positions[symbol]
			if p.Short == nil || p.Short.Contract.Expiry != day.Date && scenario != DailyITM {
				continue
			}
			c := p.Short.Contract
			ref, err := source.Reference(symbol, day)
			if err != nil || ref.At.Before(day.OpenAt) || ref.At.After(day.CloseAt) || ref.PriceMicros <= 0 || ref.PriceMicros > MaxMoney*100 || !sourceHash(ref.SourceSHA) {
				return finish(errors.New("RD1 identifiable last regular-session stock trade unknown"))
			}
			e := Effect{Ref: "expiry-" + day.Date + "-" + symbol, Action: "resolve", Symbol: symbol, At: day.CloseAt, Contract: c, SourceSHA: ref.SourceSHA, Outcome: "expired"}
			e.Outcome, err = scenario.resolution(c, ref.PriceMicros)
			if err != nil {
				return finish(err)
			}
			if c.Expiry != day.Date && e.Outcome == "expired" {
				continue
			}
			if e.Outcome != "expired" {
				f, err := reviewedTariff(source, e.At)
				if err != nil {
					return finish(err)
				}
				e.Fee = int64(f.Fees.PhysicalExercise)
				if e.Outcome == "called_away" {
					stockFee, err := f.PhysicalStockSale.Cumulative(100, wheel.Cents(100*c.Strike))
					if err != nil {
						return finish(err)
					}
					e.Fee += int64(stockFee)
				}
			}
			next, _, err := a.sessions.NextSession(day.OpenAt)
			if err != nil {
				return finish(err)
			}
			e.NoticeAt = next.OpenAt.Add(5 * time.Minute)
			if err := a.Apply(e); err != nil {
				return finish(err)
			}
		}
	}
	if result.SessionsMarked != len(days) || !result.LastNAVAt.Equal(lastDay.CloseAt.Add(-time.Minute)) {
		return finish(errors.New("RD1 path lacks all declared near-close samples"))
	}
	return finish(nil)
}

func expiryOutcome(c Contract, priceMicros int64) (string, error) {
	if c.Strike <= 0 || c.Strike > MaxMoney/100 || priceMicros <= 0 || priceMicros > MaxMoney*100 || !slices.Contains([]string{"put", "call"}, c.Kind) {
		return "", errors.New("RD1 expiry requires an exact bounded reference and contract")
	}
	if c.Kind == "put" && c.Strike*10_000-priceMicros >= 10_000 {
		return "assigned", nil
	}
	if c.Kind == "call" && priceMicros-c.Strike*10_000 >= 10_000 {
		return "called_away", nil
	}
	return "expired", nil
}

func executePrimary(a *Account, source ReplaySource, obs Observation, e Effect, log func(time.Time, string, string, string, *Contract, string) error) error {
	return executeScenario(a, source, obs, e, log, Primary)
}

func executeScenario(a *Account, source ReplaySource, obs Observation, e Effect, log func(time.Time, string, string, string, *Contract, string) error, scenario Scenario) error {
	if !obs.At.Equal(e.At) {
		return errors.New("RD1 primary execution moved from its declared bucket")
	}
	state := a.View()
	if scenario == ExecutionNext && e.Action == "close_short" {
		p := state.Positions[e.Symbol]
		if p.Short == nil || p.Short.Contract != e.Contract {
			return log(e.At, e.Symbol, "unfilled", "contract_resolved_before_delayed_attempt", new(e.Contract), obs.SourceSHA)
		}
	}
	if scenario == ExecutionNext && (e.Action == "sell_put" || e.Action == "buy_stock") {
		universe, profile, err := source.EntryUniverse(e.At)
		if err != nil || profile.Validate() != nil {
			return errors.New("RD1 delayed execution opening policy unknown")
		}
		if !slices.Contains(universe, e.Symbol) || profile != e.Profile {
			return log(e.At, e.Symbol, "unfilled", "opening_policy_changed_before_delayed_attempt", new(e.Contract), obs.SourceSHA)
		}
	}
	day, _, err := a.sessions.Session(e.At)
	if err != nil {
		return err
	}
	for _, halt := range day.Halts {
		if !e.At.Before(halt.From) && e.At.Before(halt.Through) {
			return log(e.At, e.Symbol, "unfilled", "declared_trading_halt", new(e.Contract), obs.SourceSHA)
		}
	}
	e.SourceSHA = obs.SourceSHA
	f, err := reviewedTariff(source, e.At)
	if err != nil {
		return err
	}
	b, known := obs.Options[e.Contract.ID]
	if e.Action == "buy_stock" {
		b, known = obs.Stocks[e.Symbol]
	}
	if !known {
		return log(e.At, e.Symbol, "unfilled", "missing_execution_book", new(e.Contract), obs.SourceSHA)
	}
	if e.Action == "close_short" {
		if b.Ask <= 0 || b.Ask > MaxMoney/100 {
			return log(e.At, e.Symbol, "unfilled", "invalid_close_ask", new(e.Contract), obs.SourceSHA)
		}
	} else {
		stock, present := obs.Stocks[e.Symbol]
		if !present || !stock.EntryValid() || !b.EntryValid() {
			return log(e.At, e.Symbol, "unfilled", "invalid_execution_entry_book", new(e.Contract), obs.SourceSHA)
		}
		if err := source.Standard(e.Contract, e.At); err != nil {
			return log(e.At, e.Symbol, "unfilled", "execution_deliverable_unknown", new(e.Contract), obs.SourceSHA)
		}
	}
	buy := e.Action == "close_short" || e.Action == "buy_stock"
	price, err := scenario.policyPrice(b, buy)
	if err != nil {
		return log(e.At, e.Symbol, "unfilled", "undefined_execution_cost_shock", new(e.Contract), obs.SourceSHA)
	}
	if e.Action == "close_short" {
		e.Price, e.Fee = price, int64(f.Fees.OptionClose)
	} else {
		if e.Action == "buy_stock" {
			e.Price, e.Fee = price, f.StockBuyFee
		} else {
			if price <= 0 {
				return log(e.At, e.Symbol, "unfilled", "nonpositive_execution_credit", new(e.Contract), obs.SourceSHA)
			}
			openFee, err := f.Fees.OpeningFee(wheel.Cents(100 * price))
			if err != nil {
				return err
			}
			e.Price, e.Fee = price, int64(openFee)
			if 100*e.Price <= e.Fee {
				return log(e.At, e.Symbol, "unfilled", "nonpositive_execution_credit", new(e.Contract), obs.SourceSHA)
			}
		}
	}
	s := a.View()
	settled, reserved, exposure, err := s.Resources(e.At)
	if err != nil {
		return err
	}
	p, held := s.Positions[e.Symbol]
	if controls, ok := source.(*ExperimentSource); ok && !held && controls.targetedControls.MaxActiveSymbols != 0 && len(s.Positions) >= controls.targetedControls.MaxActiveSymbols {
		return log(e.At, e.Symbol, "risk_rejection", "targeted_two_symbol_limit", new(e.Contract), obs.SourceSHA)
	}
	if e.Action == "sell_call" {
		allowed, err := callAllowed(source, p, e.Contract, e.At)
		if err != nil {
			return err
		}
		if !allowed {
			return log(e.At, e.Symbol, "unfilled", "call_below_stock_acquisition_cost", new(e.Contract), obs.SourceSHA)
		}
	}
	addition, debit := int64(0), e.Fee
	if e.Action == "sell_put" {
		addition = 100 * e.Contract.Strike
		reserved += addition
	} else if e.Action == "buy_stock" {
		debit = 100*e.Price + e.Fee
		addition = debit
	} else if e.Action == "close_short" {
		if p.Short == nil || p.Short.Contract != e.Contract {
			return errors.New("RD1 pending close no longer owns its contract")
		}
		if e.Contract.Kind == "put" {
			reserved -= 100 * e.Contract.Strike
		}
		debit = 100*e.Price + e.Fee
	}
	funded := a.capacity(settled, reserved, exposure, p.StockCost, debit, addition, len(s.Positions), !held)
	if e.Action == "close_short" {
		funded = settled-reserved >= debit
	}
	if !funded {
		return log(e.At, e.Symbol, "risk_rejection", "cash_reserve_exposure_or_slots", new(e.Contract), obs.SourceSHA)
	}
	if controls, ok := source.(*ExperimentSource); ok && controls.targetedControls.TaxReserve {
		required, err := sum(controls.taxReserveBound, debit)
		if err != nil {
			return err
		}
		if e.Action == "sell_put" || e.Action == "buy_stock" {
			required, err = sum(required, Reserve)
			if err != nil {
				return err
			}
		}
		if settled-reserved < required {
			return log(e.At, e.Symbol, "risk_rejection", "targeted_highest_ST_tax_reserve", new(e.Contract), obs.SourceSHA)
		}
	}
	allowed, err := entryStressAllowed(source, s, e, obs, f)
	if err != nil {
		return err
	}
	if !allowed {
		return log(e.At, e.Symbol, "risk_rejection", "entry_minus40_risk_budget", new(e.Contract), obs.SourceSHA)
	}
	if err := a.Apply(e); err != nil {
		return err
	}
	return log(e.At, e.Symbol, "modeled_fill", e.Action, new(e.Contract), obs.SourceSHA)
}
