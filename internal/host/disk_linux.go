package host

import "syscall"

// diskUsage returns the size and the space available to unprivileged users of
// the filesystem holding path, in bytes.
func diskUsage(path string) (total, free uint64, ok bool) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0, false
	}
	bs := uint64(st.Bsize)
	return st.Blocks * bs, st.Bavail * bs, st.Blocks > 0
}
