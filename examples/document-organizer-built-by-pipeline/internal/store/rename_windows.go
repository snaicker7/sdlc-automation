//go:build windows

package store

import (
	"errors"
	"syscall"
)

// platformIsSharingViolation reports ERROR_ACCESS_DENIED (5) and ERROR_SHARING_VIOLATION (32),
// which Windows returns while another handle holds the file open.
func platformIsSharingViolation(err error) bool {
	var errno syscall.Errno
	return errors.As(err, &errno) && (errno == 5 || errno == 32)
}
