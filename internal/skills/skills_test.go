package skills

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/abigotado/jira-cli/internal/errx"
)

func TestEmbeddedPayloadMatchesSourceAndProviders(t *testing.T) {
	codex, err := payload(ProviderCodex)
	if err != nil {
		t.Fatalf("codex payload: %v", err)
	}
	claude, err := payload(ProviderClaude)
	if err != nil {
		t.Fatalf("claude payload: %v", err)
	}
	if len(codex) != len(claude) {
		t.Fatalf("provider payload counts differ: codex=%d claude=%d", len(codex), len(claude))
	}
	for name, data := range codex {
		if string(claude[name]) != string(data) {
			t.Errorf("provider-neutral payload differs at %s", name)
		}
	}

	root := filepath.Join("..", "..", "assets", "skills", SkillName)
	source := make(map[string][]byte)
	err = filepath.Walk(root, func(name string, info os.FileInfo, walkErr error) error {
		if walkErr != nil || info.IsDir() {
			return walkErr
		}
		if info.Mode()&0o111 != 0 {
			t.Errorf("source payload file is executable: %s", name)
		}
		rel, relErr := filepath.Rel(root, name)
		if relErr != nil {
			return relErr
		}
		data, readErr := os.ReadFile(name)
		if readErr != nil {
			return readErr
		}
		source[filepath.ToSlash(rel)] = data
		return nil
	})
	if err != nil {
		t.Fatalf("walk source payload: %v", err)
	}
	if len(source) != len(codex) {
		t.Fatalf("source/embedded counts differ: source=%d embedded=%d", len(source), len(codex))
	}
	for name, data := range source {
		if string(codex[name]) != string(data) {
			t.Errorf("embedded payload differs at %s", name)
		}
	}
}

func TestParseProviderAndScope(t *testing.T) {
	for _, value := range []string{"codex", "claude", "all"} {
		t.Run(value, func(t *testing.T) {
			if _, err := ParseProvider(value); err != nil {
				t.Errorf("ParseProvider(%q): %v", value, err)
			}
		})
	}
	if _, err := ParseProvider("cursor"); errx.ExitCode(err) != errx.CodeUsage {
		t.Errorf("unknown provider code = %d, want %d", errx.ExitCode(err), errx.CodeUsage)
	}
	if _, err := ParseScope("global"); errx.ExitCode(err) != errx.CodeUsage {
		t.Errorf("unknown scope code = %d, want %d", errx.ExitCode(err), errx.CodeUsage)
	}
}

func TestRoots(t *testing.T) {
	home := t.TempDir()
	project := t.TempDir()
	tests := []struct {
		name string
		opts Options
		want string
	}{
		{
			name: "codex user uses open agent skills root",
			opts: Options{Provider: ProviderCodex, Scope: ScopeUser, HomeDir: fixedHome(home)},
			want: filepath.Join(home, ".agents", "skills"),
		},
		{
			name: "claude user uses claude skills root",
			opts: Options{Provider: ProviderClaude, Scope: ScopeUser, HomeDir: fixedHome(home)},
			want: filepath.Join(home, ".claude", "skills"),
		},
		{
			name: "codex project uses agents skills root",
			opts: Options{Provider: ProviderCodex, Scope: ScopeProject, ProjectDir: project},
			want: filepath.Join(project, ".agents", "skills"),
		},
		{
			name: "claude project without harness uses claude skills root",
			opts: Options{Provider: ProviderClaude, Scope: ScopeProject, ProjectDir: project},
			want: filepath.Join(project, ".claude", "skills"),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := Root(test.opts)
			if err != nil {
				t.Fatalf("Root(): %v", err)
			}
			if got != test.want {
				t.Errorf("root = %s, want %s", got, test.want)
			}
		})
	}
}

func TestAllProjectHarnessFailsBeforeWrites(t *testing.T) {
	project := t.TempDir()
	if err := os.MkdirAll(filepath.Join(project, ".agents", "rules"), 0o755); err != nil {
		t.Fatalf("seed harness: %v", err)
	}
	results, err := Install(context.Background(), Options{
		Provider: ProviderAll, Scope: ScopeProject, ProjectDir: project,
	})
	if results != nil {
		t.Errorf("results = %#v, want nil", results)
	}
	assertReason(t, err, "HARNESS_OWNED_DIRECTORY")
	if _, statErr := os.Stat(filepath.Join(project, ".agents", "skills", SkillName)); !os.IsNotExist(statErr) {
		t.Errorf("codex destination was written before claude preflight failed: %v", statErr)
	}
	if _, statErr := os.Stat(filepath.Join(project, ".claude")); !os.IsNotExist(statErr) {
		t.Errorf("claude destination was created: %v", statErr)
	}
}

func TestAllPreflightsForeignFilesBeforeWrites(t *testing.T) {
	home := t.TempDir()
	claudeSkill := filepath.Join(home, ".claude", "skills", SkillName)
	if err := os.MkdirAll(claudeSkill, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	foreign := filepath.Join(claudeSkill, "SKILL.md")
	if err := os.WriteFile(foreign, []byte("# hand maintained\n"), 0o644); err != nil {
		t.Fatalf("seed foreign file: %v", err)
	}

	_, err := Install(context.Background(), Options{
		Provider: ProviderAll, Scope: ScopeUser, HomeDir: fixedHome(home),
	})
	if errx.ExitCode(err) != errx.CodeConfirm {
		t.Fatalf("exit code = %d, want %d: %v", errx.ExitCode(err), errx.CodeConfirm, err)
	}
	if _, statErr := os.Stat(filepath.Join(home, ".agents")); !os.IsNotExist(statErr) {
		t.Errorf("codex destination was created before claude conflict failed: %v", statErr)
	}
	data, readErr := os.ReadFile(foreign)
	if readErr != nil || string(data) != "# hand maintained\n" {
		t.Errorf("foreign file changed: data=%q err=%v", data, readErr)
	}
}

func TestAllInstallsBothAndIsIdempotent(t *testing.T) {
	home := t.TempDir()
	opts := Options{Provider: ProviderAll, Scope: ScopeUser, HomeDir: fixedHome(home)}
	first, err := Install(context.Background(), opts)
	if err != nil {
		t.Fatalf("first install: %v", err)
	}
	if len(first) != 2 {
		t.Fatalf("result count = %d, want 2", len(first))
	}
	for _, result := range first {
		if !result.InSync || countApplied(result.Files) == 0 {
			t.Errorf("fresh %s result = %#v", result.Provider, result)
		}
	}

	second, err := Install(context.Background(), opts)
	if err != nil {
		t.Fatalf("second install: %v", err)
	}
	for _, result := range second {
		if !result.InSync || countApplied(result.Files) != 0 {
			t.Errorf("idempotent %s result = %#v", result.Provider, result)
		}
		for _, file := range result.Files {
			if file.Status != StatusCurrent {
				t.Errorf("%s status = %s, want current", file.Path, file.Status)
			}
		}
	}
}

func TestProjectAllWithoutHarnessInstallsAndReportsBoth(t *testing.T) {
	project := t.TempDir()
	results, err := Install(context.Background(), Options{
		Provider: ProviderAll, Scope: ScopeProject, ProjectDir: project,
	})
	if err != nil {
		t.Fatalf("install all: %v", err)
	}
	if len(results) != 2 || results[0].Provider != "codex" || results[1].Provider != "claude" {
		t.Fatalf("results = %#v", results)
	}
	for _, target := range []string{
		filepath.Join(project, ".agents", "skills", SkillName, "SKILL.md"),
		filepath.Join(project, ".claude", "skills", SkillName, "SKILL.md"),
	} {
		if _, err := os.Stat(target); err != nil {
			t.Errorf("missing installed skill %s: %v", target, err)
		}
	}
}

func TestDryRunCreatesNothing(t *testing.T) {
	home := t.TempDir()
	results, err := Install(context.Background(), Options{
		Provider: ProviderAll, Scope: ScopeUser, HomeDir: fixedHome(home), DryRun: true,
	})
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("result count = %d, want 2", len(results))
	}
	entries, err := os.ReadDir(home)
	if err != nil {
		t.Fatalf("read home: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("dry run created %v", entries)
	}
}

func TestForeignAndModifiedFilesNeedConfirmation(t *testing.T) {
	tests := []struct {
		name string
		seed func(t *testing.T, opts Options)
	}{
		{
			name: "foreign",
			seed: func(t *testing.T, opts Options) {
				skillDir := filepath.Join(opts.Dest, SkillName)
				if err := os.MkdirAll(skillDir, 0o755); err != nil {
					t.Fatalf("mkdir: %v", err)
				}
				if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("foreign\n"), 0o644); err != nil {
					t.Fatalf("write: %v", err)
				}
			},
		},
		{
			name: "modified",
			seed: func(t *testing.T, opts Options) {
				if _, err := Install(context.Background(), opts); err != nil {
					t.Fatalf("initial install: %v", err)
				}
				if err := os.WriteFile(filepath.Join(opts.Dest, SkillName, "SKILL.md"), []byte("modified\n"), 0o644); err != nil {
					t.Fatalf("write: %v", err)
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			opts := Options{Provider: ProviderClaude, Scope: ScopeUser, Dest: t.TempDir()}
			test.seed(t, opts)
			dryRun := opts
			dryRun.DryRun = true
			if results, err := Install(context.Background(), dryRun); err != nil || len(results) != 1 {
				t.Fatalf("dry-run classification: results=%#v err=%v", results, err)
			}
			if _, err := Install(context.Background(), opts); errx.ExitCode(err) != errx.CodeConfirm {
				t.Fatalf("exit code = %d, want %d: %v", errx.ExitCode(err), errx.CodeConfirm, err)
			}
			opts.Confirmed = true
			if _, err := Install(context.Background(), opts); err != nil {
				t.Fatalf("confirmed install: %v", err)
			}
			data, err := os.ReadFile(filepath.Join(opts.Dest, SkillName, "SKILL.md"))
			if err != nil || !strings.Contains(string(data), "jira-cli") {
				t.Errorf("confirmed install did not restore payload: data=%q err=%v", data, err)
			}
		})
	}
}

func TestManifestAndNestedPayloadSymlinksAreRefused(t *testing.T) {
	t.Run("manifest", func(t *testing.T) {
		dest := t.TempDir()
		skillDir := filepath.Join(dest, SkillName)
		if err := os.MkdirAll(skillDir, 0o755); err != nil {
			t.Fatal(err)
		}
		outside := filepath.Join(t.TempDir(), "manifest.json")
		if err := os.WriteFile(outside, []byte(`{}`), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(skillDir, manifestName)); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		_, err := Install(context.Background(), Options{Provider: ProviderClaude, Scope: ScopeUser, Dest: dest, Confirmed: true})
		assertReason(t, err, "DEST_IS_SYMLINK")
	})

	t.Run("nested payload parent on uninstall", func(t *testing.T) {
		dest := t.TempDir()
		opts := Options{Provider: ProviderClaude, Scope: ScopeUser, Dest: dest}
		if _, err := Install(context.Background(), opts); err != nil {
			t.Fatal(err)
		}
		skillDir := filepath.Join(dest, SkillName)
		reference := filepath.Join(skillDir, "reference")
		if err := os.RemoveAll(reference); err != nil {
			t.Fatal(err)
		}
		outside := t.TempDir()
		outsideFile := filepath.Join(outside, "contract.md")
		if err := os.WriteFile(outsideFile, []byte("outside\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, reference); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		_, err := Uninstall(context.Background(), opts)
		assertReason(t, err, "DEST_IS_SYMLINK")
		if data, readErr := os.ReadFile(outsideFile); readErr != nil || string(data) != "outside\n" {
			t.Fatalf("outside file changed: %q err=%v", data, readErr)
		}
	})
}

func TestOversizedManifestIsRefused(t *testing.T) {
	dest := t.TempDir()
	skillDir := filepath.Join(dest, SkillName)
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	data := make([]byte, maxManifestBytes+1)
	if err := os.WriteFile(filepath.Join(skillDir, manifestName), data, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Install(context.Background(), Options{Provider: ProviderClaude, Scope: ScopeUser, Dest: dest, Confirmed: true})
	assertReason(t, err, "DEST_FILE_TOO_LARGE")
}

func TestHashSafeRemovalPreservesConcurrentReplacement(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, "owned.md")
	if err := os.WriteFile(target, []byte("replacement\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := removeIfHashMatches(base, target, sum([]byte("previous-owned-content\n")))
	assertReason(t, err, "DEST_CHANGED")
	entries, readErr := os.ReadDir(base)
	if readErr != nil {
		t.Fatal(readErr)
	}
	found := false
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(base, entry.Name()))
		if err == nil && string(data) == "replacement\n" {
			found = true
		}
	}
	if !found {
		t.Fatal("concurrent replacement was deleted instead of preserved")
	}
}

func TestSymlinksAreRefused(t *testing.T) {
	tests := []struct {
		name string
		seed func(t *testing.T, home, elsewhere string)
	}{
		{
			name: "provider directory",
			seed: func(t *testing.T, home, elsewhere string) {
				if err := os.Symlink(elsewhere, filepath.Join(home, ".claude")); err != nil {
					t.Skipf("symlinks unavailable: %v", err)
				}
			},
		},
		{
			name: "skill directory",
			seed: func(t *testing.T, home, elsewhere string) {
				root := filepath.Join(home, ".claude", "skills")
				if err := os.MkdirAll(root, 0o755); err != nil {
					t.Fatalf("mkdir: %v", err)
				}
				if err := os.Symlink(elsewhere, filepath.Join(root, SkillName)); err != nil {
					t.Skipf("symlinks unavailable: %v", err)
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			home := t.TempDir()
			elsewhere := t.TempDir()
			test.seed(t, home, elsewhere)
			_, err := Install(context.Background(), Options{
				Provider: ProviderClaude, Scope: ScopeUser, HomeDir: fixedHome(home), Confirmed: true,
			})
			assertReason(t, err, "DEST_IS_SYMLINK")
			entries, readErr := os.ReadDir(elsewhere)
			if readErr != nil || len(entries) != 0 {
				t.Errorf("symlink target changed: entries=%v err=%v", entries, readErr)
			}
		})
	}
}

func TestSharedWritableDestinationIsRefused(t *testing.T) {
	if os.PathSeparator == '\\' {
		t.Skip("Windows ACLs are outside os.FileMode permission bits")
	}
	dest := t.TempDir()
	if err := os.Chmod(dest, 0o777); err != nil {
		t.Fatalf("make destination shared-writable: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dest, 0o700) })

	_, err := Install(context.Background(), Options{
		Provider: ProviderClaude,
		Scope:    ScopeUser,
		Dest:     dest,
	})
	assertReason(t, err, "DEST_SHARED_WRITABLE")
}

func TestDestinationBelowSharedWritableAncestorIsRefused(t *testing.T) {
	if os.PathSeparator == '\\' {
		t.Skip("Windows ACLs are outside os.FileMode permission bits")
	}
	shared := filepath.Join(t.TempDir(), "shared")
	dest := filepath.Join(shared, "private-destination")
	if err := os.MkdirAll(dest, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(shared, 0o777); err != nil {
		t.Fatalf("make ancestor shared-writable: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(shared, 0o700) })

	_, err := Install(context.Background(), Options{
		Provider: ProviderClaude,
		Scope:    ScopeUser,
		Dest:     dest,
	})
	assertReason(t, err, "DEST_SHARED_WRITABLE")
}

func TestStickySharedDestinationIsRefused(t *testing.T) {
	if os.PathSeparator == '\\' {
		t.Skip("Windows ACLs are outside os.FileMode permission bits")
	}
	dest := t.TempDir()
	if err := os.Chmod(dest, os.ModeSticky|0o777); err != nil {
		t.Fatalf("make destination sticky shared-writable: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dest, 0o700) })

	_, err := Install(context.Background(), Options{
		Provider: ProviderClaude,
		Scope:    ScopeUser,
		Dest:     dest,
	})
	assertReason(t, err, "DEST_SHARED_WRITABLE")
}

func TestManifestTraversalIsRefusedForInstallAndUninstall(t *testing.T) {
	escapes := []string{"../../../victim", "../sibling", "a/../../../victim", "/etc/passwd"}
	for _, rel := range escapes {
		t.Run(rel, func(t *testing.T) {
			dest := t.TempDir()
			skillDir := filepath.Join(dest, SkillName)
			if err := os.MkdirAll(skillDir, 0o755); err != nil {
				t.Fatalf("mkdir: %v", err)
			}
			victim := filepath.Join(dest, "victim")
			if err := os.WriteFile(victim, []byte("precious"), 0o644); err != nil {
				t.Fatalf("write victim: %v", err)
			}
			seedManifest(t, skillDir, ProviderClaude, false, []manifestFile{{Path: rel, SHA256: sum([]byte("precious"))}})
			opts := Options{Provider: ProviderClaude, Scope: ScopeUser, Dest: dest, Confirmed: true}
			if _, err := Install(context.Background(), opts); err == nil {
				t.Fatal("install accepted traversal")
			} else {
				assertReason(t, err, "MANIFEST_CORRUPT")
			}
			if _, err := Uninstall(context.Background(), opts); err == nil {
				t.Fatal("uninstall accepted traversal")
			} else {
				assertReason(t, err, "MANIFEST_CORRUPT")
			}
			data, err := os.ReadFile(victim)
			if err != nil || string(data) != "precious" {
				t.Errorf("victim changed: data=%q err=%v", data, err)
			}
		})
	}
}

func TestIncompleteInstallSelfHeals(t *testing.T) {
	dest := t.TempDir()
	opts := Options{Provider: ProviderClaude, Scope: ScopeUser, Dest: dest}
	if _, err := Install(context.Background(), opts); err != nil {
		t.Fatalf("install: %v", err)
	}
	skillDir := filepath.Join(dest, SkillName)
	data, err := os.ReadFile(filepath.Join(skillDir, manifestName))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	var current manifest
	if err := json.Unmarshal(data, &current); err != nil {
		t.Fatalf("decode manifest: %v", err)
	}
	current.Complete = false
	seedManifest(t, skillDir, ProviderClaude, false, current.Files)
	if err := os.Remove(filepath.Join(skillDir, "SKILL.md")); err != nil {
		t.Fatalf("remove payload file: %v", err)
	}
	results, err := Install(context.Background(), opts)
	if err != nil {
		t.Fatalf("repair: %v", err)
	}
	if len(results) != 1 || !results[0].InSync {
		t.Errorf("repair result = %#v", results)
	}
	if _, err := os.Stat(filepath.Join(skillDir, "SKILL.md")); err != nil {
		t.Errorf("payload was not repaired: %v", err)
	}
}

func TestUninstallRemovesOnlyHashOwnedFiles(t *testing.T) {
	dest := t.TempDir()
	opts := Options{Provider: ProviderClaude, Scope: ScopeUser, Dest: dest}
	if _, err := Install(context.Background(), opts); err != nil {
		t.Fatalf("install: %v", err)
	}
	skillDir := filepath.Join(dest, SkillName)
	edited := filepath.Join(skillDir, "SKILL.md")
	if err := os.WriteFile(edited, []byte("user edit\n"), 0o644); err != nil {
		t.Fatalf("edit: %v", err)
	}
	foreign := filepath.Join(skillDir, "notes.md")
	if err := os.WriteFile(foreign, []byte("foreign\n"), 0o644); err != nil {
		t.Fatalf("foreign: %v", err)
	}
	results, err := Uninstall(context.Background(), opts)
	if err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	if len(results) != 1 || results[0].InSync {
		t.Errorf("uninstall result = %#v", results)
	}
	for _, target := range []string{edited, foreign} {
		if _, err := os.Stat(target); err != nil {
			t.Errorf("uninstall removed user-owned file %s: %v", target, err)
		}
	}
	if _, err := os.Stat(filepath.Join(skillDir, "reference", "contract.md")); !os.IsNotExist(err) {
		t.Errorf("owned reference was not removed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(skillDir, manifestName)); !os.IsNotExist(err) {
		t.Errorf("ownership manifest was not removed: %v", err)
	}
}

func TestUninstallRemovesEmptyOwnedDirectories(t *testing.T) {
	dest := t.TempDir()
	opts := Options{Provider: ProviderCodex, Scope: ScopeUser, Dest: dest}
	if _, err := Install(context.Background(), opts); err != nil {
		t.Fatalf("install: %v", err)
	}
	if _, err := Uninstall(context.Background(), opts); err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, SkillName)); !os.IsNotExist(err) {
		t.Errorf("empty owned skill directory remains: %v", err)
	}
}

func TestStaleLockIsRecovered(t *testing.T) {
	dest := t.TempDir()
	skillDir := filepath.Join(dest, SkillName)
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	lock := filepath.Join(skillDir, manifestName+lockSuffix)
	if err := os.WriteFile(lock, nil, 0o600); err != nil {
		t.Fatalf("lock: %v", err)
	}
	old := time.Now().Add(-lockStale - time.Second)
	if err := os.Chtimes(lock, old, old); err != nil {
		t.Fatalf("age lock: %v", err)
	}
	if _, err := Install(context.Background(), Options{
		Provider: ProviderCodex, Scope: ScopeUser, Dest: dest,
	}); err != nil {
		t.Fatalf("install after stale lock: %v", err)
	}
}

func TestProviderAllRejectsExplicitDest(t *testing.T) {
	_, err := Install(context.Background(), Options{
		Provider: ProviderAll, Scope: ScopeUser, Dest: t.TempDir(),
	})
	if errx.ExitCode(err) != errx.CodeUsage {
		t.Errorf("exit code = %d, want %d", errx.ExitCode(err), errx.CodeUsage)
	}
}

func fixedHome(home string) func() (string, error) {
	return func() (string, error) { return home, nil }
}

func countApplied(files []FileResult) int {
	count := 0
	for _, file := range files {
		if file.Applied {
			count++
		}
	}
	return count
}

func assertReason(t *testing.T, err error, want string) {
	t.Helper()
	var typed *errx.Error
	if !errors.As(err, &typed) || typed.Reason != want {
		t.Fatalf("error = %v, want reason %s", err, want)
	}
}

func seedManifest(t *testing.T, skillDir string, provider Provider, complete bool, files []manifestFile) {
	t.Helper()
	current := manifest{
		Version: 1, Skill: SkillName, Provider: string(provider), Complete: complete, Files: files,
	}
	data, err := json.MarshalIndent(current, "", "  ")
	if err != nil {
		t.Fatalf("encode manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, manifestName), data, 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
}
