//go:build unix

package main

import "syscall"

// diskUsage returns (free, total) bytes for the filesystem holding
// path. Returns (0, 0) on any error — the admin panel renders "—"
// in that case rather than misleading numbers.
func diskUsage(path string) (free, total int64) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0
	}
	// Bavail is what an unprivileged user can actually use; Bsize is
	// the fs's block size. Multiplying gives bytes.
	free = int64(st.Bavail) * int64(st.Bsize)
	total = int64(st.Blocks) * int64(st.Bsize)
	return free, total
}
