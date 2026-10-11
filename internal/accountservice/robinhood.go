package accountservice

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"maps"
	"math"
	"math/big"
	"strconv"
	"time"

	"autowheel/internal/coordinator"
	"autowheel/internal/robinhood"
	"autowheel/internal/sharedbroker"
	"autowheel/internal/sharedclient"
	"autowheel/internal/wheel"
)

type Robinhood struct {
	Facts   *sharedbroker.Backend
	Options *robinhood.Broker
	// StockFeeBound must be supplied by execution-policy qualification. Missing
	// pre-trade equity fees are never replaced by a guessed zero allowance.
	StockFeeBound func(sharedbroker.Object) (int64, error)
}

func (b *Robinhood) Snapshot(ctx context.Context) (sharedbroker.Facts, error) {
	return b.Facts.Snapshot(ctx)
}
func (b *Robinhood) Underlying(ctx context.Context, symbol string) (string, error) {
	return b.Facts.Underlying(ctx, symbol)
}

type stockPreparation struct {
	Request   json.RawMessage     `json:"request"`
	Arguments sharedbroker.Object `json:"arguments"`
	Ref       string              `json:"ref_id"`
}
type optionPreparation struct {
	Candidate wheel.Candidate `json:"candidate"`
	Ref       string          `json:"ref_id"`
}

func (b *Robinhood) Plan(ctx context.Context, strategy string, payload json.RawMessage) (coordinator.Request, error) {
	if strategy == "autowheel" {
		var p optionPreparation
		if decode(payload, &p) != nil || p.Ref == "" {
			return coordinator.Request{}, errors.New("exact option preparation missing")
		}
		a := p.Candidate
		if a.Kind != "put" && a.Kind != "call" || a.Quantity != 0 && a.Quantity != 1 || a.Strike <= 0 || a.Strike > wheel.MaxMoney/100 {
			return coordinator.Request{}, errors.New("unsupported option order")
		}
		// Re-read real instrument/deliverable facts; client Standard/Tradable flags
		// are never themselves proof that this is a standard 100-share contract.
		quotes, err := b.Options.Quotes(ctx, a.Symbol, a.Kind)
		if err != nil {
			return coordinator.Request{}, err
		}
		matched := false
		for _, q := range quotes {
			if q.ID == a.ID && q.ChainID == a.ChainID && q.Kind == a.Kind && q.Symbol == a.Symbol && q.Expiry == a.Expiry && q.Strike == a.Strike && q.Bid >= a.Bid && q.Standard && q.Tradable {
				matched = true
			}
		}
		if !matched {
			return coordinator.Request{}, errors.New("candidate does not match fresh standard broker contract")
		}
		id, err := b.Underlying(ctx, a.Symbol)
		if err != nil {
			return coordinator.Request{}, err
		}
		r := coordinator.Request{IntentKey: p.Ref, RefID: p.Ref, UnderlyingID: id, Symbol: a.Symbol, Kind: a.Kind, CashCents: int64(b.Options.Config.Fee), QuantityMicros: 1_000_000, Payload: append(json.RawMessage(nil), payload...), Digest: sharedclient.Digest(payload)}
		if a.Kind == "put" {
			r.CapitalCents = int64(a.Strike) * 100
			r.CashCents += r.CapitalCents
		} else {
			r.SharesMicros = 100_000_000
		}
		return r, nil
	}
	var p stockPreparation
	if decode(payload, &p) != nil || p.Ref == "" || !json.Valid(p.Request) {
		return coordinator.Request{}, errors.New("exact stock preparation missing")
	}
	var original struct {
		Key      string      `json:"intent_key"`
		Symbol   string      `json:"symbol"`
		Side     string      `json:"side"`
		Quantity json.Number `json:"quantity"`
		Kind     string      `json:"kind"`
		Limit    json.Number `json:"limit_price"`
		Stop     json.Number `json:"stop_price"`
		TIF      string      `json:"time_in_force"`
	}
	// The original contains strategy-owned tranche/reference fields too. Decode
	// identity separately, while retaining its entire exact JSON in the digest.
	d := json.NewDecoder(bytes.NewReader(p.Request))
	d.UseNumber()
	if d.Decode(&original) != nil {
		return coordinator.Request{}, errors.New("stock request identity missing")
	}
	args := p.Arguments
	if sharedbroker.Text(args, "account_number") != b.Facts.Account || original.Key == "" || original.Symbol != sharedbroker.Text(args, "symbol") || original.Side != sharedbroker.Text(args, "side") || sharedbroker.Text(args, "market_hours") != "regular_hours" {
		return coordinator.Request{}, errors.New("stock review account or exact request mismatch")
	}
	qty, err := sharedbroker.Micros(sharedbroker.Text(args, "quantity"))
	if err != nil || qty <= 0 {
		return coordinator.Request{}, errors.New("exact stock quantity missing")
	}
	if !matchesMicroshares(original.Quantity, qty) {
		return coordinator.Request{}, errors.New("stock quantity loses broker microshare precision")
	}
	orderType := sharedbroker.Text(args, "type")
	tif := original.TIF
	if tif == "day" {
		tif = "gfd"
	}
	if original.Kind != "marketable_limit" && original.Kind != "regular_hours_market" && original.Kind != "protective_stop" || original.Kind == "marketable_limit" && orderType != "limit" || original.Kind == "regular_hours_market" && orderType != "market" || original.Kind == "protective_stop" && orderType != "stop_market" || tif != sharedbroker.Text(args, "time_in_force") {
		return coordinator.Request{}, errors.New("stock execution kind differs from exact request")
	}
	for field, expected := range map[string]json.Number{"limit_price": original.Limit, "stop_price": original.Stop} {
		actual := sharedbroker.Text(args, field)
		if expected == "" && actual == "" {
			continue
		}
		want, ok := new(big.Rat).SetString(expected.String())
		got, err := sharedbroker.Decimal(actual)
		if !ok || err != nil || want.Cmp(got) != 0 {
			return coordinator.Request{}, errors.New("stock price differs from original strategy request")
		}
	}
	if _, set := args["dollar_amount"]; set {
		return coordinator.Request{}, errors.New("dollar market orders have no qualified debit bound")
	}
	if b.StockFeeBound == nil {
		return coordinator.Request{}, errors.New("equity fee policy has not been qualified")
	}
	fees, err := b.StockFeeBound(args)
	if err != nil || fees < 0 {
		return coordinator.Request{}, errors.New("equity fee bound unavailable")
	}
	id, err := b.Underlying(ctx, original.Symbol)
	if err != nil {
		return coordinator.Request{}, err
	}
	r := coordinator.Request{IntentKey: original.Key, RefID: p.Ref, UnderlyingID: id, Symbol: original.Symbol, CashCents: fees, QuantityMicros: qty, Payload: append(json.RawMessage(nil), payload...), Digest: sharedclient.Digest(payload)}
	switch original.Side {
	case "buy":
		if orderType != "limit" || qty%1_000_000 != 0 || sharedbroker.Text(args, "time_in_force") != "gfd" {
			return coordinator.Request{}, errors.New("uncapped fractional or market buy cannot share the cash pool")
		}
		principal, err := sharedbroker.Cost(sharedbroker.Text(args, "quantity"), sharedbroker.Text(args, "limit_price"))
		if err != nil || principal <= 0 || principal > coordinator.MaxCapital-fees {
			return coordinator.Request{}, errors.New("stock maximum debit exceeds qualified capital")
		}
		r.Kind = "stock_buy"
		r.CashCents += principal
		r.CapitalCents = r.CashCents
	case "sell":
		r.Kind = "stock_sell"
		r.SharesMicros = qty
	default:
		return coordinator.Request{}, errors.New("stock side unsupported")
	}
	return r, nil
}

func (b *Robinhood) Review(ctx context.Context, in coordinator.Intent) (json.RawMessage, time.Time, error) {
	if in.Strategy == "autowheel" {
		var p optionPreparation
		if decode(in.Request.Payload, &p) != nil {
			return nil, time.Time{}, errors.New("durable option request invalid")
		}
		review, err := b.Options.ReviewEvidence(ctx, p.Candidate)
		if err != nil {
			return nil, time.Time{}, err
		}
		raw, err := json.Marshal(review)
		expiry := minTime(p.Candidate.At.Add(time.Duration(b.Options.Config.MaxQuoteAgeSeconds)*time.Second), p.Candidate.UnderlyingAt.Add(time.Duration(b.Options.Config.MaxQuoteAgeSeconds)*time.Second))
		return raw, expiry, err
	}
	var p stockPreparation
	if decode(in.Request.Payload, &p) != nil {
		return nil, time.Time{}, errors.New("durable stock request invalid")
	}
	result, err := b.Facts.Client.Call(ctx, "review_equity_order", p.Arguments)
	if err != nil {
		return nil, time.Time{}, err
	}
	review, err := sharedbroker.Data(result)
	if err != nil {
		return nil, time.Time{}, err
	}
	checks, ok := review["order_checks"].(map[string]any)
	if !ok || len(checks) != 0 || sharedbroker.Text(review, "symbol") != in.Request.Symbol || sharedbroker.Text(review, "side") != sharedbroker.Text(p.Arguments, "side") || !sameDecimal(sharedbroker.Text(review, "quantity"), sharedbroker.Text(p.Arguments, "quantity")) || sharedbroker.Text(review, "type") != sharedbroker.Text(p.Arguments, "type") {
		return nil, time.Time{}, errors.New("stock review has alerts or mismatched exact identity")
	}
	for _, field := range []string{"limit_price", "stop_price"} {
		if expected := sharedbroker.Text(p.Arguments, field); expected != "" && !sameDecimal(sharedbroker.Text(review, field), expected) {
			return nil, time.Time{}, errors.New("stock review price differs from exact request")
		}
	}
	quote, ok := review["quote_data"].(map[string]any)
	if !ok || sharedbroker.Text(quote, "symbol") != in.Request.Symbol {
		return nil, time.Time{}, errors.New("stock review quote missing")
	}
	at, err := time.Parse(time.RFC3339Nano, sharedbroker.Text(quote, "venue_last_trade_time"))
	if err != nil || time.Since(at) > 60*time.Second || time.Until(at) > 2*time.Second {
		return nil, time.Time{}, errors.New("stock review quote stale")
	}
	raw, err := json.Marshal(result)
	return raw, at.Add(60 * time.Second), err
}
func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}
func (b *Robinhood) PlaceOnce(ctx context.Context, in coordinator.Intent) (string, json.RawMessage, error) {
	if in.Strategy == "autowheel" {
		var p optionPreparation
		if decode(in.Request.Payload, &p) != nil {
			return "", nil, errors.New("durable option request invalid")
		}
		id, result, err := b.Options.SubmitEvidence(ctx, p.Candidate, in.Request.RefID)
		if err != nil {
			return "", nil, err
		}
		raw, err := json.Marshal(result)
		return id, raw, err
	}
	var p stockPreparation
	if decode(in.Request.Payload, &p) != nil {
		return "", nil, errors.New("durable stock request invalid")
	}
	args := maps.Clone(p.Arguments)
	args["ref_id"] = in.Request.RefID
	result, err := b.Facts.Client.Call(ctx, "place_equity_order", args)
	if err != nil {
		return "", nil, err
	}
	data, err := sharedbroker.Data(result)
	if err != nil {
		return "", nil, err
	}
	o, ok := data["order"].(map[string]any)
	if !ok {
		return "", nil, errors.New("stock placement receipt order missing")
	}
	id := sharedbroker.Text(o, "id")
	qty, err := sharedbroker.Micros(sharedbroker.Text(o, "quantity"))
	if err != nil || qty != in.Request.QuantityMicros || id == "" || sharedbroker.Text(o, "symbol") != in.Request.Symbol || sharedbroker.Text(o, "side") != sharedbroker.Text(args, "side") || sharedbroker.Text(o, "placed_agent") != "agentic" || sharedbroker.Text(o, "instrument_id") != in.Request.UnderlyingID {
		return "", nil, errors.New("stock placement receipt identity mismatch")
	}
	if ref := sharedbroker.Text(o, "ref_id"); ref != "" && ref != in.Request.RefID {
		return "", nil, errors.New("stock placement receipt reference mismatch")
	}
	wantType, trigger := sharedbroker.Text(args, "type"), "immediate"
	if wantType == "stop_market" {
		wantType, trigger = "market", "stop"
	}
	if sharedbroker.Text(o, "type") != wantType || sharedbroker.Text(o, "trigger") != trigger || sharedbroker.Text(o, "market_hours") != sharedbroker.Text(args, "market_hours") || sharedbroker.Text(o, "time_in_force") != sharedbroker.Text(args, "time_in_force") {
		return "", nil, errors.New("stock receipt execution terms differ from prepared order")
	}
	for requestField, receiptField := range map[string]string{"limit_price": "price", "stop_price": "stop_price"} {
		want, got := sharedbroker.Text(args, requestField), sharedbroker.Text(o, receiptField)
		// A market order may carry a broker reference price even though no
		// limit was requested. Its trigger, session and type still bind exactly.
		if requestField == "limit_price" && want == "" && wantType == "market" {
			continue
		}
		if want != "" && !sameDecimal(want, got) || want == "" && o[receiptField] != nil {
			return "", nil, errors.New("stock receipt price differs from prepared order")
		}
	}
	raw, err := json.Marshal(result)
	return id, raw, err
}

func sameDecimal(a, b string) bool {
	x, err := sharedbroker.Decimal(a)
	if err != nil {
		return false
	}
	y, err := sharedbroker.Decimal(b)
	return err == nil && x.Cmp(y) == 0
}

// TradePulse's persisted f64 quantities sometimes carry arithmetic residue.
// Accept only an exact microshare quantity or at most two binary ULPs of
// residue, strictly below half a microshare. The broker and ledger still use
// the exact integer quantity; this cannot conceal a one-microshare change.
func matchesMicroshares(number json.Number, quantity int64) bool {
	x, ok := new(big.Rat).SetString(number.String())
	if !ok || quantity <= 0 {
		return false
	}
	y := big.NewRat(quantity, 1_000_000)
	if x.Cmp(y) == 0 {
		return true
	}
	v, err := strconv.ParseFloat(number.String(), 64)
	if err != nil || math.IsInf(v, 0) || math.IsNaN(v) || v <= 0 {
		return false
	}
	tolerance := new(big.Rat).SetFloat64(2 * (math.Nextafter(v, math.Inf(1)) - v))
	if tolerance == nil || tolerance.Cmp(big.NewRat(1, 2_000_000)) >= 0 {
		return false
	}
	difference := new(big.Rat).Sub(x, y)
	return difference.Abs(difference).Cmp(tolerance) <= 0
}
func (b *Robinhood) CancelOnce(ctx context.Context, in coordinator.Intent) (json.RawMessage, error) {
	if in.Strategy != "tradepulse" || in.Request.Kind != "cancel" || in.Request.OriginalOrderID == "" {
		return nil, errors.New("stock cancellation capability denied")
	}
	result, err := b.Facts.Client.Call(ctx, "cancel_equity_order", sharedbroker.Object{"account_number": b.Facts.Account, "order_id": in.Request.OriginalOrderID})
	if err != nil {
		return nil, err
	}
	o, err := sharedbroker.Data(result)
	if err != nil {
		return nil, err
	}
	if _, ok := o["accepted"].(bool); !ok {
		return nil, errors.New("cancel acknowledgment missing")
	}
	return json.Marshal(result)
}
