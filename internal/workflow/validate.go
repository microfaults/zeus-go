package workflow

import (
	"encoding/json"
	"fmt"
)

const (
	maxNodes    = 100
	maxVariants = 16
)

// ValidateWorkflow checks required fields on a workflow.
func ValidateWorkflow(w *Workflow) error {
	if w.Name == "" {
		return fmt.Errorf("workflow: name is required")
	}
	if w.Version != "2" {
		return fmt.Errorf("workflow: version must be \"2\", got %q", w.Version)
	}
	if len(w.Root) == 0 {
		return fmt.Errorf("workflow: root must be non-empty")
	}
	if len(w.Targets) == 0 {
		return fmt.Errorf("workflow: at least one target is required")
	}
	if w.EstimatedRPSPerVU <= 0 {
		return fmt.Errorf("workflow: estimated_rps_per_vu must be > 0 (drives constant-arrival-rate executor)")
	}
	return nil
}

// node is a minimal representation used to count DSL nodes in Root.
type node struct {
	Type     string          `json:"type"`
	Children json.RawMessage `json:"children"`
	Child    json.RawMessage `json:"child"`
	Variants json.RawMessage `json:"variants"`
}

// ValidateSchema performs lightweight structural validation of the workflow root.
// It caps total nodes at maxNodes and variants per request node at maxVariants.
func ValidateSchema(w *Workflow) error {
	count, err := countNodes(w.Root)
	if err != nil {
		return fmt.Errorf("workflow: invalid root structure: %w", err)
	}
	if count > maxNodes {
		return fmt.Errorf("workflow: root contains %d nodes, max is %d", count, maxNodes)
	}
	return nil
}

// countNodes recursively counts the number of nodes in a DSL tree.
func countNodes(raw json.RawMessage) (int, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return 0, nil
	}

	var n node
	if err := json.Unmarshal(raw, &n); err != nil {
		return 0, err
	}

	total := 1

	// Count variants if present.
	if len(n.Variants) > 0 && string(n.Variants) != "null" {
		var variants []json.RawMessage
		if err := json.Unmarshal(n.Variants, &variants); err != nil {
			return 0, fmt.Errorf("invalid variants: %w", err)
		}
		if len(variants) > maxVariants {
			return 0, fmt.Errorf("node has %d variants, max is %d", len(variants), maxVariants)
		}
		for _, v := range variants {
			c, err := countNodes(v)
			if err != nil {
				return 0, err
			}
			total += c
		}
	}

	// Count children array.
	if len(n.Children) > 0 && string(n.Children) != "null" {
		var children []json.RawMessage
		if err := json.Unmarshal(n.Children, &children); err != nil {
			return 0, fmt.Errorf("invalid children: %w", err)
		}
		for _, ch := range children {
			c, err := countNodes(ch)
			if err != nil {
				return 0, err
			}
			total += c
		}
	}

	// Count single child.
	if len(n.Child) > 0 && string(n.Child) != "null" {
		c, err := countNodes(n.Child)
		if err != nil {
			return 0, err
		}
		total += c
	}

	return total, nil
}
