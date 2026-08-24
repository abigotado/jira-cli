package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/abigotado/jira-cli/internal/errx"
)

func TestGeneratedContractIsCurrent(t *testing.T) {
	root, err := repositoryRoot()
	if err != nil {
		t.Fatal(err)
	}
	want := renderMarkdown(errx.Describe())
	for _, target := range targets {
		t.Run(target, func(t *testing.T) {
			content, err := os.ReadFile(filepath.Join(root, target))
			if err != nil {
				t.Fatalf("read generated contract: %v", err)
			}
			if string(content) != want {
				t.Errorf("%s is stale; run go generate ./...", target)
			}
		})
	}
}

func TestRenderContainsEveryExitCodeAndMetadataField(t *testing.T) {
	got := renderMarkdown(errx.Describe())
	for _, info := range errx.Codes() {
		if !strings.Contains(got, "`"+info.Name+"`") || !strings.Contains(got, info.NextMove) {
			t.Errorf("rendered contract omits code %d (%s)", info.Code, info.Name)
		}
	}
	for _, field := range []string{"count", "truncated", "next_cursor", "profile", "site"} {
		if !strings.Contains(got, `"`+field+`"`) {
			t.Errorf("rendered contract omits meta.%s", field)
		}
	}
	if !strings.Contains(got, generatedNotice) {
		t.Error("rendered contract lacks generated notice")
	}
}

func TestRepositoryRootWorksBelowRoot(t *testing.T) {
	root, err := repositoryRoot()
	if err != nil {
		t.Fatal(err)
	}
	old, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(filepath.Join(root, "tools", "gencontract")); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(old); err != nil {
			t.Errorf("restore working directory: %v", err)
		}
	})
	if got, err := repositoryRoot(); err != nil || got != root {
		t.Errorf("repositoryRoot = %q, %v; want %q", got, err, root)
	}
}
