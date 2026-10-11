package databento

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBatchPlannedRetrievePinsBeforeTransfer(t *testing.T) {
	for _, change := range []string{"none", "ordering", "size", "hash"} {
		t.Run(change, func(t *testing.T) {
			client, query, ack, body, ledger, calls := retrievalFixture(t, "done", "")
			ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
			defer cancel()
			before, _ := ledger.Find(ack.OperationID)
			planDir := filepath.Join(t.TempDir(), "plan")
			if _, err := client.PlanBatchFiles(t.Context(), query, ack, body, planDir); err != nil {
				t.Fatal(err)
			}
			planned, err := os.ReadFile(filepath.Join(planDir, "files-source.json"))
			if err != nil {
				t.Fatal(err)
			}
			var files []map[string]any
			if err := json.Unmarshal(planned, &files); err != nil {
				t.Fatal(err)
			}
			switch change {
			case "ordering":
				files[0], files[1] = files[1], files[0]
			case "size":
				files[0]["size"] = float64(100)
			case "hash":
				files[0]["hash"] = "sha256:" + strings.Repeat("1", 64)
			}
			planned, err = json.Marshal(files)
			if err != nil {
				t.Fatal(err)
			}
			out := filepath.Join(t.TempDir(), "download")
			receipt, err := client.RetrievePlannedBatch(ctx, query, ack, body, planned, out)
			if change == "none" || change == "ordering" {
				if err != nil || !receipt.AllVerified || receipt.TotalBytes != 20 || *calls != 6 {
					t.Fatalf("planned transfer err=%v calls=%d", err, *calls)
				}
			} else {
				if err == nil || *calls != 4 {
					t.Fatal("changed plan reached a file transfer")
				}
				if _, err := os.Stat(filepath.Join(out, "test.definition.dbn.zst")); !os.IsNotExist(err) {
					t.Fatal("changed plan created data")
				}
			}
			after, _ := ledger.Find(ack.OperationID)
			if before != after {
				t.Fatal("planned retrieval changed the financial reservation")
			}
			for _, name := range []string{"planned-files-source.json", "files-source.json"} {
				info, err := os.Stat(filepath.Join(out+".file-plan", name))
				if err != nil || info.Mode().Perm() != 0600 {
					t.Fatal("original planned/refreshed response not retained privately")
				}
			}
		})
	}
}

func TestBatchPlannedRetrieveRejectsCapacity(t *testing.T) {
	if err := plannedBatchRoom(t.TempDir(), maxBatchTotalBytes+1); err == nil {
		t.Fatal("oversized capacity accepted")
	}
	if err := plannedBatchRoom(t.TempDir(), 0); err == nil {
		t.Fatal("empty capacity accepted")
	}
}
