package databento

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func retrievalFixture(t *testing.T, state, corruption string) (*Client, BatchQuery, BatchAcknowledgement, []byte, *Ledger, *int) {
	t.Helper()
	l := newTestLedger(t)
	if err := l.EnableUncapped(); err != nil {
		t.Fatal(err)
	}
	q := newBatchFullDefinitionQuery()
	hash, _ := q.Hash()
	op, _ := q.OperationID(BatchSubmitEndpoint)
	at := time.Now().UTC()
	r, err := l.Reserve(op, hash, BatchSubmitEndpoint, 100, at, strings.Repeat("1", 64))
	if err != nil {
		t.Fatal(err)
	}
	ackBody := batchJobBody(t, q, "OPRA-TEST", "queued")
	ack := BatchAcknowledgement{OperationID: op, QueryHash: hash, Endpoint: BatchSubmitEndpoint, CostNano: r.CostNano, ReservedNano: r.ReservedNano, ResponseHash: digestBytes(ackBody), Bytes: int64(len(ackBody)), StatusCode: 200, At: r.At, JobID: "OPRA-TEST", State: "queued"}
	data := map[string]string{"test.definition.dbn.zst": "fake-DBN-test-only", "metadata.json": "{}"}
	var plans []map[string]any
	for _, name := range []string{"test.definition.dbn.zst", "metadata.json"} {
		url := "https://api.databento.com/v0/batch/download/TEST/OPRA-TEST/" + name
		if corruption == "host" {
			url = "https://evil.example/v0/batch/download/TEST/OPRA-TEST/" + name
		}
		plans = append(plans, map[string]any{"filename": name, "size": len(data[name]), "hash": "sha256:" + digestBytes([]byte(data[name])), "urls": map[string]string{"https": url}})
	}
	manifest, _ := json.Marshal(plans)
	calls := new(0)
	rt := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		*calls++
		if req.Method != http.MethodGet {
			t.Fatal("acquisition attempted a financial POST")
		}
		var b []byte
		switch req.URL.Path {
		case "/v0" + BatchJobDetailsEndpoint:
			job := "OPRA-TEST"
			if corruption == "job" {
				job = "OPRA-WRONG"
			}
			b = batchJobBody(t, q, job, state)
		case "/v0" + BatchListFilesEndpoint:
			b = manifest
		default:
			name := filepath.Base(req.URL.Path)
			b = []byte(data[name])
			if corruption == "hash" {
				b[0] = 'X'
			}
			if corruption == "short" {
				b = b[:len(b)-1]
			}
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(string(b))), ContentLength: -1, Header: make(http.Header)}, nil
	})
	c, err := NewClientWithTransport(newTestKey(t), l, rt)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return c, q, ack, ackBody, l, calls
}

func TestBatchRetrieveVerifiedLeavesReservation(t *testing.T) {
	c, q, ack, body, l, calls := retrievalFixture(t, "done", "")
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	out := filepath.Join(t.TempDir(), "private")
	r, err := c.RetrieveBatch(ctx, q, ack, body, out, true)
	if err != nil || !r.AllVerified || len(r.Files) != 2 || r.TotalBytes != 20 || *calls != 4 {
		t.Fatalf("receipt %+v calls%d err%v", r, *calls, err)
	}
	entry, _ := l.Find(ack.OperationID)
	if entry.Status != "reserved" || entry.CostNano != 100 {
		t.Fatal("reservation changed")
	}
	b, err := os.ReadFile(filepath.Join(out, "receipt.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "api.databento.com") || strings.Contains(string(b), "TEST/OPRA") {
		t.Fatal("private URL leaked into receipt")
	}
	info, _ := os.Stat(filepath.Join(out, r.Files[0].Filename))
	if info.Mode().Perm() != 0600 {
		t.Fatal("file not private")
	}
	if _, err := c.RetrieveBatch(ctx, q, ack, body, out, true); err == nil {
		t.Fatal("output overwrite allowed")
	}
	if *calls != 4 {
		t.Fatal("rerun sent a request before refusing overwrite")
	}
}

func TestBatchRetrieveRejectsBeforeDownload(t *testing.T) {
	for _, failure := range []string{"queued", "processing", "job", "host", "hash", "short", "ack"} {
		t.Run(failure, func(t *testing.T) {
			state := "done"
			if failure == "queued" || failure == "processing" {
				state = failure
			}
			c, q, ack, body, l, calls := retrievalFixture(t, state, failure)
			if failure == "ack" {
				ack.ResponseHash = strings.Repeat("0", 64)
			}
			ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
			defer cancel()
			out := filepath.Join(t.TempDir(), "private")
			r, err := c.RetrieveBatch(ctx, q, ack, body, out, true)
			if err == nil || r.AllVerified {
				t.Fatal("invalid acquisition accepted")
			}
			if _, err := os.Stat(filepath.Join(out, "receipt.json")); !os.IsNotExist(err) {
				t.Fatal("failed acquisition left a complete receipt")
			}
			if failure == "ack" && *calls != 0 {
				t.Fatal("invalid ACK sent a request")
			}
			if failure == "queued" || failure == "processing" {
				if *calls != 1 || r.State != state || !strings.Contains(err.Error(), "job is "+state+";") {
					t.Fatal("nonterminal job requested files or lost its exact provider state")
				}
			}
			x, _ := l.Find(ack.OperationID)
			if x.Status != "reserved" {
				t.Fatal("hold changed")
			}
		})
	}
}

func TestBatchFileManifestRejectsUnsafeShape(t *testing.T) {
	good := map[string]any{"filename": "test.definition.dbn.zst", "size": 1, "hash": "sha256:" + strings.Repeat("1", 64), "urls": map[string]string{"https": "https://api.databento.com/v0/batch/download/TEST/OPRA-TEST/test.definition.dbn.zst"}}
	for _, bad := range []struct {
		field string
		value any
	}{{"filename", "../data"}, {"filename", "receipt.json"}, {"size", nil}, {"size", true}, {"size", 1.5}, {"size", -1}, {"size", maxBatchFileBytes + 1}, {"hash", "sha256:bad"}, {"urls", map[string]string{"https": "https://api.databento.com/v0/batch/download/TEST/WRONG/test.definition.dbn.zst"}}} {
		m := make(map[string]any)
		for k, v := range good {
			m[k] = v
		}
		m[bad.field] = bad.value
		b, _ := json.Marshal([]any{m})
		if _, err := validateBatchFiles(b, "OPRA-TEST"); err == nil {
			t.Fatalf("accepted %s=%v", bad.field, bad.value)
		}
	}
	b, _ := json.Marshal([]any{good, good})
	if _, err := validateBatchFiles(b, "OPRA-TEST"); err == nil {
		t.Fatal("duplicate file accepted")
	}
	dup := `[{"filename":"test.definition.dbn.zst","size":1,"size":2}]`
	if _, err := validateBatchFiles([]byte(dup), "OPRA-TEST"); err == nil {
		t.Fatal("duplicate key accepted")
	}
}

func TestBatchDecodeInputsRejectsAmbiguity(t *testing.T) {
	q, _ := json.Marshal(newBatchFullDefinitionQuery())
	ack, _ := json.Marshal(BatchAcknowledgement{})
	if _, _, err := DecodeBatchInputs(q, ack); err != nil {
		t.Fatal(err)
	}
	if _, _, err := DecodeBatchInputs(append(q, []byte("{}")...), ack); err == nil {
		t.Fatal("trailing JSON accepted")
	}
	if _, _, err := DecodeBatchInputs(q, []byte(`{"status_code":200,"status_code":200}`)); err == nil {
		t.Fatal("duplicate ACK key accepted")
	}
}
