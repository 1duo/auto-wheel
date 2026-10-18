package main

import (
	"encoding/json"
	"io"

	"autowheel/internal/wheel"
)

type configCheckReport struct {
	Version            string       `json:"version"`
	Scope              string       `json:"scope"`
	ConfigurationValid bool         `json:"configuration_valid"`
	ConfigDigest       string       `json:"config_digest"`
	AccountConfigured  bool         `json:"account_configured"`
	AccountLast4       string       `json:"account_last4,omitempty"`
	BrokerVerified     bool         `json:"broker_verified"`
	TradingAuthorized  bool         `json:"trading_authorized"`
	SymbolCollateral   wheel.Cents  `json:"max_symbol_collateral_cents"`
	AvailableCashOnly  bool         `json:"use_available_cash"`
	Config             wheel.Config `json:"config"`
}

// Configuration inspection does not open a journal, acquire a peer lock,
// connect to the broker, or authorize the staged handover.
func writeConfigCheck(out io.Writer, c wheel.Config) error {
	if err := c.Validate(); err != nil {
		return err
	}
	r := configCheckReport{
		Version: version, Scope: "configuration_only", ConfigurationValid: true,
		ConfigDigest: configDigest(c), AccountConfigured: c.Account != "", Config: c,
		SymbolCollateral:  c.SymbolCollateralLimit(),
		AvailableCashOnly: c.MaxCollateral == 0,
	}
	if r.AvailableCashOnly {
		r.SymbolCollateral = 0 // No fixed limit; the broker supplies current capacity.
	}
	if c.Account != "" {
		r.AccountLast4 = c.Account[max(0, len(c.Account)-4):]
	}
	r.Config = c.PublicCopy()
	return json.NewEncoder(out).Encode(r)
}
