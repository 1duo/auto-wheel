package moomoo

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	aruntime "autowheel/internal/runtime"
	"autowheel/internal/wheel"
)

const DeploymentID = "autowheel-moomoo"

func ConfigDigest(c Config) string {
	raw, err := json.Marshal(c)
	if err != nil {
		return ""
	}
	hash := sha256.Sum256(raw)
	return hex.EncodeToString(hash[:])
}

// Config is deliberately a different document from the Robinhood configuration.
// The business account ID is supplied by Moomoo, never a Robinhood account number.
type Config struct {
	Broker          string       `json:"broker"`
	Region          string       `json:"region"`
	Deployment      string       `json:"deployment_id"`
	AccountID       string       `json:"account_id"`
	ExecutionPolicy string       `json:"execution_policy_id,omitempty"`
	RefreshSeconds  int          `json:"refresh_seconds"`
	Strategy        wheel.Config `json:"strategy"`
}

func DefaultConfig() Config {
	strategy := wheel.DefaultConfig()
	strategy.MaxSymbols = 5
	return Config{Broker: "moomoo", Region: "US", Deployment: DeploymentID, ExecutionPolicy: wheel.FixedConservativeASAPPolicy, RefreshSeconds: 20, Strategy: strategy}
}

// WithFiveSlotPolicy records the requested target without inventing a fixed
// financial spec, broker fee evidence or execution readiness. The observer
// cannot bind that policy until its Moomoo financial sources are qualified.
func (c Config) WithFiveSlotPolicy() (Config, error) {
	if err := c.Validate(); err != nil {
		return Config{}, err
	}
	if c.Strategy.FixedPolicy != nil {
		return Config{}, errors.New("cannot replace a bound financial policy through observation configuration")
	}
	c.ExecutionPolicy, c.Strategy.MaxSymbols = wheel.FixedConservativeASAPPolicy, 5
	c.Strategy.StrategyPreset, c.Strategy.SelectionMode = "", ""
	return c, c.Validate()
}

func validAccountID(id string) bool {
	if len(id) < 4 || len(id) > 20 || id[0] == '0' {
		return false
	}
	for _, r := range id {
		if r < '0' || r > '9' {
			return false
		}
	}
	_, err := strconv.ParseUint(id, 10, 64)
	return err == nil
}

func (c Config) Validate() error {
	if c.Broker != "moomoo" || c.Region != "US" || c.Deployment != DeploymentID {
		return errors.New("configuration must bind the independent US Moomoo deployment")
	}
	if c.AccountID != "" && !validAccountID(c.AccountID) || c.Strategy.Account != c.AccountID {
		return errors.New("Moomoo business account and strategy binding differ or are invalid")
	}
	if c.RefreshSeconds < 15 || c.RefreshSeconds > 300 {
		return errors.New("Moomoo refresh interval must be 15–300 seconds")
	}
	if c.ExecutionPolicy != "" && (c.ExecutionPolicy != wheel.FixedConservativeASAPPolicy || c.Strategy.MaxSymbols != 5 ||
		c.Strategy.StrategyPreset != "" || c.Strategy.SelectionMode != "") {
		return errors.New("Moomoo target must be the explicit five-slot conservative ASAP policy without unqualified financial bindings")
	}
	if c.Strategy.FixedPolicy != nil && (c.ExecutionPolicy != wheel.FixedConservativeASAPPolicy || c.Strategy.FixedPolicy.Spec.ID != c.ExecutionPolicy || c.Strategy.OrderCoveragePolicy != wheel.CompleteMoomooOrders || c.Strategy.FixedPolicy.ImportLegacyStateSHA != "") {
		return errors.New("Moomoo execution configuration requires its own complete-coverage fixed policy without imported financial state")
	}
	if c.Strategy.TradePulseStateDir != "" || c.Strategy.RetiredTradePulse != "" || c.Strategy.CoordinatorSocket != "" || c.Strategy.CoordinatorPolicy != "" || c.Strategy.OrderCoveragePolicy == "exclusive_standard" {
		return errors.New("independent Moomoo cannot use Robinhood retirement or shared execution bindings")
	}
	return c.Strategy.Validate()
}

func LoadConfig(path string) (Config, error) {
	var c Config
	f, err := aruntime.PrivateRead(path)
	if err != nil {
		return c, errors.New("private Moomoo configuration unavailable")
	}
	defer f.Close()
	if err := privateJSON(f, 1<<20, &c); err != nil {
		return c, errors.New("invalid Moomoo configuration")
	}
	return c, c.Validate()
}

type stateBinding struct {
	Broker     string `json:"broker"`
	Region     string `json:"region"`
	Deployment string `json:"deployment_id"`
	AccountID  string `json:"account_id"`
}

type State struct {
	mu       sync.Mutex
	journal  *aruntime.Journal
	path     string
	binding  stateBinding
	closed   bool
	state    wheel.State
	clock    func() time.Time
	poisoned bool
	// preparing holds refs with a live, not yet attempted dispatch handle in
	// this process. Only those prepared records can still reach the transport.
	preparing map[string]bool
}

func (s *State) now() time.Time {
	if s.clock != nil {
		return s.clock().UTC()
	}
	return time.Now().UTC()
}

func (s *State) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return os.ErrClosed
	}
	s.closed = true
	return s.journal.Close()
}

// BindAccount seals the authenticated selection before the caller commits its
// configuration. A failed config write cannot leave the state reusable by a
// different account. Repair must preserve this recorded business account ID.
func (s *State) BindAccount(accountID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.poisoned || !validAccountID(accountID) || s.binding.AccountID != "" && s.binding.AccountID != accountID {
		return errors.New("Moomoo account binding requires its live state lock and matching business account")
	}
	bound := s.binding
	bound.AccountID = accountID
	if err := aruntime.AtomicJSON(s.path, bound); err != nil {
		return err
	}
	s.binding = bound
	return nil
}

// OpenState holds the existing journal flock for this process's entire lifetime.
// An unmarked populated directory cannot be claimed by a new broker deployment.
func OpenState(dir string, c Config) (*State, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	j, state, err := aruntime.OpenJournal(dir)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*State, error) {
		j.Close()
		return nil, err
	}
	if state.Account != "" && state.Account != c.AccountID {
		return fail(errors.New("state belongs to another broker account"))
	}
	path := filepath.Join(dir, "moomoo-deployment.json")
	expected := stateBinding{c.Broker, c.Region, c.Deployment, c.AccountID}
	handle := &State{journal: j, path: path, binding: expected, state: state}
	f, err := aruntime.PrivateRead(path)
	if errors.Is(err, os.ErrNotExist) {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return fail(err)
		}
		for _, entry := range entries {
			if entry.Name() != "lock" {
				return fail(errors.New("Moomoo requires a new empty state directory; existing state cannot be appropriated"))
			}
		}
	} else if err != nil {
		return fail(errors.New("private Moomoo deployment binding unavailable"))
	} else {
		var bound stateBinding
		err = privateJSON(f, 4096, &bound)
		f.Close()
		// The only automatic transition is initial binding from an empty account.
		if err != nil || bound.Broker != expected.Broker || bound.Region != expected.Region || bound.Deployment != expected.Deployment || bound.AccountID != "" && bound.AccountID != expected.AccountID {
			return fail(errors.New("Moomoo state deployment/account binding differs"))
		}
		if bound == expected {
			return handle, nil
		}
	}
	if err := aruntime.AtomicJSON(path, expected); err != nil {
		return fail(err)
	}
	return handle, nil
}
