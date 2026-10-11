package nativebook

import (
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"testing"
)

// Generated only from synthetic records using official databento-dbn 0.71.0.
//
//go:embed testdata/pinned-fixtures.json
var fixtureData []byte

type fixture struct {
	Schema        Schema   `json:"schema"`
	Version       uint8    `json:"version"`
	TsOut         bool     `json:"ts_out"`
	MetadataBytes uint64   `json:"metadata_bytes"`
	NativeBase64  string   `json:"native_base64"`
	Records       []Record `json:"records"`
}

func fixtures(t *testing.T) []fixture {
	t.Helper()
	var data struct {
		GeneratorVersion string    `json:"generator_version"`
		UpgradePolicy    string    `json:"upgrade_policy"`
		SyntheticOnly    bool      `json:"synthetic_only"`
		Fixtures         []fixture `json:"fixtures"`
	}
	if err := json.Unmarshal(fixtureData, &data); err != nil {
		t.Fatal(err)
	}
	if data.GeneratorVersion != "0.71.0" || data.UpgradePolicy != "AS_IS" || !data.SyntheticOnly || len(data.Fixtures) != 12 {
		t.Fatal("invalid pinned synthetic controls")
	}
	return data.Fixtures
}

func fixtureInput(t *testing.T, f fixture) ([]byte, Expected) {
	t.Helper()
	raw, err := base64.StdEncoding.DecodeString(f.NativeBase64)
	if err != nil {
		t.Fatal(err)
	}
	return raw, Expected{Schema: f.Schema, Version: f.Version, TsOut: f.TsOut,
		Records: uint64(len(f.Records)), NativeBytes: uint64(len(raw)), MetadataBytes: f.MetadataBytes,
		NativeSHA256: sha256.Sum256(raw), MetadataSHA256: sha256.Sum256(raw[:f.MetadataBytes])}
}

func TestStreamPinnedNativeReference(t *testing.T) {
	for _, f := range fixtures(t) {
		t.Run(fmt.Sprintf("%s/v%d/tsout=%t", f.Schema, f.Version, f.TsOut), func(t *testing.T) {
			raw, expected := fixtureInput(t, f)
			seen := 0
			summary, err := Stream(t.Context(), bytes.NewReader(raw), expected, func(record Record) error {
				wireStart := expected.MetadataBytes + uint64(seen)*uint64(record.Length)
				if !bytes.Equal(record.Raw[:record.Length], raw[wireStart:wireStart+uint64(record.Length)]) {
					t.Fatal("raw record changed")
				}
				record.Raw = [88]byte{}
				if record != f.Records[seen] {
					t.Fatal("native fields differ from independent pinned getters")
				}
				seen++
				return nil
			})
			if err != nil || seen != 2 || summary.Records != 2 || summary.NativeBytes != uint64(len(raw)) || summary.NativeSHA256 != expected.NativeSHA256 {
				t.Fatalf("complete native reference failed: %v", err)
			}
		})
	}
}

func TestStreamRejectsUnboundAndIncompleteInput(t *testing.T) {
	raw, expected := fixtureInput(t, fixtures(t)[0])
	noop := func(Record) error { return nil }
	tests := []struct {
		name   string
		change func([]byte, *Expected) []byte
	}{
		{"truncated prelude", func(r []byte, _ *Expected) []byte { return r[:7] }},
		{"truncated metadata", func(r []byte, e *Expected) []byte { return r[:e.MetadataBytes-1] }},
		{"truncated final record", func(r []byte, _ *Expected) []byte { return r[:len(r)-1] }},
		{"trailing native byte", func(r []byte, _ *Expected) []byte { return append(r, 0) }},
		{"changed metadata", func(r []byte, _ *Expected) []byte { r[8] ^= 1; return r }},
		{"changed field", func(r []byte, e *Expected) []byte { r[e.MetadataBytes+8] ^= 1; return r }},
		{"wrong rtype", func(r []byte, e *Expected) []byte {
			r[e.MetadataBytes+1] = 1
			e.NativeSHA256 = sha256.Sum256(r)
			return r
		}},
		{"wrong record length", func(r []byte, e *Expected) []byte {
			r[e.MetadataBytes] = 19
			e.NativeSHA256 = sha256.Sum256(r)
			return r
		}},
		{"wrong count", func(r []byte, e *Expected) []byte { e.Records--; return r }},
		{"overflow count", func(r []byte, e *Expected) []byte { e.Records = ^uint64(0); return r }},
		{"excess metadata bound", func(r []byte, e *Expected) []byte { e.MetadataBytes = MaxMetadataBytes + 1; return r }},
		{"undersized official metadata", func(r []byte, e *Expected) []byte { e.MetadataBytes = 107; return r }},
		{"excess native bound", func(r []byte, e *Expected) []byte { e.NativeBytes = MaxNativeBytes + 1; return r }},
		{"wrong version", func(r []byte, e *Expected) []byte { e.Version = 2; return r }},
		{"unsupported version", func(r []byte, e *Expected) []byte { e.Version = 4; return r }},
		{"wrong tsout", func(r []byte, e *Expected) []byte { e.TsOut = true; return r }},
		{"unknown schema", func(r []byte, e *Expected) []byte { e.Schema = "other"; return r }},
		{"missing digest", func(r []byte, e *Expected) []byte { e.NativeSHA256 = [32]byte{}; return r }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			candidate, evidence := bytes.Clone(raw), expected
			candidate = tc.change(candidate, &evidence)
			if summary, err := Stream(t.Context(), bytes.NewReader(candidate), evidence, noop); err == nil || summary != (Summary{}) {
				t.Fatal("invalid input returned successful or partial summary")
			}
		})
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := Stream(ctx, bytes.NewReader(raw), expected, noop); !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled operation accepted")
	}
	stop := errors.New("stop")
	if _, err := Stream(t.Context(), bytes.NewReader(raw), expected, func(Record) error { return stop }); !errors.Is(err, stop) {
		t.Fatal("visitor failure lost")
	}
	if _, err := Stream(t.Context(), &tailFailure{Reader: bytes.NewReader(raw)}, expected, noop); !errors.Is(err, ErrStream) {
		t.Fatal("terminal reader failure accepted")
	}
	headerOnly := raw[:expected.MetadataBytes]
	empty := expected
	empty.Records = 0
	empty.NativeBytes = expected.MetadataBytes
	empty.NativeSHA256 = sha256.Sum256(headerOnly)
	if summary, err := Stream(t.Context(), bytes.NewReader(headerOnly), empty, func(Record) error {
		return errors.New("unexpected visit for empty native file")
	}); err != nil || summary.Records != 0 || summary.NativeBytes != empty.NativeBytes {
		t.Fatal("complete zero-record native file rejected")
	}
}

type tailFailure struct{ *bytes.Reader }

func (r *tailFailure) Read(p []byte) (int, error) {
	if r.Reader.Len() == 0 {
		return 0, io.ErrUnexpectedEOF
	}
	return r.Reader.Read(p)
}
