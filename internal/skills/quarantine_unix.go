//go:build !windows

package skills

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/abigotado/jira-cli/internal/errx"
	"golang.org/x/sys/unix"
)

// removeIfHashMatches atomically detaches a target from its directory before
// hashing it. A concurrent replacement is preserved under the reported
// quarantine name and is never deleted as if it were installer-owned.
func removeIfHashMatches(base, target, expectedHash string) error {
	if expectedHash == "" {
		return changedFile(target, "the ownership manifest has no hash for this file")
	}
	if err := assertNoSymlink(base, target); err != nil {
		return err
	}
	rel, err := filepath.Rel(base, target)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return errx.Internal("resolve %s under %s", target, base)
	}
	root, err := os.OpenRoot(base)
	if err != nil {
		return translateFS("open", base, err)
	}
	defer root.Close()
	parentRel := filepath.Dir(rel)
	parent, err := root.Open(parentRel)
	if err != nil {
		return translateFS("open", filepath.Dir(target), err)
	}
	defer parent.Close()
	parentInfo, err := parent.Stat()
	if err != nil || !parentInfo.IsDir() {
		return changedFile(target, "the parent directory changed during uninstall")
	}
	quarantineName, err := newQuarantineName(filepath.Base(rel))
	if err != nil {
		return errx.Internal("create quarantine name: %v", err)
	}
	if err := unix.Renameat(int(parent.Fd()), filepath.Base(rel), int(parent.Fd()), quarantineName); err != nil {
		return translateFS("quarantine", target, err)
	}
	quarantinePath := filepath.Join(filepath.Dir(target), quarantineName)
	fileDescriptor, err := unix.Openat(int(parent.Fd()), quarantineName, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return changedFile(quarantinePath, "the quarantined file could not be opened safely")
	}
	file := os.NewFile(uintptr(fileDescriptor), quarantinePath)
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxPayloadBytes {
		return changedFile(quarantinePath, "the detached entry is not a bounded regular file")
	}
	data, err := io.ReadAll(io.LimitReader(file, maxPayloadBytes+1))
	if err != nil || len(data) > maxPayloadBytes {
		return changedFile(quarantinePath, "the detached entry could not be read within the safety limit")
	}
	if sum(data) != expectedHash {
		return changedFile(quarantinePath, "the file changed after installer classification")
	}
	if err := unix.Unlinkat(int(parent.Fd()), quarantineName, 0); err != nil {
		return translateFS("remove quarantined", quarantinePath, err)
	}
	return nil
}

func newQuarantineName(base string) (string, error) {
	var nonce [12]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", err
	}
	return fmt.Sprintf(".%s.jira-cli-remove-%s", base, hex.EncodeToString(nonce[:])), nil
}

func changedFile(path, detail string) error {
	return &errx.Error{
		Code:    errx.CodeConflict,
		Reason:  "DEST_CHANGED",
		Message: fmt.Sprintf("%s; preserved at %s", detail, path),
		Hint:    "inspect the preserved file and retry only after concurrent changes stop",
	}
}
