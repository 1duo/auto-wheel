package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"autowheel/internal/robinhood"
	aruntime "autowheel/internal/runtime"
	"autowheel/internal/wheel"
)

type auditedReads struct {
	client *robinhood.Client
	calls  []auditRead
}

type auditRead struct {
	Tool      string         `json:"tool"`
	Arguments map[string]any `json:"arguments"`
	Result    any            `json:"result"`
}

func (a *auditedReads) Supports(name, key string) bool { return a.client.Supports(name, key) }
func (a *auditedReads) Call(ctx context.Context, name string, args map[string]any) (any, error) {
	if !strings.HasPrefix(name, "get_") {
		return nil, errors.New("account audit refuses non-read tools")
	}
	v, err := a.client.Call(ctx, name, args)
	if err != nil {
		return nil, err
	}
	// Copy through JSON: pagination mutates arguments after a completed read.
	raw, err := json.Marshal(auditRead{Tool: name, Arguments: args, Result: v})
	if err != nil {
		return nil, err
	}
	var captured auditRead
	d := json.NewDecoder(strings.NewReader(string(raw)))
	d.UseNumber()
	if err = d.Decode(&captured); err != nil {
		return nil, err
	}
	a.calls = append(a.calls, captured)
	return v, nil
}

// Audit bypasses no trading gate: it has a separately restricted, read-only MCP
// client and never opens the financial journal. Raw evidence stays on msi.
func auditAccount(ctx context.Context, c wheel.Config, token, dir string) error {
	if c.Account == "" {
		return errors.New("account audit requires an explicit configured account")
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	client := robinhood.NewClient(token, false)
	if err := client.Connect(ctx); err != nil {
		return err
	}
	inventory := client.Inventory()
	slices.SortFunc(inventory, func(a, b robinhood.Tool) int { return strings.Compare(a.Name, b.Name) })
	reads := &auditedReads{client: client}
	b := &robinhood.Broker{MCP: reads, Config: c}
	snapshot, snapshotErr := b.Snapshot(ctx)
	tools := make([]string, 0, len(inventory))
	for _, t := range inventory {
		tools = append(tools, t.Name)
	}
	toolRaw, err := json.Marshal(inventory)
	if err != nil {
		return err
	}
	evidence := struct {
		At        time.Time        `json:"at"`
		Inventory []robinhood.Tool `json:"inventory"`
		Reads     []auditRead      `json:"reads"`
	}{At: time.Now().UTC(), Inventory: inventory, Reads: reads.calls}
	evidenceRaw, err := json.Marshal(evidence)
	if err != nil {
		return err
	}
	if err = aruntime.AtomicJSON(filepath.Join(dir, "account-audit-evidence.json"), evidence); err != nil {
		return err
	}
	fileRaw, err := os.ReadFile(filepath.Join(dir, "account-audit-evidence.json"))
	if err != nil {
		return err
	}
	counts := map[string]int{}
	admission := map[string]any{}
	for _, call := range reads.calls {
		counts[call.Tool]++
		data, ok := call.Result.(map[string]any)
		if !ok {
			continue
		}
		if inner, ok := data["data"].(map[string]any); ok {
			data = inner
		}
		switch call.Tool {
		case "get_trade_approval_setting":
			setting, ok := data["setting"].(map[string]any)
			if ok && setting["account_number"] == c.Account {
				if required, ok := setting["human_must_approve_trades"].(bool); ok {
					admission["human_must_approve_trades"] = required
				}
			}
		case "get_trade_approvals":
			if rows, ok := data["approvals"].([]any); ok {
				count, _ := admission["pending_approvals"].(int)
				admission["pending_approvals"] = count + len(rows)
			}
		}
	}
	metadata := map[string]any{}
	for _, call := range reads.calls {
		if call.Tool != "get_accounts" {
			continue
		}
		data, ok := call.Result.(map[string]any)
		if !ok {
			continue
		}
		if inner, ok := data["data"].(map[string]any); ok {
			data = inner
		}
		accounts, ok := data["accounts"].([]any)
		if !ok {
			continue
		}
		for _, row := range accounts {
			a, ok := row.(map[string]any)
			if !ok || a["account_number"] != c.Account {
				continue
			}
			keys := make([]string, 0, len(a))
			for key := range a {
				keys = append(keys, key)
			}
			slices.Sort(keys)
			metadata["fields"] = keys
			for _, key := range []string{"rhc_account_number", "rhs_account_number"} {
				value, present := a[key]
				text, valid := value.(string)
				metadata[key] = map[string]any{"present": present, "string": valid, "nonempty": text != "", "null": present && value == nil}
			}
		}
	}
	summary := map[string]any{
		"at": evidence.At, "release": version, "read_only": true,
		"account_last4":    c.Account[max(0, len(c.Account)-4):],
		"inventory_sha256": digestBytes(toolRaw), "evidence_sha256": digestBytes(evidenceRaw),
		"evidence_file_sha256": digestBytes(fileRaw),
		"tools":                tools, "read_counts": counts, "account_metadata": metadata,
		"order_admission":             admission,
		"advanced_order_tool_exposed": client.Supports("get_advanced_orders", ""),
		"account_snapshot_complete":   snapshotErr == nil && b.AccountInfo != nil && b.AccountInfo.SnapshotComplete,
		"execution_snapshot_usable":   snapshotErr == nil,
		"order_coverage_policy":       c.OrderCoveragePolicy,
		"available_cash_cents":        snapshot.AvailableCash,
		"options":                     len(snapshot.Options), "orders": len(snapshot.Orders), "quarantined_symbols": snapshot.ExternalSymbols,
	}
	if c.OrderCoveragePolicy == "exclusive_standard" {
		summary["coverage_warning"] = wheel.StandardOrderWarning
	}
	if snapshotErr != nil {
		summary["snapshot_error"] = snapshotErr.Error()
	}
	if err = aruntime.AtomicJSON(filepath.Join(dir, "account-audit-summary.json"), summary); err != nil {
		return err
	}
	if err = json.NewEncoder(os.Stdout).Encode(summary); err != nil {
		return err
	}
	if snapshotErr != nil {
		return fmt.Errorf("read-only account audit: %w", snapshotErr)
	}
	return nil
}
