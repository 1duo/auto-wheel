// autowheel-batch-retrieve observes or retrieves an existing prepaid job.
// It cannot submit a purchase or alter a reservation or trading configuration.
package main

import (
	"context"
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

func readInput(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, errors.New("batch evidence input open failed")
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, (1<<20)+1))
	if err != nil || len(b) == 0 || len(b) > 1<<20 {
		return nil, errors.New("batch evidence input read failed")
	}
	return b, nil
}

func run() error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	keyPath := flag.String("key-file", filepath.Join(home, ".config/autowheel/databento-pilot.key"), "private credential file")
	queryPath := flag.String("query", "", "original BatchQuery JSON")
	ackPath := flag.String("ack", "", "saved acknowledgement JSON")
	bodyPath := flag.String("ack-body", "", "private original submit response body")
	output := flag.String("output", "", "new private output directory")
	download := flag.Bool("download", false, "retrieve and verify existing prepaid files; never submit")
	timeout := flag.Duration("timeout", time.Hour, "total observation/acquisition deadline")
	flag.Parse()
	if flag.NArg() != 0 || *queryPath == "" || *ackPath == "" || *bodyPath == "" || *output == "" || *timeout <= 0 || *timeout > 24*time.Hour {
		return errors.New("use --query FILE --ack FILE --ack-body FILE --output NEW_DIRECTORY; optional --download")
	}
	queryRaw, err := readInput(*queryPath)
	if err != nil {
		return err
	}
	ackRaw, err := readInput(*ackPath)
	if err != nil {
		return err
	}
	body, err := readInput(*bodyPath)
	if err != nil {
		return err
	}
	query, ack, err := databento.DecodeBatchInputs(queryRaw, ackRaw)
	if err != nil {
		return err
	}
	ledger, err := databento.OpenLedger(filepath.Join(home, ".local/share/autowheel-research/databento-budget.json"))
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
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	receipt, err := client.RetrieveBatch(ctx, query, ack, body, *output, *download)
	if err != nil {
		return err
	}
	fmt.Printf("Batch %s: %s; verified files %d, bytes %d; purchase reservation unchanged.\n", receipt.JobID, receipt.State, len(receipt.Files), receipt.TotalBytes)
	return nil
}
