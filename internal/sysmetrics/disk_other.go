//go:build !linux

package sysmetrics

// Disk is unknown on this system.
func Disk() (total, used, free int64, ok bool) { return 0, 0, 0, false }
