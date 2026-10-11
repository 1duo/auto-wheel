package databento

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"os"

	"golang.org/x/sys/unix"
)

const plannedBatchReserveBytes = int64(6 << 30)

type plannedBatchTransport struct {
	base     http.RoundTripper
	job      string
	files    []batchFilePlan
	output   string
	total    int64
	evidence *os.Root
}

func samePlannedFiles(expected, actual []batchFilePlan) bool {
	if len(expected) != len(actual) {
		return false
	}
	type identity struct {
		size int64
		hash string
	}
	wanted := make(map[string]identity, len(expected))
	for _, file := range expected {
		wanted[file.Filename] = identity{file.Size, file.Hash}
	}
	for _, file := range actual {
		if value, ok := wanted[file.Filename]; !ok || value != (identity{file.Size, file.Hash}) {
			return false
		}
	}
	return true
}

func plannedBatchRoom(output string, total int64) error {
	fail := errors.New("databento planned batch insufficient verified disk space")
	root, err := os.OpenRoot(output)
	if err != nil {
		return fail
	}
	defer root.Close()
	dir, err := root.Open(".")
	if err != nil {
		return fail
	}
	var st unix.Statfs_t
	err = unix.Fstatfs(int(dir.Fd()), &st)
	if errors.Join(err, dir.Close()) != nil || st.Bsize <= 0 || total < 1 || total > maxBatchTotalBytes {
		return fail
	}
	needed := total + plannedBatchReserveBytes
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

func (t plannedBatchTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	response, err := t.base.RoundTrip(req)
	if response == nil && err == nil {
		return nil, errors.New("databento batch response unavailable")
	}
	if err != nil || req.Method != http.MethodGet || req.URL.Path != "/v0"+BatchListFilesEndpoint || response.StatusCode != http.StatusOK {
		return response, err
	}
	fail := errors.New("databento batch file plan changed; no data downloaded")
	raw, readErr := io.ReadAll(io.LimitReader(response.Body, (4<<20)+1))
	closeErr := response.Body.Close()
	if errors.Join(readErr, closeErr) != nil || len(raw) > 4<<20 {
		return nil, fail
	}
	if err := writeBatchPrivate(t.evidence, "files-source.json", raw); err != nil {
		return nil, err
	}
	actual, err := validateBatchFiles(raw, t.job)
	if err != nil || !samePlannedFiles(t.files, actual) {
		return nil, fail
	}
	if err := plannedBatchRoom(t.output, t.total); err != nil {
		return nil, err
	}
	response.Body = io.NopCloser(bytes.NewReader(raw))
	response.ContentLength = int64(len(raw))
	return response, nil
}

// RetrievePlannedBatch requires the same validated names, sizes and hashes as
// the earlier file plan and six GiB free after transfer. Refreshing delivery
// URLs or file ordering is allowed; file contents and sizes cannot change.
// It reuses the sealed downloader and leaves the purchase reservation held.
func (c *Client) RetrievePlannedBatch(ctx context.Context, q BatchQuery, ack BatchAcknowledgement, ackBody, plannedFiles []byte, output string) (BatchRetrievalReceipt, error) {
	files, err := validateBatchFiles(plannedFiles, ack.JobID)
	if err != nil {
		return BatchRetrievalReceipt{}, err
	}
	var total int64
	for _, file := range files {
		total += file.Size
	}
	// Keep the refreshed original response even when changed metadata blocks
	// transfer. This separate directory does not alter the sealed data roster.
	if err := os.Mkdir(output+".file-plan", 0700); err != nil {
		return BatchRetrievalReceipt{}, errors.New("databento planned batch evidence must be new")
	}
	evidence, err := os.OpenRoot(output + ".file-plan")
	if err != nil {
		return BatchRetrievalReceipt{}, errors.New("databento planned batch evidence open failed")
	}
	defer evidence.Close()
	if err := writeBatchPrivate(evidence, "planned-files-source.json", plannedFiles); err != nil {
		return BatchRetrievalReceipt{}, err
	}
	c.mu.Lock()
	if c.closed || c.httpClient.Transport == nil {
		c.mu.Unlock()
		return BatchRetrievalReceipt{}, errors.New("databento client unavailable")
	}
	transport := plannedBatchTransport{base: c.httpClient.Transport, job: ack.JobID, files: files, output: output, total: total, evidence: evidence}
	client, err := NewClientWithTransport(c.key, c.ledger, transport)
	c.mu.Unlock()
	if err != nil {
		return BatchRetrievalReceipt{}, err
	}
	defer client.Close()
	return client.RetrieveBatch(ctx, q, ack, ackBody, output, true)
}
