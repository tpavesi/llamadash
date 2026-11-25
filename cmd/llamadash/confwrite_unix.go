//go:build unix

package main

import (
	"io/fs"
	"os"
	"syscall"
)

// keepOwner gives f the owner and group of the file fi describes, when running as
// root (anyone else can only create files of their own).
func keepOwner(f *os.File, fi fs.FileInfo) error {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || geteuid() != 0 {
		return nil
	}
	return f.Chown(int(st.Uid), int(st.Gid))
}

// syncDir makes a rename in dir durable; a failure only costs durability.
func syncDir(dir string) {
	if d, err := os.Open(dir); err == nil {
		d.Sync()
		d.Close()
	}
}
