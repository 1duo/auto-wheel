package moomoo

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	aruntime "autowheel/internal/runtime"
)

func evidenceCredential(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "token.json")
	credential := Credential{Resource: Resource, ClientID: "client", AccessToken: "secret", RefreshToken: "refresh",
		ExpiresAt: time.Now().Add(time.Hour), Scope: "quote:read trade:read"}
	if err := aruntime.AtomicJSON(path, credential); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestAuditAccountRejectsForeignAccountBeforeFundsRead(t *testing.T) {
	cases := map[string]string{
		"different-account": `{"accounts":[{"account_id":9007199254740994,"security_firm":"FUTUINC","acc_type":"cash","enable_market":[2]}]}`,
		"foreign-firm":      `{"accounts":[{"account_id":9007199254740993,"security_firm":"OTHERFIRM","acc_type":"cash","enable_market":[2]}]}`,
		"non-us-market":     `{"accounts":[{"account_id":9007199254740993,"security_firm":"FUTUINC","acc_type":"cash","enable_market":[1]}]}`,
	}
	for name, accountsRaw := range cases {
		t.Run(name, func(t *testing.T) {
			fundsHit, positionsHit := false, false
			client := NewClient(evidenceCredential(t))
			client.http = &http.Client{Transport: reviewTransport(func(req *http.Request) (*http.Response, error) {
				switch req.URL.Path {
				case "/api/v1.0/accounts/authorized_trd_accs":
					return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"s":"ok","d":` + accountsRaw + `}`))}, nil
				case "/api/v1.0/accounts/9007199254740993/funds":
					fundsHit = true
				case "/api/v1.0/accounts/9007199254740993/positions":
					positionsHit = true
				}
				return nil, errors.New("read attempted before exact US-account authorization")
			})}
			evidence, err := client.AuditAccount(t.Context(), "9007199254740993")
			if err == nil || evidence != nil {
				t.Fatalf("foreign account accepted: %#v %v", evidence, err)
			}
			if fundsHit || positionsHit {
				t.Fatal("funds or positions were read before exact US-account authorization")
			}
		})
	}
}

func TestAuditAccountPreservesExactRawFieldsAndDecimals(t *testing.T) {
	const fundsRaw = `{"currency":"USD","available_funds":"12345.67","other_field":"keep-me"}`
	const positionsRaw = `[{"code":"US.AAPL261030P100000","qty":"-1","can_sell_qty":"0","position_side":"SHORT","currency":"USD","security_type":"OPTION"},{"code":"US.AAPL","qty":"100.125","can_sell_qty":"100","position_side":"LONG","currency":"USD"}]`
	client := NewClient(evidenceCredential(t))
	client.http = &http.Client{Transport: reviewTransport(func(req *http.Request) (*http.Response, error) {
		if req.Method != http.MethodGet || req.URL.Scheme != "https" || req.URL.Host != "webapi.moomoo.com" {
			t.Fatal("audit left the pinned read-only transport")
		}
		var body string
		switch req.URL.Path {
		case "/api/v1.0/accounts/authorized_trd_accs":
			body = `{"accounts":[{"account_id":9007199254740993,"security_firm":"FUTUINC","acc_type":"cash","enable_market":[2]}]}`
		case "/api/v1.0/accounts/9007199254740993/funds":
			if req.URL.Query().Get("currency") != "USD" {
				t.Fatal("funds read not pinned to USD")
			}
			body = fundsRaw
		case "/api/v1.0/accounts/9007199254740993/positions":
			if req.URL.RawQuery != "" {
				t.Fatal("position evidence was filtered")
			}
			body = positionsRaw
		default:
			t.Fatalf("unexpected path %s", req.URL.Path)
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"s":"ok","d":` + body + `}`))}, nil
	})}
	evidence, err := client.AuditAccount(t.Context(), "9007199254740993")
	if err != nil {
		t.Fatal(err)
	}
	if evidence == nil {
		t.Fatal("nil evidence")
	}
	if string(evidence.Funds) != fundsRaw || string(evidence.Positions) != positionsRaw {
		t.Fatalf("raw evidence altered:\nfunds %s\npositions %s", evidence.Funds, evidence.Positions)
	}
	if !bytes.Equal(evidence.FundsBytes, []byte(fundsRaw)) || !bytes.Equal(evidence.PositionsBytes, []byte(positionsRaw)) {
		t.Fatal("original source bytes missing")
	}
	if evidence.AvailableFunds == nil || *evidence.AvailableFunds != 1234567 || evidence.ReportedFunds != "12345.67" {
		t.Fatalf("parsed funds mismatch: %v", evidence.AvailableFunds)
	}
	if len(evidence.Holdings) != 2 || evidence.Holdings[0].Symbol != "US.AAPL" || evidence.Holdings[0].Quantity != "100.125" || evidence.Holdings[0].Side != "LONG" ||
		evidence.Holdings[1].Symbol != "US.AAPL261030P100000" || evidence.Holdings[1].Quantity != "-1" || evidence.Holdings[1].Side != "SHORT" {
		t.Fatalf("holdings mismatch: %#v", evidence.Holdings)
	}
	if evidence.ExecutionQualified {
		t.Fatal("execution marked qualified")
	}
	if evidence.CashInputs != nil || evidence.CashInputError == "" || evidence.Observation().ExecutionSnapshotUsable {
		t.Fatal("missing cash facts suppressed observation or became executable cash")
	}
	if evidence.Source != restEndpoint {
		t.Fatalf("source not pinned: %q", evidence.Source)
	}
	if evidence.Account.ID != "9007199254740993" || evidence.Account.Firm != "FUTUINC" || evidence.Account.Type != "cash" || evidence.Account.Last4 != "0993" || !evidence.Account.USMarket {
		t.Fatalf("normalized account wrong: %#v", evidence.Account)
	}
	if evidence.At.IsZero() || evidence.FinishedAt.IsZero() || evidence.FinishedAt.Before(evidence.At) {
		t.Fatal("read window missing or inverted")
	}
	for _, word := range []string{"non-atomic", "settled", "ownership", "assignment"} {
		if !strings.Contains(strings.ToLower(evidence.CoverageWarning), word) {
			t.Fatalf("coverage warning must state %s; got %q", word, evidence.CoverageWarning)
		}
	}
}

func TestAccountCashInputsPreserveSourceHashAcrossPrivatePersistence(t *testing.T) {
	fields := cashInputFixture()
	raw, _ := json.Marshal(fields)
	// Deliberate source whitespace must survive the pretty-printed private
	// packet; RawMessage alone is reformatted by MarshalIndent.
	raw = bytes.ReplaceAll(raw, []byte(`,`), []byte(",\n "))
	input, err := mapCashInputs("123456", time.Now().UTC(), raw)
	if err != nil {
		t.Fatal(err)
	}
	packet := AccountEvidence{AccountID: "123456", Funds: raw, FundsBytes: bytes.Clone(raw), CashInputs: input}
	path := filepath.Join(t.TempDir(), "evidence.json")
	if err := aruntime.AtomicJSON(path, packet); err != nil {
		t.Fatal(err)
	}
	f, err := aruntime.PrivateRead(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var restored AccountEvidence
	if err := privateJSON(f, 1<<20, &restored); err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(restored.FundsBytes)
	if !bytes.Equal(restored.FundsBytes, raw) || restored.CashInputs == nil || restored.CashInputs.FundsSHA256 != hex.EncodeToString(hash[:]) || restored.ExecutionQualified || restored.CashInputs.ExecutionQualified {
		t.Fatal("cash source provenance or unqualified boundary changed after save")
	}
}

func TestAuditAccountRejectsIncompleteOrAmbiguousDataWithoutPartialPacket(t *testing.T) {
	cases := map[string]struct{ funds, positions string }{
		"missing-funds":      {funds: `{}`, positions: `[]`},
		"wrong-currency":     {funds: `{"currency":"HKD","available_funds":"1.00"}`, positions: `[]`},
		"duplicate-fields":   {funds: `{"currency":"USD","currency":"HKD","available_funds":"1.00"}`, positions: `[]`},
		"null-positions":     {funds: `{"currency":"USD","available_funds":"1.00"}`, positions: `null`},
		"ambiguous-position": {funds: `{"currency":"USD","available_funds":"1.00"}`, positions: `[{"code":"US.AAPL","qty":"1.0000001","can_sell_qty":"1","position_side":"LONG","currency":"USD"}]`},
		"missing-identity":   {funds: `{"currency":"USD","available_funds":"1.00"}`, positions: `[{"qty":"1","can_sell_qty":"1","position_side":"LONG","currency":"USD"}]`},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			client := NewClient(evidenceCredential(t))
			client.http = &http.Client{Transport: reviewTransport(func(req *http.Request) (*http.Response, error) {
				var body string
				switch req.URL.Path {
				case "/api/v1.0/accounts/authorized_trd_accs":
					body = `{"accounts":[{"account_id":9007199254740993,"security_firm":"FUTUINC","acc_type":"cash","enable_market":[2]}]}`
				case "/api/v1.0/accounts/9007199254740993/funds":
					body = tc.funds
				case "/api/v1.0/accounts/9007199254740993/positions":
					body = tc.positions
				default:
					t.Fatalf("unexpected path %s", req.URL.Path)
				}
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"s":"ok","d":` + body + `}`))}, nil
			})}
			evidence, err := client.AuditAccount(t.Context(), "9007199254740993")
			if err == nil || evidence != nil {
				t.Fatalf("partial/ambiguous evidence accepted: %#v %v", evidence, err)
			}
		})
	}
}

func TestHoldingsRefreshPreservesSubCentFundsWithoutInventingCash(t *testing.T) {
	client := NewClient(evidenceCredential(t))
	client.http = &http.Client{Transport: reviewTransport(func(req *http.Request) (*http.Response, error) {
		var body string
		switch req.URL.Path {
		case "/api/v1.0/accounts/authorized_trd_accs":
			body = `{"accounts":[{"account_id":9007199254740993,"security_firm":"FUTUINC","acc_type":"margin","enable_market":[2]}]}`
		case "/api/v1.0/accounts/9007199254740993/funds":
			body = `{"currency":"USD","available_funds":"12.3456789"}`
		case "/api/v1.0/accounts/9007199254740993/positions":
			body = `[{"code":"US.AAPL","qty":"100","can_sell_qty":"100","position_side":"LONG","currency":"USD"}]`
		default:
			t.Fatal("unexpected read")
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"s":"ok","d":` + body + `}`))}, nil
	})}
	info, err := client.Observe(t.Context(), "9007199254740993")
	if err != nil || info == nil || len(info.Holdings) != 1 || !info.HoldingsComplete || info.AvailableFunds != nil || info.ReportedFunds != "12.3456789" || info.ExecutionSnapshotUsable {
		t.Fatalf("sub-cent display funds blocked holdings or became executable cash: %v", err)
	}
	raw, err := json.Marshal(info)
	if err != nil || strings.Contains(string(raw), `"available_cash_cents"`) || !strings.Contains(string(raw), `"reported_funds":"12.3456789"`) {
		t.Fatal("serialized observation fabricated or rounded cash")
	}
}
