// autowheel-batch-finalize verifies previously acquired research data offline.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
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
	var in databento.BatchAcquisitionInputs
	flag.StringVar(&in.QueryPath, "query", "", "original private query")
	flag.StringVar(&in.AcknowledgementPath, "ack", "", "original private acknowledgement")
	flag.StringVar(&in.AckBodyPath, "ack-body", "", "original private submit response")
	flag.StringVar(&in.SourceDirectory, "source", "", "verified private acquisition directory")
	flag.StringVar(&in.ProofPath, "proof", "", "new immutable private proof file")
	flag.StringVar(&in.PythonExecutable, "python", "", "absolute pinned decoder interpreter")
	ledgerPath := flag.String("ledger", "", "private research ledger")
	timeout := flag.Duration("timeout", time.Hour, "offline verification deadline")
	flag.Parse()
	if flag.NArg() != 0 || *ledgerPath == "" || *timeout <= 0 || *timeout > 24*time.Hour {
		return errors.New("provide original batch evidence, decoder, proof and research ledger")
	}
	ledger, err := databento.OpenLedger(*ledgerPath)
	if err != nil {
		return err
	}
	defer ledger.Close()
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	proof, err := ledger.FinalizeBatchAcquisition(ctx, in)
	if err != nil {
		return err
	}
	fmt.Printf("Acquired %s: %d native records; spending hold preserved; source qualification pending.\n", proof.Reservation.OperationID, proof.ProviderRecordCount)
	return nil
}
