package stats

import "time"

// startTime records when the stats package was initialized.
var startTime time.Time

func init() {
	startTime = time.Now()
}

// Summary holds the global system summary.
type Summary struct {
	ActiveRuns    int     `json:"active_runs"`
	ActiveAttacks int     `json:"active_attacks"`
	Datasets      int     `json:"datasets"`
	UptimeSeconds float64 `json:"uptime_seconds"`
}

// ComputeSummary builds a Summary from the provided counts.
// The caller extracts these counts from stores.
func ComputeSummary(activeRuns, activeAttacks, datasets int) Summary {
	return Summary{
		ActiveRuns:    activeRuns,
		ActiveAttacks: activeAttacks,
		Datasets:      datasets,
		UptimeSeconds: time.Since(startTime).Seconds(),
	}
}
