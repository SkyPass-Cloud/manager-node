//go:build !linux

package system

// readDisk is a no-op stub on non-Linux platforms so the package builds for
// local development on Windows/macOS. The real agent always runs on Linux.
func readDisk(path string) (total, free uint64) {
	return 0, 0
}
