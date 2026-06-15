//go:build linux

package system

import "syscall"

// readDisk returns total and free space in KB for the filesystem at path,
// using statfs on Linux.
func readDisk(path string) (total, free uint64) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return
	}
	bsize := uint64(st.Bsize)
	total = (st.Blocks * bsize) / 1024
	free = (st.Bavail * bsize) / 1024
	return
}
