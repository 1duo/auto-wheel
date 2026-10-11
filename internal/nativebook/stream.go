// Package nativebook decodes acquired, uncompressed DBN book records with
// bounded memory. It does not qualify market data or produce replay frames.
package nativebook

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

type Schema string

const (
	CMBP1            Schema = "cmbp-1"
	MBP1             Schema = "mbp-1"
	MaxMetadataBytes        = 16 << 20
	MinMetadataBytes        = 108 // Eight-byte prelude plus official fixed metadata.
	MaxNativeBytes          = 64 << 30
)

var (
	ErrEvidence = errors.New("nativebook: invalid expected evidence")
	ErrStream   = errors.New("nativebook: invalid native stream")
)

// Expected must come from separately verified acquisition and pinned AS_IS
// metadata evidence. MetadataBytes includes the eight-byte DBN prelude.
// Matching these hashes proves byte identity, not causal usability. The caller
// must verify private file descriptors, compressed source hashes, decompressor
// exit status, and the attestation binding before accepting a result.
type Expected struct {
	Schema         Schema
	Version        uint8
	TsOut          bool
	Records        uint64
	NativeBytes    uint64
	MetadataBytes  uint64
	NativeSHA256   [32]byte
	MetadataSHA256 [32]byte
}

// Record preserves wire widths, signed prices, uint64 timestamp sentinels,
// enums, and file order. Raw includes reserved bytes; only Raw[:Length] is used.
// Symbol identity is deliberately left to the dated, attested metadata mapping.
type Record struct {
	Index        uint64
	Version      uint8
	Length       uint8
	RType        uint8
	PublisherID  uint16
	InstrumentID uint32
	TsEvent      uint64
	Price        int64
	Size         uint32
	Action       byte
	Side         byte
	Flags        uint8
	TsRecv       uint64
	TsInDelta    int32
	BidPx        int64
	AskPx        int64
	BidSz        uint32
	AskSz        uint32
	BidPublisher uint16
	AskPublisher uint16
	BidCount     uint32
	AskCount     uint32
	Depth        uint8
	Sequence     uint32
	TsOut        uint64
	HasTsOut     bool
	Raw          [88]byte
}

type Summary struct {
	Records        uint64
	NativeBytes    uint64
	NativeSHA256   [32]byte
	MetadataSHA256 [32]byte
}

// Stream calls visit in original file order, without retiming or sorting.
// Visits are provisional until Stream returns successfully: downstream writers
// must stage output and discard it on any error, including cancellation or a
// final digest mismatch. No partial Summary is returned on failure.
func Stream(ctx context.Context, source io.Reader, expected Expected, visit func(Record) error) (Summary, error) {
	if ctx == nil || source == nil || visit == nil {
		return Summary{}, ErrEvidence
	}
	length, rtype := uint64(80), uint8(177)
	if expected.Schema == MBP1 {
		rtype = 1
	} else if expected.Schema != CMBP1 {
		return Summary{}, ErrEvidence
	}
	if expected.TsOut {
		length += 8
	}
	if expected.Version < 1 || expected.Version > 3 ||
		expected.MetadataBytes < MinMetadataBytes || expected.MetadataBytes > MaxMetadataBytes ||
		expected.NativeBytes > MaxNativeBytes || expected.NativeBytes < expected.MetadataBytes ||
		expected.Records > (MaxNativeBytes-expected.MetadataBytes)/length ||
		expected.MetadataBytes+expected.Records*length != expected.NativeBytes ||
		expected.NativeSHA256 == ([32]byte{}) || expected.MetadataSHA256 == ([32]byte{}) {
		return Summary{}, ErrEvidence
	}
	if err := ctx.Err(); err != nil {
		return Summary{}, err
	}
	whole := sha256.New()
	input := bufio.NewReaderSize(io.TeeReader(io.LimitReader(source, int64(expected.NativeBytes)+1), whole), 64<<10)
	header := sha256.New()
	metadata := io.TeeReader(input, header)
	var prelude [8]byte
	if _, err := io.ReadFull(metadata, prelude[:]); err != nil {
		return Summary{}, fmt.Errorf("%w: prelude", ErrStream)
	}
	if string(prelude[:3]) != "DBN" || prelude[3] != expected.Version ||
		uint64(binary.LittleEndian.Uint32(prelude[4:]))+8 != expected.MetadataBytes {
		return Summary{}, fmt.Errorf("%w: metadata scope", ErrStream)
	}
	if _, err := io.CopyN(io.Discard, metadata, int64(expected.MetadataBytes)-8); err != nil {
		return Summary{}, fmt.Errorf("%w: metadata length", ErrStream)
	}
	if [32]byte(header.Sum(nil)) != expected.MetadataSHA256 {
		return Summary{}, fmt.Errorf("%w: metadata digest", ErrStream)
	}
	var record Record
	for index := range expected.Records {
		if err := ctx.Err(); err != nil {
			return Summary{}, err
		}
		record = Record{Index: index, Version: expected.Version, Length: uint8(length), HasTsOut: expected.TsOut}
		wire := record.Raw[:length]
		if _, err := io.ReadFull(input, wire); err != nil {
			return Summary{}, fmt.Errorf("%w: record %d length", ErrStream, index)
		}
		if uint64(wire[0])*4 != length || wire[1] != rtype {
			return Summary{}, fmt.Errorf("%w: record %d type or size", ErrStream, index)
		}
		decode(&record, expected.Schema)
		if err := visit(record); err != nil {
			return Summary{}, fmt.Errorf("nativebook: visit %d: %w", index, err)
		}
	}
	// Only literal stream EOF certifies completion; wrapped reader failures
	// must fail closed even when they contain EOF in their error chain.
	if _, err := input.ReadByte(); err != io.EOF {
		return Summary{}, fmt.Errorf("%w: trailing bytes or reader failure", ErrStream)
	}
	if err := ctx.Err(); err != nil {
		return Summary{}, err
	}
	digest := [32]byte(whole.Sum(nil))
	if digest != expected.NativeSHA256 {
		return Summary{}, fmt.Errorf("%w: native digest", ErrStream)
	}
	return Summary{Records: expected.Records, NativeBytes: expected.NativeBytes,
		NativeSHA256: digest, MetadataSHA256: expected.MetadataSHA256}, nil
}

func decode(record *Record, schema Schema) {
	w := record.Raw[:]
	le := binary.LittleEndian
	record.RType = w[1]
	record.PublisherID = le.Uint16(w[2:4])
	record.InstrumentID = le.Uint32(w[4:8])
	record.TsEvent = le.Uint64(w[8:16])
	record.Price = int64(le.Uint64(w[16:24]))
	record.Size = le.Uint32(w[24:28])
	record.Action, record.Side, record.Flags = w[28], w[29], w[30]
	record.TsRecv = le.Uint64(w[32:40])
	record.TsInDelta = int32(le.Uint32(w[40:44]))
	record.BidPx = int64(le.Uint64(w[48:56]))
	record.AskPx = int64(le.Uint64(w[56:64]))
	record.BidSz, record.AskSz = le.Uint32(w[64:68]), le.Uint32(w[68:72])
	if schema == CMBP1 {
		record.BidPublisher, record.AskPublisher = le.Uint16(w[72:74]), le.Uint16(w[76:78])
	} else {
		record.Depth, record.Sequence = w[31], le.Uint32(w[44:48])
		record.BidCount, record.AskCount = le.Uint32(w[72:76]), le.Uint32(w[76:80])
	}
	if record.HasTsOut {
		record.TsOut = le.Uint64(w[80:88])
	}
}
