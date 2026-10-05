package dozor

import "syscall"

func DiskUsage(path string) (used, total uint64, err error) {
	var s syscall.Statfs_t
	err = syscall.Statfs(path, &s)
	if err != nil {
		return
	}
	total = s.Blocks * uint64(s.Bsize)
	used = (s.Blocks - s.Bavail) * uint64(s.Bsize)
	return
}
