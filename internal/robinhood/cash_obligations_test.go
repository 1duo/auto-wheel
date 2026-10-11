package robinhood

import (
	"context"
	"errors"
	"testing"

	"autowheel/internal/wheel"
)

type obligationsClient struct {
	settlementSnapshotClient
	account        object
	orders         []any
	positions      []any
	secondPage     bool
	wrongAccount   bool
	noScope        bool
	advanced       bool
	advancedOrders []any
	approvalFlag   any
	approvalWrong  bool
	approvals      []any
	cryptoReads    int
}

func (c *obligationsClient) Supports(name, key string) bool {
	if name == "get_advanced_orders" {
		return c.advanced && (key == "" || key == "account_number")
	}
	if name == "get_crypto_orders" || name == "get_crypto_positions" {
		return !c.noScope && (key == "rhs_account_number" || key == "cursor")
	}
	return c.settlementSnapshotClient.Supports(name, key)
}
func (c *obligationsClient) Call(ctx context.Context, name string, args object) (any, error) {
	if name == "get_trade_approval_setting" {
		account := "12345678"
		if c.approvalWrong {
			account = "other-account"
		}
		return object{"setting": object{"account_number": account, "human_must_approve_trades": c.approvalFlag}}, nil
	}
	if name == "get_trade_approvals" {
		return object{"approvals": c.approvals}, nil
	}
	if name == "get_accounts" {
		v, err := c.settlementSnapshotClient.Call(ctx, name, args)
		if err != nil {
			return nil, err
		}
		a := v.(object)["accounts"].([]any)[0].(object)
		for k, v := range c.account {
			a[k] = v
		}
		return v, nil
	}
	if name == "get_advanced_orders" {
		return object{"orders": c.advancedOrders}, nil
	}
	if name == "get_crypto_orders" || name == "get_crypto_positions" {
		c.cryptoReads++
		if args["rhs_account_number"] != "87654321" {
			return nil, errors.New("crypto read escaped numeric brokerage binding")
		}
		if name == "get_crypto_positions" {
			return object{"results": c.positions}, nil
		}
		rhs := "87654321"
		if c.wrongAccount {
			rhs = "another-account"
		}
		if c.secondPage && args["cursor"] == nil {
			return object{"rhs_account_number": rhs, "results": []any{}, "next": "second-page"}, nil
		}
		return object{"rhs_account_number": rhs, "results": c.orders, "next": ""}, nil
	}
	return c.settlementSnapshotClient.Call(ctx, name, args)
}

func TestSnapshotAccountsForLinkedCryptoAndAdvancedOrders(t *testing.T) {
	for _, name := range []string{"unlinked", "clean", "zero_inventory", "working_crypto", "second_page_working", "crypto_inventory", "crypto_sale_hold", "crypto_transfer", "missing_quantity", "missing_hold", "wrong_account", "missing_rhs", "malformed_link", "no_scope", "working_advanced", "terminal_advanced", "advanced_unavailable", "approval_on", "approval_unproven", "approval_wrong_account", "pending_approval"} {
		t.Run(name, func(t *testing.T) {
			base := settlementSnapshotClient{position: object{"quantity": "0", "pending_assignment_quantity": "0", "pending_expiration_quantity": "0", "pending_exercise_quantity": "0"}}
			fake := &obligationsClient{settlementSnapshotClient: base, account: object{"rhc_account_number": "RHC-test", "rhs_account_number": "87654321"}, orders: []any{}, positions: []any{}, advanced: true, advancedOrders: []any{}, approvalFlag: false, approvals: []any{}}
			allowed := name == "unlinked" || name == "clean" || name == "zero_inventory"
			switch name {
			case "unlinked":
				delete(fake.account, "rhc_account_number")
			case "zero_inventory":
				fake.positions = []any{object{"quantity": "0E-8", "quantity_transferable": "0", "quantity_held_for_sell": "0"}}
			case "crypto_sale_hold":
				fake.positions = []any{object{"quantity": "0", "quantity_transferable": "0", "quantity_held_for_sell": "1"}}
			case "crypto_transfer":
				fake.positions = []any{object{"quantity": "0", "quantity_transferable": "1", "quantity_held_for_sell": "0"}}
			case "missing_hold":
				fake.positions = []any{object{"quantity": "0", "quantity_transferable": "0"}}
			case "working_crypto", "second_page_working":
				fake.orders = []any{object{"state": "confirmed"}}
				fake.secondPage = name == "second_page_working"
			case "crypto_inventory":
				fake.positions = []any{object{"quantity": "0.000001"}}
			case "missing_quantity":
				fake.positions = []any{object{}}
			case "wrong_account":
				fake.wrongAccount = true
			case "missing_rhs":
				delete(fake.account, "rhs_account_number")
			case "malformed_link":
				fake.account["rhc_account_number"] = nil
			case "no_scope":
				fake.noScope = true
			case "working_advanced":
				fake.advanced = true
				fake.advancedOrders = []any{object{"state": "queued"}}
			case "terminal_advanced":
				fake.advanced = true
				fake.advancedOrders = []any{object{"state": "filled"}}
			case "advanced_unavailable":
				fake.advanced = false
			case "approval_on":
				fake.approvalFlag = true
			case "approval_unproven":
				fake.approvalFlag = "false"
			case "approval_wrong_account":
				fake.approvalWrong = true
			case "pending_approval":
				fake.approvals = []any{object{"state": "APPROVAL_STATE_PENDING", "account_number": "12345678"}}
			}
			c := wheel.DefaultConfig()
			c.Account = "12345678"
			b := &Broker{MCP: fake, Config: c}
			s, err := b.Snapshot(t.Context())
			if allowed {
				if err != nil || !b.AccountInfo.SnapshotComplete || s.AvailableCash != 5_000_000 {
					t.Fatal("clean cash snapshot failed", name, err)
				}
				if name == "unlinked" && fake.cryptoReads != 0 {
					t.Fatal("unlinked account queried crypto")
				}
				if name != "unlinked" && fake.cryptoReads != 2 {
					t.Fatal("linked cash did not cover both crypto collections", fake.cryptoReads)
				}
			} else if err == nil || b.AccountInfo.SnapshotComplete {
				t.Fatal("unsupported obligation admitted", name, err)
			}
			if name == "second_page_working" && fake.cryptoReads != 2 {
				t.Fatal("later-page obligation was not examined", fake.cryptoReads)
			}
		})
	}
}
