package moomoo

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"autowheel/internal/contractpolicy"
)

func chainRow(expiry, kind string, strike int64) map[string]any {
	date, _ := time.Parse(time.DateOnly, expiry)
	return map[string]any{"code": "US.AAPL" + date.Format("060102") + kind[:1] + strconv.FormatInt(strike*10, 10), "stock_type": "DRVT", "stock_owner": "US.AAPL", "option_type": kind,
		"strike_time": expiry, "strike_price": json.Number(fmt.Sprintf("%d.%02d", strike/100, strike%100)), "lot_size": 100, "option_standard_type": "STANDARD", "index_option_type": "N/A"}
}

func chainResponse(rows []map[string]any, pagination map[string]any) string {
	body := map[string]any{"ret_code": 0, "data": map[string]any{"option_chain": rows}}
	if pagination != nil {
		body["pagination"] = pagination
	}
	raw, _ := json.Marshal(body)
	return string(raw)
}

func chainClient(t *testing.T, responder func(*http.Request, int) string) *Client {
	t.Helper()
	c := NewClient(evidenceCredential(t))
	hits := 0
	c.http.Transport = reviewTransport(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodGet || r.URL.Host != "webapi.moomoo.com" || r.URL.Scheme != "https" {
			t.Fatal("option chain left its pinned read-only origin/method")
		}
		body := ""
		if r.URL.Path == "/api/v1.0/accounts/authorized_trd_accs" {
			body = `{"s":"ok","d":{"accounts":[{"account_id":123456,"security_firm":"FUTUINC","acc_type":"margin","enable_market":[2]}]}}`
		} else {
			if r.URL.Path != "/api/v1.0/quote/US.AAPL/option-chain" {
				t.Fatal("chain reader reached an unrelated resource")
			}
			hits++
			body = responder(r, hits)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}, nil
	})
	return c
}

func TestOptionChainFullWindowPaginationAndConservativeSelection(t *testing.T) {
	c := chainClient(t, func(r *http.Request, hit int) string {
		q := r.URL.Query()
		if q.Get("filter_standard") != "STANDARD" || q.Has("limit") || hit != 2 && q.Has("next_key") {
			t.Fatal("chain request changed its documented standard contract scope")
		}
		if hit <= 2 {
			if q.Get("start") != "2026-11-17" || q.Get("end") != "2026-12-06" {
				t.Fatal("first span omitted part of the policy's date window")
			}
			if hit == 1 {
				if q.Get("next_key") != "" {
					t.Fatal("first page invented a cursor")
				}
				return chainResponse([]map[string]any{chainRow("2026-11-25", "PUT", 9000), chainRow("2026-11-25", "PUT", 8500)}, map[string]any{"has_more": true, "next_key": "opaque+/cursor=", "total": 5})
			}
			if q.Get("next_key") != "opaque+/cursor=" {
				t.Fatal("opaque cursor was modified")
			}
			return chainResponse([]map[string]any{chainRow("2026-11-25", "PUT", 10000), chainRow("2026-11-25", "CALL", 10500), chainRow("2026-11-25", "CALL", 11000)}, map[string]any{"has_more": false, "next_key": "-1", "total": 5})
		}
		if hit != 3 || q.Get("start") != "2026-12-07" || q.Get("end") != "2026-12-11" || q.Get("next_key") != "" {
			t.Fatal("second span did not cover the final five calendar days exactly once")
		}
		return chainResponse([]map[string]any{chainRow("2026-12-11", "PUT", 9000)}, map[string]any{"has_more": false, "total": 1})
	})
	audit, err := c.AuditOptionChain(t.Context(), "123456", "AAPL", "2026-10-27")
	if err != nil {
		t.Fatal(err)
	}
	if !audit.Complete || len(audit.Pages) != 3 || len(audit.Contracts) != 6 || audit.From != "2026-11-17" || audit.Through != "2026-12-11" || audit.ExecutionQualified {
		t.Fatal("full chain window or qualification boundary was lost")
	}
	stock := contractpolicy.Book{Bid: 9900, Ask: 10100}
	put, err := audit.ChooseIdentity("put", stock)
	if err != nil || put == nil || put.Strike != 9000 || put.Expiry != "2026-11-25" {
		t.Fatal("identity did not use the existing conservative nearest-expiry/90% rule")
	}
	call, err := audit.ChooseIdentity("call", stock)
	if err != nil || call == nil || call.Strike != 10500 {
		t.Fatal("identity did not preserve the 105% call target")
	}
	menu, _ := audit.SelectionMenu()
	menu[0].Strike = 1
	putAgain, _ := audit.ChooseIdentity("put", stock)
	if *putAgain != *put {
		t.Fatal("caller mutated the retained selection menu")
	}
	raw, _ := json.MarshalIndent(audit, "", "  ")
	var restored OptionChainAudit
	if exactJSON(raw, &restored) != nil {
		t.Fatal("chain evidence serialization failed")
	}
	for _, page := range restored.Pages {
		hash := sha256.Sum256(page.Bytes)
		if hex.EncodeToString(hash[:]) != page.SHA256 {
			t.Fatal("chain source bytes/hash changed during persistence")
		}
	}
	if _, err := restored.SelectionMenu(); err == nil {
		t.Fatal("persisted completion flag created fresh selection capability")
	}
	audit.selectionAt = time.Now().Add(-31 * time.Second)
	if _, err := audit.SelectionMenu(); err == nil {
		t.Fatal("stale in-memory chain authorized selection")
	}
}

// The chain reference documents an unpaginated, date-bounded list. Each
// twenty-date subrange cannot exceed its twenty-expiry cap.
func TestOptionChainDocumentedUnpaginatedResponseIsComplete(t *testing.T) {
	c := chainClient(t, func(r *http.Request, _ int) string {
		return chainResponse([]map[string]any{chainRow(r.URL.Query().Get("start"), "PUT", 9000)}, nil)
	})
	audit, err := c.AuditOptionChain(t.Context(), "123456", "AAPL", "2026-10-27")
	if err != nil || audit == nil || !audit.Complete || audit.Reason != "" || len(audit.Pages) != 2 || len(audit.Contracts) != 2 {
		t.Fatal("documented unpaginated chain was not accepted as complete", err)
	}
	if put, err := audit.ChooseIdentity("put", contractpolicy.Book{Bid: 9900, Ask: 10100}); err != nil || put == nil {
		t.Fatal("complete unpaginated menu could not select", err)
	}
}

// A continuation page without its own pagination facts cannot be completed.
func TestOptionChainContinuationWithoutPaginationRemainsEvidenceOnly(t *testing.T) {
	c := chainClient(t, func(r *http.Request, hit int) string {
		if hit == 1 {
			return chainResponse([]map[string]any{chainRow("2026-11-25", "PUT", 9000)}, map[string]any{"has_more": true, "next_key": "next"})
		}
		if hit == 2 {
			return chainResponse([]map[string]any{chainRow("2026-11-25", "PUT", 8500)}, nil)
		}
		return chainResponse([]map[string]any{chainRow(r.URL.Query().Get("start"), "PUT", 9000)}, nil)
	})
	audit, err := c.AuditOptionChain(t.Context(), "123456", "AAPL", "2026-10-27")
	if err != nil || audit == nil || audit.Complete || audit.Reason == "" {
		t.Fatal("continuation without pagination facts was promoted to completion", err)
	}
	if _, err := audit.ChooseIdentity("put", contractpolicy.Book{Bid: 9900, Ask: 10100}); err == nil {
		t.Fatal("incomplete identity menu produced a conservative selection")
	}
}

func TestOptionChainRejectsAmbiguousPagination(t *testing.T) {
	for _, name := range []string{"duplicate", "cursor-repeat", "cursor-missing", "terminal-total", "changing-total", "alias", "missing-has-more", "page-cap"} {
		t.Run(name, func(t *testing.T) {
			reads := 0
			c := chainClient(t, func(r *http.Request, hit int) string {
				reads = hit
				row := chainRow(r.URL.Query().Get("start"), "PUT", 9000+int64(hit))
				p := map[string]any{"has_more": true, "next_key": strconv.Itoa(hit)}
				switch name {
				case "duplicate":
					row = chainRow(r.URL.Query().Get("start"), "PUT", 9000)
				case "cursor-repeat":
					p["next_key"] = "same"
				case "cursor-missing":
					delete(p, "next_key")
				case "terminal-total":
					p["has_more"], p["total"] = false, 2
				case "changing-total":
					p["total"] = hit
				case "alias":
					p["HAS_MORE"] = false
				case "missing-has-more":
					delete(p, "has_more")
				}
				return chainResponse([]map[string]any{row}, p)
			})
			if audit, err := c.AuditOptionChain(t.Context(), "123456", "AAPL", "2026-10-27"); err == nil || audit != nil || reads > 64 {
				t.Fatal("ambiguous/capped chain pagination was accepted")
			}
		})
	}
}

func TestOptionChainExactStandardIdentityAndRequestGates(t *testing.T) {
	for _, name := range []string{"scale", "quoted-strike", "subcent-strike", "side", "date", "outside-date", "underlying", "standard", "index", "lot", "alias"} {
		t.Run(name, func(t *testing.T) {
			row := chainRow("2026-11-25", "PUT", 9000)
			switch name {
			case "scale":
				row["strike_price"] = json.Number("90000000000")
			case "quoted-strike":
				row["strike_price"] = "90.00"
			case "subcent-strike":
				row["strike_price"] = json.Number("90.001")
			case "side":
				row["option_type"] = "CALL"
			case "date":
				row["strike_time"] = "2026-11-24"
			case "outside-date":
				row = chainRow("2026-12-11", "PUT", 9000)
			case "underlying":
				row["stock_owner"] = "US.MSFT"
			case "standard":
				row["option_standard_type"] = "NON_STANDARD"
			case "index":
				row["index_option_type"] = "NORMAL"
			case "lot":
				row["lot_size"] = 10
			case "alias":
				row["STRIKE_PRICE"] = 1
			}
			raw, _ := json.Marshal(row)
			if _, err := parseChainContract(raw, "AAPL", "2026-11-17", "2026-12-06"); err == nil {
				t.Fatal("contradictory contract identity became native menu input")
			}
		})
	}
	c := NewClient("/does-not-exist")
	for _, symbol := range []string{"", "US.AAPL", "AAPL/../../accounts", "AAPL\n"} {
		if _, err := c.AuditOptionChain(t.Context(), "123456", symbol, "2026-10-27"); err == nil || !strings.Contains(err.Error(), "explicit equity symbol") {
			t.Fatal("unsafe chain symbol reached credentials or transport")
		}
	}
	if _, _, err := c.readChainPage(t.Context(), "AAPL/../../accounts", "2026-11-17", "2026-12-06", ""); err == nil || !strings.Contains(err.Error(), "bounded Moomoo chain resource") {
		t.Fatal("chain transport exposed a path escape hatch")
	}
}
