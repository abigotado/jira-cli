package profile

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func validProfile(name string) Profile {
	return Profile{
		Name:      name,
		Site:      "https://tenant.atlassian.net",
		Email:     "user@example.com",
		TokenKind: TokenKindClassic,
	}
}

func TestRegistryRoundTripAndPermissions(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "jira-cli")
	registry := NewRegistry(filepath.Join(dir, registryFilename))
	ctx := context.Background()

	if err := registry.Add(ctx, validProfile("work")); err != nil {
		t.Fatalf("Add() error = %v", err)
	}
	got, err := registry.Get(ctx, "work")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.Name != "work" {
		t.Fatalf("Get().Name = %q, want work", got.Name)
	}
	dirInfo, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("Stat(dir) error = %v", err)
	}
	if got := dirInfo.Mode().Perm(); got != 0o700 {
		t.Errorf("directory mode = %o, want 700", got)
	}
	fileInfo, err := os.Stat(registry.Path())
	if err != nil {
		t.Fatalf("Stat(file) error = %v", err)
	}
	if got := fileInfo.Mode().Perm(); got != 0o600 {
		t.Errorf("file mode = %o, want 600", got)
	}
	if err := registry.Remove(ctx, "work"); err != nil {
		t.Fatalf("Remove() error = %v", err)
	}
	profiles, err := registry.List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(profiles) != 0 {
		t.Fatalf("List() = %v, want empty", profiles)
	}
}

func TestRegistryFailsClosed(t *testing.T) {
	tests := []struct {
		name    string
		content string
		mode    os.FileMode
		wantErr error
	}{
		{name: "corrupt JSON", content: `{`, mode: 0o600, wantErr: ErrCorruptRegistry},
		{name: "unknown token field", content: `[{"name":"work","site":"https://tenant.atlassian.net","email":"user@example.com","token_kind":"classic","token":"must-not-be-here"}]`, mode: 0o600, wantErr: ErrCorruptRegistry},
		{name: "duplicate name", content: `[{"name":"work","site":"https://tenant.atlassian.net","email":"user@example.com","token_kind":"classic"},{"name":"work","site":"https://tenant.atlassian.net","email":"user@example.com","token_kind":"classic"}]`, mode: 0o600, wantErr: ErrCorruptRegistry},
		{name: "insecure file permissions", content: `[]`, mode: 0o644, wantErr: ErrInsecurePermissions},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "jira-cli")
			if err := os.Mkdir(dir, 0o700); err != nil {
				t.Fatalf("Mkdir() error = %v", err)
			}
			path := filepath.Join(dir, registryFilename)
			if err := os.WriteFile(path, []byte(tt.content), tt.mode); err != nil {
				t.Fatalf("WriteFile() error = %v", err)
			}
			_, err := NewRegistry(path).List(context.Background())
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("List() error = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func TestRegistryRejectsInsecureDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "jira-cli")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatalf("Mkdir() error = %v", err)
	}
	registry := NewRegistry(filepath.Join(dir, registryFilename))
	if err := registry.Add(context.Background(), validProfile("work")); !errors.Is(err, ErrInsecurePermissions) {
		t.Fatalf("Add() error = %v, want ErrInsecurePermissions", err)
	}
}

func TestRegistryConcurrentAddsAreNotLost(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "jira-cli")
	path := filepath.Join(dir, registryFilename)
	ctx := context.Background()
	names := []string{"alpha", "bravo", "charlie", "delta", "echo", "foxtrot"}
	start := make(chan struct{})
	var wait sync.WaitGroup
	errorsByName := make(chan error, len(names))
	for _, name := range names {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			errorsByName <- NewRegistry(path).Add(ctx, validProfile(name))
		}()
	}
	close(start)
	wait.Wait()
	close(errorsByName)
	for err := range errorsByName {
		if err != nil {
			t.Fatalf("concurrent Add() error = %v", err)
		}
	}
	profiles, err := NewRegistry(path).List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(profiles) != len(names) {
		t.Fatalf("List() returned %d profiles, want %d", len(profiles), len(names))
	}
}

func TestRegistryDoesNotOverwriteExistingProfile(t *testing.T) {
	registry := NewRegistry(filepath.Join(t.TempDir(), "jira-cli", registryFilename))
	ctx := context.Background()
	if err := registry.Add(ctx, validProfile("work")); err != nil {
		t.Fatalf("first Add() error = %v", err)
	}
	if err := registry.Add(ctx, validProfile("work")); !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("second Add() error = %v, want ErrAlreadyExists", err)
	}
}

func TestRegistryReportsDirectoryOpenFailureAsCommitted(t *testing.T) {
	registry := NewRegistry(filepath.Join(t.TempDir(), "jira-cli", registryFilename))
	registry.openDir = func(string) (*os.File, error) {
		return nil, errors.New("injected directory open failure")
	}
	err := registry.Add(context.Background(), validProfile("work"))
	if !WasCommitted(err) {
		t.Fatalf("Add() error = %v, want CommitError", err)
	}
	got, getErr := registry.Get(context.Background(), "work")
	if getErr != nil || got.Name != "work" {
		t.Fatalf("committed profile = %#v, error = %v", got, getErr)
	}
}
