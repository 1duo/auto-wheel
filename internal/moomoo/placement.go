package moomoo

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"time"
)

// PlacementRequest is an offline encoding, never a capability to place an
// order. The future writer must independently prove the durable intent,
// account authorization, collateral, review and native activation before any
// transport. Reconstructing this encoding never authorizes replay of an intent.
type PlacementRequest struct {
	Path string          `json:"path"`
	Body json.RawMessage `json:"body"`
}

// PreparePlacement preserves explicit, previously prepared broker terms. It
// does not infer open/close semantics from BUY/SELL, prove an instrument is a
// standard option, or claim that the supplied intent has been persisted.
func PreparePlacement(terms OrderIntentTerms, now time.Time) (PlacementRequest, error) {
	if err := terms.validate(now); err != nil {
		return PlacementRequest{}, err
	}
	if terms.OrderID != "" {
		return PlacementRequest{}, errors.New("Moomoo placement encoding cannot reuse an intent with a known order receipt")
	}
	// Only these documented single-leg fields are encoded. Session is a
	// US-stock-only placement parameter, so it is not sent for an option.
	body, err := json.Marshal(struct {
		Code  string `json:"code"`
		Qty   string `json:"qty"`
		Price string `json:"price"`
		Side  string `json:"side"`
		Type  string `json:"order_type"`
		TIF   string `json:"time_in_force"`
		Ref   string `json:"remark"`
	}{terms.Code, "1", terms.LimitCents.Dollars(), terms.Side, "LIMIT", terms.timeInForce(), terms.Ref})
	if err != nil {
		return PlacementRequest{}, errors.New("Moomoo placement encoding failed")
	}
	return PlacementRequest{Path: "/api/v1.0/accounts/" + terms.AccountID + "/orders", Body: body}, nil
}

// ErrPlacementOutcomeUnknown always retains the durable intent. In particular,
// a provider error/challenge after a placement attempt is not ErrNotSubmitted.
// Nothing in this decoder confirms, opens a URL, retries or resubmits an order.
var ErrPlacementOutcomeUnknown = errors.New("Moomoo placement outcome unknown; retain durable intent and do not resubmit")

const maxPlacementResponse = 256 << 10

// PlacementResponseEvidence is private diagnostic material. Body is []byte so
// even invalid JSON/UTF-8 is retained losslessly as base64 when saved as JSON.
// The caller must persist it privately, never log it: errors/challenges can
// contain sensitive confirmation identifiers or URLs. BodySHA256 hashes the
// retained bytes; BodyTruncated explicitly marks an oversized response prefix.
type PlacementResponseEvidence struct {
	HTTPStatus    int    `json:"http_status"`
	Body          []byte `json:"body_base64"`
	BodySHA256    string `json:"body_sha256"`
	BodyTruncated bool   `json:"body_truncated"`
	OrderID       string `json:"order_id,omitempty"`
}

// DecodePlacementResponse recognizes only the reviewed HTTP-200 success
// schema. An order ID is an acknowledgement, not proof of its terms, fills,
// collateral or ownership; subsequent exact-reference reconciliation is still
// required. The future transport must supply a fully read response and must
// disable redirects/replays. All unrecognized outcomes remain unresolved.
func DecodePlacementResponse(status int, raw []byte) (PlacementResponseEvidence, error) {
	retained := raw[:min(len(raw), maxPlacementResponse)]
	digest := sha256.Sum256(retained)
	evidence := PlacementResponseEvidence{HTTPStatus: status, Body: bytes.Clone(retained),
		BodySHA256: hex.EncodeToString(digest[:]), BodyTruncated: len(raw) > maxPlacementResponse}
	if status != http.StatusOK || evidence.BodyTruncated {
		return evidence, ErrPlacementOutcomeUnknown
	}
	// Strict decoding rejects contradictory success/error/challenge fields,
	// unknown extensions and duplicate keys rather than silently ignoring them.
	// Check map keys as well: encoding/json struct matching accepts case aliases.
	var envelope map[string]json.RawMessage
	if exactJSON(raw, &envelope) != nil || len(envelope) != 2 || envelope["s"] == nil || envelope["d"] == nil {
		return evidence, ErrPlacementOutcomeUnknown
	}
	var data map[string]json.RawMessage
	if exactJSON(envelope["d"], &data) != nil || len(data) != 1 || data["order_id"] == nil {
		return evidence, ErrPlacementOutcomeUnknown
	}
	var response struct {
		Status string `json:"s"`
		Data   struct {
			OrderID string `json:"order_id"`
		} `json:"d"`
	}
	if privateJSON(bytes.NewReader(raw), maxPlacementResponse, &response) != nil ||
		response.Status != "ok" || !validPlacementID(response.Data.OrderID) {
		return evidence, ErrPlacementOutcomeUnknown
	}
	evidence.OrderID = response.Data.OrderID
	return evidence, nil
}

func validPlacementID(id string) bool {
	if len(id) == 0 || len(id) > 128 {
		return false
	}
	for _, char := range id {
		if !(char >= '0' && char <= '9' || char >= 'A' && char <= 'Z' || char >= 'a' && char <= 'z' || char == '-' || char == '_') {
			return false
		}
	}
	return true
}
