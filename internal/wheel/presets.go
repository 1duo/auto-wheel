package wheel

import "errors"

// Presets are deterministic execution policies shared by every configured
// symbol. They do not change ownership, account binding, reserves or limits.
// An empty preset preserves the existing delta-first selection behavior.
func StrategyPresetNames() []string {
	return []string{"conservative-roi-v1", "balanced-roi-v1", "liquid-roi-v1", "cost-protected-roi-v1"}
}

func (c Config) WithStrategyPreset(id string) (Config, error) {
	mode := SelectionConservative
	switch id {
	case "conservative-roi-v1", "liquid-roi-v1", "cost-protected-roi-v1":
	case "balanced-roi-v1":
		mode = SelectionBalanced
	default:
		return Config{}, errors.New("unknown strategy preset")
	}
	c, err := c.WithSelectionMode(mode)
	if err != nil {
		return Config{}, err
	}
	c.StrategyPreset = id
	c.CallMinDelta, c.CallMaxDelta = .50, .85
	c.MaxSpreadBPS, c.AllowBelowCost = 2000, true
	if id == "liquid-roi-v1" {
		c.MaxSpreadBPS = 1000
	}
	if id == "cost-protected-roi-v1" {
		c.AllowBelowCost = false
	}
	return c, nil
}

func (c Config) validStrategyPreset() bool {
	if c.StrategyPreset == "" {
		return true
	}
	want, err := c.WithStrategyPreset(c.StrategyPreset)
	return err == nil && c.SelectionMode == want.SelectionMode &&
		c.PutMinDelta == want.PutMinDelta && c.PutMaxDelta == want.PutMaxDelta &&
		c.CallMinDelta == want.CallMinDelta && c.CallMaxDelta == want.CallMaxDelta &&
		c.MaxSpreadBPS == want.MaxSpreadBPS && c.AllowBelowCost == want.AllowBelowCost
}
