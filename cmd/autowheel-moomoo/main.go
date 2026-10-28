// autowheel-moomoo is the isolated US Moomoo deployment. Native execution
// requires its bound financial policy, trading consent and native activation.
package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"
	_ "time/tzdata"

	"autowheel/internal/moomoo"
	aruntime "autowheel/internal/runtime"
	"autowheel/internal/wheel"
)

var version = "dev"

type readiness struct {
	At     time.Time `json:"at"`
	Ready  bool      `json:"ready"`
	Reason string    `json:"reason"`
}

type status struct {
	BrokerID           string                    `json:"broker_id"`
	Deployment         string                    `json:"deployment_id"`
	Version            string                    `json:"version"`
	Mode               string                    `json:"mode"`
	Stage              string                    `json:"stage"`
	Reason             string                    `json:"reason,omitempty"`
	At                 time.Time                 `json:"at"`
	ServerTime         time.Time                 `json:"server_time"`
	Halted             bool                      `json:"halted"`
	LiveCapable        bool                      `json:"live_capable"`
	ExecutionInstalled bool                      `json:"execution_installed"`
	TradingWindowOpen  bool                      `json:"trading_window_open"`
	Readiness          readiness                 `json:"live_readiness"`
	AccountConfigured  bool                      `json:"account_configured"`
	TokenPresent       bool                      `json:"token_present"`
	Config             wheel.Config              `json:"config"`
	ExecutionPolicy    string                    `json:"execution_policy_id,omitempty"`
	AccountInfo        *moomoo.AccountInfo       `json:"account_info,omitempty"`
	Broker             *wheel.Snapshot           `json:"broker,omitempty"`
	Positions          map[string]wheel.Position `json:"positions,omitempty"`
	Intents            map[string]wheel.Intent   `json:"intents,omitempty"`
}

const executionBlocker = "Moomoo financial-source mapping is unqualified: settled collateral, obligations, account permissions, executable ticks, prospective fees and assignment/settlement reconciliation require broker evidence"

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))
	if err := run(os.Args[1:]); err != nil {
		slog.Error("Moomoo stopped", "reason", err.Error())
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: autowheel-moomoo version|init|init-state|check-config|configure-five-slots|configure-execution|login|login-trading|doctor|audit-orders|audit-transactions|audit-market|audit-chain|audit-execution|bind-account|run")
	}
	command := args[0]
	if command == "version" && len(args) == 1 {
		fmt.Println("AutoWheel Moomoo", version)
		return nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	f := flag.NewFlagSet(command, flag.ContinueOnError)
	configPath := f.String("config", filepath.Join(home, ".config", moomoo.DeploymentID, "config.json"), "private Moomoo configuration")
	stateDir := f.String("state", filepath.Join(home, ".local", "state", moomoo.DeploymentID), "private independent Moomoo state")
	listen := f.String("listen", "127.0.0.1:9842", "loopback status address")
	callbackPort := f.Int("callback-port", 8766, "Moomoo OAuth loopback callback port")
	accountID := f.String("account-id", "", "explicit Moomoo business account ID for bind-account")
	ordersSince := f.String("orders-since", "", "explicit RFC3339 start for a read-only order audit")
	transactionsSince := f.String("transactions-since", "", "explicit RFC3339 update-time start for a read-only fill/cash audit")
	quoteCodes := f.String("quote-codes", "", "comma-separated explicit US instrument codes for a read-only market audit")
	chainSymbol := f.String("chain-symbol", "", "explicit US equity ticker for the conservative option-chain input")
	var roster rosterFlags
	f.StringVar(&roster.symbols, "roster-symbols", "", "comma-separated reviewed conservative roster for configure-execution")
	f.StringVar(&roster.source, "roster-source-sha256", "", "SHA256 of the reviewed roster source")
	f.StringVar(&roster.knownAt, "roster-known-at", "", "RFC3339 time the roster was known")
	f.StringVar(&roster.from, "roster-from", "", "first session date the roster applies to")
	f.StringVar(&roster.through, "roster-through", "", "last session date the roster applies to")
	if err := f.Parse(args[1:]); err != nil {
		return err
	}
	if f.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	if command == "init" {
		if _, err := os.Lstat(*configPath); !errors.Is(err, os.ErrNotExist) {
			return errors.New("init requires a new Moomoo configuration path")
		}
		return aruntime.AtomicJSON(*configPath, moomoo.DefaultConfig())
	}
	c, err := moomoo.LoadConfig(*configPath)
	if err != nil {
		return err
	}
	if command == "check-config" {
		reason := executionBlocker
		if c.Strategy.FixedPolicy != nil {
			reason = "Financial policy bound; trading consent and native activation are required"
		}
		return json.NewEncoder(os.Stdout).Encode(map[string]any{"broker_id": "moomoo", "region": "US", "deployment_id": c.Deployment,
			"account_configured": c.AccountID != "", "execution_qualified": c.Strategy.FixedPolicy != nil, "mode": "paused", "reason": reason,
			"execution_policy_id": c.ExecutionPolicy, "max_managed_symbols": c.Strategy.MaxSymbols})
	}
	if !slices.Contains([]string{"init-state", "configure-five-slots", "configure-execution", "login", "login-trading", "doctor", "audit-orders", "audit-transactions", "audit-market", "audit-chain", "audit-execution", "bind-account", "run"}, command) {
		return errors.New("unknown Moomoo command; live trading is unavailable until qualified")
	}
	j, err := moomoo.OpenState(*stateDir, c)
	if err != nil {
		return err
	}
	defer j.Close()
	if command == "init-state" {
		return nil
	}
	if command == "configure-five-slots" {
		if err := configureFiveSlots(*configPath, *stateDir, c); err != nil {
			return err
		}
		fmt.Println("Five-slot conservative ASAP target configured; account and cash limits preserved. Trading remains paused and unqualified.")
		return nil
	}
	tokenPath := filepath.Join(*stateDir, "moomoo.token.json")
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if command == "login" || command == "login-trading" {
		if *callbackPort < 1024 || *callbackPort > 65535 {
			return errors.New("callback port must be 1024–65535")
		}
		ctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
		defer cancel()
		if command == "login-trading" {
			if c.Strategy.FixedPolicy == nil {
				return errors.New("qualify the Moomoo financial policy before requesting trading consent")
			}
			if c.AccountID == "" {
				return errors.New("trading consent requires a sealed US business account")
			}
			if _, err := moomoo.NewClient(tokenPath).Observe(ctx, c.AccountID); err != nil {
				return err
			}
			return moomoo.LoginTrading(ctx, filepath.Join(*stateDir, "moomoo-trading.token.json"), *callbackPort, c.AccountID,
				func(u string) { fmt.Println("Open the separate account-bound Moomoo trading consent page:\n" + u) })
		}
		return moomoo.Login(ctx, tokenPath, *callbackPort, func(u string) { fmt.Println("Open the Moomoo read-only authorization page:\n" + u) })
	}
	client := moomoo.NewClient(tokenPath)
	if command == "configure-execution" {
		ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
		defer cancel()
		if err := configureExecution(ctx, *configPath, *stateDir, c, client, roster); err != nil {
			return err
		}
		fmt.Println("Five-slot conservative ASAP policy bound to the Moomoo account with complete order coverage. Trading remains paused; trading consent and native activation are still required.")
		return nil
	}
	if command == "audit-execution" {
		ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
		defer cancel()
		return auditExecution(ctx, *stateDir, c, client, j, *chainSymbol)
	}
	if command == "audit-chain" {
		if *chainSymbol == "" || c.AccountID == "" {
			return errors.New("option-chain audit requires a sealed account and explicit --chain-symbol")
		}
		ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
		defer cancel()
		audit, err := client.AuditOptionChain(ctx, c.AccountID, *chainSymbol, time.Now().In(wheel.Eastern).Format(time.DateOnly))
		if err != nil {
			return err
		}
		if err := aruntime.AtomicJSON(filepath.Join(*stateDir, "moomoo-option-chain-audit.json"), audit); err != nil {
			return err
		}
		fmt.Printf("Read-only option-chain input saved privately; complete pagination: %t. Execution remains unqualified.\n", audit.Complete)
		return nil
	}
	if command == "audit-market" {
		if *quoteCodes == "" || c.AccountID == "" {
			return errors.New("market audit requires a sealed account and explicit --quote-codes")
		}
		ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
		defer cancel()
		audit, err := client.AuditMarket(ctx, c.AccountID, strings.Split(*quoteCodes, ","))
		if err != nil {
			return err
		}
		if err := aruntime.AtomicJSON(filepath.Join(*stateDir, "moomoo-market-audit.json"), audit); err != nil {
			return err
		}
		fmt.Println("Read-only instrument and market facts saved privately; tick, clock and financial-source qualification remain required.")
		return nil
	}
	if command == "audit-transactions" {
		since, err := time.Parse(time.RFC3339, *transactionsSince)
		if err != nil || c.AccountID == "" {
			return errors.New("transaction audit requires a bound account and explicit --transactions-since RFC3339 timestamp")
		}
		ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		defer cancel()
		audit, err := client.AuditTransactions(ctx, c.AccountID, since)
		if err != nil {
			return err
		}
		if err := aruntime.AtomicJSON(filepath.Join(*stateDir, "moomoo-transactions-audit.json"), audit); err != nil {
			return err
		}
		fmt.Println("Read-only fill/cash audit saved privately; settlement and execution remain unqualified.")
		return nil
	}
	if command == "audit-orders" {
		since, err := time.Parse(time.RFC3339, *ordersSince)
		if err != nil || c.AccountID == "" {
			return errors.New("order audit requires a bound account and explicit --orders-since RFC3339 timestamp")
		}
		ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		defer cancel()
		audit, err := client.AuditOrders(ctx, c.AccountID, since)
		if err != nil {
			return err
		}
		if err := aruntime.AtomicJSON(filepath.Join(*stateDir, "moomoo-orders-audit.json"), audit); err != nil {
			return err
		}
		fmt.Println("Read-only US order audit saved privately; this evidence does not qualify execution.")
		return nil
	}
	if command == "doctor" || command == "bind-account" {
		ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		defer cancel()
		accounts, err := client.Accounts(ctx)
		if err != nil {
			return err
		}
		if err := aruntime.AtomicJSON(filepath.Join(*stateDir, "moomoo-accounts.json"), accounts); err != nil {
			return err
		}
		if command == "bind-account" {
			if c.AccountID != "" && c.AccountID != *accountID {
				return errors.New("existing Moomoo account binding cannot be replaced; use a new independent deployment")
			}
			candidate := c
			candidate.AccountID, candidate.Strategy.Account = *accountID, *accountID
			if err := candidate.Validate(); err != nil || *accountID == "" {
				return errors.New("explicit valid Moomoo business account ID required")
			}
			if _, err := client.Observe(ctx, *accountID); err != nil {
				return err
			}
			if err := j.BindAccount(*accountID); err != nil {
				return err
			}
			if err := aruntime.AtomicJSON(*configPath, candidate); err != nil {
				return err
			}
			fmt.Println("Moomoo account bound for observation; execution remains unqualified and paused.")
			return nil
		}
		if err := client.Connect(ctx); err != nil {
			return err
		}
		if err := aruntime.AtomicJSON(filepath.Join(*stateDir, "moomoo-mcp-tools.json"), client.Inventory()); err != nil {
			return err
		}
		if c.AccountID != "" {
			evidence, err := client.AuditAccount(ctx, c.AccountID)
			if err != nil {
				return err
			}
			if err := aruntime.AtomicJSON(filepath.Join(*stateDir, "moomoo-account-evidence.json"), evidence); err != nil {
				return err
			}
			if err := aruntime.AtomicJSON(filepath.Join(*stateDir, "moomoo-observation.json"), evidence.Observation()); err != nil {
				return err
			}
		}
		fmt.Println("Read-only Moomoo discovery saved privately. This does not qualify execution or activate trading.")
		return nil
	}
	return serve(ctx, c, client, tokenPath, *stateDir, *listen, *configPath, j)
}

// The caller owns the account state lock. This migration is only for the
// observation deployment; a trading journal needs a qualified policy transition.
func configureFiveSlots(configPath, dir string, c moomoo.Config) error {
	next, err := c.WithFiveSlotPolicy()
	if err != nil {
		return err
	}
	for _, name := range []string{"LIVE.json", "journal.json"} {
		if _, err := os.Lstat(filepath.Join(dir, name)); !errors.Is(err, os.ErrNotExist) {
			return errors.New("five-slot observation configuration requires no activation or trading journal")
		}
	}
	if err := aruntime.AtomicJSON(filepath.Join(dir, "HALT"), map[string]string{"reason": "five-slot policy configuration requires qualified native activation"}); err != nil {
		return err
	}
	return aruntime.AtomicJSON(configPath, next)
}

func ensureOperatorToken(dir string) (string, error) {
	token, err := readOperatorToken(dir)
	if err == nil {
		return token, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", errors.New("private Moomoo operator credential unavailable or invalid")
	}
	var secret [32]byte
	if _, err := rand.Read(secret[:]); err != nil {
		return "", err
	}
	token = hex.EncodeToString(secret[:])
	return token, aruntime.AtomicJSON(filepath.Join(dir, "operator-token.json"), token)
}

func readOperatorToken(dir string) (string, error) {
	path := filepath.Join(dir, "operator-token.json")
	f, err := aruntime.PrivateRead(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, 4097))
	var token string
	if err != nil || len(raw) > 4096 || json.Unmarshal(raw, &token) != nil || len(token) != 64 {
		return "", errors.New("invalid Moomoo operator credential")
	}
	decoded, err := hex.DecodeString(token)
	if err != nil || len(decoded) != 32 || strings.ToLower(token) != token {
		return "", errors.New("invalid Moomoo operator credential")
	}
	return token, nil
}

func serve(ctx context.Context, c moomoo.Config, client *moomoo.Client, tokenPath, dir, address, configPath string, store *moomoo.State) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil || host != "127.0.0.1" && host != "::1" {
		return errors.New("Moomoo status listener must use a numeric loopback address")
	}
	if _, err := ensureOperatorToken(dir); err != nil {
		return err
	}
	// The production source exists only for a bound financial policy. Without
	// it, native execution cannot turn observations into cash or lots.
	var source moomoo.ExecutionSource
	if c.Strategy.FixedPolicy != nil {
		qualified, err := moomoo.NewSource(client, c)
		if err != nil {
			return err
		}
		source = qualified
	}
	execution, err := nativeExecution(c, store, dir, configPath, source)
	if err != nil {
		return err
	}
	// Retain the observer migration marker. Native execution above always
	// revokes live permission on restart, including an uncertain prior write.
	if _, err := os.Lstat(filepath.Join(dir, "observer-initialized.json")); errors.Is(err, os.ErrNotExist) {
		if err := aruntime.AtomicJSON(filepath.Join(dir, "HALT"), map[string]string{"reason": "deployment default"}); err != nil {
			return err
		}
		if err := aruntime.AtomicJSON(filepath.Join(dir, "observer-initialized.json"), true); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	var mu sync.Mutex
	current := status{BrokerID: "moomoo", Deployment: moomoo.DeploymentID, Version: version, Mode: "read_only", Stage: "waiting_configuration",
		At: time.Now().UTC(), Config: c.Strategy.PublicCopy(), ExecutionPolicy: c.ExecutionPolicy, ExecutionInstalled: true, AccountConfigured: c.AccountID != ""}
	wake := make(chan struct{}, 1)
	read := func() status {
		mu.Lock()
		s := current
		mu.Unlock()
		s.ServerTime = time.Now().UTC()
		s.TradingWindowOpen = wheel.TradingWindow(s.ServerTime)
		s.LiveCapable = execution.Broker.Source != nil && c.Strategy.FixedPolicy != nil
		check := execution.Readiness()
		s.Readiness = readiness{At: check.At, Ready: check.Ready, Reason: check.Reason}
		view := execution.View()
		s.Broker, s.Positions, s.Intents = view.Broker, view.Positions, view.Intents
		if execution.Broker.Activation.Enabled() {
			s.Mode = "live"
			s.Stage, s.Reason = "active", "Moomoo native trading enabled"
		}
		if !check.Ready && c.Strategy.FixedPolicy != nil {
			s.Stage, s.Reason = "blocked", check.Reason
		}
		_, err := os.Lstat(filepath.Join(dir, "HALT"))
		s.Halted = !errors.Is(err, os.ErrNotExist)
		f, err := aruntime.PrivateRead(tokenPath)
		s.TokenPresent = err == nil
		if err == nil {
			f.Close()
		}
		return s
	}
	observe := func() error {
		at := time.Now().UTC()
		stage, reason := "waiting_configuration", "Authorize Moomoo and bind its explicit US business account for read-only holdings"
		var info *moomoo.AccountInfo
		cycleCtx, stopCycle := context.WithTimeout(ctx, 25*time.Second)
		cycleErr := execution.Cycle(cycleCtx)
		stopCycle()
		if cycleFatal(cycleErr, store, execution.Broker.Activation) {
			return errors.Join(cycleErr, errors.New("Moomoo journal or activation durability is uncertain; restarting paused"))
		}
		if cycleErr != nil && !errors.Is(cycleErr, moomoo.ErrAwaitingBrokerReconciliation) {
			slog.Warn("Moomoo execution cycle blocked", "reason", cycleErr.Error())
		}
		if c.AccountID != "" {
			queryCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
			var err error
			info, err = client.Observe(queryCtx, c.AccountID)
			cancel()
			stage, reason = "planned", "Read-only Moomoo holdings refreshed; execution remains unqualified"
			if c.Strategy.FixedPolicy != nil {
				reason = "Moomoo holdings refreshed; execution requires fresh readiness and native activation"
			}
			if err != nil {
				stage, reason = "blocked", err.Error()
			}
		}
		mu.Lock()
		current.At, current.Stage, current.Reason = at, stage, reason
		// A failed fetch keeps the last observed holdings with their old timestamp.
		// Missing data is never published as an authoritative empty account.
		if info != nil {
			current.AccountInfo = info
		}
		mu.Unlock()
		slog.Info("Moomoo observation", "stage", stage)
		return nil
	}
	mux := operatorAPI(read, dir, wake, execution)
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return err
	}
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 25 * time.Second, IdleTimeout: 30 * time.Second}
	serverDone := make(chan error, 1)
	go func() { serverDone <- server.Serve(listener) }()
	defer server.Close()
	if err := observe(); err != nil {
		return err
	}
	timer := time.NewTicker(time.Duration(c.RefreshSeconds) * time.Second)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-serverDone:
			return err
		case <-timer.C:
			if err := observe(); err != nil {
				return err
			}
		case <-wake:
			if err := observe(); err != nil {
				return err
			}
		}
	}
}

// cycleFatal matches the Robinhood daemon. A failed cycle blocks only itself:
// readiness is published false, so no order passes the final guard until a
// later cycle re-reads and reconciles every fact, while unknown outcomes stay
// durably fenced. Only uncertain journal or activation durability stops the
// process, which restarts paused.
func cycleFatal(err error, store *moomoo.State, activation *moomoo.Activation) bool {
	return errors.Is(err, wheel.ErrFixedJournalDurability) || store.Poisoned() || activation != nil && activation.Poisoned()
}

func operatorAPI(read func() status, dir string, wake chan<- struct{}, executions ...*moomoo.Execution) http.Handler {
	var execution *moomoo.Execution
	if len(executions) == 1 {
		execution = executions[0]
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintln(w, `{"alive":true,"broker_id":"moomoo"}`)
	})
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		json.NewEncoder(w).Encode(read())
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		s := read()
		if !s.LiveCapable || !s.Readiness.Ready {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		json.NewEncoder(w).Encode(s)
	})
	var controlMu sync.Mutex
	mux.HandleFunc("POST /control/{action}", func(w http.ResponseWriter, r *http.Request) {
		controlMu.Lock()
		defer controlMu.Unlock()
		token, err := readOperatorToken(dir)
		if err != nil || r.Header.Get("Origin") != "" || r.ContentLength != 0 || len(r.TransferEncoding) != 0 || r.Header.Get("X-AutoWheel-Control") != "confirm" || subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+token)) != 1 {
			http.Error(w, "authenticated native Moomoo control required", http.StatusForbidden)
			return
		}
		switch r.PathValue("action") {
		case "halt":
			if execution != nil {
				err = execution.Broker.Activation.Pause("operator pause")
			} else {
				err = aruntime.AtomicJSON(filepath.Join(dir, "HALT"), map[string]string{"reason": "operator pause"})
			}
		case "resume":
			// Observation resume must also revoke a retained record behind a
			// manually created HALT; removing HALT alone could restore trading.
			if execution != nil {
				err = execution.Broker.Activation.Pause("observation resumed without trading permission")
				if err != nil {
					break
				}
			}
			err = os.Remove(filepath.Join(dir, "HALT"))
			if errors.Is(err, os.ErrNotExist) {
				err = nil
			}
			if err == nil {
				var f *os.File
				f, err = os.Open(dir)
				if err == nil {
					err = errors.Join(f.Sync(), f.Close())
				}
			}
		case "refresh":
		case "live":
			activationCtx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
			defer cancel()
			if err := enableExecution(activationCtx, execution); err != nil {
				http.Error(w, "Moomoo native readiness did not pass", http.StatusConflict)
				return
			}
		default:
			http.NotFound(w, r)
			return
		}
		if err != nil {
			http.Error(w, "Moomoo control persistence failed", http.StatusInternalServerError)
			return
		}
		select {
		case wake <- struct{}{}:
		default:
		}
		w.WriteHeader(http.StatusAccepted)
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		if host != "127.0.0.1" && host != "::1" {
			http.Error(w, "numeric loopback Host required", http.StatusForbidden)
			return
		}
		mux.ServeHTTP(w, r)
	})
}
