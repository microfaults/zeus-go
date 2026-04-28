package run

import "fmt"

// transitions defines the legal state transitions for a run.
var transitions = map[string][]string{
	StatusStarting:   {StatusValidating, StatusStopped, StatusFailed},
	StatusValidating: {StatusRunning, StatusRejected, StatusStopped, StatusFailed},
	StatusRunning:    {StatusCompleting, StatusStopped, StatusFailed},
	StatusCompleting: {StatusCompleted, StatusFailed},
}

// terminalStates is the set of states from which no transitions are allowed.
var terminalStates = map[string]bool{
	StatusCompleted: true,
	StatusStopped:   true,
	StatusFailed:    true,
	StatusRejected:  true,
}

// Transition validates that moving from one status to another is legal.
func Transition(from, to string) error {
	allowed, ok := transitions[from]
	if !ok {
		if terminalStates[from] {
			return fmt.Errorf("run: cannot transition from terminal state %q", from)
		}
		return fmt.Errorf("run: unknown state %q", from)
	}
	for _, s := range allowed {
		if s == to {
			return nil
		}
	}
	return fmt.Errorf("run: illegal transition %q -> %q", from, to)
}

// IsTerminal returns true if the given status is a terminal state.
func IsTerminal(status string) bool {
	return terminalStates[status]
}
