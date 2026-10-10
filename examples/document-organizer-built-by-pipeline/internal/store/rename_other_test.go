//go:build !windows

package store

import (
	"errors"
	"testing"
)

func TestIsSharingViolationFalse(t *testing.T) {
	if platformIsSharingViolation(errors.New("anything")) {
		t.Error("expected false on non-Windows platforms")
	}
}
