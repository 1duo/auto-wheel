package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"

	"autowheel/internal/causaloi"
)

func TestNativeFieldNullAndWidths(t *testing.T) {
	row := []any{0, 1, "TSLA  260422P00255000", 30, 1, uint64(100), 99,
		uint64(math.MaxUint64), int64(math.MinInt64), 0, uint32(math.MaxUint32),
		int32(math.MinInt32), 9, math.MaxUint16, 1, 0, 24}
	encode := func(values []any) []byte {
		t.Helper()
		b, err := json.Marshal(values)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	r, err := record(encode(row))
	if err != nil || r.Quantity != 0 || r.ReferenceAt != math.MaxUint64 || r.Price != math.MinInt64 {
		t.Fatal("native extremes and real zero were changed", err)
	}
	for i := range 17 {
		copy := append([]any(nil), row...)
		copy[i] = nil
		if _, err := record(encode(copy)); err == nil {
			t.Fatalf("field %d null accepted as zero", i)
		}
	}
	for _, bad := range []any{int64(math.MaxInt32) + 1, -int64(math.MaxInt32) - 2, 0.5, "0", json.Number("0e0"), true} {
		copy := append([]any(nil), row...)
		copy[9] = bad
		if _, err := record(encode(copy)); err == nil {
			t.Fatal("invalid native int32 accepted")
		}
	}
}

func fixture(t *testing.T) (string, string, string) {
	t.Helper()
	dir := t.TempDir()
	start := uint64(20_000) * causaloi.DayNanos
	row := []any{0, 1, "TSLA  260422P00255000", 30, 1, start + 100, start + 99,
		uint64(math.MaxUint64), 0, 0, 1, 0, 9, 0, 1, 0, 24}
	plain, err := json.Marshal(row)
	if err != nil {
		t.Fatal(err)
	}
	plain = append(plain, '\n')
	var buffer bytes.Buffer
	gz := gzip.NewWriter(&buffer)
	if _, err := gz.Write(plain); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	compressed, decoded := sha256.Sum256(buffer.Bytes()), sha256.Sum256(plain)
	c := config{Schema: "AutoWheelNativeOIScopeV1", Dataset: "OPRA.PILLAR", UTCStart: start,
		NativeVersion: 1, ExpectedRecords: 1, VectorSHA256: hex.EncodeToString(compressed[:]),
		VectorJSONLSHA256: hex.EncodeToString(decoded[:]), Cuts: []uint64{start + 99, start + 100},
		Bindings: []causaloi.Binding{{RawSymbol: row[2].(string), InstrumentID: 1,
			Definition: &causaloi.Definition{InstrumentID: 1, ReceiveAt: start + 1, EventAt: start + 1}}}}
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	configPath, vectorPath, out := filepath.Join(dir, "config.json"), filepath.Join(dir, "vectors.gz"), filepath.Join(dir, "out")
	if err := os.WriteFile(configPath, b, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(vectorPath, buffer.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	return configPath, vectorPath, out
}

func TestExportPublishesOnlyAfterHashAndCompleteEOF(t *testing.T) {
	for _, kind := range []string{"valid", "truncated_gzip", "trailing_garbage", "wrong_plain_hash", "wrong_count", "cancelled"} {
		t.Run(kind, func(t *testing.T) {
			configPath, vectorPath, out := fixture(t)
			c, _, err := readConfig(configPath)
			if err != nil {
				t.Fatal(err)
			}
			b, err := os.ReadFile(vectorPath)
			if err != nil {
				t.Fatal(err)
			}
			ctx := t.Context()
			switch kind {
			case "truncated_gzip":
				b = b[:len(b)-1]
			case "trailing_garbage":
				b = append(b, []byte("garbage")...)
			case "wrong_plain_hash":
				c.VectorJSONLSHA256 = c.VectorSHA256
			case "wrong_count":
				c.ExpectedRecords++
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			// Corrupt gzip cases retain the matching compressed hash, so only the
			// actual gzip EOF/CRC parser can reject them.
			h := sha256.Sum256(b)
			c.VectorSHA256 = hex.EncodeToString(h[:])
			configBytes, err := json.Marshal(c)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(configPath, configBytes, 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(vectorPath, b, 0600); err != nil {
				t.Fatal(err)
			}
			err = export(ctx, configPath, vectorPath, out)
			if kind != "valid" {
				if err == nil {
					t.Fatal("invalid source published")
				}
				if _, err := os.Stat(out); !os.IsNotExist(err) {
					t.Fatal("failed export left publishable output")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			rawReport, err := os.ReadFile(filepath.Join(out, "report.json"))
			if err != nil {
				t.Fatal(err)
			}
			var result report
			if json.Unmarshal(rawReport, &result) != nil || result.Summary.Records != 1 || result.Summary.Observations != 2 ||
				result.StatusCounts["not_observed"] != 1 || result.StatusCounts["observed_reference_missing"] != 1 ||
				result.SourceGatePassed || result.HistoricalPerformance || result.FinancialRequests != 0 {
				t.Fatal("wrong provisional result")
			}
			// A second invocation must preserve the existing private output.
			if export(ctx, configPath, vectorPath, out) == nil {
				t.Fatal("existing output overwritten")
			}
			after, err := os.ReadFile(filepath.Join(out, "report.json"))
			if err != nil || !bytes.Equal(after, rawReport) {
				t.Fatal("existing output changed")
			}
		})
	}
}
