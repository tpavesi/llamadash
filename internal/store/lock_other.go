//go:build !linux

package store

// Lock is only implemented on Linux, the dashboard's target; elsewhere it always
// succeeds and holds nothing.
func Lock(string) (*FileLock, error) { return &FileLock{}, nil }
