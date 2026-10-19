package researchcampaign

import "strings"

// Preserved from the bound campaign checkpoint source; only calendar validation
// needs this helper in the reduced-data dependency subset.
func checkpointHash(value string) bool {
	return len(value) == 64 && strings.Trim(value, "0123456789abcdef") == ""
}
