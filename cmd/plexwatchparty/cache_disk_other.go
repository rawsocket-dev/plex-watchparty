//go:build !unix

package main

// diskUsage stub for non-Unix platforms (Windows). Returns zeros so
// the admin panel knows to render "—".
func diskUsage(path string) (free, total int64) { return 0, 0 }
