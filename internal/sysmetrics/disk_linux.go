//go:build linux

package sysmetrics

import "syscall"

// Disk measures the file system of the root directory.
func Disk() (total, used, free int64, ok bool) {
	var s syscall.Statfs_t
	if err := syscall.Statfs("/", &s); err != nil {
		return 0, 0, 0, false
	}
	size := int64(s.Bsize)
	return int64(s.Blocks) * size, int64(s.Blocks-s.Bfree) * size, int64(s.Bavail) * size, true
}
