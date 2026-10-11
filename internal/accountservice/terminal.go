package accountservice

import (
	"errors"
	"math/big"
	"time"

	"autowheel/internal/coordinator"
	"autowheel/internal/sharedbroker"
)

// Terminal observations are cumulative. Each accepted stock order is applied
// once, including fills that raced a cancel. Sale proceeds remain excluded
// until separate settlement evidence qualifies a credit.
func (s *Server) reconcileTerminalStocks(f sharedbroker.Facts) error {
	st, err := s.Ledger.Status()
	if err != nil {
		return err
	}
	if !st.Initialized {
		return nil
	}
	orders := make(map[string]sharedbroker.Object)
	for _, order := range f.Orders {
		id := sharedbroker.Text(order, "id")
		if id == "" || orders[id] != nil {
			return errors.New("stock order identity is missing or duplicated")
		}
		orders[id] = order
	}
	for _, in := range st.Intents {
		if in.Strategy != "tradepulse" || in.State != "accepted" && in.State != "terminal" || in.Request.Kind != "stock_buy" && in.Request.Kind != "stock_sell" {
			continue
		}
		order := orders[in.BrokerOrderID]
		if order == nil || !sharedbroker.Terminal(sharedbroker.Text(order, "state")) {
			continue
		}
		filled, debit, err := stockTerminal(in, order, f.At)
		if err != nil {
			return err
		}
		if in.State == "terminal" {
			if filled != in.FilledMicros || debit != in.DebitCents {
				return errors.New("previously reconciled terminal stock economics changed")
			}
			continue
		}
		current, err := s.Ledger.Status()
		if err != nil {
			return err
		}
		var owner coordinator.Ownership
		found := false
		for _, o := range current.Owners {
			if o.UnderlyingID == in.Request.UnderlyingID {
				owner = o
				found = true
			}
		}
		if !found || owner.Strategy != in.Strategy {
			return errors.New("terminal stock fill lost its durable owner")
		}
		if in.Request.Kind == "stock_buy" {
			owner.QuantityMicros += filled
			owner.CostCents += debit
		} else {
			if filled > owner.QuantityMicros {
				return errors.New("terminal sell exceeds owned stock")
			}
			owner.CostCents, err = coordinator.RemainingCost(owner.CostCents, owner.QuantityMicros, owner.QuantityMicros-filled)
			if err != nil {
				return err
			}
			owner.QuantityMicros -= filled
		}
		if err = s.Ledger.ApplyTerminal(in.Strategy, in.Request.IntentKey, coordinator.TerminalProof{BrokerOrderID: in.BrokerOrderID, EvidenceDigest: f.Digest, FilledMicros: filled, DebitCents: debit, Ownership: owner}); err != nil {
			return err
		}
	}
	for _, in := range st.Intents {
		if in.Request.Kind != "cancel" || in.State != "cancel_pending" {
			continue
		}
		order := orders[in.Request.OriginalOrderID]
		if order == nil || !sharedbroker.Terminal(sharedbroker.Text(order, "state")) {
			continue
		}
		// The original terminal fill above must have been applied successfully.
		current, err := s.Ledger.Status()
		if err != nil {
			return err
		}
		terminal := false
		for _, original := range current.Intents {
			if original.Strategy == in.Strategy && original.BrokerOrderID == in.Request.OriginalOrderID && original.State == "terminal" {
				terminal = true
			}
		}
		if !terminal {
			return errors.New("cancel terminal evidence precedes original fill accounting")
		}
		if err = s.Ledger.ConfirmCancelTerminal(in.Strategy, in.Request.IntentKey, in.Request.OriginalOrderID, f.Digest); err != nil {
			return err
		}
	}
	return nil
}

func stockTerminal(in coordinator.Intent, order sharedbroker.Object, observed time.Time) (int64, int64, error) {
	var p stockPreparation
	if decode(in.Request.Payload, &p) != nil {
		return 0, 0, errors.New("durable stock terminal request missing")
	}
	args := p.Arguments
	qty, err := sharedbroker.Micros(sharedbroker.Text(order, "quantity"))
	filled, ferr := sharedbroker.Micros(sharedbroker.Text(order, "cumulative_quantity"))
	if err != nil || ferr != nil || qty != in.Request.QuantityMicros || filled > qty || sharedbroker.Text(order, "id") != in.BrokerOrderID || sharedbroker.Text(order, "instrument_id") != in.Request.UnderlyingID || sharedbroker.Text(order, "symbol") != in.Request.Symbol || sharedbroker.Text(order, "side") != sharedbroker.Text(args, "side") || sharedbroker.Text(order, "placed_agent") != "agentic" || sharedbroker.Text(order, "state") == "filled" && filled != qty {
		return 0, 0, errors.New("terminal stock receipt differs from exact intent")
	}
	if ref := sharedbroker.Text(order, "ref_id"); ref != "" && ref != in.Request.RefID {
		return 0, 0, errors.New("terminal stock reference changed")
	}
	fees, err := sharedbroker.Decimal(sharedbroker.Text(order, "fees"))
	if err != nil {
		return 0, 0, errors.New("terminal stock fees unavailable")
	}
	created, err := time.Parse(time.RFC3339Nano, sharedbroker.Text(order, "created_at"))
	if err != nil {
		return 0, 0, errors.New("stock creation timestamp missing")
	}
	gross := new(big.Rat)
	executionFees := new(big.Rat)
	var cumulative int64
	seen := make(map[string]bool)
	executions, ok := order["executions"].([]any)
	if !ok && filled > 0 {
		return 0, 0, errors.New("filled stock execution evidence missing")
	}
	for _, value := range executions {
		x, ok := value.(map[string]any)
		id := sharedbroker.Text(x, "id")
		q, err := sharedbroker.Micros(sharedbroker.Text(x, "quantity"))
		price, perr := sharedbroker.Decimal(sharedbroker.Text(x, "price"))
		fee, feerr := sharedbroker.Decimal(sharedbroker.Text(x, "fees"))
		at, terr := time.Parse(time.RFC3339Nano, sharedbroker.Text(x, "timestamp"))
		if !ok || id == "" || seen[id] || err != nil || q <= 0 || q > qty-cumulative || perr != nil || price.Sign() <= 0 || feerr != nil || terr != nil || at.Before(created) || at.After(observed.Add(2*time.Second)) {
			return 0, 0, errors.New("stock execution identity, amount or timing is ambiguous")
		}
		if in.Request.Kind == "stock_buy" {
			limit, err := sharedbroker.Decimal(sharedbroker.Text(args, "limit_price"))
			if err != nil || price.Cmp(limit) > 0 {
				return 0, 0, errors.New("stock fill exceeded its debit-capped limit")
			}
		}
		seen[id] = true
		cumulative += q
		gross.Add(gross, new(big.Rat).Mul(price, big.NewRat(q, 1_000_000)))
		executionFees.Add(executionFees, fee)
	}
	if cumulative != filled || fees.Cmp(executionFees) != 0 {
		return 0, 0, errors.New("cumulative stock executions or fees disagree")
	}
	debit := fees
	if in.Request.Kind == "stock_buy" {
		debit = new(big.Rat).Add(gross, fees)
	}
	cents := new(big.Rat).Mul(debit, big.NewRat(100, 1))
	n, r := new(big.Int), new(big.Int)
	n.QuoRem(cents.Num(), cents.Denom(), r)
	if r.Sign() > 0 {
		n.Add(n, big.NewInt(1))
	}
	if !n.IsInt64() || n.Int64() < 0 || n.Int64() > in.Request.CashCents {
		return 0, 0, errors.New("terminal debit exceeded reserved broker bound")
	}
	return filled, n.Int64(), nil
}
