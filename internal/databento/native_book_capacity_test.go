package databento

import (
	"errors"
	"testing"
)

func TestNativeBookCapacityAndRouting(t *testing.T) {
	// The actual first purchased full quote chunk has this provider count.
	// Its 80-byte records alone exceed the legacy 8GiB decoded-file bound.
	const records int64 = 108_838_353
	limit, err := nativeBookExpansionLimit(records)
	if err != nil || limit <= maxBatchFileBytes || limit < records*80 || limit > maxNativeBookFileBytes {
		t.Fatalf("actual full book capacity rejected: limit=%d err=%v", limit, err)
	}
	for _, bad := range []int64{0, -1, maxInt64Headroom} {
		if _, err := nativeBookExpansionLimit(bad); !errors.Is(err, errBatchProof) {
			t.Fatalf("invalid provider count accepted: %d", bad)
		}
	}
	if limit, err := nativeBookExpansionLimit(1_000_000_000); err != nil || limit != maxNativeBookFileBytes {
		t.Fatalf("absolute expansion bound not enforced: %d %v", limit, err)
	}
	for _, q := range []BatchQuery{{Dataset: "OPRA.PILLAR", Schema: "definition"}, {Dataset: "OPRA.PILLAR", Schema: "statistics"}} {
		if batchVerifier(q) != nativeBatchVerifier {
			t.Fatal("legacy proof verifier changed")
		}
	}
	for _, q := range []BatchQuery{{Dataset: "OPRA.PILLAR", Schema: "cmbp-1"}, {Dataset: "EQUS.MINI", Schema: "mbp-1"}} {
		if batchVerifier(q) != nativeBookVerifier || nativeBookVerifier == nativeBatchVerifier {
			t.Fatal("large book verifier was not isolated from legacy proofs")
		}
	}
}
