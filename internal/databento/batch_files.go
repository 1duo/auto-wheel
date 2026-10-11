package databento

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"time"
)

// BatchFilePlan contains validated provider metadata, never downloaded data.
// Explicit zero totals and false qualification distinguish an unfinished plan.
type BatchFilePlan struct {
	SchemaVersion   string                `json:"schema_version"`
	Observation     BatchRetrievalReceipt `json:"observation"`
	Ready           bool                  `json:"ready"`
	MetadataOnly    bool                  `json:"metadata_only"`
	Files           []BatchFileResult     `json:"files"`
	TotalBytes      int64                 `json:"total_bytes"`
	FileListSHA256  string                `json:"file_list_sha256"`
	ObservedAt      time.Time             `json:"observed_at"`
	SourceQualified bool                  `json:"source_qualified"`
}

// PlanBatchFiles observes an existing reserved job and its validated file list.
// It cannot submit, transfer data, finalize a reservation or qualify a source.
func (c *Client) PlanBatchFiles(ctx context.Context, q BatchQuery, ack BatchAcknowledgement, ackBody []byte, output string) (BatchFilePlan, error) {
	observation, err := c.RetrieveBatch(ctx, q, ack, ackBody, output, false)
	plan := BatchFilePlan{SchemaVersion: "AutoWheelBatchFilePlanV1", Observation: observation, MetadataOnly: true}
	if err != nil {
		return plan, err
	}
	root, err := os.OpenRoot(output)
	if err != nil {
		return plan, errors.New("databento batch file plan output open failed")
	}
	defer root.Close()
	if observation.State == "done" {
		raw, err := c.batchGET(ctx, BatchListFilesEndpoint, observation.JobID)
		if err != nil {
			return plan, err
		}
		if err := writeBatchPrivate(root, "files-source.json", raw); err != nil {
			return plan, err
		}
		files, err := validateBatchFiles(raw, observation.JobID)
		if err != nil {
			return plan, err
		}
		for _, file := range files {
			plan.Files = append(plan.Files, BatchFileResult{Filename: file.Filename, Bytes: file.Size, SHA256: file.Hash[7:]})
			plan.TotalBytes += file.Size
		}
		plan.FileListSHA256 = digestBytes(raw)
		plan.Ready = true
	}
	plan.ObservedAt = time.Now().UTC()
	body, err := json.MarshalIndent(plan, "", "  ")
	if err != nil {
		return plan, errors.New("databento batch file plan encoding failed")
	}
	if err := writeBatchPrivate(root, "file-plan-receipt.json", append(body, '\n')); err != nil {
		return plan, err
	}
	return plan, nil
}
