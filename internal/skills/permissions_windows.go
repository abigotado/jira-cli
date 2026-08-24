//go:build windows

package skills

import "os"

// Windows ACLs are not represented by os.FileMode permission bits. Windows
// installs retain the symlink/reparse-point checks and document a same-user
// trust boundary.
func isSharedWritable(os.FileInfo) bool {
	return false
}

func isStickyDirectory(os.FileInfo) bool {
	return false
}
