//go:build windows

package skills

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/abigotado/jira-cli/internal/errx"
)

func removeIfHashMatches(base, target, expectedHash string) error {
	data, exists, err := readRegularBounded(base, target, maxPayloadBytes)
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}
	if sum(data) != expectedHash {
		return &errx.Error{Code: errx.CodeConflict, Reason: "DEST_CHANGED", Message: fmt.Sprintf("%s changed during uninstall", target), Hint: "stop concurrent changes and retry"}
	}
	return removeRegular(base, target)
}

func removeRegular(base, target string) error {
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
	info, err := root.Lstat(rel)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return &errx.Error{Code: errx.CodeUsage, Reason: "DEST_NOT_A_FILE", Message: fmt.Sprintf("%s is not a regular file", target), Hint: "move the conflicting filesystem entry and retry"}
	}
	if err := root.Remove(rel); err != nil {
		return translateFS("remove", target, err)
	}
	return nil
}
