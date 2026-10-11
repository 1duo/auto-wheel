// autowheel-batch-planned-retrieve downloads one existing prepaid job.
// It pins file names, sizes and hashes and keeps six GiB of disk room.
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
	b, err := io.ReadAll(io.LimitReader(f, (4<<20)+1))
	if err != nil || len(b) == 0 || len(b) > 4<<20 {
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
	planPath := flag.String("planned-files", "", "original validated file list from autowheel-batch-files")
	timeout := flag.Duration("timeout", time.Hour, "total observation/acquisition deadline")
	flag.Parse()
	if flag.NArg() != 0 || *queryPath == "" || *ackPath == "" || *bodyPath == "" || *output == "" || *planPath == "" || *timeout <= 0 || *timeout > 24*time.Hour {
		return errors.New("use --query FILE --ack FILE --ack-body FILE --planned-files FILE --output NEW_DIRECTORY")
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
	planned, err := readInput(*planPath)
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
	receipt, err := client.RetrievePlannedBatch(ctx, query, ack, body, planned, *output)
	if err != nil {
		return err
	}
	fmt.Printf("Verified files %d, bytes %d; purchase reservation unchanged.\n", len(receipt.Files), receipt.TotalBytes)
	return nil
}
