package researchrd1

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"autowheel/internal/researchcampaign"
	"autowheel/internal/wheel"
)

const ProtocolSHA256 = "87c07e6d9b5bf6ccbf6c7299d47d09c0a50195e3d38e7c76ef1250c04707188f"

type OpeningPolicy struct {
	From      string   `json:"from"`
	Through   string   `json:"through"`
	Symbols   []string `json:"symbols"`
	Profile   Profile  `json:"profile"`
	SourceSHA string   `json:"stock_selection_sha256"`
}

// Terms describes an admitted interval for one exact standard security. The
// source review must establish identity/actions/deliverables for that interval;
// a syntactically valid source hash does not establish those facts by itself.
type Terms struct {
	Symbol     string    `json:"symbol"`
	From       time.Time `json:"from"`
	Through    time.Time `json:"through"`
	KnownAt    time.Time `json:"known_at"`
	Shares     int       `json:"shares"`
	Multiplier int       `json:"multiplier"`
	SourceSHA  string    `json:"source_sha256"`
}

type ScheduledDividend struct {
	ID          string    `json:"id"`
	Symbol      string    `json:"symbol"`
	ExAt        time.Time `json:"ex_at"`
	KnownAt     time.Time `json:"known_at"`
	Amount      int64     `json:"amount_for_100_shares_cents"`
	PayAt       time.Time `json:"pay_at"`
	AvailableAt time.Time `json:"available_at"`
	SourceSHA   string    `json:"source_sha256"`
}

type DeclaredReference struct {
	Symbol string `json:"symbol"`
	Day    string `json:"day"`
	TradeReference
}

type UnsupportedAction struct {
	Symbol    string    `json:"symbol"`
	At        time.Time `json:"at"`
	Reason    string    `json:"reason"`
	SourceSHA string    `json:"source_sha256"`
}

// Packet is the file boundary for an offline replay. AdmissionStatus is a
// required external-review declaration, not a capability to qualify its sources.
// The dispatch/verifier must bind the exact packet bytes and admission receipt.
type Packet struct {
	Schema          string                         `json:"schema"`
	ProtocolSHA     string                         `json:"protocol_sha256"`
	AdmissionStatus string                         `json:"admission_status"`
	AdmissionSHA    string                         `json:"admission_receipt_sha256"`
	Unqualified     []string                       `json:"unqualified_inputs"`
	CalendarSHA     string                         `json:"calendar_source_sha256"`
	ClearingSHA     string                         `json:"clearing_source_sha256"`
	First           string                         `json:"first_session"`
	Last            string                         `json:"last_session"`
	Calendar        []researchcampaign.ExchangeDay `json:"exchange_days"`
	Clearing        []researchcampaign.ClearingDay `json:"clearing_days"`
	Observations    []Observation                  `json:"observations"`
	Policies        []OpeningPolicy                `json:"opening_policies"`
	Tariffs         []Tariff                       `json:"tariffs"`
	Terms           []Terms                        `json:"standard_terms"`
	Dividends       []ScheduledDividend            `json:"dividends"`
	References      []DeclaredReference            `json:"regular_session_references"`
	Unsupported     []UnsupportedAction            `json:"unsupported_actions"`
}

type PacketSource struct {
	packet     Packet
	samples    map[time.Time]Observation
	tariffs    map[string]Tariff
	references map[string]TradeReference
}

func isoDay(value string) bool {
	d, err := time.Parse(time.DateOnly, value)
	return err == nil && d.Format(time.DateOnly) == value
}

// NewPacketSource validates framing. It never changes an unknown admission to
// admitted, defaults a tariff to zero, infers a clearing holiday or repairs marks.
func NewPacketSource(p Packet) (*PacketSource, *researchcampaign.SessionCalendar, *researchcampaign.SettlementCalendar, []researchcampaign.ExchangeDay, error) {
	if p.Schema != "AutoWheelRD1ReplayPacketV1" || p.ProtocolSHA != ProtocolSHA256 || p.AdmissionStatus != "admitted_under_RD1" {
		return nil, nil, nil, nil, errors.New("RD1 packet is unadmitted or outside its frozen envelope")
	}
	return newPacketSource(p, false)
}

func newPacketSource(p Packet, modeledCosts bool) (*PacketSource, *researchcampaign.SessionCalendar, *researchcampaign.SettlementCalendar, []researchcampaign.ExchangeDay, error) {
	if len(p.Unqualified) != 0 || !sourceHash(p.AdmissionSHA) || !sourceHash(p.CalendarSHA) || !sourceHash(p.ClearingSHA) || !isoDay(p.First) || !isoDay(p.Last) || p.First > p.Last || p.First < "2021-01-01" || p.Last > "2026-10-09" || len(p.Observations) > 10000 || len(p.References) > 100000 || len(p.Policies) == 0 || len(p.Policies) > 10 || len(p.Terms) > 1000 || len(p.Dividends) > 2000 || len(p.Unsupported) > 1000 {
		return nil, nil, nil, nil, errors.New("RD1 packet is unadmitted or outside its frozen envelope")
	}
	sessions, err := researchcampaign.NewSessionCalendar(p.Calendar)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	clearing, err := researchcampaign.NewSettlementCalendar(p.Clearing)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	s := &PacketSource{packet: p, samples: map[time.Time]Observation{}, tariffs: map[string]Tariff{}, references: map[string]TradeReference{}}
	var days []researchcampaign.ExchangeDay
	for _, day := range p.Calendar {
		if day.Date >= p.First && day.Date <= p.Last && !day.OpenAt.IsZero() {
			days = append(days, day)
		}
	}
	if len(days) == 0 || days[0].Date != p.First || days[len(days)-1].Date != p.Last {
		return nil, nil, nil, nil, errors.New("RD1 packet path endpoints are not reviewed sessions")
	}
	menuLimit := 10
	if modeledCosts {
		// Quotes for retained names may outlive an annual opening list. Extra
		// source menus do not change the ten-name opening or three-slot limits.
		menuLimit = 50
	}
	for _, obs := range p.Observations {
		if obs.At.IsZero() || !sourceHash(obs.SourceSHA) || len(obs.Stocks) > 50 || len(obs.Options) > 500 || len(obs.Menus) > menuLimit {
			return nil, nil, nil, nil, errors.New("RD1 ambiguous or unbounded observation")
		}
		if _, duplicate := s.samples[obs.At.UTC()]; duplicate {
			return nil, nil, nil, nil, errors.New("RD1 duplicate observation bucket")
		}
		for _, menu := range obs.Menus {
			if len(menu) > 10000 {
				return nil, nil, nil, nil, errors.New("RD1 observed menu exceeds bounded packet")
			}
		}
		s.samples[obs.At.UTC()] = obs
	}
	for i, policy := range p.Policies {
		if !isoDay(policy.From) || !isoDay(policy.Through) || policy.From > policy.Through || len(policy.Symbols) > 10 || policy.Profile.Validate() != nil || !sourceHash(policy.SourceSHA) || i > 0 && policy.From <= p.Policies[i-1].Through {
			return nil, nil, nil, nil, errors.New("RD1 overlapping/invalid frozen opening policies")
		}
	}
	if !modeledCosts && len(p.Tariffs) == 0 || len(p.Tariffs) > 4000 || modeledCosts && len(p.Tariffs) != 0 {
		return nil, nil, nil, nil, errors.New("RD1 explicit dated tariffs required")
	}
	feeDays := make([]researchcampaign.DatedFees, 0, len(p.Tariffs))
	for _, f := range p.Tariffs {
		if _, duplicate := s.tariffs[f.Fees.Date]; duplicate || f.StockBuyFee < 0 || f.StockBuyFee > 10000 {
			return nil, nil, nil, nil, errors.New("RD1 duplicate/invalid stock purchase tariff")
		}
		if err := f.PhysicalStockSale.Validate(); err != nil {
			return nil, nil, nil, nil, err
		}
		s.tariffs[f.Fees.Date] = f
		feeDays = append(feeDays, f.Fees)
	}
	if !modeledCosts {
		if _, err := researchcampaign.NewFeeSchedule(feeDays); err != nil {
			return nil, nil, nil, nil, err
		}
	}
	for _, term := range p.Terms {
		if term.Symbol == "" || term.From.IsZero() || term.Through.Before(term.From) || term.KnownAt.IsZero() || !sourceHash(term.SourceSHA) || term.Shares != 100 || term.Multiplier != 100 {
			return nil, nil, nil, nil, errors.New("RD1 standard terms unknown/unsupported")
		}
	}
	if err := validateDividends(p.Dividends, sessions); err != nil {
		return nil, nil, nil, nil, err
	}
	for _, ref := range p.References {
		key := ref.Symbol + "|" + ref.Day
		if _, duplicate := s.references[key]; duplicate || ref.Symbol == "" || !isoDay(ref.Day) || ref.At.In(wheel.Eastern).Format(time.DateOnly) != ref.Day || ref.PriceMicros <= 0 || ref.PriceMicros > MaxMoney*100 || !sourceHash(ref.SourceSHA) {
			return nil, nil, nil, nil, errors.New("RD1 duplicate/unknown regular-session trade reference")
		}
		s.references[key] = ref.TradeReference
	}
	for _, action := range p.Unsupported {
		if action.Symbol == "" || action.At.IsZero() || action.Reason == "" || !sourceHash(action.SourceSHA) {
			return nil, nil, nil, nil, errors.New("RD1 unsupported action has no exact source declaration")
		}
	}
	return s, sessions, clearing, days, nil
}

func (s *PacketSource) Sample(at time.Time) (Observation, error) {
	value, known := s.samples[at.UTC()]
	if !known {
		return Observation{}, errors.New("RD1 sampled bucket absent")
	}
	return value, nil
}

func (s *PacketSource) EntryUniverse(at time.Time) ([]string, Profile, error) {
	day := at.In(wheel.Eastern).Format(time.DateOnly)
	for _, policy := range s.packet.Policies {
		if policy.From <= day && day <= policy.Through {
			return slices.Clone(policy.Symbols), policy.Profile, nil
		}
	}
	return nil, Profile{}, errors.New("RD1 stock list/opening policy not frozen for date")
}

func (s *PacketSource) Tariff(at time.Time) (Tariff, error) {
	f, known := s.tariffs[at.In(wheel.Eastern).Format(time.DateOnly)]
	if !known {
		return Tariff{}, errors.New("RD1 dated fee schedule absent")
	}
	return f, nil
}

func (s *PacketSource) standardSymbol(symbol string, at time.Time) error {
	for _, action := range s.packet.Unsupported {
		if action.Symbol == symbol && !at.Before(action.At) {
			return fmt.Errorf("RD1 unsupported action: %s", action.Reason)
		}
	}
	matches := 0
	for _, term := range s.packet.Terms {
		if term.Symbol == symbol && !at.Before(term.From) && !at.After(term.Through) && !at.Before(term.KnownAt) {
			matches++
		}
	}
	if matches != 1 {
		return errors.New("RD1 standard security/deliverable interval unknown or ambiguous")
	}
	return nil
}

func (s *PacketSource) Standard(c Contract, at time.Time) error {
	if !validContract(c, c.Symbol, at.In(wheel.Eastern).Format(time.DateOnly)) {
		return errors.New("RD1 exact standard contract identity absent")
	}
	return s.standardSymbol(c.Symbol, at)
}

func (s *PacketSource) Reference(symbol string, day researchcampaign.ExchangeDay) (TradeReference, error) {
	ref, known := s.references[symbol+"|"+day.Date]
	if !known {
		return TradeReference{}, errors.New("RD1 declared last regular-session trade absent")
	}
	return ref, nil
}

func (s *PacketSource) ReviewHoldings(state State, at time.Time) error {
	for symbol := range state.Positions {
		if err := s.standardSymbol(symbol, at); err != nil {
			return err
		}
	}
	return nil
}

func (s *PacketSource) BeforeSession(day researchcampaign.ExchangeDay, state State) ([]Effect, error) {
	if err := s.ReviewHoldings(state, day.OpenAt); err != nil {
		return nil, err
	}
	return dividendEffects(s.packet.Dividends, day, state)
}

// Explicit zero fees are valid; an omitted fee is unknown, not a zero default.
// Duplicate-key rejection belongs to the outer packet decoder as well.
func (t *Tariff) UnmarshalJSON(raw []byte) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return err
	}
	for _, key := range []string{"dated_fees", "stock_buy_fee_cents", "physical_stock_sale"} {
		if len(fields[key]) == 0 || string(fields[key]) == "null" {
			return fmt.Errorf("RD1 missing explicit fee field %s", key)
		}
	}
	var fees map[string]json.RawMessage
	if err := json.Unmarshal(fields["dated_fees"], &fees); err != nil {
		return err
	}
	for _, key := range []string{"date", "known_at", "source_sha256", "option_open_cents", "option_close_cents", "physical_exercise_cents", "stock_sale"} {
		if len(fees[key]) == 0 || string(fees[key]) == "null" {
			return fmt.Errorf("RD1 missing explicit dated fee field %s", key)
		}
	}
	type plain Tariff
	var value plain
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&value); err != nil {
		return err
	}
	*t = Tariff(value)
	return nil
}
