package databento

import (
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

//go:embed native_batch_completeness.py
var nativeBatchVerifier string

//go:embed native_book_completeness.py
var nativeBookVerifier string

const (
	maxNativeBookFileBytes  int64 = 64 << 30
	maxNativeBookMetaBytes  int64 = 16 << 20
	maxNativeBookRecordSize int64 = 88
)

func isNativeBook(q BatchQuery) bool {
	return q.Dataset == "OPRA.PILLAR" && q.Schema == "cmbp-1" || q.Dataset == "EQUS.MINI" && q.Schema == "mbp-1"
}

func nativeBookExpansionLimit(records int64) (int64, error) {
	if records <= 0 || records > (maxInt64Headroom-maxNativeBookMetaBytes)/maxNativeBookRecordSize {
		return 0, errBatchProof
	}
	return min(maxNativeBookFileBytes, records*maxNativeBookRecordSize+maxNativeBookMetaBytes), nil
}

func batchVerifier(q BatchQuery) string {
	if isNativeBook(q) {
		return nativeBookVerifier
	}
	return nativeBatchVerifier
}

// BatchAcquisitionInputs names original private evidence and a new proof file.
// Finalization is offline: no key, HTTP request, purchase, or invoice inference.
type BatchAcquisitionInputs struct {
	QueryPath, AcknowledgementPath, AckBodyPath  string
	SourceDirectory, ProofPath, PythonExecutable string
}

type nativeBatchFile struct {
	Filename           string `json:"filename"`
	CompressedSHA256   string `json:"compressed_sha256"`
	CompressedBytes    int64  `json:"compressed_bytes"`
	UncompressedSHA256 string `json:"uncompressed_sha256"`
	UncompressedBytes  int64  `json:"uncompressed_bytes"`
	Records            int64  `json:"records"`
	NativeVersion      int    `json:"native_version"`
	EOFVerified        bool   `json:"eof_verified"`
}

type nativeBatchReport struct {
	SchemaVersion  string            `json:"schema_version"`
	DecoderVersion string            `json:"decoder_version"`
	UpgradePolicy  string            `json:"upgrade_policy"`
	Files          []nativeBatchFile `json:"files"`
	Records        int64             `json:"records"`
	AllEOFVerified bool              `json:"all_eof_verified"`
}

// BatchAcquisitionProof binds all delivered bytes and native EOF/counts to the
// original held reservation. DataSHA256 in an acquired row hashes this artifact;
// Bytes counts the complete manifest. Acquisition never qualifies a replay.
type BatchAcquisitionProof struct {
	SchemaVersion             string               `json:"schema_version"`
	Reservation               Reservation          `json:"reservation"`
	Query                     BatchQuery           `json:"query"`
	Acknowledgement           BatchAcknowledgement `json:"acknowledgement"`
	QueryPath                 string               `json:"query_path"`
	AcknowledgementPath       string               `json:"acknowledgement_path"`
	AckBodyPath               string               `json:"ack_body_path"`
	SourceDirectory           string               `json:"source_directory"`
	QueryFileSHA256           string               `json:"query_file_sha256"`
	AcknowledgementFileSHA256 string               `json:"acknowledgement_file_sha256"`
	AckBodySHA256             string               `json:"ack_body_sha256"`
	JobSHA256                 string               `json:"job_sha256"`
	FileListSHA256            string               `json:"file_list_sha256"`
	ReceiptSHA256             string               `json:"receipt_sha256"`
	JobID                     string               `json:"job_id"`
	Files                     []BatchFileResult    `json:"files"`
	TotalBytes                int64                `json:"total_bytes"`
	ProviderRecordCount       int64                `json:"provider_record_count"`
	NativeVerifierSHA256      string               `json:"native_verifier_sha256"`
	Native                    nativeBatchReport    `json:"native"`
	VerifiedAt                time.Time            `json:"verified_at"`
	InvoiceCostVerified       bool                 `json:"invoice_cost_verified"`
	SourceQualified           bool                 `json:"source_qualified"`
}

var errBatchProof = errors.New("databento offline batch evidence invalid; hold preserved")

func privateBatchRoot(path string) (*os.Root, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, errBatchProof
	}
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0700 || info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
		return nil, errBatchProof
	}
	var owner unix.Stat_t
	if unix.Lstat(path, &owner) != nil || int(owner.Uid) != os.Getuid() {
		return nil, errBatchProof
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		return nil, errBatchProof
	}
	return root, nil
}

func privateBatchFile(root *os.Root, name string) (*os.File, error) {
	f, err := root.OpenFile(name, os.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, errBatchProof
	}
	var st unix.Stat_t
	if unix.Fstat(int(f.Fd()), &st) != nil || st.Mode&unix.S_IFMT != unix.S_IFREG || st.Mode&07777 != 0600 || int(st.Uid) != os.Getuid() || st.Nlink != 1 {
		f.Close()
		return nil, errBatchProof
	}
	return f, nil
}

func batchEvidence(path string, bound int64) ([]byte, error) {
	root, err := privateBatchRoot(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	defer root.Close()
	f, err := privateBatchFile(root, filepath.Base(path))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, bound+1))
	if err != nil || len(raw) == 0 || int64(len(raw)) > bound {
		return nil, errBatchProof
	}
	return raw, nil
}

func strictBatchJSON(raw []byte, target any) error {
	if rejectDuplicateKeys(raw) != nil {
		return errBatchProof
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(target) != nil || d.Decode(new(any)) != io.EOF {
		return errBatchProof
	}
	return nil
}

type boundedBatchOutput struct{ bytes.Buffer }

func (b *boundedBatchOutput) Write(p []byte) (int, error) {
	if len(p) > (4<<20)-b.Len() {
		return 0, errBatchProof
	}
	return b.Buffer.Write(p)
}

func runNativeBatch(ctx context.Context, python string, q BatchQuery, directory string, files []BatchFileResult, providerRecords int64) (nativeBatchReport, error) {
	var report nativeBatchReport
	maxExpanded, schema := maxBatchFileBytes, "AutoWheelNativeBatchCompletenessV1"
	var bookRecords int64
	if isNativeBook(q) {
		var err error
		maxExpanded, err = nativeBookExpansionLimit(providerRecords)
		if err != nil {
			return report, err
		}
		bookRecords, schema = providerRecords, "AutoWheelNativeBookCompletenessV1"
	}
	if !filepath.IsAbs(python) {
		return report, errBatchProof
	}
	resolved, err := filepath.EvalSymlinks(python)
	if err != nil {
		return report, errBatchProof
	}
	var st unix.Stat_t
	if unix.Stat(resolved, &st) != nil || st.Mode&unix.S_IFMT != unix.S_IFREG || st.Mode&0022 != 0 || st.Mode&0111 == 0 || st.Mode&(unix.S_ISUID|unix.S_ISGID|unix.S_ISVTX) != 0 || (st.Uid != 0 && int(st.Uid) != os.Getuid()) {
		return report, errBatchProof
	}
	input := struct {
		SourceDirectory string            `json:"source_directory"`
		Dataset         string            `json:"dataset"`
		Schema          string            `json:"schema"`
		Start           string            `json:"start"`
		End             string            `json:"end"`
		Files           []BatchFileResult `json:"files"`
		RecordCount     int64             `json:"record_count,omitzero"`
	}{directory, q.Dataset, q.Schema, q.Start, q.End, files, bookRecords}
	raw, err := json.Marshal(input)
	if err != nil || len(raw) > 4<<20 {
		return report, errBatchProof
	}
	cmd := exec.CommandContext(ctx, python, "-I", "-c", batchVerifier(q))
	cmd.Env = []string{"PATH=/usr/bin:/bin", "LC_ALL=C", "TZ=UTC"}
	cmd.Stdin = bytes.NewReader(raw)
	var out boundedBatchOutput
	cmd.Stdout, cmd.Stderr = &out, io.Discard
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if err := unix.Kill(-cmd.Process.Pid, unix.SIGKILL); err != nil && !errors.Is(err, unix.ESRCH) {
			return err
		}
		return os.ErrProcessDone
	}
	cmd.WaitDelay = 5 * time.Second
	if cmd.Run() != nil || strictBatchJSON(out.Bytes(), &report) != nil {
		return report, errBatchProof
	}
	if report.SchemaVersion != schema || report.DecoderVersion != "0.71.0" || report.UpgradePolicy != "AS_IS" || !report.AllEOFVerified || report.Records <= 0 || len(report.Files) != len(files) {
		return report, errBatchProof
	}
	var count int64
	for i, member := range report.Files {
		if member.Filename != files[i].Filename || member.CompressedSHA256 != files[i].SHA256 || member.CompressedBytes != files[i].Bytes || !validBatchHash(member.UncompressedSHA256) || member.UncompressedBytes <= 0 || member.UncompressedBytes > maxExpanded || member.Records < 0 || member.Records > maxInt64Headroom-count || member.NativeVersion < 1 || member.NativeVersion > 3 || !member.EOFVerified {
			return report, errBatchProof
		}
		count += member.Records
	}
	if count != report.Records || isNativeBook(q) && count != providerRecords {
		return report, errBatchProof
	}
	return report, nil
}

func validBatchHash(value string) bool {
	_, err := hex.DecodeString(value)
	return len(value) == 64 && err == nil
}

type batchContextReader struct {
	ctx    context.Context
	source io.Reader
}

func (r batchContextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.source.Read(p)
}

func verifyBatchMembers(ctx context.Context, root *os.Root, files []BatchFileResult) error {
	for _, member := range files {
		if ctx.Err() != nil {
			return errBatchProof
		}
		f, err := privateBatchFile(root, member.Filename)
		if err != nil {
			return err
		}
		// Hash through the checked descriptor; bound even a concurrently growing file.
		info, err := f.Stat()
		if err != nil || info.Size() != member.Bytes {
			f.Close()
			return errBatchProof
		}
		h := sha256.New()
		n, err := io.Copy(h, io.LimitReader(batchContextReader{ctx, f}, member.Bytes+1))
		cerr := f.Close()
		if err != nil || cerr != nil || n != member.Bytes || hex.EncodeToString(h.Sum(nil)) != member.SHA256 {
			return errBatchProof
		}
	}
	return nil
}

// FinalizeBatchAcquisition records acquired bytes only after an offline native
// EOF/count verification. It preserves every original financial field and hold.
func (l *Ledger) FinalizeBatchAcquisition(ctx context.Context, in BatchAcquisitionInputs) (BatchAcquisitionProof, error) {
	var p BatchAcquisitionProof
	if _, ok := ctx.Deadline(); !ok || ctx.Err() != nil {
		return p, errBatchProof
	}
	for _, path := range []string{in.QueryPath, in.AcknowledgementPath, in.AckBodyPath, in.SourceDirectory, in.ProofPath, in.PythonExecutable} {
		if !filepath.IsAbs(path) || filepath.Clean(path) != path {
			return p, errBatchProof
		}
	}
	queryRaw, err := batchEvidence(in.QueryPath, 1<<20)
	if err != nil {
		return p, err
	}
	ackRaw, err := batchEvidence(in.AcknowledgementPath, 1<<20)
	if err != nil {
		return p, err
	}
	ackBody, err := batchEvidence(in.AckBodyPath, 1<<20)
	if err != nil {
		return p, err
	}
	q, ack, err := DecodeBatchInputs(queryRaw, ackRaw)
	if err != nil {
		return p, errBatchProof
	}
	hash, _ := q.Hash()
	op, _ := q.OperationID(BatchSubmitEndpoint)
	r, found := l.Find(op)
	if !found || !l.IsUncapped() || r.Status != "reserved" || r.Endpoint != BatchSubmitEndpoint || r.QueryHash != hash || ack.OperationID != op || ack.QueryHash != hash || ack.Endpoint != r.Endpoint || ack.CostNano != r.CostNano || ack.ReservedNano != r.ReservedNano || !ack.At.Equal(r.At) || ack.StatusCode != 200 || ack.Bytes != int64(len(ackBody)) || ack.ResponseHash != digestBytes(ackBody) {
		return p, errBatchProof
	}
	id, state, err := validateBatchSubmitBody(ackBody, q)
	if err != nil || id != ack.JobID || state != ack.State {
		return p, errBatchProof
	}
	root, err := privateBatchRoot(in.SourceDirectory)
	if err != nil {
		return p, err
	}
	defer root.Close()
	jobRaw, err := batchEvidence(filepath.Join(in.SourceDirectory, "job-source.json"), 4<<20)
	if err != nil {
		return p, err
	}
	listRaw, err := batchEvidence(filepath.Join(in.SourceDirectory, "files-source.json"), 4<<20)
	if err != nil {
		return p, err
	}
	receiptRaw, err := batchEvidence(filepath.Join(in.SourceDirectory, "receipt.json"), 4<<20)
	if err != nil {
		return p, err
	}
	id, state, err = validateBatchSubmitBody(jobRaw, q)
	if err != nil || id != ack.JobID || state != "done" {
		return p, errBatchProof
	}
	var job struct {
		RecordCount int64 `json:"record_count"`
	}
	if json.Unmarshal(jobRaw, &job) != nil || job.RecordCount <= 0 {
		return p, errBatchProof
	}
	plans, err := validateBatchFiles(listRaw, id)
	if err != nil {
		return p, errBatchProof
	}
	var receipt BatchRetrievalReceipt
	if strictBatchJSON(receiptRaw, &receipt) != nil || !reflect.DeepEqual(receipt.Query, q) || receipt.OperationID != op || receipt.QueryHash != hash || receipt.JobID != id || receipt.State != "done" || !receipt.AllVerified || receipt.AckSHA256 != digestBytes(ackBody) || receipt.JobSHA256 != digestBytes(jobRaw) || receipt.FileListSHA256 != digestBytes(listRaw) || receipt.ObservedAt.Before(r.At) || receipt.CompletedAt.Before(receipt.ObservedAt) || receipt.CompletedAt.After(time.Now()) || len(receipt.Files) != len(plans) {
		return p, errBatchProof
	}
	var total int64
	var shards []BatchFileResult
	for i, plan := range plans {
		member := BatchFileResult{Filename: plan.Filename, Bytes: plan.Size, SHA256: plan.Hash[7:]}
		if member != receipt.Files[i] {
			return p, errBatchProof
		}
		total += member.Bytes
		if strings.HasSuffix(member.Filename, "."+q.Schema+".dbn.zst") {
			shards = append(shards, member)
		}
	}
	if total <= 0 || total != receipt.TotalBytes || len(shards) == 0 || verifyBatchMembers(ctx, root, receipt.Files) != nil {
		return p, errBatchProof
	}
	native, err := runNativeBatch(ctx, in.PythonExecutable, q, in.SourceDirectory, shards, job.RecordCount)
	if err != nil || native.Records != job.RecordCount {
		return p, errBatchProof
	}
	// Recheck originals and every member after the subprocess; a source swap
	// cannot make the ledger bind evidence different from what was verified.
	for _, source := range []struct {
		path  string
		raw   []byte
		bound int64
	}{{in.QueryPath, queryRaw, 1 << 20}, {in.AcknowledgementPath, ackRaw, 1 << 20}, {in.AckBodyPath, ackBody, 1 << 20}, {filepath.Join(in.SourceDirectory, "job-source.json"), jobRaw, 4 << 20}, {filepath.Join(in.SourceDirectory, "files-source.json"), listRaw, 4 << 20}, {filepath.Join(in.SourceDirectory, "receipt.json"), receiptRaw, 4 << 20}} {
		raw, err := batchEvidence(source.path, source.bound)
		if err != nil || !bytes.Equal(raw, source.raw) {
			return p, errBatchProof
		}
	}
	if verifyBatchMembers(ctx, root, receipt.Files) != nil || ctx.Err() != nil {
		return p, errBatchProof
	}
	p = BatchAcquisitionProof{SchemaVersion: "AutoWheelBatchAcquisitionProofV1", Reservation: r, Query: q, Acknowledgement: ack, QueryPath: in.QueryPath, AcknowledgementPath: in.AcknowledgementPath, AckBodyPath: in.AckBodyPath, SourceDirectory: in.SourceDirectory, QueryFileSHA256: digestBytes(queryRaw), AcknowledgementFileSHA256: digestBytes(ackRaw), AckBodySHA256: digestBytes(ackBody), JobSHA256: digestBytes(jobRaw), FileListSHA256: digestBytes(listRaw), ReceiptSHA256: digestBytes(receiptRaw), JobID: id, Files: receipt.Files, TotalBytes: total, ProviderRecordCount: job.RecordCount, NativeVerifierSHA256: digestBytes([]byte(batchVerifier(q))), Native: native, VerifiedAt: time.Now().UTC()}
	raw, err := json.MarshalIndent(p, "", "  ")
	if err != nil || len(raw) > 4<<20 {
		return p, errBatchProof
	}
	raw = append(raw, '\n')
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.lockFile == nil || l.failed || !uncappedState(l.state) || ctx.Err() != nil {
		return p, errBatchProof
	}
	index := slices.IndexFunc(l.state.Reservations, func(next Reservation) bool { return next.OperationID == op })
	if index < 0 || l.state.Reservations[index] != r {
		return p, errBatchProof
	}
	proofRoot, err := privateBatchRoot(filepath.Dir(in.ProofPath))
	if err != nil {
		return p, err
	}
	defer proofRoot.Close()
	if err := writeBatchPrivate(proofRoot, filepath.Base(in.ProofPath), raw); err != nil {
		return p, errBatchProof
	}
	if ctx.Err() != nil {
		return p, errBatchProof
	}
	updated := l.state
	updated.Reservations = slices.Clone(l.state.Reservations)
	updated.Reservations[index].Status = "acquired"
	updated.Reservations[index].DataSHA256 = digestBytes(raw)
	updated.Reservations[index].Bytes = total
	if validateLedgerState(updated) != nil {
		return p, errBatchProof
	}
	if err := writeLedgerFile(l.path, updated); err != nil {
		l.failed = true
		return p, err
	}
	l.state = updated
	return p, nil
}

func uncappedState(st ledgerState) bool {
	return st.Version == uncappedLedgerVersion && st.LifetimeNano == uncappedLifetimeNano && st.AuthorizationMode == uncappedAuthorizationMode
}
