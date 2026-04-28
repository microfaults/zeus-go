package dataset

import (
	"fmt"

	"atropos-go/loadgen/internal/workflow"
)

// ValidationResult describes whether a dataset satisfies a workflow data schema.
type ValidationResult struct {
	OK       bool     `json:"ok"`
	Missing  []string `json:"missing"`
	Warnings []string `json:"warnings"`
}

// ValidateDataset checks that ds contains the pools and fields required by schema.
func ValidateDataset(ds *Dataset, schema *workflow.DataSchema) *ValidationResult {
	r := &ValidationResult{OK: true}

	for name, ps := range schema.Pools {
		pool, exists := ds.Pools[name]

		// Pool missing entirely.
		if !exists {
			if ps.Optional {
				r.Warnings = append(r.Warnings,
					fmt.Sprintf("optional pool %q not present in dataset", name))
			} else {
				r.OK = false
				r.Missing = append(r.Missing,
					fmt.Sprintf("required pool %q not found", name))
			}
			continue
		}

		// Check required fields by sampling the first row.
		if len(pool.Rows) > 0 {
			first := pool.Rows[0]
			for _, f := range ps.Fields {
				if _, ok := first[f]; !ok {
					r.OK = false
					r.Missing = append(r.Missing,
						fmt.Sprintf("pool %q: missing field %q", name, f))
				}
			}
		}

		// Check minimum size.
		if ps.MinSize > 0 && pool.Stats.RowCount < ps.MinSize {
			r.OK = false
			r.Missing = append(r.Missing,
				fmt.Sprintf("pool %q: has %d rows, need at least %d",
					name, pool.Stats.RowCount, ps.MinSize))
		}
	}

	return r
}
