// Package id provides cryptographically random hex identifiers.
package id

import (
	"crypto/rand"
	"encoding/hex"
)

// New returns a random 16-character hex string (8 bytes of entropy).
// Used for workflow, run, dataset, and attack identifiers.
func New() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// NewLong returns a random 32-character hex string (16 bytes of entropy).
// Used for meta-trace-id values.
func NewLong() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
