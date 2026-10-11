package databento

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

const (
	BatchJobDetailsEndpoint = "/batch.get_job_details"
	BatchListFilesEndpoint  = "/batch.list_files"
	maxBatchFiles           = 4096
	maxBatchFileBytes       = int64(8 << 30)
	maxBatchTotalBytes      = int64(64 << 30)
)

type BatchFileResult struct {
	Filename string `json:"filename"`
	Bytes    int64  `json:"bytes"`
	SHA256   string `json:"sha256"`
}

// BatchRetrievalReceipt proves downloaded bytes only. Retrieval leaves the
// purchase reservation reserved; offline finalization and historical source
// qualification each require separate verification.
type BatchRetrievalReceipt struct {
	Query          BatchQuery        `json:"query"`
	OperationID    string            `json:"operation_id"`
	QueryHash      string            `json:"query_hash"`
	JobID          string            `json:"job_id"`
	State          string            `json:"state"`
	AckSHA256      string            `json:"ack_sha256"`
	JobSHA256      string            `json:"job_sha256"`
	FileListSHA256 string            `json:"file_list_sha256,omitempty"`
	Files          []BatchFileResult `json:"files,omitempty"`
	TotalBytes     int64             `json:"total_bytes"`
	ObservedAt     time.Time         `json:"observed_at"`
	CompletedAt    time.Time         `json:"completed_at,omitzero"`
	AllVerified    bool              `json:"all_verified"`
}

type batchFilePlan struct {
	Filename string            `json:"filename"`
	Size     int64             `json:"size"`
	Hash     string            `json:"hash"`
	URLs     map[string]string `json:"urls"`
}

// DecodeBatchInputs rejects duplicate keys, unknown fields, and trailing data
// in the locally saved query and ACK. The original ACK body is verified later.
func DecodeBatchInputs(query, acknowledgement []byte) (BatchQuery, BatchAcknowledgement, error) {
	var q BatchQuery
	var ack BatchAcknowledgement
	for _, input := range []struct {
		raw    []byte
		target any
	}{{query, &q}, {acknowledgement, &ack}} {
		if len(input.raw) == 0 || len(input.raw) > 1<<20 || rejectDuplicateKeys(input.raw) != nil {
			return q, ack, errors.New("databento saved batch input invalid")
		}
		dec := json.NewDecoder(bytes.NewReader(input.raw))
		dec.DisallowUnknownFields()
		if dec.Decode(input.target) != nil || dec.Decode(new(any)) != io.EOF {
			return q, ack, errors.New("databento saved batch input invalid")
		}
	}
	return q, ack, q.Validate()
}

func digestBytes(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func batchBasename(s string) bool {
	if len(s) == 0 || len(s) > 255 || s[0] == '.' {
		return false
	}
	for _, ch := range s {
		if !(ch >= 'A' && ch <= 'Z' || ch >= 'a' && ch <= 'z' || ch >= '0' && ch <= '9' || ch == '_' || ch == '-' || ch == '.') {
			return false
		}
	}
	return s != "job-source.json" && s != "files-source.json" && s != "receipt.json"
}

func validateBatchFiles(raw []byte, job string) ([]batchFilePlan, error) {
	fail := errors.New("databento batch file manifest invalid")
	if len(raw) == 0 || len(raw) > 4<<20 || rejectDuplicateKeys(raw) != nil {
		return nil, fail
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var files []batchFilePlan
	if dec.Decode(&files) != nil || dec.Decode(new(any)) != io.EOF || len(files) == 0 || len(files) > maxBatchFiles {
		return nil, fail
	}
	var objects []map[string]json.RawMessage
	if json.Unmarshal(raw, &objects) != nil {
		return nil, fail
	}
	for _, obj := range objects {
		value, ok := obj["size"]
		if !ok || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return nil, fail
		}
	}
	seen := make(map[string]bool, len(files))
	var total int64
	data := false
	for _, f := range files {
		if !batchBasename(f.Filename) || seen[f.Filename] || f.Size < 0 || f.Size > maxBatchFileBytes || f.Size > maxBatchTotalBytes-total {
			return nil, fail
		}
		seen[f.Filename] = true
		total += f.Size
		if len(f.Hash) != 71 || !strings.HasPrefix(f.Hash, "sha256:") {
			return nil, fail
		}
		if _, err := hex.DecodeString(f.Hash[7:]); err != nil || f.Hash != strings.ToLower(f.Hash) {
			return nil, fail
		}
		u, err := url.Parse(f.URLs["https"])
		if err != nil || u.Scheme != "https" || (u.Host != "api.databento.com" && u.Host != "hist.databento.com") || u.User != nil || u.Fragment != "" || u.RawQuery != "" || u.ForceQuery || u.EscapedPath() != u.Path {
			return nil, fail
		}
		parts := strings.Split(u.Path, "/")
		if len(parts) != 7 || parts[0] != "" || parts[1] != "v0" || parts[2] != "batch" || parts[3] != "download" || !isBatchJobID(parts[4]) || parts[5] != job || parts[6] != f.Filename {
			return nil, fail
		}
		data = data || strings.HasSuffix(f.Filename, ".dbn.zst") && f.Size > 0
	}
	if !data {
		return nil, fail
	}
	return files, nil
}

func batchDiskRoom(root *os.Root, files []batchFilePlan) error {
	fail := errors.New("databento batch insufficient verified disk space")
	dir, err := root.Open(".")
	if err != nil {
		return fail
	}
	var st unix.Statfs_t
	err = unix.Fstatfs(int(dir.Fd()), &st)
	if errors.Join(err, dir.Close()) != nil || st.Bsize <= 0 {
		return fail
	}
	needed := int64(1 << 30) // Keep room for the production services sharing this disk.
	for _, f := range files {
		needed += f.Size
	} // Manifest validation bounds the total to64GiB.
	blockSize := int64(st.Bsize)
	blocks := uint64(needed / blockSize)
	if needed%blockSize != 0 {
		blocks++
	}
	if st.Bavail < blocks {
		return fail
	}
	return nil
}

func writeBatchPrivate(root *os.Root, name string, b []byte) error {
	f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return errors.New("databento private evidence create failed")
	}
	_, err = f.Write(b)
	if err == nil {
		err = f.Sync()
	}
	if errors.Join(err, f.Close()) != nil {
		return errors.New("databento private evidence write failed")
	}
	dir, err := root.Open(".")
	if err != nil {
		return errors.New("databento private directory sync failed")
	}
	if errors.Join(dir.Sync(), dir.Close()) != nil {
		return errors.New("databento private directory sync failed")
	}
	return nil
}

func (c *Client) batchGET(ctx context.Context, endpoint, job string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, BaseURL+endpoint+"?"+url.Values{"job_id": {job}}.Encode(), nil)
	if err != nil {
		return nil, errors.New("databento batch observation failed")
	}
	c.auth(req)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, errors.New("databento batch observation failed")
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, (4<<20)+1))
	if err != nil || len(raw) > 4<<20 || resp.StatusCode != http.StatusOK {
		return nil, errors.New("databento batch observation failed")
	}
	return raw, nil
}

// RetrieveBatch issues GET requests only for a job proved by its original ACK
// body and retained purchase reservation. It never submits, retries, reprices,
// releases a hold, or calls Complete. Partial files remain private evidence.
func (c *Client) RetrieveBatch(ctx context.Context, q BatchQuery, ack BatchAcknowledgement, ackBody []byte, output string, download bool) (BatchRetrievalReceipt, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var result BatchRetrievalReceipt
	fail := errors.New("databento batch acquisition identity invalid")
	if c.closed || !c.ledger.IsUncapped() {
		return result, fail
	}
	if download {
		if _, ok := ctx.Deadline(); !ok {
			return result, errors.New("databento batch download requires a context deadline")
		}
	}
	hash, err := q.Hash()
	if err != nil {
		return result, fail
	}
	op, err := q.OperationID(BatchSubmitEndpoint)
	if err != nil {
		return result, fail
	}
	r, ok := c.ledger.Find(op)
	if !ok || r.Status != "reserved" || r.Endpoint != BatchSubmitEndpoint || r.QueryHash != hash || ack.OperationID != op || ack.QueryHash != hash || ack.Endpoint != BatchSubmitEndpoint || ack.CostNano != r.CostNano || ack.ReservedNano != r.ReservedNano || !ack.At.Equal(r.At) || ack.StatusCode != http.StatusOK || ack.Bytes != int64(len(ackBody)) || ack.ResponseHash != digestBytes(ackBody) {
		return result, fail
	}
	id, state, err := validateBatchSubmitBody(ackBody, q)
	if err != nil || id != ack.JobID || state != ack.State {
		return result, fail
	}
	if err := os.Mkdir(output, 0700); err != nil {
		return result, errors.New("databento batch output must be a new private directory")
	}
	root, err := os.OpenRoot(output)
	if err != nil {
		return result, errors.New("databento batch output open failed")
	}
	defer root.Close()
	raw, err := c.batchGET(ctx, BatchJobDetailsEndpoint, id)
	if err != nil {
		return result, err
	}
	if err := writeBatchPrivate(root, "job-source.json", raw); err != nil {
		return result, err
	}
	gotID, state, err := validateBatchSubmitBody(raw, q)
	if err != nil || gotID != id {
		return result, fail
	}
	result = BatchRetrievalReceipt{Query: q, OperationID: op, QueryHash: hash, JobID: id, State: state, AckSHA256: digestBytes(ackBody), JobSHA256: digestBytes(raw), ObservedAt: time.Now().UTC()}
	if download {
		if state != "done" {
			return result, errors.New("databento batch job is " + state + "; no files downloaded")
		}
		raw, err = c.batchGET(ctx, BatchListFilesEndpoint, id)
		if err != nil {
			return result, err
		}
		if err := writeBatchPrivate(root, "files-source.json", raw); err != nil {
			return result, err
		}
		files, err := validateBatchFiles(raw, id)
		if err != nil {
			return result, err
		}
		if err := batchDiskRoom(root, files); err != nil {
			return result, err
		}
		result.FileListSHA256 = digestBytes(raw)
		for _, file := range files {
			if err := c.downloadBatchFile(ctx, root, file); err != nil {
				return result, err
			}
			result.Files = append(result.Files, BatchFileResult{Filename: file.Filename, Bytes: file.Size, SHA256: file.Hash[7:]})
			result.TotalBytes += file.Size
		}
		if ctx.Err() != nil {
			return result, errors.New("databento batch acquisition interrupted")
		}
		result.AllVerified = true
		result.CompletedAt = time.Now().UTC()
	}
	body, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return result, errors.New("databento batch receipt encoding failed")
	}
	if err := writeBatchPrivate(root, "receipt.json", append(body, '\n')); err != nil {
		result.AllVerified = false
		return result, err
	}
	return result, nil
}

func (c *Client) downloadBatchFile(ctx context.Context, root *os.Root, file batchFilePlan) error {
	fail := errors.New("databento batch file acquisition failed; partial private evidence retained")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, file.URLs["https"], nil)
	if err != nil {
		return fail
	}
	c.auth(req)
	client := *c.httpClient
	client.Timeout = 0 // The required context deadline bounds the complete transfer.
	resp, err := client.Do(req)
	if err != nil {
		return fail
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.ContentLength >= 0 && resp.ContentLength != file.Size {
		return fail
	}
	f, err := root.OpenFile(file.Filename, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return fail
	}
	hash := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, hash), io.LimitReader(resp.Body, file.Size+1))
	if err == nil {
		err = f.Sync()
	}
	err = errors.Join(err, f.Close())
	if err != nil || n != file.Size || hex.EncodeToString(hash.Sum(nil)) != file.Hash[7:] {
		return fail
	}
	return nil
}
