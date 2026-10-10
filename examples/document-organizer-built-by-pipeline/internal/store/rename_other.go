//go:build !windows

package store

// platformIsSharingViolation is always false: other platforms replace open files freely.
func platformIsSharingViolation(error) bool { return false }
