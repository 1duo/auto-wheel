package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"autowheel/internal/accountservice"
	"autowheel/internal/coordinator"
	"autowheel/internal/robinhood"
	"autowheel/internal/sharedbroker"
	"autowheel/internal/wheel"
)

var version = "development"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	if len(os.Args) < 2 {
		return errors.New("usage: account-coordinator version|inspect|observe|preflight --config FILE --state DIRECTORY [--tradepulse-state DIRECTORY]")
	}
	op := os.Args[1]
	if op == "version" {
		printVersion(version)
		return nil
	}
	if op != "inspect" && op != "observe" && op != "preflight" {
		return errors.New("unsupported coordinator command; financial execution is not qualified")
	}
	flags := flag.NewFlagSet(op, flag.ContinueOnError)
	configPath := flags.String("config", "", "private account configuration")
	state := flags.String("state", "", "private account state directory")
	tradepulseState := flags.String("tradepulse-state", "", "TradePulse journal directory (preflight only)")
	if err := flags.Parse(os.Args[2:]); err != nil {
		return err
	}
	if flags.NArg() != 0 || !filepath.IsAbs(*configPath) || !filepath.IsAbs(*state) {
		return errors.New("absolute configuration and state paths are required")
	}
	cfg, err := readConfig(*configPath)
	if err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	if op == "preflight" {
		if !filepath.IsAbs(*tradepulseState) {
			return errors.New("absolute TradePulse state path is required")
		}
		return preflight(ctx, cfg, *state, *tradepulseState)
	}
	if *tradepulseState != "" {
		return errors.New("TradePulse state flag is only valid for preflight")
	}
	ledger, err := coordinator.OpenLedger(*state)
	if err != nil {
		return err
	}
	defer ledger.Close()
	if op == "inspect" {
		return inspect(ctx, cfg, *state)
	}
	for _, name := range []string{"tradepulse", "autowheel"} {
		if !canonicalHash(cfg.Clients[name]) {
			return errors.New("both strategy executable identities are required for observation sockets")
		}
	}
	client := robinhood.NewCoordinatorClient(cfg.TokenFile, false)
	if err = client.Connect(ctx); err != nil {
		return err
	}
	facts := &sharedbroker.Backend{Client: client, Account: cfg.Account}
	optionConfig := wheel.DefaultConfig()
	optionConfig.Account = cfg.Account
	broker := &accountservice.Robinhood{Facts: facts, Options: &robinhood.Broker{MCP: client, Config: optionConfig}}
	server := accountservice.Server{Config: accountservice.Config{
		SocketDir: *state, PolicyHash: cfg.PolicyHash, Releases: cfg.Clients,
		PendingQualification: []string{
			"stock entry sizing and fee bounds have not been qualified",
			"option lifecycle and settled cash reconciliation have not been qualified",
			"single writer migration and compatible rollback have not been qualified",
		},
	}, Ledger: ledger, Broker: broker}
	// No live flag, import mutation, policy override or qualification bypass is
	// exposed by this executable. Production cutover is a separate guarded step.
	return server.Run(ctx)
}
