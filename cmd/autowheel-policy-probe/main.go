// autowheel-policy-probe records source fields or one fixed-policy observation.
// It has no account, review, activation or order path.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"time"

	"autowheel/internal/contractpolicy"
	"autowheel/internal/robinhood"
	"autowheel/internal/wheel"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	token := flag.String("token", "", "existing private OAuth file; never copied to output")
	symbol := flag.String("symbol", "AAPL", "one equity symbol")
	out := flag.String("out", "", "new private output directory under an existing private parent")
	policy := flag.String("policy", "", "optional fixed policy: conservative, balanced or aggressive; observation only")
	kind := flag.String("kind", "put", "put or call for the optional fixed-policy observation")
	flag.Parse()
	if *token == "" || !filepath.IsAbs(*token) || !filepath.IsAbs(*out) || len(*symbol) < 1 || len(*symbol) > 5 || flag.NArg() != 0 {
		return errors.New("require absolute token/output paths and one uppercase equity symbol")
	}
	for _, c := range *symbol {
		if c < 'A' || c > 'Z' {
			return errors.New("symbol must contain uppercase letters only")
		}
	}
	if *policy != "" {
		if _, err := contractpolicy.Choose(*policy, *kind, time.Now().In(wheel.Eastern).Format(time.DateOnly), *symbol, contractpolicy.Book{Bid: 100, Ask: 100}, nil); err != nil {
			return err
		}
	}
	parent, err := os.Lstat(filepath.Dir(*out))
	if err != nil || !parent.IsDir() || parent.Mode().Perm()&0077 != 0 {
		return errors.New("output parent must be an existing private directory")
	}
	if err = os.Mkdir(*out, 0700); err != nil {
		return errors.New("output directory must be new")
	}
	write := func(name string, value any) error {
		f, err := os.OpenFile(filepath.Join(*out, name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return err
		}
		defer f.Close()
		if err = json.NewEncoder(f).Encode(value); err != nil {
			return err
		}
		return f.Sync()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	client := robinhood.NewClient(*token, false)
	if err = client.Connect(ctx); err != nil {
		return errors.New("read-only market connection failed; credentials were not printed or copied")
	}
	var inventory []robinhood.Tool
	wanted := []string{"get_equity_quotes", "get_option_chains"}
	if *policy != "" {
		wanted = append(wanted, "get_option_instruments", "get_option_quotes")
	}
	for _, tool := range client.Inventory() {
		if slices.Contains(wanted, tool.Name) {
			inventory = append(inventory, tool)
		}
	}
	if len(inventory) != len(wanted) {
		return errors.New("required read-only market tools unavailable")
	}
	if err = write("schemas.json", inventory); err != nil {
		return err
	}
	if *policy != "" {
		market := &recordingMarketClient{client: client, write: write}
		broker := robinhood.Broker{MCP: market, Config: wheel.DefaultConfig()}
		observation, observeErr := broker.ObserveFixedPolicy(ctx, *policy, *kind, *symbol, time.Now)
		if err = write("observation.json", observation); err != nil {
			return err
		}
		report := map[string]any{"schema": "AutoWheelFixedPolicyObservationV1", "symbol": *symbol,
			"policy": *policy, "kind": *kind, "market_calls": market.calls, "financial_calls": 0,
			"activation": false, "observation_succeeded": observeErr == nil, "production_readiness_granted": false}
		if observeErr != nil {
			report["error"] = observeErr.Error()
		}
		if err = write("summary.json", report); err != nil {
			return err
		}
		if observeErr != nil {
			return errors.New("fixed-policy market observation failed; see the private summary")
		}
		return nil
	}
	for _, call := range []struct {
		name string
		args map[string]any
	}{
		{"get_equity_quotes", map[string]any{"symbols": []string{*symbol}}},
		{"get_option_chains", map[string]any{"underlying_symbol": *symbol}},
	} {
		value, err := client.Call(ctx, call.name, call.args)
		if err != nil {
			return fmt.Errorf("read-only %s failed", call.name)
		}
		if err = write(call.name+".json", value); err != nil {
			return err
		}
	}
	return write("summary.json", map[string]any{
		"schema": "AutoWheelReadOnlyPolicyProbeV1", "symbol": *symbol, "observed_at": time.Now().UTC(),
		"market_calls": 2, "financial_calls": 0, "activation": false,
		"quote_freshness_for_trading_asserted": false, "purpose": "inspect actual source fields for exact policy integration",
	})
}

// Restrict even future adapter changes to these four non-financial operations.
type recordingMarketClient struct {
	client robinhood.BrokerClient
	write  func(string, any) error
	calls  int
}

func (c *recordingMarketClient) Supports(tool, key string) bool {
	return c.client.Supports(tool, key)
}

func (c *recordingMarketClient) Call(ctx context.Context, tool string, args map[string]any) (any, error) {
	if !slices.Contains([]string{"get_equity_quotes", "get_option_chains", "get_option_instruments", "get_option_quotes"}, tool) {
		return nil, errors.New("non-market operation denied by observation client")
	}
	c.calls++
	if err := c.write(fmt.Sprintf("request-%03d.json", c.calls), map[string]any{"tool": tool, "arguments": args}); err != nil {
		return nil, err
	}
	value, err := c.client.Call(ctx, tool, args)
	if err != nil {
		return nil, err
	}
	if err := c.write(fmt.Sprintf("response-%03d.json", c.calls), value); err != nil {
		return nil, err
	}
	return value, nil
}
