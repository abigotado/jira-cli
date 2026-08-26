package arch_test

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const module = "github.com/abigotado/jira-cli"

func dependencies(t *testing.T, pkg string) []string {
	t.Helper()
	out, err := exec.Command("go", "list", "-deps", pkg).CombinedOutput()
	if err != nil {
		t.Fatalf("go list -deps %s: %v\n%s", pkg, err, out)
	}
	return strings.Fields(string(out))
}

func directImports(t *testing.T, pkg string) []string {
	t.Helper()
	out, err := exec.Command("go", "list", "-f", `{{join .Imports "\n"}}`, pkg).CombinedOutput()
	if err != nil {
		t.Fatalf("go list %s: %v\n%s", pkg, err, out)
	}
	return strings.Fields(string(out))
}

func packageExists(t *testing.T, relative string) bool {
	t.Helper()
	root, err := repositoryRoot()
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(root, relative))
	if errors.Is(err, os.ErrNotExist) {
		return false
	}
	if err != nil {
		t.Fatalf("inspect package %s: %v", relative, err)
	}
	return info.IsDir()
}

func repositoryRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", os.ErrNotExist
		}
		dir = parent
	}
}

// Jira credentials must be injected as values. Importing auth or profile here
// would couple every HTTP-client test to credential and filesystem machinery.
func TestJiraDoesNotImportAuthOrProfile(t *testing.T) {
	if !packageExists(t, "internal/jira") {
		t.Skip("internal/jira has not been added yet")
	}
	forbidden := map[string]bool{
		module + "/internal/auth":    true,
		module + "/internal/profile": true,
	}
	for _, dependency := range dependencies(t, module+"/internal/jira") {
		if forbidden[dependency] {
			t.Errorf("internal/jira depends on %s; inject non-secret config and credentials as values", dependency)
		}
	}
}

// CLI commands delegate transport behavior to internal/jira so pacing,
// retries, and credential redaction have exactly one implementation.
func TestCLIDoesNotImportNetHTTP(t *testing.T) {
	if !packageExists(t, "internal/cli") {
		t.Skip("internal/cli has not been added yet")
	}
	for _, imported := range directImports(t, module+"/internal/cli") {
		if imported == "net/http" {
			t.Error("internal/cli imports net/http; add the operation to internal/jira")
		}
	}
}

// errx is the leaf of the module dependency graph.
func TestErrxImportsNothingFromThisModule(t *testing.T) {
	for _, dependency := range dependencies(t, module+"/internal/errx") {
		if dependency != module+"/internal/errx" && strings.HasPrefix(dependency, module+"/") {
			t.Errorf("internal/errx depends on %s; keep contract errors independent", dependency)
		}
	}
}

// Write policy is local, non-secret authorization state. It must not acquire
// credentials or reach Jira on its own.
func TestWritePolicyDoesNotImportAuthOrJira(t *testing.T) {
	if !packageExists(t, "internal/writepolicy") {
		t.Skip("internal/writepolicy has not been added yet")
	}
	forbidden := map[string]bool{
		module + "/internal/auth": true,
		module + "/internal/jira": true,
	}
	for _, dependency := range dependencies(t, module+"/internal/writepolicy") {
		if forbidden[dependency] {
			t.Errorf("internal/writepolicy depends on %s; keep policy storage independent of credentials and Jira", dependency)
		}
	}
}
