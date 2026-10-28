package moomoo

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"time"

	"autowheel/internal/wheel"
)

// FeeRecord is one provider order-fee document for a filled one-contract
// option order of this account, kept with its exact source bytes.
type FeeRecord struct {
	OrderID string      `json:"order_id"`
	Code    string      `json:"code"`
	Side    string      `json:"side"`
	Limit   wheel.Cents `json:"limit_cents"`
	Total   wheel.Cents `json:"total_fee_cents"`
	Bound   wheel.Cents `json:"reviewed_bound_cents"`
	Bytes   []byte      `json:"source_bytes"`
}

// FeeEvidence binds the reviewed fee schedule to actual account records.
type FeeEvidence struct {
	AccountID  string      `json:"account_id"`
	At         time.Time   `json:"at"`
	Schedule   string      `json:"schedule"`
	Records    []FeeRecord `json:"records"`
	SHA256     string      `json:"sha256"`
	MaxSeen    wheel.Cents `json:"max_observed_cents"`
	BudgetUsed wheel.Cents `json:"option_fee_budget_cents"`
}

const feeSchedule = "per one-contract order: commission 199, platform 30, contract 65, ORF<=3, OCC<=3, CAT<=1; sales add TAF<=1 and ceil(SEC 20.60/million of premium); delivery: ceil(SEC on strike notional)+3"

func (c *Client) orderFee(ctx context.Context, account, orderID string) (wheel.Cents, []byte, error) {
	raw, err := c.readGET(ctx, "/api/v1.0/accounts/"+account+"/orders/"+orderID+"/fees", nil)
	if err != nil {
		return 0, nil, err
	}
	var doc struct {
		OrderID  string `json:"order_id"`
		Currency string `json:"currency"`
		Total    string `json:"total_fee"`
	}
	if exactJSON(raw, &doc) != nil || doc.OrderID != orderID || doc.Currency != "USD" {
		return 0, nil, errors.New("Moomoo order fee document is missing or ambiguous")
	}
	total, err := wheel.ParseCents(doc.Total)
	if err != nil {
		return 0, nil, errors.New("Moomoo order fee is not exact cents")
	}
	return total, bytes.Clone(raw), nil
}

// CollectFeeEvidence reads the actual fees of this account's most recent
// filled one-contract option orders and requires each to fit the reviewed
// bound and the configured budget. A schedule change fails closed.
func (c *Client) CollectFeeEvidence(ctx context.Context, account string, orders []OrderEvidence, budget wheel.Cents) (*FeeEvidence, error) {
	evidence := &FeeEvidence{AccountID: account, At: time.Now().UTC(), Schedule: feeSchedule, BudgetUsed: budget}
	rows := slices.Clone(orders)
	slices.SortFunc(rows, func(a, b OrderEvidence) int { return b.CreatedAt.Compare(a.CreatedAt) })
	seen := map[string]bool{} // /orders and history both list fills from the last 24 hours.
	for _, e := range rows {
		if len(evidence.Records) == 6 {
			break
		}
		if seen[e.ID] {
			continue
		}
		seen[e.ID] = true
		var row receiptRow
		if e.Status != "FILLED_ALL" || e.Remark == nil || *e.Remark == "" || exactJSON(e.Raw, &row) != nil || row.SecurityType != "OPTION" ||
			row.Side != "SELL_SHORT" && row.Side != "BUY_BACK" {
			continue
		}
		if contracts, err := receiptContracts(row.Quantity); err != nil || contracts != 1 {
			continue
		}
		limit, err := wheel.ParseCents(row.Price)
		if err != nil || limit <= 0 || row.Average == nil {
			continue
		}
		if fill, err := wheel.ParseCents(*row.Average); err == nil && fill > limit && row.Side == "SELL_SHORT" {
			limit = fill // SEC is charged on actual sale proceeds.
		}
		total, raw, err := c.orderFee(ctx, account, e.ID)
		if err != nil {
			return nil, err
		}
		bound := optionFeeBound(row.Side == "SELL_SHORT", limit)
		if total > bound || total > budget || bound > budget {
			return nil, errors.New("an actual Moomoo option fee exceeds the reviewed schedule or budget")
		}
		evidence.MaxSeen = max(evidence.MaxSeen, total)
		evidence.Records = append(evidence.Records, FeeRecord{OrderID: e.ID, Code: row.Code, Side: row.Side, Limit: limit, Total: total, Bound: bound, Bytes: raw})
	}
	if len(evidence.Records) < 2 {
		return nil, errors.New("too few filled one-contract option orders to verify Moomoo fees")
	}
	raw, err := json.Marshal(struct {
		Schedule string      `json:"schedule"`
		Records  []FeeRecord `json:"records"`
	}{evidence.Schedule, evidence.Records})
	if err != nil {
		return nil, err
	}
	hash := sha256.Sum256(raw)
	evidence.SHA256 = hex.EncodeToString(hash[:])
	return evidence, nil
}

// FullOrderAudit returns a complete audit only when every order since 2000 is
// terminal. Its time becomes the cutoff; any later order is visible in history.
func (c *Client) FullOrderAudit(ctx context.Context, account string) (*OrderAudit, error) {
	audit, err := c.AuditOrders(ctx, account, time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		return nil, err
	}
	for _, group := range [][]OrderEvidence{audit.Recent, audit.History} {
		for _, o := range group {
			if !slices.Contains(terminalOrderStatuses, o.Status) || o.UpdatedAt.After(audit.At) {
				return nil, errors.New("Moomoo account has an uncompleted or changing order; cancel or resolve it before binding execution")
			}
		}
	}
	return audit, nil
}
