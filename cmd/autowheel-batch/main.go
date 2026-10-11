// autowheel-batch is a separate research tool for bulk batch quote+submit.
// It never trades, never downloads market data, and never completes ledger
// rows: submit only records a batch acknowledgement (job ID) with the hold
// remaining reserved. Downloading, normalization, and completion are later.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"autowheel/internal/databento"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	keyPath := flag.String("key-file", filepath.Join(home, ".config/autowheel/databento-pilot.key"), "private credential file; never the key itself")
	queryPath := flag.String("query", "", "exact frozen bulk BatchQuery JSON")
	output := flag.String("output", "", "new private evidence directory; refuses overwrite")
	submit := flag.Bool("submit", false, "explicitly purchase: reserve and submit one batch job against the uncapped ledger")
	flag.Parse()
	if flag.NArg() != 0 || *queryPath == "" || *output == "" {
		return errors.New("use --query FILE with --output NEW_DIRECTORY; --submit explicitly purchases")
	}
	runtime := filepath.Join(home, ".local/share/autowheel-research")
	if err := os.MkdirAll(runtime, 0700); err != nil {
		return err
	}
	ledger, err := databento.OpenLedger(filepath.Join(runtime, "databento-budget.json"))
	if err != nil {
		return err
	}
	defer ledger.Close()
	key, err := databento.LoadKey(*keyPath)
	if err != nil {
		return err
	}
	defer databento.ZeroKey(key)
	client, err := databento.NewClient(key, ledger)
	if err != nil {
		return err
	}
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	var query databento.BatchQuery
	f, err := os.Open(*queryPath)
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(io.LimitReader(f, 1<<20))
	decoder.DisallowUnknownFields()
	err = decoder.Decode(&query)
	if err == nil && decoder.Decode(new(any)) != io.EOF {
		err = errors.New("query has trailing content")
	}
	f.Close()
	if err != nil {
		return errors.New("invalid query file")
	}
	if err := query.Validate(); err != nil {
		return err
	}
	if err := os.Mkdir(*output, 0700); err != nil {
		return err
	}
	quote, err := client.GetBatchCost(ctx, query)
	if err != nil {
		return err
	}
	if err := saveJSON(filepath.Join(*output, "quote.json"), struct {
		Query databento.BatchQuery `json:"query"`
		Quote databento.Quote      `json:"quote"`
	}{query, quote}); err != nil {
		return err
	}
	if !*submit {
		fmt.Printf("Quoted USD %s; no billable request.\n", quote.RawCost)
		return nil
	}
	if !ledger.IsUncapped() {
		return errors.New("databento batch requires uncapped ledger")
	}
	ack, raw, err := client.SubmitBatch(ctx, query, quote)
	if err != nil {
		if ack.OperationID != "" {
			err = errors.Join(err, saveJSON(filepath.Join(*output, "attempt.json"), ack))
			if len(raw) > 0 {
				err = errors.Join(err, save(filepath.Join(*output, "job.json"), raw))
			}
		}
		return err
	}
	if err := saveJSON(filepath.Join(*output, "ack.json"), ack); err != nil {
		return err
	}
	if err := save(filepath.Join(*output, "job.json"), raw); err != nil {
		return err
	}
	fmt.Printf("Submitted batch job %s state %s; quoted USD %s; hold remains reserved, not complete.\n", ack.JobID, ack.State, quote.RawCost)
	return nil
}

func saveJSON(path string, value any) error {
	raw, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return save(path, append(raw, '\n'))
}

func save(path string, body []byte) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	if _, err = f.Write(body); err == nil {
		err = f.Sync()
	}
	err = errors.Join(err, f.Close())
	if err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	return errors.Join(dir.Sync(), dir.Close())
}
