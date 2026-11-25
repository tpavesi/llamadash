package host

import "testing"

func TestDiskUsageRoot(t *testing.T) {
	total, free, ok := diskUsage("/")
	if !ok || total == 0 || free > total {
		t.Fatalf("total=%d free=%d ok=%v", total, free, ok)
	}
}
