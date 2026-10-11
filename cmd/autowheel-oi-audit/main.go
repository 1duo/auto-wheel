// autowheel-oi-audit exports a bounded, private causal OI view for independent
// offline comparison. It has no network, credential, broker or financial imports.
// A parsed view does not qualify source coverage or constitute replay input.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"autowheel/internal/quotehistory"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	source := flag.String("source", "", "private statistics JSONL")
	asOfText := flag.String("as-of", "", "causal cutoff, RFC3339Nano")
	output := flag.String("output", "", "new private OI view; refuses overwrite")
	flag.Parse()
	if flag.NArg() != 0 || *source == "" || *output == "" {
		return errors.New("source, as-of and output are required")
	}
	at, err := time.Parse(time.RFC3339Nano, *asOfText)
	if err != nil || at.IsZero() {
		return errors.New("invalid OI cutoff")
	}
	f, err := os.Open(*source)
	if err != nil {
		return err
	}
	defer f.Close()
	hash := sha256.New()
	events, err := quotehistory.ReadStatistics(io.TeeReader(f, hash))
	if err != nil {
		return err
	}
	view, err := quotehistory.SnapshotOpenInterest(events, at)
	if err != nil {
		return err
	}
	result := struct {
		Schema       string                               `json:"schema_version"`
		Status       string                               `json:"status"`
		SourceSHA256 string                               `json:"source_sha256"`
		AsOf         time.Time                            `json:"as_of"`
		Records      int                                  `json:"source_records"`
		Snapshot     map[string]quotehistory.OpenInterest `json:"snapshot"`
	}{"AutoWheelCausalOIViewV1", "partial_unqualified", hex.EncodeToString(hash.Sum(nil)), at, len(events), view}
	encoded, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return err
	}
	dest, err := os.OpenFile(*output, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	_, writeErr := dest.Write(append(encoded, '\n'))
	if err := errors.Join(writeErr, dest.Sync(), dest.Close()); err != nil {
		return err
	}
	fmt.Printf("Parsed %d statistics rows; %d causal OI symbols; unqualified for replay.\n", len(events), len(view))
	return nil
}
