package moomoo

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"strings"
	"time"

	"autowheel/internal/wheel"
)

// OrderIntentTerms must be reconstructed from a durably saved intent, never
// from an order found in broker history. Side is the exact prepared broker side;
// this type does not infer an opening/closing effect or contract economics.
type OrderIntentTerms struct {
	AccountID   string      `json:"account_id"`
	Ref         string      `json:"ref_id"`
	OrderID     string      `json:"order_id,omitempty"`
	Code        string      `json:"code"`
	Side        string      `json:"side"`
	Quantity    int         `json:"quantity"`
	LimitCents  wheel.Cents `json:"limit_cents"`
	TimeInForce string      `json:"time_in_force,omitempty"`
	CreatedAt   time.Time   `json:"created_at"`
}

func (t OrderIntentTerms) validate(now time.Time) error {
	if !validAccountID(t.AccountID) || !validSubmissionRef(t.Ref) || !validUSOptionCode(t.Code) ||
		!slices.Contains([]string{"BUY", "SELL", "SELL_SHORT", "BUY_BACK"}, t.Side) ||
		!slices.Contains([]string{"", "DAY", "GTC"}, t.TimeInForce) ||
		t.Quantity != 1 || t.LimitCents <= 0 || t.LimitCents > wheel.MaxMoney/100 ||
		t.CreatedAt.Before(time.Date(2000, time.January, 1, 0, 0, 0, 0, time.UTC)) || t.CreatedAt.After(now) ||
		len(t.OrderID) > 128 || strings.TrimSpace(t.OrderID) != t.OrderID {
		return errors.New("invalid durable Moomoo order terms")
	}
	return nil
}

// Empty preserves existing durable DAY intents. A resting target must declare
// GTC explicitly; duration never comes from the broker receipt being recovered.
func (t OrderIntentTerms) timeInForce() string {
	if t.TimeInForce == "" {
		return "DAY"
	}
	return t.TimeInForce
}

func validSubmissionRef(ref string) bool {
	if len(ref) != 36 || ref[14] != '4' || !strings.ContainsRune("89ab", rune(ref[19])) {
		return false
	}
	for i, char := range ref {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if char != '-' {
				return false
			}
		} else if !strings.ContainsRune("0123456789abcdef", char) {
			return false
		}
	}
	return true
}

func validUSOptionCode(code string) bool {
	if !strings.HasPrefix(code, "US.") || len(code) < 5 || len(code) > 128 {
		return false
	}
	hasDigit := false
	for _, char := range code[3:] {
		if char >= '0' && char <= '9' {
			hasDigit = true
		} else if !(char >= 'A' && char <= 'Z' || char == '.' || char == '-') {
			return false
		}
	}
	return hasDigit // SecurityType in the broker row must independently prove OPTION.
}

// receiptClockSkew matches the reconciliation allowance between this host's
// intent clock and Moomoo's order creation clock.
const receiptClockSkew = 2 * time.Second

var errReceiptAbsent = errors.New("Moomoo submission reference not found; outcome remains unknown, retain intent and do not resubmit")

// ResolveOrderReceipt makes only fresh, scoped read queries. A failed or absent
// match always leaves the intent unresolved. It never authorizes resubmission,
// creates an execution snapshot, or proves lifecycle ownership/settlement.
func (c *Client) ResolveOrderReceipt(ctx context.Context, want OrderIntentTerms) (OrderEvidence, error) {
	if err := want.validate(time.Now()); err != nil {
		return OrderEvidence{}, err
	}
	audit, err := c.AuditOrders(ctx, want.AccountID, want.CreatedAt)
	if err != nil {
		return OrderEvidence{}, err
	}
	return MatchOrderReceipt(audit, want, time.Now())
}

// MatchOrderReceipt recognizes one exact account/reference/terms match in a
// completed audit. The returned provider evidence is not a wheel.Order: opening
// effect, fees, deliverables, fills, settlement and ownership remain unqualified.
func MatchOrderReceipt(audit *OrderAudit, want OrderIntentTerms, now time.Time) (OrderEvidence, error) {
	if err := want.validate(now); err != nil {
		return OrderEvidence{}, err
	}
	if audit == nil || audit.AccountID != want.AccountID || audit.Market != "US" ||
		audit.At.IsZero() || audit.At.After(now) || now.Sub(audit.At) > 30*time.Second ||
		audit.HistoryFrom.IsZero() || audit.HistoryFrom.After(want.CreatedAt) ||
		!audit.HistoryTo.Equal(audit.At) || audit.HistoryTo.Before(want.CreatedAt) ||
		audit.RecentPages < 1 || audit.RecentPages > 64 || audit.HistoryPages < 1 || audit.HistoryPages > 64 ||
		audit.Recent == nil || audit.History == nil || len(audit.Recent) > 6400 || len(audit.History) > 6400 {
		return OrderEvidence{}, errors.New("Moomoo receipt recovery requires a fresh, complete account-bound audit covering the intent")
	}
	var found *OrderEvidence
	var foundTerms receiptTerms
	totalBytes := 0
	for _, group := range [][]OrderEvidence{audit.Recent, audit.History} {
		seen := map[string]bool{}
		for _, evidence := range group {
			totalBytes += len(evidence.Raw)
			if totalBytes > 32<<20 || len(evidence.Raw) == 0 || seen[evidence.ID] {
				return OrderEvidence{}, errors.New("Moomoo receipt audit is oversized, missing raw evidence or contains duplicate IDs within a query")
			}
			seen[evidence.ID] = true
			var row receiptRow
			if exactJSON(evidence.Raw, &row) != nil || !row.agrees(evidence) {
				return OrderEvidence{}, errors.New("Moomoo receipt audit summary differs from its original provider evidence")
			}
			if row.Remark == nil || *row.Remark != want.Ref {
				continue
			}
			terms, err := row.match(want, audit.HistoryTo, now)
			if err != nil {
				return OrderEvidence{}, err
			}
			if found != nil && (found.ID != evidence.ID || foundTerms != terms) {
				return OrderEvidence{}, errors.New("Moomoo submission reference is duplicated or recent/history receipts conflict; retain intent, do not resubmit")
			}
			copy := evidence
			copy.Raw = bytes.Clone(evidence.Raw)
			copy.Remark = new(*evidence.Remark)
			found, foundTerms = &copy, terms
		}
	}
	if found == nil {
		return OrderEvidence{}, errReceiptAbsent
	}
	return *found, nil
}

type receiptRow struct {
	ID           string          `json:"order_id"`
	Code         string          `json:"code"`
	Side         string          `json:"side"`
	Type         string          `json:"order_type"`
	Status       string          `json:"order_status"`
	Remark       *string         `json:"remark"`
	Created      json.Number     `json:"create_time"`
	Updated      json.Number     `json:"updated_time"`
	SecurityType string          `json:"security_type"`
	Exchange     string          `json:"exchange"`
	Currency     string          `json:"currency"`
	Quantity     string          `json:"qty"`
	Price        string          `json:"price"`
	Dealt        *string         `json:"dealt_qty"`
	Average      *string         `json:"dealt_avg_price"`
	TIF          string          `json:"time_in_force"`
	Session      string          `json:"session"`
	OrderClass   string          `json:"order_class"`
	AuxPrice     *string         `json:"aux_price"`
	TrailType    string          `json:"trail_type"`
	TrailValue   *string         `json:"trail_value"`
	TrailSpread  *string         `json:"trail_spread"`
	Expiry       json.RawMessage `json:"expire_time"`
	MultiLeg     json.RawMessage `json:"multi_leg_info"`
	Algo         json.RawMessage `json:"algo_info"`
	Extra        json.RawMessage `json:"extra_info"`
	PositionOpen string          `json:"position_open_type"`
	SubAccount   string          `json:"sub_acc_id"`
	PositionID   string          `json:"position_id"`
	QuoteID      string          `json:"quote_id"`
	RFQID        string          `json:"rfq_id"`
	MarginType   string          `json:"margin_trading_type"`
	MarginPeriod string          `json:"margin_period_type"`
}

func (r receiptRow) agrees(e OrderEvidence) bool {
	created, err := r.Created.Int64()
	updated, updateErr := r.Updated.Int64()
	return err == nil && updateErr == nil && r.ID != "" && len(r.ID) <= 128 && strings.TrimSpace(r.ID) == r.ID && r.ID == e.ID && r.Code == e.Code && r.Side == e.Side &&
		r.Type == e.Type && r.Status == e.Status && sameRemark(r.Remark, e.Remark) &&
		time.UnixMicro(created).Equal(e.CreatedAt) && time.UnixMicro(updated).Equal(e.UpdatedAt)
}

// sameRemark compares two optional provider strings: both absent or equal.
func sameRemark(a, b *string) bool { return a == nil && b == nil || a != nil && b != nil && *a == *b }

type receiptTerms struct {
	Status  string
	Created int64
	Updated int64
	Price   wheel.Cents
	Filled  int64
	Average wheel.Cents
}

func (r receiptRow) match(want OrderIntentTerms, through, now time.Time) (receiptTerms, error) {
	created, _ := r.Created.Int64()
	updated, _ := r.Updated.Int64()
	price, err := wheel.ParseCents(r.Price)
	quantity, quantityErr := receiptContracts(r.Quantity)
	if err != nil || price != want.LimitCents || quantityErr != nil || quantity != 1 ||
		r.Code != want.Code || r.Side != want.Side || r.Type != "LIMIT" || r.SecurityType != "OPTION" || r.Exchange != "US" || r.Currency != "USD" ||
		r.TIF != want.timeInForce() || r.Session != "RTH" || want.OrderID != "" && r.ID != want.OrderID ||
		time.UnixMicro(created).Before(want.CreatedAt.Add(-receiptClockSkew)) || time.UnixMicro(created).After(through) || updated < created || time.UnixMicro(updated).After(now) ||
		!slices.Contains(append([]string{"WAITING_SUBMIT", "SUBMITTING", "SUBMITTED", "FILLED_PART"}, terminalOrderStatuses...), r.Status) {
		return receiptTerms{}, errors.New("Moomoo reference matched different, missing or unqualified order terms; retain intent")
	}
	if r.OrderClass != "" || r.PositionOpen != "" || r.SubAccount != "" || r.PositionID != "" || r.QuoteID != "" || r.RFQID != "" || r.MarginType != "" || r.MarginPeriod != "" ||
		r.TrailType != "" && r.TrailType != "NONE" || !inactiveDecimal(r.AuxPrice) || !inactiveDecimal(r.TrailValue) || !inactiveDecimal(r.TrailSpread) ||
		!inactiveObject(r.MultiLeg) || !inactiveObject(r.Algo) || !inactiveExpiry(r.Expiry) || !inactiveExtra(r.Extra) {
		return receiptTerms{}, errors.New("Moomoo receipt contains conditional, multi-leg, algorithm or other unsupported order instructions")
	}
	if r.Dealt == nil || r.Average == nil {
		return receiptTerms{}, errors.New("Moomoo receipt fill facts are unavailable")
	}
	filled, err := receiptContracts(*r.Dealt)
	if err != nil {
		return receiptTerms{}, errors.New("Moomoo receipt fill quantity is not a whole single contract")
	}
	var average wheel.Cents
	if *r.Average != "" {
		average, err = wheel.ParseCents(*r.Average)
		if err != nil {
			return receiptTerms{}, errors.New("Moomoo receipt fill price is not exact cents")
		}
	}
	if r.Status == "FILLED_ALL" && (filled != 1 || average <= 0 || average > wheel.MaxMoney/100) ||
		r.Status != "FILLED_ALL" && (filled != 0 || average != 0) || r.Status == "FILLED_PART" || r.Status == "CANCELLED_PART" {
		return receiptTerms{}, errors.New("Moomoo single-contract receipt status and fill facts conflict")
	}
	if filled == 1 && ((r.Side == "BUY" || r.Side == "BUY_BACK") && average > price || (r.Side == "SELL" || r.Side == "SELL_SHORT") && average < price) {
		return receiptTerms{}, errors.New("Moomoo fill violates its limit price; retain intent")
	}
	return receiptTerms{Status: r.Status, Created: created, Updated: updated, Price: price, Filled: filled, Average: average}, nil
}

func receiptContracts(value string) (int64, error) {
	whole, fraction, dot := strings.Cut(value, ".")
	if whole == "" || len(fraction) > 12 || dot && fraction == "" || strings.Trim(fraction, "0") != "" {
		return 0, errors.New("not a whole contract count")
	}
	for _, digit := range whole {
		if digit < '0' || digit > '9' {
			return 0, errors.New("invalid contract count")
		}
	}
	n, err := strconv.ParseInt(whole, 10, 64)
	if err != nil || n > 1 {
		return 0, errors.New("contract count exceeds one")
	}
	return n, nil
}

func inactiveDecimal(s *string) bool {
	if s == nil || *s == "" {
		return true
	}
	cents, err := wheel.ParseCents(*s)
	return err == nil && cents == 0
}

func inactiveObject(raw json.RawMessage) bool {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return true
	}
	var fields map[string]json.RawMessage
	return exactJSON(raw, &fields) == nil && len(fields) == 0
}

func inactiveExpiry(raw json.RawMessage) bool {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return true
	}
	var value json.Number
	if exactJSON(raw, &value) != nil {
		return false
	}
	n, err := value.Int64()
	return err == nil && n == 0
}

func inactiveExtra(raw json.RawMessage) bool {
	if inactiveObject(raw) {
		return true
	}
	var extra map[string]json.RawMessage
	return len(raw) <= 1024 && exactJSON(raw, &extra) == nil && len(extra) == 1 && bytes.Equal(bytes.TrimSpace(extra["is_ec_combo"]), []byte("false"))
}
