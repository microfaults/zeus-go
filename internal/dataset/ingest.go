package dataset

import (
	"encoding/json"
	"fmt"
	"io"
)

// IngestResult summarises the outcome of an NDJSON ingest operation.
type IngestResult struct {
	PoolCounts map[string]int `json:"pool_counts"`
	TotalRows  int            `json:"total_rows"`
	Errors     []string       `json:"errors"`
}

// ndjsonLine is the expected shape of each line in the NDJSON stream.
type ndjsonLine struct {
	Pool string           `json:"pool"`
	Rows []map[string]any `json:"rows"`
}

// IngestNDJSON reads newline-delimited JSON from reader and appends each batch
// of rows to the corresponding pool in the dataset identified by datasetID.
// Each line must be: {"pool":"<name>","rows":[{...},...]}
// Decode errors are recorded but do not abort the ingest (lenient mode).
func IngestNDJSON(store *Store, datasetID string, reader io.Reader) (*IngestResult, error) {
	if _, ok := store.Get(datasetID); !ok {
		return nil, fmt.Errorf("dataset: id %q not found", datasetID)
	}

	result := &IngestResult{
		PoolCounts: make(map[string]int),
	}

	dec := json.NewDecoder(reader)
	lineNum := 0
	for dec.More() {
		lineNum++
		var line ndjsonLine
		if err := dec.Decode(&line); err != nil {
			result.Errors = append(result.Errors, fmt.Sprintf("line %d: %v", lineNum, err))
			continue
		}
		if line.Pool == "" {
			result.Errors = append(result.Errors, fmt.Sprintf("line %d: missing pool name", lineNum))
			continue
		}
		if len(line.Rows) == 0 {
			continue
		}
		if err := store.AddRows(datasetID, line.Pool, line.Rows); err != nil {
			result.Errors = append(result.Errors, fmt.Sprintf("line %d: %v", lineNum, err))
			continue
		}
		result.PoolCounts[line.Pool] += len(line.Rows)
		result.TotalRows += len(line.Rows)
	}

	return result, nil
}
