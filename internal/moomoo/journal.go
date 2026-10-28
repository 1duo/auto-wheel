package moomoo

import (
	"encoding/json"
	"errors"

	"autowheel/internal/wheel"
)

func cloneState(state wheel.State) (wheel.State, error) {
	raw, err := json.Marshal(state)
	if err != nil {
		return wheel.State{}, err
	}
	var copy wheel.State
	if err := exactJSON(raw, &copy); err != nil {
		return wheel.State{}, err
	}
	return copy, copy.Validate()
}

func (s *State) JournalState() (wheel.State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.poisoned {
		return wheel.State{}, errors.New("Moomoo state lock is closed")
	}
	return cloneState(s.state)
}

// Poisoned reports uncertain journal durability; only a restart clears it.
func (s *State) Poisoned() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.poisoned
}

func (s *State) Save(next wheel.State) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.poisoned || next.Account != s.binding.AccountID || !validAccountID(next.Account) {
		return errors.New("Moomoo journal save requires its exact active account lock")
	}
	copy, err := cloneState(next)
	if err != nil {
		return err
	}
	if err := s.journal.Save(copy); err != nil {
		s.poisoned = true
		return errors.New("Moomoo journal durability uncertain; restart required")
	}
	s.state = copy
	return nil
}

func (s *State) ArchiveFixedBatch(batch wheel.FixedBatch) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.poisoned || batch.PolicySHA == "" || s.state.Fixed == nil || batch.PolicySHA != s.state.Fixed.PolicySHA {
		return "", errors.New("Moomoo fixed archive requires its active policy lock")
	}
	return s.journal.ArchiveFixedBatch(batch)
}

func (s *State) ArchiveFixedPolicyState(state wheel.State) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.poisoned || state.Account != s.binding.AccountID {
		return "", errors.New("Moomoo policy archive requires its exact account lock")
	}
	return s.journal.ArchiveFixedPolicyState(state)
}
