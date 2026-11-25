//go:build !linux

package host

// diskUsage is only implemented on Linux; elsewhere HasDisk stays false.
func diskUsage(string) (total, free uint64, ok bool) { return 0, 0, false }
