// autowheel-batch-files plans compressed downloads for an existing prepaid job.
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
		return nil, errors.New("batch file plan input open failed")
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, (1<<20)+1))
	if err != nil || len(b) == 0 || len(b) > 1<<20 {
		return nil, errors.New("batch file plan input read failed")
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
	output := flag.String("output", "", "new private metadata directory")
	timeout := flag.Duration("timeout", time.Minute, "metadata observation deadline")
	flag.Parse()
	if flag.NArg() != 0 || *queryPath == "" || *ackPath == "" || *bodyPath == "" || *output == "" || *timeout <= 0 || *timeout > time.Hour {
		return errors.New("use --query FILE --ack FILE --ack-body FILE --output NEW_DIRECTORY")
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
	plan, err := client.PlanBatchFiles(ctx, query, ack, body, *output)
	if err != nil {
		return err
	}
	fmt.Printf("Metadata only: ready %t; planned files %d, bytes %d; no data downloaded.\n", plan.Ready, len(plan.Files), plan.TotalBytes)
	return nil
}
