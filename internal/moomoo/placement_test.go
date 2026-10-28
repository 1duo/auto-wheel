package moomoo

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"autowheel/internal/wheel"
)

func TestPreparePlacementPreservesExactTerms(t *testing.T) {
	terms, now, _, _ := receiptFixture(t)
	for _, price := range []wheel.Cents{1, 99, 125, wheel.MaxMoney / 100} {
		terms.LimitCents = price
		request, err := PreparePlacement(terms, now)
		if err != nil || request.Path != "/api/v1.0/accounts/"+terms.AccountID+"/orders" {
			t.Fatalf("exact account encoding: %v", err)
		}
		var fields map[string]string
		if err := json.Unmarshal(request.Body, &fields); err != nil {
			t.Fatal(err)
		}
		parsed, err := wheel.ParseCents(fields["price"])
		if err != nil || parsed != price || fields["price"] != price.Dollars() || len(fields) != 7 ||
			fields["qty"] != "1" || fields["code"] != terms.Code || fields["side"] != terms.Side ||
			fields["order_type"] != "LIMIT" || fields["time_in_force"] != "DAY" || fields["remark"] != terms.Ref {
			t.Fatal("placement changed prepared terms or added unsupported instructions")
		}
	}
}

func TestPreparePlacementRejectsUnsafeTerms(t *testing.T) {
	for name, change := range map[string]func(*OrderIntentTerms){
		"account-path-injection": func(p *OrderIntentTerms) { p.AccountID = "1234/../9999" },
		"account-overflow":       func(p *OrderIntentTerms) { p.AccountID = "18446744073709551616" },
		"known-receipt":          func(p *OrderIntentTerms) { p.OrderID = "already-submitted" },
		"foreign-market":         func(p *OrderIntentTerms) { p.Code = "HK.00001" },
		"symbol-injection":       func(p *OrderIntentTerms) { p.Code += "\"" },
		"non-uuid-reference":     func(p *OrderIntentTerms) { p.Ref = "new-reference" },
		"side-inference":         func(p *OrderIntentTerms) { p.Side = "sell_to_open" },
		"multiple-contracts":     func(p *OrderIntentTerms) { p.Quantity = 2 },
		"negative-contracts":     func(p *OrderIntentTerms) { p.Quantity = -1 },
		"zero-limit":             func(p *OrderIntentTerms) { p.LimitCents = 0 },
		"negative-limit":         func(p *OrderIntentTerms) { p.LimitCents = -1 },
		"overflow-limit":         func(p *OrderIntentTerms) { p.LimitCents = wheel.MaxMoney/100 + 1 },
		"future-intent":          func(p *OrderIntentTerms) { p.CreatedAt = p.CreatedAt.Add(time.Hour) },
		"unreviewed-duration":    func(p *OrderIntentTerms) { p.TimeInForce = "IOC" },
		"duration-case-alias":    func(p *OrderIntentTerms) { p.TimeInForce = "gtc" },
	} {
		t.Run(name, func(t *testing.T) {
			terms, now, _, _ := receiptFixture(t)
			change(&terms)
			request, err := PreparePlacement(terms, now)
			if err == nil || request.Path != "" || len(request.Body) != 0 {
				t.Fatal("unsafe terms produced a placement encoding")
			}
		})
	}
}

func TestPreparePlacementExplicitGTCDuration(t *testing.T) {
	terms, now, audit, row := receiptFixture(t)
	terms.TimeInForce = "GTC"
	request, err := PreparePlacement(terms, now)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]string
	if json.Unmarshal(request.Body, &fields) != nil || fields["time_in_force"] != "GTC" || fields["remark"] != terms.Ref {
		t.Fatal("explicit resting duration was not encoded")
	}
	if _, err := MatchOrderReceipt(audit, terms, now); err == nil {
		t.Fatal("DAY receipt cleared a GTC intent")
	}
	row["time_in_force"] = "GTC"
	audit.Recent = []OrderEvidence{receiptEvidence(t, row)}
	if _, err := MatchOrderReceipt(audit, terms, now); err != nil {
		t.Fatal(err)
	}
	terms.TimeInForce = ""
	if _, err := MatchOrderReceipt(audit, terms, now); err == nil {
		t.Fatal("GTC receipt cleared an existing DAY intent")
	}
}

func TestPlacementResponseAcknowledgementRetainsPrivateBytes(t *testing.T) {
	raw := []byte(" {\"s\":\"ok\",\"d\":{\"order_id\":\"9007199254740993\"}}\n")
	got, err := DecodePlacementResponse(http.StatusOK, raw)
	digest := sha256.Sum256(raw)
	if err != nil || got.OrderID != "9007199254740993" || got.BodyTruncated ||
		!bytes.Equal(got.Body, raw) || got.BodySHA256 != hex.EncodeToString(digest[:]) {
		t.Fatalf("exact acknowledgement not preserved: %v", err)
	}
	raw[0] = 'x'
	if got.Body[0] != ' ' {
		t.Fatal("diagnostic body shares mutable storage with caller")
	}
}

func TestPlacementResponseAmbiguityNeverAuthorizesReplay(t *testing.T) {
	for name, raw := range map[string]string{
		"provider-error":     `{"s":"error","errcode":-1200,"errmsg":"PRIVATE"}`,
		"confirmation":       `{"s":"error","need_order_confirm":true,"confirm_id":"PRIVATE"}`,
		"browser-action":     `{"s":"error","jump_url":"https://example.invalid/PRIVATE"}`,
		"conflicting-error":  `{"s":"ok","d":{"order_id":"123"},"errcode":-1200}`,
		"conflicting-id":     `{"s":"ok","d":{"order_id":"123"},"confirm_id":"PRIVATE"}`,
		"inactive-challenge": `{"s":"ok","d":{"order_id":"123"},"need_order_confirm":false}`,
		"unknown-extension":  `{"s":"ok","d":{"order_id":"123","confirmed":true}}`,
		"duplicate-status":   `{"s":"error","s":"ok","d":{"order_id":"123"}}`,
		"duplicate-id":       `{"s":"ok","d":{"order_id":"123","order_id":"456"}}`,
		"case-alias":         `{"s":"ok","S":"ok","d":{"order_id":"123"}}`,
		"nested-case-alias":  `{"s":"ok","d":{"Order_ID":"123"}}`,
		"numeric-id":         `{"s":"ok","d":{"order_id":9007199254740993}}`,
		"empty-id":           `{"s":"ok","d":{"order_id":""}}`,
		"unsafe-id":          `{"s":"ok","d":{"order_id":"123\n"}}`,
		"id-too-long":        `{"s":"ok","d":{"order_id":"` + strings.Repeat("a", 129) + `"}}`,
		"missing-data":       `{"s":"ok"}`,
		"null-data":          `{"s":"ok","d":null}`,
		"trailing-json":      `{"s":"ok","d":{"order_id":"123"}} {}`,
		"truncated-json":     `{"s":"ok","d":{"order_id":"123"}`,
		"invalid-utf8":       "{\"s\":\"ok\",\"d\":{\"order_id\":\"\xff\"}}",
	} {
		t.Run(name, func(t *testing.T) {
			got, err := DecodePlacementResponse(http.StatusOK, []byte(raw))
			if !errors.Is(err, ErrPlacementOutcomeUnknown) || errors.Is(err, wheel.ErrNotSubmitted) ||
				got.OrderID != "" || !bytes.Equal(got.Body, []byte(raw)) || strings.Contains(err.Error(), "PRIVATE") {
				t.Fatal("ambiguous response was accepted, lost evidence, disclosed provider text or authorized replay")
			}
		})
	}
}

func TestPlacementResponseHTTPAndSizeRemainUnknown(t *testing.T) {
	raw := []byte(`{"s":"ok","d":{"order_id":"123"}}`)
	for _, status := range []int{0, 201, 202, 204, 301, 307, 400, 401, 429, 500} {
		got, err := DecodePlacementResponse(status, raw)
		if !errors.Is(err, ErrPlacementOutcomeUnknown) || errors.Is(err, wheel.ErrNotSubmitted) || got.OrderID != "" {
			t.Fatalf("HTTP %d accepted or authorized replay", status)
		}
	}
	oversize := append(bytes.Clone(raw), bytes.Repeat([]byte(" "), maxPlacementResponse)...)
	got, err := DecodePlacementResponse(http.StatusOK, oversize)
	if !errors.Is(err, ErrPlacementOutcomeUnknown) || !got.BodyTruncated || len(got.Body) != maxPlacementResponse || got.OrderID != "" {
		t.Fatal("oversized response was not bounded and unresolved")
	}
}
