package moomoo

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func quoteSnapshotFixture(at time.Time) string {
	return fmt.Sprintf(`{"code":"US.AAPL","update_time":%d,"data_date":"%s","bid_price":1.230000000000,"ask_price":1.25,"bid_vol":1,"ask_vol":2,"sec_status":"NORMAL","suspension":false,"option_valid":false}`, at.UnixMilli(), at.Format(time.DateOnly))
}

func TestMarketAuditPinnedReadsCompleteIdentityAndSourceFidelity(t *testing.T) {
	c := NewClient(evidenceCredential(t))
	at := time.Now().UTC()
	requests := 0
	c.http.Transport = reviewTransport(func(r *http.Request) (*http.Response, error) {
		requests++
		if r.URL.Scheme != "https" || r.URL.Host != "webapi.moomoo.com" || r.URL.RawQuery != "" {
			t.Fatal("market audit left the pinned origin")
		}
		body := ""
		if r.URL.Path == "/api/v1.0/accounts/authorized_trd_accs" && r.Method == http.MethodGet {
			body = `{"s":"ok","d":{"accounts":[{"account_id":123456,"security_firm":"FUTUINC","acc_type":"margin","enable_market":[2]}]}}`
		} else {
			if requests == 1 || r.Method != http.MethodPost || r.GetBody != nil {
				t.Fatal("quote read preceded account authorization or gained replay capability")
			}
			raw, _ := io.ReadAll(r.Body)
			if string(raw) != `{"code_list":["US.AAPL"]}` {
				t.Fatal("request changed explicit instrument identity")
			}
			switch r.URL.Path {
			case "/api/v1.0/quote/stock-basicinfo":
				body = `{"ret_code":0,"data":{"basic_list":[{"code":"US.AAPL","stock_type":"STOCK","exchange":"US","state":"NORMAL","lot_size":1,"suspension":false,"contract_size":null}]}}`
			case "/api/v1.0/quote/snapshot":
				body = `{"ret_code":0,"data":{"snapshot_list":[` + quoteSnapshotFixture(at) + `]}}`
			default:
				t.Fatal("market reader reached a financial resource")
			}
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}, nil
	})
	audit, err := c.AuditMarket(t.Context(), "123456", []string{"US.AAPL"})
	if err != nil {
		t.Fatal(err)
	}
	if requests != 3 || audit.ExecutionQualified || audit.AccountID != "123456" || audit.Books["US.AAPL"].Bid != 123 || audit.Books["US.AAPL"].Ask != 125 || audit.Books["US.AAPL"].PriceSpread != nil || audit.Instruments["US.AAPL"].LotSize != 1 {
		t.Fatal("market facts invented qualification/ticks or lost exact book data")
	}
	raw, err := json.MarshalIndent(audit, "", "  ")
	var restored MarketAudit
	if err != nil || privateJSON(strings.NewReader(string(raw)), 1<<20, &restored) != nil {
		t.Fatal("market evidence persistence failed")
	}
	for _, packet := range []*QuotePacket{restored.Basic, restored.Snapshot} {
		hash := sha256.Sum256(packet.Bytes)
		if packet.SHA256 != hex.EncodeToString(hash[:]) || packet.FinishedAt.Before(packet.At) || packet.Rows["US.AAPL"] == nil {
			t.Fatal("market source bytes/hash/read window did not survive persistence")
		}
	}
}

func TestQuoteReaderRejectsIncompleteAmbiguousAndUnsafeRequests(t *testing.T) {
	for _, body := range []string{
		`{"s":"ok","d":{"basic_list":[{"code":"US.AAPL"}]}}`,
		`{"ret_code":"0","data":{"basic_list":[{"code":"US.AAPL"}]}}`,
		`{"ret_code":0.0,"data":{"basic_list":[{"code":"US.AAPL"}]}}`,
		`{"ret_code":-9,"data":{"basic_list":[{"code":"US.AAPL"}]}}`,
		`{"ret_code":0,"error":{"code":"no_data"},"data":{"basic_list":[{"code":"US.AAPL"}]}}`,
		`{"ret_code":0,"RET_CODE":-1,"data":{"basic_list":[{"code":"US.AAPL"}]}}`,
		`{"ret_code":0,"data":{"basic_list":[]}}`,
		`{"ret_code":0,"data":{"basic_list":[{"Code":"US.AAPL"}]}}`,
		`{"ret_code":0,"data":{"basic_list":[{"code":"US.MSFT"}]}}`,
		`{"ret_code":0,"data":{"basic_list":[{"code":"US.AAPL","code":"US.MSFT"}]}}`,
		`{"ret_code":0,"data":{"basic_list":[{"code":"US.AAPL"},{"code":"US.AAPL"}]}}`,
	} {
		c := NewClient(evidenceCredential(t))
		hits := 0
		c.http.Transport = reviewTransport(func(*http.Request) (*http.Response, error) {
			hits++
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}, nil
		})
		if packet, err := c.readQuoteBatch(t.Context(), "/api/v1.0/quote/stock-basicinfo", []string{"US.AAPL"}); err == nil || packet != nil || hits != 1 {
			t.Fatal("partial or ambiguous market response was accepted/retried")
		}
	}
	c := NewClient("/does-not-exist")
	if _, err := c.readQuoteBatch(t.Context(), "/api/v1.0/accounts/123456/orders", []string{"US.AAPL"}); err == nil || !strings.Contains(err.Error(), "blocks this resource") {
		t.Fatal("quote reader exposed an arbitrary write path")
	}
	for _, codes := range [][]string{nil, {"US.AAPL", "US.AAPL"}, {"HK.00700"}, {"US.AAPL/../../accounts"}, {" US.AAPL"}, {"US.AAPL\n"}} {
		if _, err := c.readQuoteBatch(t.Context(), "/api/v1.0/quote/snapshot", codes); err == nil || !strings.Contains(err.Error(), "distinct explicit US codes") {
			t.Fatal("invalid quote request reached credentials/transport")
		}
	}
}

func TestMarketFactsRejectUnqualifiedMoneyClockAndDeliverables(t *testing.T) {
	at := time.Now().UTC()
	base := quoteSnapshotFixture(at)
	locked := strings.TrimSuffix(base, "}") + `,"price_spread":0}`
	book, err := marketQuoteFacts([]byte(locked), at)
	if err != nil || book.PriceSpread == nil || *book.PriceSpread != 0 {
		t.Fatal("a zero reported spread was confused with a positive execution tick")
	}
	for _, change := range [][2]string{
		{`"bid_price":1.230000000000`, `"bid_price":1.231`},
		{`"bid_price":1.230000000000`, `"bid_price":"1.23"`},
		{`"ask_price":1.25`, `"ask_price":1.22`},
		{`"bid_vol":1`, `"bid_vol":1.5`},
		{`"bid_vol":1`, `"bid_vol":-1`},
		{`"bid_vol":1`, `"bid_vol":1,"BID_VOL":100`},
		{`"option_valid":false`, `"option_valid":null`},
		{fmt.Sprintf(`"update_time":%d`, at.UnixMilli()), fmt.Sprintf(`"update_time":%d`, at.UnixMicro())},
		{fmt.Sprintf(`"update_time":%d`, at.UnixMilli()), fmt.Sprintf(`"update_time":%d`, at.Add(time.Hour).UnixMilli())},
	} {
		if _, err := marketQuoteFacts([]byte(strings.Replace(base, change[0], change[1], 1)), at); err == nil {
			t.Fatal("unqualified market scalar/clock was normalized into a usable book")
		}
	}
	option := `{"code":"US.AAPL261120C100000","stock_type":"DRVT","exchange":"US","state":"NORMAL","lot_size":100,"suspension":false,"contract_size":100,"stock_owner":"US.AAPL"}`
	facts, err := instrumentFacts([]byte(option))
	if err != nil || facts.ContractSize == nil || *facts.ContractSize != 100 || facts.Owner != "US.AAPL" {
		t.Fatal("exact derivative metadata was not retained")
	}
	for _, changed := range []string{strings.Replace(option, `"contract_size":100`, `"contract_size":null`, 1), strings.Replace(option, `"stock_owner":"US.AAPL"`, `"stock_owner":"HK.00700"`, 1), strings.Replace(option, `"contract_size":100`, `"CONTRACT_SIZE":100`, 1)} {
		if _, err := instrumentFacts([]byte(changed)); err == nil {
			t.Fatal("unknown/foreign deliverable metadata accepted")
		}
	}
}
