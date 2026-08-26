package writepolicy

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/abigotado/jira-cli/internal/lockfile"
	"github.com/abigotado/jira-cli/internal/profile"
)

func testProfile(name string) profile.Profile {
	return profile.Profile{
		Name: name, Site: "https://example.atlassian.net", Email: name + "@example.invalid",
		TokenKind: profile.TokenKindScoped, CloudID: "cloud-" + name,
	}
}

func TestCanonicalProjects(t *testing.T) {
	tests := []struct {
		name     string
		projects []string
		want     []string
		wantErr  bool
	}{
		{name: "normalizes sorts and deduplicates", projects: []string{" wl ", "FL", "wl", "A_2"}, want: []string{"A_2", "FL", "WL"}},
		{name: "empty is rejected", wantErr: true},
		{name: "hyphen is rejected", projects: []string{"WL-1"}, wantErr: true},
		{name: "leading digit is rejected", projects: []string{"1WL"}, wantErr: true},
		{name: "overlong key is rejected", projects: []string{"ABCDEFGHIJKLMNOPQRSTUVWXYZ1234567"}, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := CanonicalProjects(test.projects)
			if test.wantErr {
				if !errors.Is(err, ErrInvalid) {
					t.Fatalf("error = %v, want ErrInvalid", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("CanonicalProjects() error = %v", err)
			}
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("projects = %#v, want %#v", got, test.want)
			}
		})
	}
}

func TestRegistryPersistsStrictIdentityBoundPolicy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config", "write-policies.json")
	registry := NewRegistry(path)
	value := testProfile("work")

	policy, err := registry.Set(context.Background(), value, []string{"fl", "WL", "fl"})
	if err != nil {
		t.Fatalf("Set() error = %v", err)
	}
	if want := []string{"FL", "WL"}; !reflect.DeepEqual(policy.Projects, want) {
		t.Fatalf("projects = %#v, want %#v", policy.Projects, want)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("mode = %04o, want 0600", got)
	}
	if got, err := registry.RequireProject(context.Background(), value, "WL"); err != nil || got.Profile != "work" {
		t.Fatalf("RequireProject() = %#v, %v", got, err)
	}

	tests := []struct {
		name    string
		profile profile.Profile
		project string
		wantErr error
	}{
		{name: "project comparison is exact", profile: value, project: "wl", wantErr: ErrProjectDenied},
		{name: "changed email makes policy stale", profile: func() profile.Profile { changed := value; changed.Email = "other@example.invalid"; return changed }(), project: "WL", wantErr: ErrStale},
		{name: "changed site makes policy stale", profile: func() profile.Profile { changed := value; changed.Site = "https://other.atlassian.net"; return changed }(), project: "WL", wantErr: ErrStale},
		{name: "changed token kind makes policy stale", profile: func() profile.Profile {
			changed := value
			changed.TokenKind = profile.TokenKindClassic
			changed.CloudID = ""
			return changed
		}(), project: "WL", wantErr: ErrStale},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := registry.RequireProject(context.Background(), test.profile, test.project)
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("error = %v, want %v", err, test.wantErr)
			}
		})
	}

	if err := registry.Clear(context.Background(), value.Name); err != nil {
		t.Fatalf("Clear() error = %v", err)
	}
	if err := registry.Clear(context.Background(), value.Name); err != nil {
		t.Fatalf("idempotent Clear() error = %v", err)
	}
	if _, err := registry.Get(context.Background(), value.Name); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get() error = %v, want ErrNotFound", err)
	}
}

func TestRegistryRejectsNonCanonicalOrInsecureFiles(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		mode    os.FileMode
		wantErr error
	}{
		{name: "unknown field", body: `{"version":1,"policies":[],"extra":true}`, mode: 0o600, wantErr: ErrCorruptRegistry},
		{name: "trailing JSON", body: `{"version":1,"policies":[]} {}`, mode: 0o600, wantErr: ErrCorruptRegistry},
		{name: "duplicate profiles", body: `{"version":1,"policies":[{"profile":"work","identity":{"site":"https://example.atlassian.net","email":"work@example.invalid","token_kind":"classic"},"projects":["WL"]},{"profile":"work","identity":{"site":"https://example.atlassian.net","email":"work@example.invalid","token_kind":"classic"},"projects":["FL"]}]}`, mode: 0o600, wantErr: ErrCorruptRegistry},
		{name: "noncanonical projects", body: `{"version":1,"policies":[{"profile":"work","identity":{"site":"https://example.atlassian.net","email":"work@example.invalid","token_kind":"classic"},"projects":["WL","FL"]}]}`, mode: 0o600, wantErr: ErrCorruptRegistry},
		{name: "broad file permissions", body: `{"version":1,"policies":[]}`, mode: 0o644, wantErr: ErrInsecurePermissions},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.Chmod(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "write-policies.json")
			if err := os.WriteFile(path, []byte(test.body), test.mode); err != nil {
				t.Fatal(err)
			}
			_, err := NewRegistry(path).Get(context.Background(), "work")
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("error = %v, want %v", err, test.wantErr)
			}
		})
	}
}

func TestRegistryReportsDirectoryOpenFailureAsCommitted(t *testing.T) {
	registry := NewRegistry(filepath.Join(t.TempDir(), "config", "write-policies.json"))
	registry.openDir = func(string) (*os.File, error) {
		return nil, errors.New("injected directory open failure")
	}
	value := testProfile("work")
	_, err := registry.Set(context.Background(), value, []string{"WL"})
	if !WasCommitted(err) {
		t.Fatalf("Set() error = %v, want CommitError", err)
	}
	policy, getErr := registry.Get(context.Background(), value.Name)
	if getErr != nil || !reflect.DeepEqual(policy.Projects, []string{"WL"}) {
		t.Fatalf("committed policy = %#v, error = %v", policy, getErr)
	}
}

func TestRegistryRefusesToWriteBeyondReadableSizeLimit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config", "write-policies.json")
	registry := NewRegistry(path)
	err := registry.write(registryFile{
		Version: registryVersion,
		Policies: []Policy{{
			Profile: strings.Repeat("x", maxRegistryBytes),
		}},
	})
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("write() error = %v, want ErrInvalid", err)
	}
	if _, statErr := os.Stat(path); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("registry path exists after oversized write: %v", statErr)
	}
}

func TestRegistryConcurrentSetsDoNotLosePolicies(t *testing.T) {
	registry := NewRegistry(filepath.Join(t.TempDir(), "config", "write-policies.json"))
	const count = 12
	var wait sync.WaitGroup
	errorsFound := make(chan error, count)
	for index := 0; index < count; index++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			name := "work" + string(rune('a'+index))
			_, err := registry.Set(context.Background(), testProfile(name), []string{"WL"})
			errorsFound <- err
		}(index)
	}
	wait.Wait()
	close(errorsFound)
	for err := range errorsFound {
		if err != nil {
			t.Fatalf("Set() error = %v", err)
		}
	}
	for index := 0; index < count; index++ {
		name := "work" + string(rune('a'+index))
		if _, err := registry.Get(context.Background(), name); err != nil {
			t.Fatalf("Get(%q) error = %v", name, err)
		}
	}
}

func TestRegistryDoesNotWriteAfterContextCanceledWhileWaitingForLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config", "write-policies.json")
	registry := NewRegistry(path)
	locked := make(chan struct{})
	release := make(chan struct{})
	holderDone := make(chan error, 1)
	go func() {
		holderDone <- lockfile.With(path, func() error {
			close(locked)
			<-release
			return nil
		})
	}()
	<-locked

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := registry.Set(ctx, testProfile("work"), []string{"WL"})
		result <- err
	}()
	time.Sleep(25 * time.Millisecond)
	cancel()
	close(release)
	if err := <-holderDone; err != nil {
		t.Fatalf("lock holder error = %v", err)
	}
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("Set() error = %v, want context canceled", err)
	}
	if _, err := registry.Get(context.Background(), "work"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get() error = %v, want no persisted policy", err)
	}
}
