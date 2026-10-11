// autowheel-native-oi exports provisional OI views from audited native vectors.
// It is offline and has no credential, broker, financial or network imports.
package main

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"os/signal"
	"path/filepath"
	"time"

	"autowheel/internal/causaloi"
)

const maxDecoded = 8 << 30

var errInput = errors.New("native OI: invalid private input or source completion")

type config struct {
	Schema            string             `json:"schema"`
	Dataset           string             `json:"dataset"`
	UTCStart          uint64             `json:"utc_start"`
	NativeVersion     uint8              `json:"native_version"`
	ExpectedRecords   uint64             `json:"expected_records"`
	VectorSHA256      string             `json:"vector_sha256"`
	VectorJSONLSHA256 string             `json:"vector_jsonl_sha256"`
	Bindings          []causaloi.Binding `json:"bindings"`
	Cuts              []uint64           `json:"cuts"`
}

type report struct {
	Schema                string            `json:"schema"`
	ObservedAt            string            `json:"observed_at"`
	Summary               causaloi.Summary  `json:"summary"`
	StatusCounts          map[string]uint64 `json:"status_counts"`
	ConfigSHA256          string            `json:"config_sha256"`
	VectorSHA256          string            `json:"vector_sha256"`
	VectorJSONLSHA256     string            `json:"vector_jsonl_sha256"`
	ViewsSHA256           string            `json:"views_sha256"`
	ViewsJSONLSHA256      string            `json:"views_jsonl_sha256"`
	SourceGatePassed      bool              `json:"source_gate_passed"`
	HistoricalPerformance bool              `json:"historical_performance_available"`
	FinancialRequests     int               `json:"financial_requests"`
}

func main() {
	configPath := flag.String("config", "", "private attested input scope")
	vectorPath := flag.String("vectors", "", "private audited gzip vector")
	out := flag.String("output-directory", "", "fresh private directory")
	flag.Parse()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if flag.NArg() != 0 || *configPath == "" || *vectorPath == "" || *out == "" || export(ctx, *configPath, *vectorPath, *out) != nil {
		// Do not expose licensed rows or arbitrary input/provider text.
		fmt.Fprintln(os.Stderr, "native OI export failed; no qualified result published")
		os.Exit(1)
	}
	fmt.Println("provisional native OI export complete; source gate remains pending")
}

func privateFile(path string) (*os.File, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return nil, errInput
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, errInput
	}
	opened, err := f.Stat()
	if err != nil || !os.SameFile(info, opened) {
		f.Close()
		return nil, errInput
	}
	return f, nil
}

func readConfig(path string) (config, string, error) {
	f, err := privateFile(path)
	if err != nil {
		return config{}, "", err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, (2<<20)+1))
	if err != nil || len(b) > 2<<20 {
		return config{}, "", errInput
	}
	var c config
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err := d.Decode(&c); err != nil || d.Decode(new(any)) != io.EOF ||
		c.Schema != "AutoWheelNativeOIScopeV1" || c.Dataset != "OPRA.PILLAR" {
		return config{}, "", errInput
	}
	h := sha256.Sum256(b)
	return c, hex.EncodeToString(h[:]), nil
}

func hashValue(s string) ([32]byte, error) {
	var out [32]byte
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != len(out) || hex.EncodeToString(b) != s {
		return out, errInput
	}
	copy(out[:], b)
	if out == ([32]byte{}) {
		return out, errInput
	}
	return out, nil
}

// JSON null must be rejected explicitly: unmarshalling null into an integer
// otherwise succeeds as zero. Native integer widths reject fractions/overflow.
func field[T any](raw json.RawMessage, out *T) error {
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) || json.Unmarshal(raw, out) != nil {
		return errInput
	}
	return nil
}

func record(line []byte) (causaloi.Record, error) {
	var fields []json.RawMessage
	var r causaloi.Record
	if json.Unmarshal(line, &fields) != nil || len(fields) != 17 {
		return r, errInput
	}
	errs := []error{
		field(fields[0], &r.Index), field(fields[1], &r.NativeVersion), field(fields[2], &r.RawSymbol),
		field(fields[3], &r.PublisherID), field(fields[4], &r.InstrumentID), field(fields[5], &r.ReceiveAt),
		field(fields[6], &r.EventAt), field(fields[7], &r.ReferenceAt), field(fields[8], &r.Price),
		field(fields[9], &r.Quantity), field(fields[10], &r.Sequence), field(fields[11], &r.SendDelta),
		field(fields[12], &r.StatType), field(fields[13], &r.ChannelID), field(fields[14], &r.UpdateAction),
		field(fields[15], &r.Flags), field(fields[16], &r.Rtype),
	}
	if errors.Join(errs...) != nil {
		return causaloi.Record{}, errInput
	}
	return r, nil
}

type vectorReader struct {
	reader *bufio.Reader
	bytes  int64
}

func (v *vectorReader) next(ctx context.Context) (causaloi.Record, error) {
	if err := ctx.Err(); err != nil {
		return causaloi.Record{}, err
	}
	line, err := v.reader.ReadSlice('\n')
	v.bytes += int64(len(line))
	if err == io.EOF && len(line) == 0 && v.bytes <= maxDecoded {
		return causaloi.Record{}, io.EOF
	}
	if err != nil || len(line) > 4096 || v.bytes > maxDecoded {
		return causaloi.Record{}, errInput
	}
	return record(line)
}

func compact(s causaloi.Snapshot, counts map[string]uint64) []any {
	rows := make([]any, 0, len(s.Observations))
	for _, o := range s.Observations {
		counts[string(o.Status)]++
		var instrument, receive, event, reference any
		if o.HasRecord {
			instrument, receive, event = o.Record.InstrumentID, o.Record.ReceiveAt, o.Record.EventAt
			if o.Record.ReferenceAt != math.MaxUint64 {
				reference = o.Record.ReferenceAt
			}
		}
		// Retain raw reference clocks even when a quantity is withheld.
		rows = append(rows, []any{o.RawSymbol, o.Status, instrument, o.Quantity, receive, event, reference, o.Copies})
	}
	return []any{s.Through, rows}
}

func writeReport(path string, value report) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	err = json.NewEncoder(f).Encode(value)
	return errors.Join(err, f.Sync(), f.Close())
}

func fileHash(path string) (string, error) {
	f, err := privateFile(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func export(ctx context.Context, configPath, vectorPath, out string) (err error) {
	c, configSHA, err := readConfig(configPath)
	if err != nil {
		return err
	}
	plainWant, err := hashValue(c.VectorJSONLSHA256)
	if err != nil {
		return err
	}
	if _, err := hashValue(c.VectorSHA256); err != nil {
		return err
	}
	in, err := privateFile(vectorPath)
	if err != nil {
		return err
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil || info.Size() <= 0 || info.Size() > 2<<30 {
		return errInput
	}
	compressedHash, plainHash := sha256.New(), sha256.New()
	gz, err := gzip.NewReader(io.TeeReader(in, compressedHash))
	if err != nil {
		return errInput
	}
	defer gz.Close()
	v := &vectorReader{reader: bufio.NewReaderSize(io.TeeReader(io.LimitReader(gz, maxDecoded+1), plainHash), 4097)}
	if err := os.Mkdir(out, 0700); err != nil {
		return errInput
	}
	// Only this invocation owns the fresh directory. Discard all staged outputs
	// on any hash/EOF/callback/cancellation error, including a late source error.
	defer func() {
		if err != nil {
			os.RemoveAll(out)
		}
	}()
	stage, final := filepath.Join(out, "OI-views.stage.jsonl.gz"), filepath.Join(out, "OI-views.jsonl.gz")
	f, err := os.OpenFile(stage, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	closed := false
	defer func() {
		if !closed {
			f.Close()
		}
	}()
	viewsHash, viewsPlain := sha256.New(), sha256.New()
	output := gzip.NewWriter(io.MultiWriter(f, viewsHash))
	encoder := json.NewEncoder(io.MultiWriter(output, viewsPlain))
	counts := make(map[string]uint64)
	summary, err := causaloi.Reduce(ctx, causaloi.Source{UTCStart: c.UTCStart, NativeVersion: c.NativeVersion,
		ExpectedRecords: c.ExpectedRecords, SourceSHA256: plainWant, Bindings: c.Bindings, Next: v.next}, c.Cuts,
		func(s causaloi.Snapshot) error { return encoder.Encode(compact(s, counts)) })
	if err != nil {
		output.Close()
		return err
	}
	if hex.EncodeToString(compressedHash.Sum(nil)) != c.VectorSHA256 || !bytes.Equal(plainHash.Sum(nil), plainWant[:]) {
		output.Close()
		return errInput
	}
	if err := errors.Join(output.Close(), f.Sync(), f.Close()); err != nil {
		closed = true
		return err
	}
	closed = true
	vectorAfter, err := fileHash(vectorPath)
	if err != nil || vectorAfter != c.VectorSHA256 {
		return errInput
	}
	configAfter, err := fileHash(configPath)
	if err != nil || configAfter != configSHA || ctx.Err() != nil {
		return errInput
	}
	if err := os.Rename(stage, final); err != nil {
		return err
	}
	r := report{Schema: "AutoWheelNativeOIExportV1", ObservedAt: time.Now().UTC().Format(time.RFC3339Nano),
		Summary: summary, StatusCounts: counts, ConfigSHA256: configSHA, VectorSHA256: c.VectorSHA256,
		VectorJSONLSHA256: c.VectorJSONLSHA256, ViewsSHA256: hex.EncodeToString(viewsHash.Sum(nil)),
		ViewsJSONLSHA256: hex.EncodeToString(viewsPlain.Sum(nil))}
	if err := writeReport(filepath.Join(out, "report.json"), r); err != nil {
		return err
	}
	dir, err := os.Open(out)
	if err != nil {
		return err
	}
	err = errors.Join(dir.Sync(), dir.Close(), ctx.Err())
	return err
}
