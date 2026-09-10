//go:build unix

package main

import (
	"os"
	"syscall"
)

// ownerUID reports the uid owning path. It is separated by build tag
// because the owner lives in the platform's stat struct rather than in
// fs.FileInfo, and only mkdirError reads it — one message, on the platform
// where the mount whose ownership is the question actually exists.
func ownerUID(path string) (int, bool) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return int(stat.Uid), true
}
