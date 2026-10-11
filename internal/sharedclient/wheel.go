package sharedclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"autowheel/internal/robinhood"
	"autowheel/internal/sharedbroker"
	"autowheel/internal/wheel"
)

type ownedClient struct {
	Market *robinhood.Client
	Owned  sharedbroker.Object
}

func (c *ownedClient) Supports(name, key string) bool { return c.Market.Supports(name, key) }
func (c *ownedClient) Call(ctx context.Context, name string, args map[string]any) (any, error) {
	fields := map[string]string{"get_accounts": "accounts", "get_portfolio": "portfolio", "get_equity_positions": "positions", "get_equity_orders": "orders", "get_option_positions": "option_positions", "get_option_orders": "option_orders"}
	if field, ok := fields[name]; ok {
		if cursor, _ := args["cursor"].(string); cursor != "" {
			return nil, errors.New("owned view is already fully paginated")
		}
		v, present := c.Owned[field]
		if !present {
			return nil, errors.New("coordinator owned view is incomplete")
		}
		return v, nil
	}
	if name == "place_option_order" || name == "place_equity_order" || name == "cancel_equity_order" {
		return nil, errors.New("direct financial transport disabled in shared mode")
	}
	return c.Market.Call(ctx, name, args)
}
func (c Client) WheelSnapshot(ctx context.Context, market *robinhood.Client, cfg wheel.Config) (wheel.Snapshot, *robinhood.AccountInfo, error) {
	var view View
	if err := c.Call(ctx, "view", map[string]any{}, &view); err != nil {
		return wheel.Snapshot{}, nil, err
	}
	if err := c.ValidateView(view, Digest([]byte(cfg.Account)), time.Now()); err != nil {
		return wheel.Snapshot{}, nil, err
	}
	var full sharedbroker.Facts
	if err := json.Unmarshal([]byte(view.FullJSON), &full); err != nil || full.AccountHash != view.Evidence.AccountHash {
		return wheel.Snapshot{}, nil, errors.New("complete account proof invalid")
	}
	var owned sharedbroker.Object
	if err := json.Unmarshal([]byte(view.OwnedJSON), &owned); err != nil {
		return wheel.Snapshot{}, nil, errors.New("owned account projection invalid")
	}
	if err := sharedbroker.ValidateProjection(full, owned); err != nil {
		return wheel.Snapshot{}, nil, err
	}
	b := robinhood.Broker{MCP: &ownedClient{Market: market, Owned: owned}, Config: cfg}
	snapshot, err := b.Snapshot(ctx)
	if err != nil {
		return snapshot, b.AccountInfo, err
	}
	var cash struct {
		Available   int64    `json:"available_cash_cents"`
		PeerCapital int64    `json:"peer_capital_cents"`
		Peers       []string `json:"peer_symbols"`
	}
	if err = json.Unmarshal([]byte(view.OwnedJSON), &cash); err != nil || cash.Available < 0 || cash.Available > full.EligibleCash || cash.PeerCapital < 0 {
		return snapshot, nil, errors.New("shared cash/capital projection invalid")
	}
	snapshot.AvailableCash = min(snapshot.AvailableCash, wheel.Cents(cash.Available))
	snapshot.ExternalCapital = wheel.Cents(cash.PeerCapital)
	snapshot.ExternalSymbols = append(snapshot.ExternalSymbols, cash.Peers...)
	snapshot.At = view.Evidence.At
	info := &robinhood.AccountInfo{At: view.Evidence.At, AccountLast4: cfg.Account[max(0, len(cfg.Account)-4):], OptionsLevel: sharedbroker.Text(full.Account, "option_level"), AvailableCash: snapshot.AvailableCash, SnapshotComplete: true}
	for _, row := range full.Positions {
		info.Holdings = append(info.Holdings, robinhood.HoldingInfo{Symbol: sharedbroker.Text(row, "symbol"), Quantity: sharedbroker.Text(row, "quantity"), Available: sharedbroker.Text(row, "shares_available_for_sells")})
	}
	return snapshot, info, nil
}
func (c Client) SubmitOption(ctx context.Context, a wheel.Candidate, ref string) (string, error) {
	var prepared struct {
		Key     string          `json:"intent_key"`
		Digest  string          `json:"request_digest"`
		Version int64           `json:"ticket_version"`
		Review  json.RawMessage `json:"review"`
	}
	err := c.Call(ctx, "prepare_option", map[string]any{"candidate": a, "ref_id": ref}, &prepared)
	if err != nil {
		if denial, ok := errors.AsType[*Denial](err); ok && denial.Unsent {
			return "", fmt.Errorf("%w: %s", wheel.ErrNotSubmitted, denial.Reason)
		}
		return "", err
	}
	if prepared.Key != ref || prepared.Version <= 0 || len(prepared.Digest) != 64 {
		return "", errors.New("coordinator preparation identity mismatch")
	}
	var result map[string]any
	if err = c.Call(ctx, "commit_option", map[string]any{"intent_key": ref, "request_digest": prepared.Digest, "ticket_version": prepared.Version}, &result); err != nil {
		if denial, ok := errors.AsType[*Denial](err); ok && denial.Unsent {
			// A commit denial proves only that this RPC did not send. Fence off
			// delayed commits and release the durable preparation before allowing
			// the strategy to discard its own intent.
			var withdrawn struct {
				State string `json:"state"`
			}
			if cleanup := c.Call(ctx, "withdraw", map[string]any{"intent_key": ref}, &withdrawn); cleanup != nil || withdrawn.State != "withdrawn" {
				return "", errors.New("unsent preparation could not be conclusively withdrawn; intent retained")
			}
			return "", fmt.Errorf("%w: %s", wheel.ErrNotSubmitted, denial.Reason)
		}
		return "", err
	}
	return robinhood.VerifyOptionReceipt(result, a)
}
