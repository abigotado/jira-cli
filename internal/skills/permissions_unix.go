//go:build !windows

package skills

import "os"

func isSharedWritable(info os.FileInfo) bool {
	return info.Mode().Perm()&0o022 != 0
}

func isStickyDirectory(info os.FileInfo) bool {
	mode := info.Mode()
	return mode.IsDir() && mode&os.ModeSticky != 0
}
