package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"autowheel/internal/robinhood"
	aruntime "autowheel/internal/runtime"
	"autowheel/internal/sharedbroker"
)

type config struct {
	Account    string            `json:"account_number"`
	TokenFile  string            `json:"token_file"`
	MaxCapital int64             `json:"max_capital_cents"`
	PolicyHash string            `json:"policy_hash"`
	Clients    map[string]string `json:"client_releases"`
}

func readConfig(path string) (config, error) {
	var cfg config
	f, err := aruntime.PrivateRead(path)
	if err != nil {
		return cfg, err
	}
	defer f.Close()
	d := json.NewDecoder(f)
	d.DisallowUnknownFields()
	if err = d.Decode(&cfg); err != nil {
		return cfg, errors.New("account coordinator configuration invalid")
	}
	var trailing any
	if d.Decode(&trailing) != io.EOF {
		return cfg, errors.New("coordinator configuration has trailing JSON")
	}
	if cfg.Account == "" || !filepath.IsAbs(cfg.TokenFile) || cfg.MaxCapital != 8_000_000 || !canonicalHash(cfg.PolicyHash) {
		return cfg, errors.New("account coordinator binding incomplete")
	}
	return cfg, nil
}

func collect(ctx context.Context, cfg config) (sharedbroker.Facts, error) {
	client := robinhood.NewCoordinatorClient(cfg.TokenFile, false)
	if err := client.Connect(ctx); err != nil {
		return sharedbroker.Facts{}, err
	}
	broker := sharedbroker.Backend{Client: client, Account: cfg.Account}
	return broker.Snapshot(ctx)
}

func inspect(ctx context.Context, cfg config, state string) error {
	facts, err := collect(ctx, cfg)
	if err != nil {
		return err
	}
	if err = aruntime.AtomicJSON(filepath.Join(state, "observed-account.json"), facts); err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(map[string]any{
		"mode": "observation_only", "at": facts.At, "account_last4": cfg.Account[max(0, len(cfg.Account)-4):],
		"eligible_cash_cents": facts.EligibleCash, "stock_capital_cents": facts.Capital,
		"equity_positions": len(facts.Positions), "equity_orders": len(facts.Orders), "option_positions": len(facts.Options), "option_orders": len(facts.OptionOrders),
		"facts_digest": facts.Digest, "financial_transport_enabled": false,
		"qualification_required": []string{"frozen TradePulse ownership import", "shared execution policy qualification", "single writer cutover and compatible rollback"},
	})
}

func canonicalHash(value string) bool {
	return len(value) == 64 && strings.Trim(value, "0123456789abcdef") == ""
}
func printVersion(version string) { fmt.Println("AccountCoordinator", version) }
