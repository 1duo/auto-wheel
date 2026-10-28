package main

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"autowheel/internal/moomoo"
	aruntime "autowheel/internal/runtime"
	"autowheel/internal/wheel"
)

func TestConfigureFiveSlotsPreservesAccountAndCredentials(t *testing.T) {
	dir := t.TempDir()
	configPath, stateDir := filepath.Join(dir, "config.json"), filepath.Join(dir, "state")
	c := moomoo.DefaultConfig()
	c.AccountID, c.Strategy.Account = "123456", "123456"
	c.ExecutionPolicy, c.Strategy.MaxSymbols = "", 3
	c.Strategy, _ = c.Strategy.WithStrategyPreset("conservative-roi-v1")
	c.Strategy.Reserve = 12345
	if err := aruntime.AtomicJSON(configPath, c); err != nil {
		t.Fatal(err)
	}
	j, err := moomoo.OpenState(stateDir, c)
	if err != nil {
		t.Fatal(err)
	}
	args := []string{"configure-five-slots", "--config", configPath, "--state", stateDir}
	if err := run(args); err == nil {
		t.Fatal("configuration changed while daemon held the state lock")
	}
	j.Close()
	tokenPath := filepath.Join(stateDir, "moomoo.token.json")
	credential := []byte("private credential unchanged; configuration must not read it")
	if err := os.WriteFile(tokenPath, credential, 0600); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := run(args); err != nil {
			t.Fatal(err)
		}
	}
	next, err := moomoo.LoadConfig(configPath)
	if err != nil || next.AccountID != c.AccountID || next.Strategy.Account != c.AccountID || next.Strategy.MaxSymbols != 5 ||
		next.ExecutionPolicy != wheel.FixedConservativeASAPPolicy || next.Strategy.MaxCollateral != c.Strategy.MaxCollateral || next.Strategy.Reserve != c.Strategy.Reserve {
		t.Fatalf("unsafe five-slot configuration: %v", err)
	}
	got, err := os.ReadFile(tokenPath)
	if err != nil || !bytes.Equal(got, credential) {
		t.Fatal("configuration touched the OAuth credential")
	}
	if _, err := os.Stat(filepath.Join(stateDir, "HALT")); err != nil {
		t.Fatal("configuration change did not retain durable pause")
	}
	if _, err := os.Stat(filepath.Join(stateDir, "LIVE.json")); !os.IsNotExist(err) {
		t.Fatal("configuration change created activation")
	}
}

func TestConfigureFiveSlotsRejectsTradingState(t *testing.T) {
	for _, marker := range []string{"LIVE.json", "journal.json"} {
		t.Run(marker, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "config.json")
			c := moomoo.DefaultConfig()
			if err := aruntime.AtomicJSON(path, c); err != nil {
				t.Fatal(err)
			}
			if err := aruntime.AtomicJSON(filepath.Join(dir, marker), wheel.NewState()); err != nil {
				t.Fatal(err)
			}
			before, _ := os.ReadFile(path)
			if err := configureFiveSlots(path, dir, c); err == nil {
				t.Fatal("observation migration crossed a trading-state boundary")
			}
			after, _ := os.ReadFile(path)
			if !bytes.Equal(before, after) {
				t.Fatal("rejected migration altered configuration")
			}
		})
	}
}

func TestMoomooControlIsolation(t *testing.T) {
	dir := t.TempDir()
	token, err := ensureOperatorToken(dir)
	if err != nil {
		t.Fatal(err)
	}
	handler := operatorAPI(func() status { return status{BrokerID: "moomoo", Mode: "read_only"} }, dir, make(chan struct{}, 1))
	request := func(host, action, auth string) int {
		r := httptest.NewRequest(http.MethodPost, "http://"+host+"/control/"+action, nil)
		r.Header.Set("X-AutoWheel-Control", "confirm")
		r.Header.Set("Authorization", "Bearer "+auth)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w.Code
	}
	if got := request("127.0.0.1:9842", "halt", "another-account-token"); got != http.StatusForbidden {
		t.Fatalf("other account control: %d", got)
	}
	if got := request("untrusted.example", "halt", token); got != http.StatusForbidden {
		t.Fatalf("DNS rebinding control: %d", got)
	}
	if got := request("127.0.0.1:9842", "live", token); got != http.StatusConflict {
		t.Fatalf("unqualified live activation: %d", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "LIVE.json")); !os.IsNotExist(err) {
		t.Fatal("activation created by observer")
	}
	if got := request("127.0.0.1:9842", "halt", token); got != http.StatusAccepted {
		t.Fatalf("authenticated pause: %d", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "HALT")); err != nil {
		t.Fatal("pause not durable")
	}
	if err := os.Remove(filepath.Join(dir, "operator-token.json")); err != nil {
		t.Fatal(err)
	}
	if got := request("127.0.0.1:9842", "halt", token); got != http.StatusForbidden {
		t.Fatalf("removed credential authorized control: %d", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "operator-token.json")); !os.IsNotExist(err) {
		t.Fatal("unauthenticated request recreated the operator credential")
	}
}

func TestTransactionAuditRequiresExplicitRangeBeforeCredentials(t *testing.T) {
	dir := t.TempDir()
	config := moomoo.DefaultConfig()
	config.AccountID, config.Strategy.Account = "123456", "123456"
	path, state := filepath.Join(dir, "config.json"), filepath.Join(dir, "state")
	if err := aruntime.AtomicJSON(path, config); err != nil {
		t.Fatal(err)
	}
	args := []string{"audit-transactions", "--config", path, "--state", state}
	if err := run(args); err == nil || !strings.Contains(err.Error(), "explicit --transactions-since") {
		t.Fatalf("missing range reached credentials: %v", err)
	}
	if err := run(append(args, "--transactions-since", time.Now().Add(time.Hour).Format(time.RFC3339))); err == nil || !strings.Contains(err.Error(), "explicit past history start") {
		t.Fatalf("future range reached credentials: %v", err)
	}
	for _, name := range []string{"moomoo.token.json", "moomoo-transactions-audit.json", "LIVE.json"} {
		if _, err := os.Stat(filepath.Join(state, name)); !os.IsNotExist(err) {
			t.Fatalf("failed audit created %s", name)
		}
	}
}

func TestNativeObservationResumeRevokesRetainedActivation(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	c := moomoo.DefaultConfig()
	c.AccountID, c.Strategy.Account = "123456", "123456"
	store, err := moomoo.OpenState(dir, c)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	execution, err := nativeExecution(c, store, dir, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := aruntime.AtomicJSON(filepath.Join(dir, "LIVE.json"), map[string]string{"retained": "behind manual HALT"}); err != nil {
		t.Fatal(err)
	}
	token, err := ensureOperatorToken(dir)
	if err != nil {
		t.Fatal(err)
	}
	handler := operatorAPI(func() status { return status{BrokerID: "moomoo", Mode: "read_only"} }, dir, make(chan struct{}, 1), execution)
	r := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:9842/control/resume", nil)
	r.Header.Set("X-AutoWheel-Control", "confirm")
	r.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != http.StatusAccepted {
		t.Fatalf("observation resume: %d", w.Code)
	}
	if _, err := os.Lstat(filepath.Join(dir, "LIVE.json")); !os.IsNotExist(err) || execution.Broker.Activation.Enabled() {
		t.Fatal("observation resume retained live permission")
	}
}

func TestMarketAuditRequiresExplicitCodesBeforeCredentials(t *testing.T) {
	dir := t.TempDir()
	c := moomoo.DefaultConfig()
	c.AccountID, c.Strategy.Account = "123456", "123456"
	path, state := filepath.Join(dir, "config.json"), filepath.Join(dir, "state")
	if err := aruntime.AtomicJSON(path, c); err != nil {
		t.Fatal(err)
	}
	args := []string{"audit-market", "--config", path, "--state", state}
	if err := run(args); err == nil || !strings.Contains(err.Error(), "explicit --quote-codes") {
		t.Fatal("missing market codes reached credentials")
	}
	if err := run(append(args, "--quote-codes", "US.AAPL,US.AAPL")); err == nil || !strings.Contains(err.Error(), "distinct explicit US quote codes") {
		t.Fatal("duplicate market codes reached credentials")
	}
	for _, name := range []string{"moomoo.token.json", "moomoo-market-audit.json", "LIVE.json"} {
		if _, err := os.Lstat(filepath.Join(state, name)); !os.IsNotExist(err) {
			t.Fatalf("invalid market audit created %s", name)
		}
	}
}

func TestChainAuditRequiresExplicitSymbolBeforeCredentials(t *testing.T) {
	dir := t.TempDir()
	c := moomoo.DefaultConfig()
	c.AccountID, c.Strategy.Account = "123456", "123456"
	path, state := filepath.Join(dir, "config.json"), filepath.Join(dir, "state")
	if err := aruntime.AtomicJSON(path, c); err != nil {
		t.Fatal(err)
	}
	args := []string{"audit-chain", "--config", path, "--state", state}
	if err := run(args); err == nil || !strings.Contains(err.Error(), "explicit --chain-symbol") {
		t.Fatal("missing chain symbol reached credentials")
	}
	if err := run(append(args, "--chain-symbol", "US.AAPL")); err == nil || !strings.Contains(err.Error(), "explicit equity symbol") {
		t.Fatal("invalid chain symbol reached credentials")
	}
	if _, err := os.Lstat(filepath.Join(state, "moomoo-option-chain-audit.json")); !os.IsNotExist(err) {
		t.Fatal("rejected chain request created a completion packet")
	}
}

// Like Robinhood, an ordinary failed cycle blocks only itself; uncertain
// journal durability stops the daemon so it restarts paused.
func TestFailedCycleBlocksOnlyItselfUnlessJournalDurabilityIsUncertain(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	c := moomoo.DefaultConfig()
	c.AccountID, c.Strategy.Account = "123456", "123456"
	store, err := moomoo.OpenState(dir, c)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if cycleFatal(nil, store, nil) || cycleFatal(errors.New("Moomoo observation HTTP 403; no automatic replay"), store, nil) ||
		cycleFatal(moomoo.ErrAwaitingBrokerReconciliation, store, nil) || cycleFatal(moomoo.ErrPlacementOutcomeUnknown, store, nil) {
		t.Fatal("an ordinary failed cycle stopped the daemon")
	}
	if !cycleFatal(errors.Join(wheel.ErrFixedJournalDurability, errors.New("disk")), store, nil) {
		t.Fatal("uncertain fixed journal durability did not stop the daemon")
	}
	if err := os.Chmod(dir, 0500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(dir, 0700)
	st := wheel.NewState()
	st.Account = c.AccountID
	if store.Save(st) == nil || !cycleFatal(errors.New("later read"), store, nil) {
		t.Fatal("a poisoned journal did not stop the daemon")
	}
}
