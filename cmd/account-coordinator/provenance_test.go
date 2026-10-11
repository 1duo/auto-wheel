package main

import (
	"crypto/sha256"
	"fmt"
	"strings"
	"testing"
	"time"

	"autowheel/internal/sharedbroker"
)

const (
	provenanceBuyID  = "6a7f2017-9e97-4fb5-a674-5415cb484e3e"
	provenanceRefID  = "a8014e49-ff2f-5172-8756-490912f7a4f6"
	provenanceExtra  = "6a848711-9594-4811-b84f-259a9dfefa19"
	provenancePolicy = "abababababababababababababababababababababababababababababababab"
)

func provenancePayload(brokerID, ref, symbol, side, qty string) (string, string) {
	raw := fmt.Sprintf(`{"intent_key":"intent-1","broker_order_id":%q,"ref_id":%q,"symbol":%q,"side":%q,"quantity":%q}`,
		brokerID, ref, symbol, side, qty)
	return raw, fmt.Sprintf("%x", sha256.Sum256([]byte(raw)))
}

func validProvenanceFixture() (sharedbroker.Facts, journalSnapshot) {
	ref := peerOrderReference("intent-1")
	payload, hash := provenancePayload(provenanceBuyID, ref, "INTC", "buy", "2.000000")
	facts := sharedbroker.Facts{
		At:        time.Now().UTC(),
		Positions: []sharedbroker.Object{{"symbol": "INTC", "quantity": "2.000000", "shares_available_for_sells": "2.000000"}},
		Orders: []sharedbroker.Object{{
			"id": provenanceBuyID, "ref_id": ref, "symbol": "INTC",
			"side": "buy", "quantity": "2.000000", "state": "filled", "placed_agent": "agentic",
		}},
		TaxLots: map[string][]sharedbroker.Object{
			"INTC": {{"open_lot_id": "lot-1", "quantity": "2.000000", "order_id": provenanceBuyID}},
		},
	}
	snap := journalSnapshot{
		Orders: []tpIntent{{
			IntentKey: "intent-1", Symbol: "INTC", PayloadHash: hash,
			PayloadJSON: payload, Status: "filled", BrokerOrderID: provenanceBuyID,
		}},
		Lifecycles: []tpLifecycle{{
			Symbol: "INTC", StateJSON: `{"schema_version":"PilotLifecycleV1","current_quantity":2.000000}`,
			ReleaseHash: provenancePolicy, UpdatedSequence: 7,
		}},
		MaxSequence: 7,
	}
	return facts, snap
}

func TestProvenanceKnownUUIDLotsSucceed(t *testing.T) {
	facts, snap := validProvenanceFixture()
	if blockers := validateProvenance(facts, snap); len(blockers) != 0 {
		t.Fatalf("exact UUID provenance blocked: %v", blockers)
	}
}

func TestProvenanceUnknownTaxLotIDsReject(t *testing.T) {
	facts, snap := validProvenanceFixture()
	facts.TaxLots["INTC"][0]["order_id"] = "300000123456789"
	blockers := validateProvenance(facts, snap)
	joined := strings.Join(blockers, "; ")
	if len(blockers) == 0 || !strings.Contains(joined, "internal decimal") {
		t.Fatalf("internal decimal lot opener accepted: %v", blockers)
	}
}

func TestProvenanceUnknownBrokerOrderRejects(t *testing.T) {
	facts, snap := validProvenanceFixture()
	facts.Orders = append(facts.Orders, sharedbroker.Object{
		"id": provenanceExtra, "ref_id": provenanceRefID, "symbol": "INTC",
		"side": "buy", "quantity": "1.000000", "state": "filled", "placed_agent": "agentic",
	})
	blockers := validateProvenance(facts, snap)
	joined := strings.Join(blockers, "; ")
	if len(blockers) == 0 || !strings.Contains(joined, "unaccounted broker order") {
		t.Fatalf("manual broker order accepted: %v", blockers)
	}
}

func TestProvenanceShareMismatchRejects(t *testing.T) {
	facts, snap := validProvenanceFixture()
	facts.Positions[0]["quantity"] = "5.000000"
	facts.Positions[0]["shares_available_for_sells"] = "5.000000"
	blockers := validateProvenance(facts, snap)
	if len(blockers) == 0 || !strings.Contains(strings.Join(blockers, "; "), "mismatch") {
		t.Fatalf("share mismatch accepted: %v", blockers)
	}
}
