//go:build windows

package store

import (
	"os"
	"syscall"
	"testing"
)

func TestIsSharingViolation(t *testing.T) {
	for _, tc := range []struct {
		errno syscall.Errno
		want  bool
	}{{5, true}, {32, true}, {2, false}, {0, false}} {
		if got := platformIsSharingViolation(tc.errno); got != tc.want {
			t.Errorf("errno %d = %v, want %v", tc.errno, got, tc.want)
		}
	}
	wrapped := &os.LinkError{Op: "rename", Old: "a", New: "b", Err: syscall.Errno(32)}
	if !platformIsSharingViolation(wrapped) {
		t.Error("wrapped LinkError not detected")
	}
	if platformIsSharingViolation(os.ErrNotExist) {
		t.Error("ErrNotExist misclassified")
	}
}
