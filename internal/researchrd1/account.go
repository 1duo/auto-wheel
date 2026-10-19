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

// Account is RD1's offline money kernel. Scheduled decisions, price/fee source
// admission, dividend entitlement and contract deliverables belong to the replay
// adapter. Supplying an Effect does not qualify those inputs or a broker fill.
// The existing campaign calendars are reused without its native entry resolver.
type Account struct {
	state      State
	clearing   *researchcampaign.SettlementCalendar
	sessions   *researchcampaign.SessionCalendar
	journal    func(Record) error
	model      Model
	uncertain  bool
	accountCap int64
	symbolCap  int64
}

type Sale struct {
	Contract Contract  `json:"contract"`
	Ref      string    `json:"opening_ref"`
	Net      int64     `json:"opening_net_credit_cents"`
	Fee      int64     `json:"opening_fee_cents"`
	At       time.Time `json:"opened_at"`
	Resolved time.Time `json:"resolved_at,omitzero"`
	Outcome  string    `json:"outcome,omitempty"`
	Close    int64     `json:"close_cost_cents"`
}

type Position struct {
	Profile     Profile   `json:"opening_profile"`
	Shares      int       `json:"shares"`
	StockCost   int64     `json:"stock_acquisition_cents"`
	AvailableAt time.Time `json:"stock_available_at,omitzero"`
	Short       *Sale     `json:"short,omitzero"`
}

// CashLock includes unsettled proceeds and collateral pending lapse notice.
// These are portions of economic Cash, never additional assets or receivables.
type CashLock struct {
	Ref    string    `json:"ref"`
	Amount int64     `json:"amount_cents"`
	Until  time.Time `json:"available_at"`
}

type Fence struct {
	Session int       `json:"earliest_session"`
	At      time.Time `json:"earliest_at"`
}

type Dividend struct {
	Symbol      string    `json:"symbol"`
	Amount      int64     `json:"amount_cents"`
	PayAt       time.Time `json:"payment_at"`
	AvailableAt time.Time `json:"available_at"`
	Paid        bool      `json:"paid"`
}

type State struct {
	Seed       int64               `json:"seed_cents"`
	Cash       int64               `json:"economic_cash_cents"`
	Fees       int64               `json:"fees_cents"`
	StockPnL   int64               `json:"realized_stock_pnl_cents"`
	At         time.Time           `json:"at,omitzero"`
	Sequence   int                 `json:"sequence"`
	Positions  map[string]Position `json:"positions"`
	Sales      map[string]Sale     `json:"option_sales"`
	Locks      []CashLock          `json:"cash_locks"`
	Fences     map[string]Fence    `json:"reentry_fences"`
	LastPolicy map[string]int      `json:"last_policy_session"`
	Dividends  map[string]Dividend `json:"dividends"`
	Used       map[string]bool     `json:"used_refs"`
}

// Effect carries already admitted source observations. Fee is the total dated
// fee for this event, not an optional default; SourceSHA binds the input packet.
// Price is per-share/per-option cents, except dividend: its exact 100-share
// aggregate is supplied as Price. Resolve is an explicit counterfactual outcome.
type Effect struct {
	Ref         string    `json:"ref"`
	Action      string    `json:"action"`
	Symbol      string    `json:"symbol"`
	At          time.Time `json:"at"`
	SignalAt    time.Time `json:"signal_at,omitzero"`
	Profile     Profile   `json:"profile"`
	Contract    Contract  `json:"contract"`
	Price       int64     `json:"price_cents"`
	Fee         int64     `json:"fee_cents"`
	SourceSHA   string    `json:"source_packet_sha256"`
	Outcome     string    `json:"outcome,omitempty"`
	NoticeAt    time.Time `json:"notice_at,omitzero"`
	PayAt       time.Time `json:"payment_at,omitzero"`
	AvailableAt time.Time `json:"available_at,omitzero"`
	DividendID  string    `json:"dividend_id,omitempty"`
}

type Record struct {
	Sequence        int     `json:"sequence"`
	Effect          Effect  `json:"effect"`
	CashDelta       int64   `json:"cash_delta_cents"`
	CashAfter       int64   `json:"cash_after_cents"`
	ReceivableDelta int64   `json:"receivable_delta_cents"`
	FeesAfter       int64   `json:"fees_after_cents"`
	Mark            *Sample `json:"mark,omitzero"`
}

type Sample struct {
	NAV     int64           `json:"NAV_cents"`
	Stocks  map[string]Book `json:"stock_books"`
	Options map[string]Book `json:"option_books"`
}

func NewAccount(clearing *researchcampaign.SettlementCalendar, sessions *researchcampaign.SessionCalendar, journal func(Record) error) (*Account, error) {
	return NewModelAccount(clearing, sessions, journal, Model{})
}

// Model changes only the frozen one-business-day availability sensitivity.
// Other sensitivity prices, execution and exercise decisions remain in replay.
type Model struct {
	SettlementExtraDays int `json:"settlement_extra_business_days"`
}

func NewModelAccount(clearing *researchcampaign.SettlementCalendar, sessions *researchcampaign.SessionCalendar, journal func(Record) error, model Model) (*Account, error) {
	if clearing == nil || sessions == nil || journal == nil {
		return nil, errors.New("RD1 requires explicit calendars and durable journal")
	}
	if model.SettlementExtraDays < 0 || model.SettlementExtraDays > 1 {
		return nil, errors.New("RD1 availability sensitivity is zero or one extra business day")
	}
	return &Account{state: flatSeedState(), clearing: clearing, sessions: sessions, journal: journal, model: model}, nil
}

func flatSeedState() State {
	return State{Seed: 8_404_717, Cash: 8_404_717, Positions: map[string]Position{}, Sales: map[string]Sale{}, Fences: map[string]Fence{}, LastPolicy: map[string]int{}, Dividends: map[string]Dividend{}, Used: map[string]bool{}}
}

func (a *Account) settlement(at time.Time, flow researchcampaign.SettlementFlow) (time.Time, error) {
	available, err := a.clearing.Next(at, flow)
	if err != nil {
		return available, err
	}
	return a.delayAvailability(available)
}

func (a *Account) delayAvailability(base time.Time) (time.Time, error) {
	if a.model.SettlementExtraDays == 0 {
		return base, nil
	}
	next, err := a.clearing.Next(base, researchcampaign.OptionPremium)
	if err != nil {
		return time.Time{}, err
	}
	day, clock := next.In(wheel.Eastern), base.In(wheel.Eastern)
	// Preserve a notice's 09:35 clock as well as advancing its clearing date.
	shifted := time.Date(day.Year(), day.Month(), day.Day(), clock.Hour(), clock.Minute(), clock.Second(), clock.Nanosecond(), wheel.Eastern)
	return maxTime(next, shifted), nil
}

func (a *Account) View() State {
	s := a.state
	s.Positions, s.Sales = maps.Clone(s.Positions), maps.Clone(s.Sales)
	for symbol, p := range s.Positions {
		if p.Short != nil {
			p.Short = new(*p.Short)
		}
		s.Positions[symbol] = p
	}
	s.Locks, s.Fences, s.LastPolicy = slices.Clone(s.Locks), maps.Clone(s.Fences), maps.Clone(s.LastPolicy)
	s.Dividends, s.Used = maps.Clone(s.Dividends), maps.Clone(s.Used)
	return s
}

func sum(a, b int64) (int64, error) {
	if a < -MaxMoney || a > MaxMoney || b < -MaxMoney || b > MaxMoney || a+b < -MaxMoney || a+b > MaxMoney {
		return 0, errors.New("RD1 checked money envelope")
	}
	return a + b, nil
}

func (s State) Resources(at time.Time) (settled, reserved, exposure int64, err error) {
	settled = s.Cash
	for _, lock := range s.Locks {
		if at.Before(lock.Until) {
			settled, err = sum(settled, -lock.Amount)
			if err != nil || settled < 0 {
				return 0, 0, 0, errors.New("RD1 unavailable proceeds exceed cash")
			}
		}
	}
	for _, p := range s.Positions {
		capital := p.StockCost
		if p.Short != nil && p.Short.Contract.Kind == "put" {
			capital = 100 * p.Short.Contract.Strike
			reserved, err = sum(reserved, capital)
			if err != nil {
				return 0, 0, 0, err
			}
		}
		exposure, err = sum(exposure, capital)
		if err != nil {
			return 0, 0, 0, err
		}
	}
	if reserved > settled {
		return 0, 0, 0, errors.New("RD1 put collateral exceeds settled cash")
	}
	return settled, reserved, exposure, nil
}

func (s State) Receivables() (int64, error) {
	var amount int64
	for _, d := range s.Dividends {
		if !d.Paid {
			var err error
			amount, err = sum(amount, d.Amount)
			if err != nil {
				return 0, err
			}
		}
	}
	return amount, nil
}

func (a *Account) clock(at time.Time) error {
	if a == nil || a.uncertain || at.IsZero() || at.Before(a.state.At) {
		return errors.New("RD1 nonmonotonic clock or uncertain journal")
	}
	return nil
}

func validContract(c Contract, symbol, day string) bool {
	expiry, err := time.Parse(time.DateOnly, c.Expiry)
	return err == nil && expiry.Format(time.DateOnly) == c.Expiry && c.Expiry >= day && c.ID != "" && len(c.ID) <= 128 && c.Symbol == symbol && slices.Contains([]string{"put", "call"}, c.Kind) && c.Strike > 0 && c.Strike <= MaxMoney/100
}

// Apply stages every effect before journaling it. A journal failure poisons the
// account and never commits the staged fill; callers must retain it as failed.
func (a *Account) Apply(e Effect) error {
	if err := a.clock(e.At); err != nil {
		return err
	}
	if e.Ref == "" || len(e.Ref) > 128 || a.state.Used[e.Ref] || e.Symbol == "" || len(e.Symbol) > 5 || strings.Trim(e.Symbol, "ABCDEFGHIJKLMNOPQRSTUVWXYZ") != "" || e.Fee < 0 || e.Fee > 10000 || e.Price < 0 || e.Price > MaxMoney/100 || len(e.SourceSHA) != 64 || strings.Trim(e.SourceSHA, "0123456789abcdef") != "" {
		return errors.New("RD1 missing, reused or unbounded effect")
	}
	day, index, err := a.sessions.Session(e.At)
	if err != nil && e.Action != "pay_dividend" {
		return err
	}
	s := a.View()
	p, owned := s.Positions[e.Symbol]
	settled, reserved, exposure, err := s.Resources(e.At)
	if err != nil {
		return err
	}
	policy := slices.Contains([]string{"sell_put", "buy_stock", "sell_call", "close_short"}, e.Action)
	entry := policy && e.Action != "close_short"
	if policy {
		if prior, known := s.LastPolicy[e.Symbol]; known && prior == index {
			return errors.New("RD1 one policy transaction per symbol/session")
		}
		if e.At.Before(day.OpenAt) || !e.At.Before(day.CloseAt) {
			return errors.New("RD1 policy effect outside regular session")
		}
		for _, halt := range day.Halts {
			if !e.At.Before(halt.From) && e.At.Before(halt.Through) {
				return errors.New("RD1 policy effect during declared halt")
			}
		}
	}
	if entry {
		fence, fenced := s.Fences[e.Symbol]
		signalDay, signalIndex, err := a.sessions.Session(e.SignalAt)
		if err != nil || !e.SignalAt.Before(e.At) || e.SignalAt.Before(signalDay.OpenAt) || !e.SignalAt.Before(signalDay.CloseAt) {
			return errors.New("RD1 entry requires its preceding regular-session signal")
		}
		first, err := a.sessions.FirstSessionOfWeek(e.SignalAt)
		if err != nil || !first || fenced && (signalIndex < fence.Session || e.SignalAt.Before(fence.At)) {
			return errors.New("RD1 weekly entry/reentry fence")
		}
		if signalDay.Date != day.Date {
			next, _, err := a.sessions.NextSession(e.SignalAt)
			if err != nil || next.Date != day.Date {
				return errors.New("RD1 delayed entry exceeds the next reviewed session")
			}
		}
		if err := e.Profile.Validate(); err != nil {
			return err
		}
	}
	var delta, receivableDelta int64
	switch e.Action {
	case "sell_put", "sell_call":
		if !validContract(e.Contract, e.Symbol, day.Date) || e.Price == 0 || 100*e.Price <= e.Fee || e.Contract.Kind != strings.TrimPrefix(e.Action, "sell_") {
			return errors.New("RD1 invalid opening contract/credit")
		}
		addition, symbolCapital := int64(0), p.StockCost
		if e.Action == "sell_put" {
			if owned || e.Profile.Mode == "aggressive" {
				return errors.New("RD1 put needs flat nonaggressive lifecycle")
			}
			addition = 100 * e.Contract.Strike
			reserved += addition
			p.Profile = e.Profile
		} else if !owned || p.Shares != 100 || p.Short != nil || e.At.Before(p.AvailableAt) || e.Profile != p.Profile {
			return errors.New("RD1 call lacks settled owned shares/opening profile")
		}
		if !a.capacity(settled, reserved, exposure, symbolCapital, e.Fee, addition, len(s.Positions), !owned) {
			return errors.New("RD1 option entry capacity")
		}
		available, err := a.settlement(e.At, researchcampaign.OptionPremium)
		if err != nil {
			return err
		}
		delta = 100*e.Price - e.Fee
		p.Short = &Sale{Contract: e.Contract, Ref: e.Ref, Net: delta, Fee: e.Fee, At: e.At}
		s.Sales[e.Ref] = *p.Short
		// Debit fees from settled cash immediately; the gross proceeds are
		// unavailable until dated settlement. No same-day premium financing.
		s.Locks = append(s.Locks, CashLock{Ref: e.Ref, Amount: 100 * e.Price, Until: available})
		s.Positions[e.Symbol] = p
	case "buy_stock":
		if owned || e.Profile.Mode != "aggressive" || e.Price == 0 {
			return errors.New("RD1 direct stock entry needs flat aggressive lifecycle")
		}
		cost := 100*e.Price + e.Fee
		if !a.capacity(settled, reserved, exposure, 0, cost, cost, len(s.Positions), true) {
			return errors.New("RD1 stock entry capacity")
		}
		available, err := a.settlement(e.At, researchcampaign.StockTrade)
		if err != nil {
			return err
		}
		delta = -cost
		s.Positions[e.Symbol] = Position{Profile: e.Profile, Shares: 100, StockCost: cost, AvailableAt: available}
	case "close_short":
		if !owned || p.Short == nil || p.Short.Contract != e.Contract || e.Price == 0 || e.Contract.Expiry < day.Date {
			return errors.New("RD1 close lacks exact owned short/ask")
		}
		// The closing transaction releases its own put collateral atomically;
		// other collateral and all unsettled proceeds remain unavailable.
		otherReserved := reserved
		if p.Short.Contract.Kind == "put" {
			otherReserved -= 100 * p.Short.Contract.Strike
		}
		delta = -100*e.Price - e.Fee
		if settled-otherReserved < -delta {
			return errors.New("RD1 close cannot use unavailable funds")
		}
		sale := *p.Short
		sale.Resolved, sale.Outcome, sale.Close = e.At, "managed_close", -delta
		s.Sales[sale.Ref] = sale
		p.Short = nil
		s.Fences[e.Symbol] = Fence{Session: index + 2, At: e.At}
		if p.Shares == 0 {
			delete(s.Positions, e.Symbol)
		} else {
			s.Positions[e.Symbol] = p
		}
	case "resolve":
		if !owned || p.Short == nil || p.Short.Contract != e.Contract || !e.At.Equal(day.CloseAt) || e.Contract.Expiry < day.Date {
			return errors.New("RD1 resolution lacks owned short/close/notice")
		}
		next, _, err := a.sessions.NextSession(e.At)
		if err != nil || !e.NoticeAt.Equal(next.OpenAt.Add(5*time.Minute)) {
			return errors.New("RD1 resolution needs next-session open plus five minutes notice")
		}
		if !slices.Contains([]string{"expired", "assigned", "called_away"}, e.Outcome) || e.Outcome == "expired" && e.Contract.Expiry != day.Date || e.Outcome == "expired" && e.Fee != 0 {
			return errors.New("RD1 unsupported lifecycle/expiry fee")
		}
		sale := *p.Short
		sale.Resolved, sale.Outcome = e.At, e.Outcome
		s.Sales[sale.Ref] = sale
		p.Short = nil
		s.Fences[e.Symbol] = Fence{Session: index + 2, At: e.NoticeAt}
		switch e.Outcome {
		case "expired":
			available, err := a.delayAvailability(e.NoticeAt)
			if err != nil {
				return err
			}
			if p.Shares == 0 {
				s.Locks = append(s.Locks, CashLock{Ref: e.Ref, Amount: 100 * e.Contract.Strike, Until: available})
			} else {
				p.AvailableAt = maxTime(p.AvailableAt, available)
			}
		case "assigned", "called_away":
			available, err := a.clearing.Next(e.At, researchcampaign.PhysicalExercise)
			if err != nil {
				return err
			}
			available, err = a.delayAvailability(maxTime(available, e.NoticeAt))
			if err != nil {
				return err
			}
			if e.Fee > settled-reserved {
				return errors.New("RD1 exercise fee lacks settled funds")
			}
			gross := 100 * e.Contract.Strike
			if e.Outcome == "assigned" {
				if e.Contract.Kind != "put" || p.Shares != 0 {
					return errors.New("RD1 assignment duplicates/unowns stock")
				}
				delta = -gross - e.Fee
				p.Shares, p.StockCost, p.AvailableAt = 100, -delta, available
			} else {
				if e.Contract.Kind != "call" || p.Shares != 100 || e.At.Before(p.AvailableAt) {
					return errors.New("RD1 call-away lacks available covered stock")
				}
				delta = gross - e.Fee
				s.StockPnL, err = sum(s.StockPnL, delta-p.StockCost)
				if err != nil {
					return err
				}
				p.Shares, p.StockCost = 0, 0
				s.Locks = append(s.Locks, CashLock{Ref: e.Ref, Amount: gross, Until: available})
			}
		}
		if p.Shares == 0 {
			delete(s.Positions, e.Symbol)
		} else {
			s.Positions[e.Symbol] = p
		}
	case "dividend":
		if !e.At.Equal(day.OpenAt) || !owned || p.Shares != 100 || e.Price == 0 || e.Fee != 0 || e.DividendID == "" || e.PayAt.Before(e.At) || e.AvailableAt.Before(e.PayAt) {
			return errors.New("RD1 dividend needs exact economic shares and dated entitlement")
		}
		if _, duplicate := s.Dividends[e.DividendID]; duplicate {
			return errors.New("RD1 dividend already assessed")
		}
		e.AvailableAt, err = a.delayAvailability(e.AvailableAt)
		if err != nil {
			return err
		}
		s.Dividends[e.DividendID] = Dividend{Symbol: e.Symbol, Amount: e.Price, PayAt: e.PayAt, AvailableAt: e.AvailableAt}
		receivableDelta = e.Price
	case "pay_dividend":
		d, known := s.Dividends[e.DividendID]
		if !known || d.Paid || d.Symbol != e.Symbol || !e.At.Equal(d.PayAt) || e.Fee != 0 || e.Price != d.Amount {
			return errors.New("RD1 dividend unknown, repeated or mistimed")
		}
		delta, receivableDelta, d.Paid = d.Amount, -d.Amount, true
		s.Dividends[e.DividendID] = d
		if e.At.Before(d.AvailableAt) {
			s.Locks = append(s.Locks, CashLock{Ref: e.Ref, Amount: d.Amount, Until: d.AvailableAt})
		}
	default:
		return errors.New("unsupported RD1 money effect")
	}
	s.Cash, err = sum(s.Cash, delta)
	if err != nil || s.Cash < 0 {
		return errors.New("RD1 effect makes economic cash negative/excessive")
	}
	s.Fees, err = sum(s.Fees, e.Fee)
	if err != nil {
		return err
	}
	if _, _, _, err := s.Resources(e.At); err != nil {
		return err
	}
	if _, err := s.Receivables(); err != nil {
		return err
	}
	s.Sequence++
	if err := a.journal(Record{Sequence: s.Sequence, Effect: e, CashDelta: delta, CashAfter: s.Cash, ReceivableDelta: receivableDelta, FeesAfter: s.Fees}); err != nil {
		a.uncertain = true
		return fmt.Errorf("RD1 journal uncertain: %w", err)
	}
	s.At, s.Used[e.Ref] = e.At, true
	if policy {
		s.LastPolicy[e.Symbol] = index
	}
	a.state = s
	return nil
}

func maxTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return b
	}
	return a
}

// Mark persists observed liabilities and the observation clock. Failed marks do
// not change state, erase liabilities or allow a later financial event to repair
// an already assessed earlier sample. Replay must stop the affected path on error.
func (a *Account) Mark(ref string, at time.Time, sourceSHA string, stocks, options map[string]Book) (int64, error) {
	if err := a.clock(at); err != nil {
		return 0, err
	}
	if ref == "" || len(ref) > 128 || a.state.Used[ref] || len(sourceSHA) != 64 || strings.Trim(sourceSHA, "0123456789abcdef") != "" {
		return 0, errors.New("RD1 mark requires unique source binding")
	}
	day, _, err := a.sessions.Session(at)
	if err != nil || at.Before(day.OpenAt) || !at.Before(day.CloseAt) {
		return 0, errors.New("RD1 sampled mark outside reviewed regular session")
	}
	var holdings []Holding
	for symbol, p := range a.state.Positions {
		h := Holding{Shares: p.Shares}
		if p.Shares > 0 {
			b, known := stocks[symbol]
			if !known {
				return 0, errors.New("RD1 held stock mark absent")
			}
			h.Stock = new(b)
		}
		if p.Short != nil {
			b, known := options[p.Short.Contract.ID]
			if !known {
				return 0, errors.New("RD1 held option mark absent")
			}
			h.Short = new(b)
		}
		holdings = append(holdings, h)
	}
	receivables, err := a.state.Receivables()
	if err != nil {
		return 0, err
	}
	nav, err := NAV(a.state.Cash, receivables, holdings)
	if err != nil {
		return 0, err
	}
	sample := Sample{NAV: nav, Stocks: maps.Clone(stocks), Options: maps.Clone(options)}
	record := Record{Sequence: a.state.Sequence + 1, Effect: Effect{Action: "mark", Ref: ref, At: at, SourceSHA: sourceSHA}, CashAfter: a.state.Cash, FeesAfter: a.state.Fees, Mark: new(sample)}
	if err := a.journal(record); err != nil {
		a.uncertain = true
		return 0, fmt.Errorf("RD1 mark journal uncertain: %w", err)
	}
	a.state.Sequence, a.state.At, a.state.Used[ref] = record.Sequence, at, true
	return nav, nil
}
