//go:build !unix

package run

import (
	"errors"
	"os"
)

// openSamplePipe is unsupported off unix: runs still execute and finalize
// from the summary export, but without live stats.
func openSamplePipe(string) (r, hold *os.File, err error) {
	return nil, nil, errors.New("named pipes are not supported on this platform")
}
