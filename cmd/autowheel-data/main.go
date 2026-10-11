// autowheel-data is a separate research tool with no broker or trading imports.
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
	queryPath := flag.String("query", "", "exact bounded historical Query JSON")
	output := flag.String("output", "", "new private evidence directory; refuses overwrite")
	download := flag.Bool("download", false, "purchase a cost-qualified sample against the durable research spending ledger")
	list := flag.Bool("list", false, "authenticate and list datasets using free metadata")
	removeLimit := flag.Bool("remove-spending-limit", false, "explicitly remove the cumulative $10 spending cap; writes authorization result")
	flag.Parse()
	if flag.NArg() != 0 || *output == "" {
		return errors.New("use --list or --query FILE, with --output NEW_DIRECTORY; --download and --remove-spending-limit are optional")
	}
	standaloneRemove := *removeLimit && *queryPath == "" && !*list
	if *list && (*queryPath != "" || *download) {
		return errors.New("use --list or --query FILE, with --output NEW_DIRECTORY; --download is optional")
	}
	if !*list && *queryPath == "" && !standaloneRemove {
		return errors.New("use --list or --query FILE, with --output NEW_DIRECTORY; --download is optional")
	}
	if standaloneRemove && *download {
		return errors.New("use --list or --query FILE, with --output NEW_DIRECTORY; --download is optional")
	}
	// One fixed ledger outside checkouts and data directories: rebuilding or
	// moving the research workspace cannot reset the lifetime authorization.
	// New ledgers remain capped; a migrated ledger persists uncapped.
	runtime := filepath.Join(home, ".local/share/autowheel-research")
	if err := os.MkdirAll(runtime, 0700); err != nil {
		return err
	}
	ledger, err := databento.OpenLedger(filepath.Join(runtime, "databento-budget.json"))
	if err != nil {
		return err
	}
	defer ledger.Close()
	if *removeLimit {
		if err := ledger.EnableUncapped(); err != nil {
			return err
		}
	}
	if standaloneRemove {
		if err := os.Mkdir(*output, 0700); err != nil {
			return err
		}
		auth := struct {
			Version           int    `json:"version"`
			LifetimeNano      int64  `json:"lifetime_nano"`
			AuthorizationMode string `json:"authorization_mode,omitempty"`
			TotalReservedNano int64  `json:"total_reserved_nano"`
			Uncapped          bool   `json:"uncapped"`
		}{
			Version:           2,
			LifetimeNano:      0,
			AuthorizationMode: "uncapped",
			TotalReservedNano: ledger.TotalReserved(),
			Uncapped:          ledger.IsUncapped(),
		}
		if err := saveJSON(filepath.Join(*output, "authorization.json"), auth); err != nil {
			return err
		}
		fmt.Println("cap removed; measured/estimated costs remain separate.")
		return nil
	}
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
	var query databento.Query
	if !*list {
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
	}
	if err := os.Mkdir(*output, 0700); err != nil {
		return err
	}
	if *list {
		datasets, err := client.ListDatasets(ctx)
		if err != nil {
			return err
		}
		if err := saveJSON(filepath.Join(*output, "datasets.json"), datasets); err != nil {
			return err
		}
		fmt.Printf("Authenticated; %d datasets available; no billable request.\n", len(datasets))
		return nil
	}
	quote, err := client.GetCost(ctx, query)
	if err != nil {
		return err
	}
	if err := saveJSON(filepath.Join(*output, "quote.json"), struct {
		Query databento.Query `json:"query"`
		Quote databento.Quote `json:"quote"`
	}{query, quote}); err != nil {
		return err
	}
	if !*download {
		fmt.Printf("Quoted USD %s; no billable request.\n", quote.RawCost)
		return nil
	}
	receipt, body, err := client.GetRange(ctx, query, quote, 16<<20)
	if err != nil {
		if receipt.OperationID != "" {
			err = errors.Join(err, saveJSON(filepath.Join(*output, "attempt.json"), receipt))
		}
		return err
	}
	if err := save(filepath.Join(*output, "source.jsonl"), body); err != nil {
		return err
	}
	if err := saveJSON(filepath.Join(*output, "receipt.json"), receipt); err != nil {
		return err
	}
	total := ledger.TotalReserved()
	if ledger.IsUncapped() {
		fmt.Printf("Saved %d bytes; quoted USD %s; lifetime reserved USD %d.%09d (uncapped).\n",
			receipt.Bytes, quote.RawCost, total/1_000_000_000, total%1_000_000_000)
		return nil
	}
	fmt.Printf("Saved %d bytes; quoted USD %s; lifetime reserved USD %d.%09d of 10.\n",
		receipt.Bytes, quote.RawCost, total/1_000_000_000, total%1_000_000_000)
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
