package databento

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBatchFilePlanMetadataOnly(t *testing.T) {
	for _, state := range []string{"queued", "processing", "done"} {
		t.Run(state, func(t *testing.T) {
			client, query, ack, body, ledger, calls := retrievalFixture(t, state, "")
			before, _ := ledger.Find(ack.OperationID)
			out := filepath.Join(t.TempDir(), "private")
			plan, err := client.PlanBatchFiles(t.Context(), query, ack, body, out)
			if err != nil || !plan.MetadataOnly || plan.SourceQualified || plan.Observation.AllVerified || plan.Ready != (state == "done") {
				t.Fatalf("metadata plan state=%s err=%v", state, err)
			}
			wantCalls, wantFiles := 1, 3
			if state == "done" {
				wantCalls, wantFiles = 2, 4
				if plan.TotalBytes != 20 || len(plan.Files) != 2 || len(plan.FileListSHA256) != 64 {
					t.Fatal("compressed file metadata absent")
				}
			} else if plan.TotalBytes != 0 || len(plan.Files) != 0 || plan.FileListSHA256 != "" {
				t.Fatal("pending job acquired a file list")
			}
			if *calls != wantCalls {
				t.Fatal("unexpected request or data download")
			}
			entries, err := os.ReadDir(out)
			if err != nil || len(entries) != wantFiles {
				t.Fatal("unexpected metadata/data roster")
			}
			for _, entry := range entries {
				info, err := entry.Info()
				if err != nil || info.Mode().Perm() != 0600 || strings.HasSuffix(entry.Name(), ".dbn.zst") {
					t.Fatal("private metadata boundary failed")
				}
			}
			after, _ := ledger.Find(ack.OperationID)
			if before != after {
				t.Fatal("purchase reservation changed")
			}
			receipt, err := os.ReadFile(filepath.Join(out, "file-plan-receipt.json"))
			if err != nil || strings.Contains(string(receipt), "api.databento.com") {
				t.Fatal("receipt absent or contains download URL")
			}
			if _, err := client.PlanBatchFiles(t.Context(), query, ack, body, out); err == nil || *calls != wantCalls {
				t.Fatal("overwrite allowed or rerun made requests")
			}
		})
	}
}

func TestBatchFilePlanRejectsUnsafeMetadata(t *testing.T) {
	for _, failure := range []string{"ack", "job", "host"} {
		t.Run(failure, func(t *testing.T) {
			client, query, ack, body, ledger, calls := retrievalFixture(t, "done", failure)
			before, _ := ledger.Find(ack.OperationID)
			if failure == "ack" {
				ack.ResponseHash = strings.Repeat("0", 64)
			}
			out := filepath.Join(t.TempDir(), "private")
			if _, err := client.PlanBatchFiles(t.Context(), query, ack, body, out); err == nil {
				t.Fatal("unsafe metadata accepted")
			}
			if _, err := os.Stat(filepath.Join(out, "file-plan-receipt.json")); !os.IsNotExist(err) {
				t.Fatal("failed plan published a complete receipt")
			}
			if failure == "ack" && *calls != 0 || *calls > 2 {
				t.Fatal("invalid metadata triggered a transfer")
			}
			after, _ := ledger.Find(ack.OperationID)
			if before != after {
				t.Fatal("failed metadata observation changed reservation")
			}
		})
	}
}
