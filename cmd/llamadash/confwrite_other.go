//go:build !unix

package main

import (
	"io/fs"
	"os"
)

// keepOwner is a no-op where files have no Unix owner.
func keepOwner(*os.File, fs.FileInfo) error { return nil }

// syncDir is a no-op where directories cannot be synced.
func syncDir(string) {}
