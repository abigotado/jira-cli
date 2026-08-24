// Command gencommands renders the public command reference from the Cobra tree.
package main

import (
	"fmt"
	"os"
	"path/filepath"
)

var targets = []string{
	filepath.Join("docs", "commands.md"),
	filepath.Join("assets", "skills", "jira", "reference", "commands.md"),
}

func main() {
	if err := run(); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "gencommands: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	root, err := repoRoot()
	if err != nil {
		return err
	}
	content := renderMarkdown(collect(rootCommand()))
	for _, target := range targets {
		path := filepath.Join(root, target)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return fmt.Errorf("create %s: %w", filepath.Dir(target), err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			return fmt.Errorf("write %s: %w", target, err)
		}
	}
	return nil
}

func repoRoot() (string, error) {
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
			return "", fmt.Errorf("no go.mod found above the working directory")
		}
		dir = parent
	}
}
