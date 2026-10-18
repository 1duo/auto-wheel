package wheel

import (
	"errors"
	"maps"
	"strings"
	"time"
)

// FixedLegacyImport retains the original own journal. It is never a receipt for
// external shares, a new entry signal, or permission for a legacy order writer.
type FixedLegacyImport struct {
	StateSHA string `json:"state_sha256"`
	Source   State  `json:"source"`
}

// FixedLegacyDigest is an offline, canonical binding. Broker agreement is still
// required by BindNative before this journal can be imported.
func FixedLegacyDigest(s State) (string, error) {
	if s.Fixed != nil || len(s.Intents) != 0 || len(s.Positions) == 0 || s.Account == "" {
		return "", errors.New("legacy transition needs owned inventory and no unresolved submission")
	}
	if err := s.Validate(); err != nil {
		return "", err
	}
	for symbol, p := range s.Positions {
		ref, ok := strings.CutPrefix(p.Provenance, "autowheel put fill ")
		ref, _, _ = strings.Cut(ref, ";")
		if !ok || ref == "" || !fixedSymbol(symbol) || p.PutOpenedAt.IsZero() || !p.MissingSince.IsZero() ||
			p.PutID == "" && p.OwnedShares != 100 || p.CallID != "" && p.OwnedShares != 100 ||
			p.OwnedShares == 100 && (p.InventoryAt.IsZero() || p.InventoryAt.Before(p.PutOpenedAt)) {
			return "", errors.New("legacy inventory lacks settled own-put provenance")
		}
		matches := 0
		for _, receipt := range s.CompletedOrders {
			if receipt.Ref == ref && receipt.Symbol == symbol && receipt.At.Equal(p.PutOpenedAt) &&
				!receipt.Closing && receipt.Filled == 1 && (p.PutID == "" || receipt.OptionID == p.PutID) {
				matches++
			}
		}
		if matches != 1 {
			return "", errors.New("legacy lifecycle lacks one exact completed own opening receipt")
		}
		if p.CallID != "" {
			calls := 0
			for _, receipt := range s.CompletedOrders {
				if receipt.OptionID == p.CallID && receipt.Symbol == symbol && !receipt.Closing && receipt.Filled == 1 {
					calls++
				}
			}
			if calls != 1 {
				return "", errors.New("legacy covered call lacks its completed own opening receipt")
			}
		}
	}
	return fixedDigest(s), nil
}

func (f FixedPolicyState) validateLegacy() error {
	if f.Legacy == nil {
		return nil
	}
	digest, err := FixedLegacyDigest(f.Legacy.Source)
	if err != nil || digest != f.Legacy.StateSHA || f.Legacy.Source.Account != f.Spec.Account {
		return errors.New("legacy import source journal or binding changed")
	}
	for _, p := range f.Legacy.Source.Positions {
		if !p.PutOpenedAt.Before(f.BoundAt) || p.InventoryAt.After(f.BoundAt) {
			return errors.New("legacy inventory postdates its policy handover")
		}
	}
	return nil
}

func (f FixedPolicyState) inherited(symbol string, p Position) bool {
	if f.Legacy == nil {
		return false
	}
	old, found := f.Legacy.Source.Positions[symbol]
	return found && p.PutOpenedAt.Equal(old.PutOpenedAt) &&
		(p.Provenance == old.Provenance || old.PutID != "" &&
			p.Provenance == old.Provenance+"; assignment inferred from settled broker inventory and no equity orders") &&
		(p.PutID == "" || p.PutID == old.PutID && p.PutStrike == old.PutStrike && p.PutExpiry == old.PutExpiry) &&
		(p.OwnedShares == 0 || p.Cost == old.Cost || old.PutID != "" && p.Cost == old.PutStrike)
}

func (c *FixedController) importLegacy(config Config, s Snapshot, now time.Time) error {
	digest, err := FixedLegacyDigest(*c.state)
	if err != nil || digest != config.FixedPolicy.ImportLegacyStateSHA ||
		s.Account != c.spec.Account || !fresh(s.At, now, c.spec.MaxQuoteAge) ||
		c.spec.FeeKnownAt.After(now) || s.AvailableCash < 0 || s.AvailableCash > MaxMoney {
		return errors.New("legacy import needs its exact reviewed journal and fresh broker account")
	}
	for _, o := range s.Orders {
		if !o.Terminal() {
			return errors.New("legacy import waits for working orders to reconcile")
		}
	}
	seenShares := map[string]bool{}
	for _, h := range s.Shares {
		p, own := c.state.Positions[h.Symbol]
		if seenShares[h.Symbol] || h.Quantity != 0 && (!own || p.OwnedShares != 100 ||
			h.Quantity != 100 || h.Cost != p.Cost || h.Available < 0 || h.Available > 100 ||
			p.CallID == "" && h.Available != 100) {
			return errors.New("legacy share ownership, cost or availability is ambiguous")
		}
		seenShares[h.Symbol] = true
	}
	// Reconcile a copy; drift or missing positions cannot rewrite the approved
	// source and then be mistaken for an authorized import.
	source := *c.state
	source.Positions = maps.Clone(source.Positions)
	source.CompletedOrders = maps.Clone(source.CompletedOrders)
	source.Intents = maps.Clone(source.Intents)
	next := source
	next.Positions = maps.Clone(source.Positions)
	if err := Reconcile(&next, s, now); err != nil || fixedDigest(next) != digest {
		return errors.New("legacy inventory differs from the reviewed journal; reconcile before transition")
	}
	f := FixedPolicyState{Version: 1, Spec: c.spec, PolicySHA: c.spec.Digest(), BoundAt: now,
		Legacy: &FixedLegacyImport{StateSHA: digest, Source: source}, Contracts: map[string]Option{}}
	for _, op := range s.Options {
		p, owned := source.Positions[op.Symbol]
		if !owned || op.ChainID == "" || op.Quantity != 1 ||
			op.ID != p.PutID && op.ID != p.CallID || f.Contracts[op.Symbol].ID != "" {
			return errors.New("legacy option metadata or ownership is ambiguous")
		}
		f.Contracts[op.Symbol] = op
	}
	for symbol, p := range source.Positions {
		if p.PutID+p.CallID != "" && f.Contracts[symbol].ID == "" {
			return errors.New("legacy short option is absent from broker inventory")
		}
	}
	return c.save(f)
}
