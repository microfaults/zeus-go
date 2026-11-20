//go:build unix

package run

import (
	"os"
	"syscall"
)

// openSamplePipe creates the named pipe k6's JSON output writes into and
// opens it for zeus. A pipe rather than a regular file keeps disk use at
// zero: k6 emits ~10 samples per HTTP request, which on a file would grow by
// gigabytes per hour.
//
// It returns the read end and a placeholder write end. The placeholder
// keeps the reader from seeing EOF before k6 opens the pipe (or if k6 never
// does); close it once k6 has exited, and the reader drains what k6 wrote
// and then gets EOF. Both opens are non-blocking, so nothing here waits on
// k6.
func openSamplePipe(path string) (r, hold *os.File, err error) {
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		return nil, nil, err
	}
	rfd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, nil, err
	}
	// Blocking reads from here on: the reader goroutine waits for data.
	if err := syscall.SetNonblock(rfd, false); err != nil {
		_ = syscall.Close(rfd)
		return nil, nil, err
	}
	wfd, err := syscall.Open(path, syscall.O_WRONLY|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		_ = syscall.Close(rfd)
		return nil, nil, err
	}
	return os.NewFile(uintptr(rfd), path), os.NewFile(uintptr(wfd), path), nil
}
