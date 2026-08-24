// Package skills installs the embedded provider-neutral Jira Agent Skill for
// Codex and Claude without touching credentials or the network.
package skills

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/abigotado/jira-cli/assets"
	"github.com/abigotado/jira-cli/internal/errx"
	"github.com/abigotado/jira-cli/internal/lockfile"
)

const (
	// SkillName is the installed Agent Skill directory.
	SkillName = "jira"

	manifestName = ".jira-cli-install.json"
	lockSuffix   = ".lock"
	lockStale    = 10 * time.Second

	maxManifestBytes = 1 << 20
	maxPayloadBytes  = 4 << 20
)

// Provider selects an Agent Skill host.
type Provider string

const (
	ProviderCodex  Provider = "codex"
	ProviderClaude Provider = "claude"
	ProviderAll    Provider = "all"
)

// Providers returns concrete providers in stable application order.
func Providers() []Provider {
	return []Provider{ProviderCodex, ProviderClaude}
}

// ParseProvider validates an installer provider.
func ParseProvider(value string) (Provider, error) {
	switch Provider(value) {
	case ProviderCodex, ProviderClaude, ProviderAll:
		return Provider(value), nil
	default:
		return "", errx.Usage("unknown provider %q: want codex, claude, or all", value)
	}
}

// Scope selects a user-wide or project-local install.
type Scope string

const (
	ScopeUser    Scope = "user"
	ScopeProject Scope = "project"
)

// ParseScope validates an installer scope.
func ParseScope(value string) (Scope, error) {
	switch Scope(value) {
	case ScopeUser, ScopeProject:
		return Scope(value), nil
	default:
		return "", errx.Usage("unknown scope %q: want user or project", value)
	}
}

// Options describes an install or uninstall operation.
type Options struct {
	Provider   Provider
	Scope      Scope
	ProjectDir string
	Dest       string
	Confirmed  bool
	DryRun     bool

	// HomeDir is injectable so tests never use a developer's real home.
	HomeDir func() (string, error)
}

// Status describes one destination file before the requested operation.
type Status string

const (
	StatusAbsent   Status = "absent"
	StatusCurrent  Status = "current"
	StatusStale    Status = "stale"
	StatusModified Status = "modified"
	StatusForeign  Status = "foreign"
	StatusOrphan   Status = "orphan"
)

// FileResult reports one file's classification and whether this call changed
// it.
type FileResult struct {
	Path    string `json:"path"`
	Status  Status `json:"status"`
	Applied bool   `json:"applied"`
}

// Result reports one concrete provider's outcome. A provider=all operation
// returns two Results only after both providers pass preflight.
type Result struct {
	Provider string       `json:"provider"`
	Scope    string       `json:"scope"`
	Root     string       `json:"root"`
	Skill    string       `json:"skill"`
	InSync   bool         `json:"in_sync"`
	DryRun   bool         `json:"dry_run"`
	Files    []FileResult `json:"files"`
}

type manifest struct {
	Version  int            `json:"version"`
	Skill    string         `json:"skill"`
	Provider string         `json:"provider"`
	Complete bool           `json:"complete"`
	Files    []manifestFile `json:"files"`
}

type manifestFile struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

type installPlan struct {
	opts     Options
	root     string
	guard    string
	skillDir string
	want     map[string][]byte
	result   Result
}

type uninstallPlan struct {
	opts     Options
	root     string
	guard    string
	skillDir string
	recorded map[string]string
	result   Result
}

func (o Options) homeDir() (string, error) {
	homeDir := o.HomeDir
	if homeDir == nil {
		homeDir = os.UserHomeDir
	}
	home, err := homeDir()
	if err != nil || home == "" {
		return "", &errx.Error{
			Code:    errx.CodeUsage,
			Reason:  "NO_HOME_DIR",
			Message: "cannot determine a home directory",
			Hint:    "set HOME or pass --dest with an explicit existing directory",
		}
	}
	return filepath.Abs(home)
}

func concreteProviders(provider Provider) ([]Provider, error) {
	switch provider {
	case ProviderCodex, ProviderClaude:
		return []Provider{provider}, nil
	case ProviderAll:
		return Providers(), nil
	default:
		return nil, errx.Usage("unknown provider %q: want codex, claude, or all", provider)
	}
}

// Root returns the skills root for one concrete provider.
func Root(opts Options) (string, error) {
	if opts.Provider == ProviderAll {
		return "", errx.Usage("provider all has two roots; select codex or claude")
	}
	if _, err := concreteProviders(opts.Provider); err != nil {
		return "", err
	}
	if _, err := ParseScope(string(opts.Scope)); err != nil {
		return "", err
	}
	if opts.Dest != "" {
		if opts.Scope == ScopeProject {
			return "", errx.Usage("--dest cannot be combined with --scope project; use --project-dir")
		}
		return resolveDest(opts.Dest)
	}

	switch opts.Scope {
	case ScopeUser:
		home, err := opts.homeDir()
		if err != nil {
			return "", err
		}
		if opts.Provider == ProviderCodex {
			return filepath.Join(home, ".agents", "skills"), nil
		}
		return filepath.Join(home, ".claude", "skills"), nil
	case ScopeProject:
		if opts.ProjectDir == "" {
			return "", errx.Usage("--scope project needs a project directory")
		}
		project, err := filepath.Abs(opts.ProjectDir)
		if err != nil {
			return "", errx.Internal("resolve project directory: %v", err)
		}
		if info, statErr := os.Lstat(project); statErr != nil {
			return "", translateFS("inspect", project, statErr)
		} else if info.Mode()&os.ModeSymlink != 0 {
			return "", symlinkError(project)
		} else if !info.IsDir() {
			return "", errx.Usage("project directory %s is not a directory", project)
		}
		if opts.Provider == ProviderClaude && hasAgentHarness(project) {
			return "", &errx.Error{
				Code:    errx.CodeUsage,
				Reason:  "HARNESS_OWNED_DIRECTORY",
				Message: fmt.Sprintf("%s has an .agents harness that owns generated .claude content", project),
				Hint:    "install the canonical skill under .agents/skills for Codex, add it to the harness source, or use --scope user for Claude",
			}
		}
		if opts.Provider == ProviderCodex {
			return filepath.Join(project, ".agents", "skills"), nil
		}
		return filepath.Join(project, ".claude", "skills"), nil
	default:
		return "", errx.Usage("unknown scope %q: want user or project", opts.Scope)
	}
}

func resolveDest(dest string) (string, error) {
	if dest == "~" || strings.HasPrefix(dest, "~/") {
		return "", errx.Usage("--dest %q is not expanded; pass an absolute path", dest)
	}
	abs, err := filepath.Abs(dest)
	if err != nil {
		return "", errx.Internal("resolve --dest: %v", err)
	}
	info, err := os.Lstat(abs)
	if err != nil {
		return "", &errx.Error{
			Code:    errx.CodeUsage,
			Reason:  "DEST_NOT_A_DIRECTORY",
			Message: fmt.Sprintf("--dest %s does not exist", abs),
			Hint:    "create the directory first or omit --dest",
		}
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return "", symlinkError(abs)
	}
	if !info.IsDir() {
		return "", &errx.Error{
			Code:    errx.CodeUsage,
			Reason:  "DEST_NOT_A_DIRECTORY",
			Message: fmt.Sprintf("--dest %s is not a directory", abs),
			Hint:    "pass a directory",
		}
	}
	return abs, nil
}

func hasAgentHarness(project string) bool {
	markers := []string{"agents.toml", "hooks.toml", "rules", "providers", "agents"}
	for _, marker := range markers {
		if _, err := os.Lstat(filepath.Join(project, ".agents", marker)); err == nil {
			return true
		}
	}
	return false
}

func guardRoot(opts Options, root string) (string, error) {
	if opts.Dest != "" {
		return root, nil
	}
	if opts.Scope == ScopeProject {
		return filepath.Abs(opts.ProjectDir)
	}
	return opts.homeDir()
}

func payload(provider Provider) (map[string][]byte, error) {
	if provider != ProviderCodex && provider != ProviderClaude {
		return nil, errx.Usage("embedded payload needs a concrete provider")
	}
	root := path.Join("skills", SkillName)
	out := make(map[string][]byte)
	err := fs.WalkDir(assets.FS, root, func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		rel := strings.TrimPrefix(name, root+"/")
		if rel == name || rel == "" {
			return fmt.Errorf("embedded path %q is outside %q", name, root)
		}
		data, err := assets.FS.ReadFile(name)
		if err != nil {
			return err
		}
		out[rel] = data
		return nil
	})
	if err != nil {
		return nil, errx.Internal("read embedded skill: %v", err)
	}
	if len(out) == 0 {
		return nil, errx.Internal("the embedded skill payload is empty")
	}
	return out, nil
}

// Install preflights every requested provider before creating or changing any
// destination. ProviderAll therefore cannot partially install Codex when the
// Claude project target is harness-owned.
func Install(ctx context.Context, opts Options) ([]Result, error) {
	plans, err := preflightInstall(opts)
	if err != nil {
		return nil, err
	}
	results := make([]Result, 0, len(plans))
	for i := range plans {
		plan := &plans[i]
		if opts.DryRun {
			results = append(results, plan.result)
			continue
		}
		if err := applyInstallPlan(ctx, plan); err != nil {
			if len(results) > 0 {
				return results, partialProviderError("install", results, plan.opts.Provider, err)
			}
			return nil, err
		}
		results = append(results, plan.result)
	}
	return results, nil
}

func preflightInstall(opts Options) ([]installPlan, error) {
	providers, err := concreteProviders(opts.Provider)
	if err != nil {
		return nil, err
	}
	if opts.Provider == ProviderAll && opts.Dest != "" {
		return nil, errx.Usage("--dest cannot be combined with --provider all")
	}

	plans := make([]installPlan, 0, len(providers))
	for _, provider := range providers {
		providerOpts := opts
		providerOpts.Provider = provider
		root, err := Root(providerOpts)
		if err != nil {
			return nil, err
		}
		guard, err := guardRoot(providerOpts, root)
		if err != nil {
			return nil, err
		}
		skillDir := filepath.Join(root, SkillName)
		if err := assertNoSymlink(guard, skillDir); err != nil {
			return nil, err
		}
		want, err := payload(provider)
		if err != nil {
			return nil, err
		}
		files, err := classify(skillDir, want, provider)
		if err != nil {
			return nil, err
		}
		if !opts.DryRun {
			if err := refuseUnmanaged(files, opts.Confirmed); err != nil {
				return nil, err
			}
		}
		plans = append(plans, installPlan{
			opts: providerOpts, root: root, guard: guard, skillDir: skillDir, want: want,
			result: Result{
				Provider: string(provider), Scope: string(opts.Scope), Root: root,
				Skill: SkillName, InSync: inSync(files, false), DryRun: opts.DryRun, Files: files,
			},
		})
	}
	return plans, nil
}

func applyInstallPlan(ctx context.Context, plan *installPlan) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("install %s skill: %w", plan.opts.Provider, err)
	}
	if err := assertNoSymlink(plan.guard, plan.skillDir); err != nil {
		return err
	}
	if err := os.MkdirAll(plan.skillDir, 0o755); err != nil {
		return translateFS("create", plan.skillDir, err)
	}
	return withLock(ctx, plan.guard, filepath.Join(plan.root, ".jira-cli-"+SkillName+"-skill"), func() error {
		// Reclassify under the lock so a file created after preflight cannot be
		// silently overwritten.
		files, err := classify(plan.skillDir, plan.want, plan.opts.Provider)
		if err != nil {
			return err
		}
		if err := refuseUnmanaged(files, plan.opts.Confirmed); err != nil {
			return err
		}
		if err := apply(ctx, plan.guard, plan.skillDir, plan.want, files, plan.opts.Provider); err != nil {
			return err
		}
		plan.result.Files = files
		plan.result.InSync = inSync(files, true)
		return nil
	})
}

func classify(skillDir string, want map[string][]byte, provider Provider) ([]FileResult, error) {
	current, err := readManifest(skillDir, provider)
	if err != nil {
		return nil, err
	}
	recorded := current.hashes()
	seen := make(map[string]bool, len(want))
	files := make([]FileResult, 0, len(want)+len(recorded))
	for rel, data := range want {
		seen[rel] = true
		target, err := safeJoin(skillDir, rel)
		if err != nil {
			return nil, err
		}
		recordedHash, wasRecorded := recorded[rel]
		status, err := classifyOne(skillDir, target, recordedHash, sum(data), wasRecorded)
		if err != nil {
			return nil, err
		}
		files = append(files, FileResult{Path: target, Status: status})
	}
	for rel, recordedHash := range recorded {
		if seen[rel] {
			continue
		}
		target, err := safeJoin(skillDir, rel)
		if err != nil {
			return nil, err
		}
		data, exists, err := readRegularBounded(skillDir, target, maxPayloadBytes)
		if !exists && err == nil {
			continue
		}
		if err != nil {
			return nil, translateFS("read", target, err)
		}
		status := StatusOrphan
		if sum(data) != recordedHash {
			status = StatusModified
		}
		files = append(files, FileResult{Path: target, Status: status})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return files, nil
}

func classifyOne(skillDir, target, recordedHash, wantHash string, wasRecorded bool) (Status, error) {
	data, exists, err := readRegularBounded(skillDir, target, maxPayloadBytes)
	if !exists && err == nil {
		return StatusAbsent, nil
	}
	if err != nil {
		return "", err
	}
	diskHash := sum(data)
	if !wasRecorded {
		return StatusForeign, nil
	}
	if diskHash != recordedHash {
		return StatusModified, nil
	}
	if diskHash != wantHash {
		return StatusStale, nil
	}
	return StatusCurrent, nil
}

func refuseUnmanaged(files []FileResult, confirmed bool) error {
	if confirmed {
		return nil
	}
	blocked := make([]string, 0)
	for _, file := range files {
		if file.Status == StatusModified || file.Status == StatusForeign {
			blocked = append(blocked, file.Path)
		}
	}
	if len(blocked) == 0 {
		return nil
	}
	return &errx.Error{
		Code:    errx.CodeConfirm,
		Reason:  "UNMANAGED_FILES",
		Message: fmt.Sprintf("%d destination file(s) are foreign or were modified: %s", len(blocked), strings.Join(blocked, ", ")),
		Hint:    "review with --dry-run, then use --yes only if overwriting those files is intended",
	}
}

func apply(ctx context.Context, guard, skillDir string, want map[string][]byte, files []FileResult, provider Provider) error {
	previous, err := readManifest(skillDir, provider)
	if err != nil {
		return err
	}
	previousHashes := previous.hashes()
	record := make([]manifestFile, 0, len(want))
	for rel, data := range want {
		record = append(record, manifestFile{Path: rel, SHA256: sum(data)})
	}
	sort.Slice(record, func(i, j int) bool { return record[i].Path < record[j].Path })
	if err := writeManifest(guard, skillDir, manifest{
		Version: 1, Skill: SkillName, Provider: string(provider), Complete: false, Files: record,
	}); err != nil {
		return err
	}

	for i := range files {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("install skill payload: %w", err)
		}
		file := &files[i]
		rel, err := filepath.Rel(skillDir, file.Path)
		if err != nil {
			return errx.Internal("resolve %s: %v", file.Path, err)
		}
		data, shipped := want[filepath.ToSlash(rel)]
		switch file.Status {
		case StatusCurrent:
			continue
		case StatusOrphan:
			if err := removeIfHashMatches(skillDir, file.Path, previousHashes[filepath.ToSlash(rel)]); err != nil && !os.IsNotExist(err) {
				return err
			}
			file.Applied = true
			continue
		case StatusModified, StatusForeign:
			if !shipped {
				continue
			}
		}
		if !shipped {
			continue
		}
		if err := writeFile(guard, file.Path, data); err != nil {
			return err
		}
		file.Applied = true
	}
	return writeManifest(guard, skillDir, manifest{
		Version: 1, Skill: SkillName, Provider: string(provider), Complete: true, Files: record,
	})
}

// Uninstall preflights every requested provider, then removes only files whose
// current hashes still match the ownership manifest.
func Uninstall(ctx context.Context, opts Options) ([]Result, error) {
	plans, err := preflightUninstall(opts)
	if err != nil {
		return nil, err
	}
	results := make([]Result, 0, len(plans))
	for i := range plans {
		plan := &plans[i]
		if !opts.DryRun && len(plan.recorded) > 0 {
			if err := applyUninstallPlan(ctx, plan); err != nil {
				if len(results) > 0 {
					return results, partialProviderError("uninstall", results, plan.opts.Provider, err)
				}
				return nil, err
			}
		}
		results = append(results, plan.result)
	}
	return results, nil
}

func partialProviderError(action string, applied []Result, failed Provider, cause error) error {
	names := make([]string, len(applied))
	for index, result := range applied {
		names[index] = result.Provider
	}
	return (&errx.Error{
		Code:    errx.CodeConflict,
		Reason:  "PARTIAL_SKILL_" + strings.ToUpper(action),
		Message: fmt.Sprintf("skill %s changed provider(s) %s before %s failed", action, strings.Join(names, ", "), failed),
		Hint:    "inspect both providers with --dry-run and reconcile them before retrying",
	}).Wrap(cause)
}

func preflightUninstall(opts Options) ([]uninstallPlan, error) {
	providers, err := concreteProviders(opts.Provider)
	if err != nil {
		return nil, err
	}
	if opts.Provider == ProviderAll && opts.Dest != "" {
		return nil, errx.Usage("--dest cannot be combined with --provider all")
	}
	plans := make([]uninstallPlan, 0, len(providers))
	for _, provider := range providers {
		providerOpts := opts
		providerOpts.Provider = provider
		root, err := Root(providerOpts)
		if err != nil {
			return nil, err
		}
		guard, err := guardRoot(providerOpts, root)
		if err != nil {
			return nil, err
		}
		skillDir := filepath.Join(root, SkillName)
		if err := assertNoSymlink(guard, skillDir); err != nil {
			return nil, err
		}
		current, err := readManifest(skillDir, provider)
		if err != nil {
			return nil, err
		}
		recorded := current.hashes()
		files, err := classifyUninstall(skillDir, recorded)
		if err != nil {
			return nil, err
		}
		plans = append(plans, uninstallPlan{
			opts: providerOpts, root: root, guard: guard, skillDir: skillDir, recorded: recorded,
			result: Result{
				Provider: string(provider), Scope: string(opts.Scope), Root: root,
				Skill: SkillName, InSync: uninstallInSync(files, false), DryRun: opts.DryRun, Files: files,
			},
		})
	}
	return plans, nil
}

func classifyUninstall(skillDir string, recorded map[string]string) ([]FileResult, error) {
	files := make([]FileResult, 0, len(recorded))
	for rel, hash := range recorded {
		target, err := safeJoin(skillDir, rel)
		if err != nil {
			return nil, err
		}
		data, exists, err := readRegularBounded(skillDir, target, maxPayloadBytes)
		if !exists && err == nil {
			files = append(files, FileResult{Path: target, Status: StatusAbsent})
			continue
		}
		if err != nil {
			return nil, err
		}
		status := StatusOrphan
		if sum(data) != hash {
			status = StatusModified
		}
		files = append(files, FileResult{Path: target, Status: status})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return files, nil
}

func applyUninstallPlan(ctx context.Context, plan *uninstallPlan) error {
	err := withLock(ctx, plan.guard, filepath.Join(plan.root, ".jira-cli-"+SkillName+"-skill"), func() error {
		// The manifest may have changed while this process waited for the lock.
		current, err := readManifest(plan.skillDir, plan.opts.Provider)
		if err != nil {
			return err
		}
		plan.recorded = current.hashes()
		files, err := classifyUninstall(plan.skillDir, plan.recorded)
		if err != nil {
			return err
		}
		for i := range files {
			if err := ctx.Err(); err != nil {
				return fmt.Errorf("uninstall skill payload: %w", err)
			}
			file := &files[i]
			if file.Status != StatusOrphan {
				continue
			}
			rel, relErr := filepath.Rel(plan.skillDir, file.Path)
			if relErr != nil {
				return errx.Internal("resolve uninstall target: %v", relErr)
			}
			if err := removeIfHashMatches(plan.skillDir, file.Path, plan.recorded[filepath.ToSlash(rel)]); err != nil && !os.IsNotExist(err) {
				return err
			}
			file.Applied = true
		}
		manifestPath := filepath.Join(plan.skillDir, manifestName)
		manifestData, exists, readErr := readRegularBounded(plan.skillDir, manifestPath, maxManifestBytes)
		if readErr != nil {
			return readErr
		}
		if exists {
			if err := removeIfHashMatches(plan.skillDir, manifestPath, sum(manifestData)); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
		removeEmptyOwnedDirs(plan.skillDir, plan.recorded)
		plan.result.Files = files
		plan.result.InSync = uninstallInSync(files, true)
		return nil
	})
	if err != nil {
		return err
	}
	// withLock owns the final directory entry while the callback runs. Remove
	// the now-empty skill directory only after the deferred lock cleanup.
	_ = os.Remove(plan.skillDir)
	return nil
}

func removeEmptyOwnedDirs(skillDir string, recorded map[string]string) {
	dirs := make(map[string]bool)
	for rel := range recorded {
		dir := filepath.Dir(filepath.Join(skillDir, filepath.FromSlash(rel)))
		for dir != skillDir && strings.HasPrefix(dir, skillDir+string(filepath.Separator)) {
			dirs[dir] = true
			dir = filepath.Dir(dir)
		}
	}
	ordered := make([]string, 0, len(dirs))
	for dir := range dirs {
		ordered = append(ordered, dir)
	}
	sort.Slice(ordered, func(i, j int) bool { return len(ordered[i]) > len(ordered[j]) })
	for _, dir := range ordered {
		_ = os.Remove(dir)
	}
	_ = os.Remove(skillDir)
}

func safeJoin(skillDir, rel string) (string, error) {
	slashed := filepath.ToSlash(rel)
	if slashed == "" || slashed == "." || path.IsAbs(slashed) || filepath.IsAbs(rel) {
		return "", corruptManifest(rel)
	}
	for _, part := range strings.Split(slashed, "/") {
		if part == ".." {
			return "", corruptManifest(rel)
		}
	}
	target := filepath.Join(skillDir, filepath.FromSlash(slashed))
	inside, err := filepath.Rel(skillDir, target)
	if err != nil || inside == ".." || strings.HasPrefix(inside, ".."+string(filepath.Separator)) {
		return "", corruptManifest(rel)
	}
	return target, nil
}

func readRegularBounded(base, target string, limit int64) ([]byte, bool, error) {
	if err := assertNoSymlink(base, target); err != nil {
		return nil, false, err
	}
	rel, err := filepath.Rel(base, target)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil, false, errx.Internal("resolve %s under %s", target, base)
	}
	root, err := os.OpenRoot(base)
	if os.IsNotExist(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, translateFS("open", base, err)
	}
	defer root.Close()
	info, err := root.Lstat(rel)
	if os.IsNotExist(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, translateFS("inspect", target, err)
	}
	if !info.Mode().IsRegular() {
		return nil, false, &errx.Error{
			Code: errx.CodeUsage, Reason: "DEST_NOT_A_FILE",
			Message: fmt.Sprintf("%s is not a regular file", target),
			Hint:    "move the conflicting filesystem entry and retry",
		}
	}
	if info.Size() > limit {
		return nil, false, &errx.Error{
			Code: errx.CodeUsage, Reason: "DEST_FILE_TOO_LARGE",
			Message: fmt.Sprintf("%s exceeds the %d-byte safety limit", target, limit),
			Hint:    "move the oversized file and retry",
		}
	}
	file, err := root.Open(rel)
	if err != nil {
		return nil, false, translateFS("read", target, err)
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() || opened.Size() > limit {
		return nil, false, &errx.Error{
			Code: errx.CodeUsage, Reason: "DEST_NOT_A_FILE",
			Message: fmt.Sprintf("%s changed while it was being inspected", target),
			Hint:    "stop the concurrent filesystem change and retry",
		}
	}
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, false, translateFS("read", target, err)
	}
	if int64(len(data)) > limit {
		return nil, false, &errx.Error{
			Code: errx.CodeUsage, Reason: "DEST_FILE_TOO_LARGE",
			Message: fmt.Sprintf("%s exceeds the %d-byte safety limit", target, limit),
			Hint:    "move the oversized file and retry",
		}
	}
	return data, true, nil
}

func corruptManifest(rel string) error {
	return &errx.Error{
		Code:    errx.CodeUsage,
		Reason:  "MANIFEST_CORRUPT",
		Message: fmt.Sprintf("the install manifest records a path outside the skill directory: %q", rel),
		Hint:    "remove the skill directory and install again; an outside path was not written by jira-cli",
	}
}

func (m manifest) hashes() map[string]string {
	if len(m.Files) == 0 {
		return nil
	}
	hashes := make(map[string]string, len(m.Files))
	for _, file := range m.Files {
		hashes[file.Path] = file.SHA256
	}
	return hashes
}

func readManifest(skillDir string, provider Provider) (manifest, error) {
	target := filepath.Join(skillDir, manifestName)
	data, exists, err := readRegularBounded(skillDir, target, maxManifestBytes)
	if err != nil {
		return manifest{}, err
	}
	if !exists {
		return manifest{}, nil
	}
	var current manifest
	if json.Unmarshal(data, &current) != nil || current.Version != 1 || current.Skill != SkillName || current.Provider != string(provider) {
		return manifest{}, &errx.Error{
			Code:    errx.CodeUsage,
			Reason:  "MANIFEST_CORRUPT",
			Message: "the jira-cli skill ownership manifest is invalid",
			Hint:    "move the skill directory aside, inspect it, and install again",
		}
	}
	if len(current.Files) > 128 {
		return manifest{}, corruptManifest("too many recorded files")
	}
	seen := make(map[string]bool, len(current.Files))
	for _, file := range current.Files {
		if seen[file.Path] {
			return manifest{}, corruptManifest(file.Path)
		}
		seen[file.Path] = true
		if _, err := safeJoin(skillDir, file.Path); err != nil {
			return manifest{}, err
		}
		decoded, err := hex.DecodeString(file.SHA256)
		if err != nil || len(decoded) != sha256.Size {
			return manifest{}, corruptManifest(file.Path)
		}
	}
	return current, nil
}

func writeManifest(guard, skillDir string, current manifest) error {
	data, err := json.MarshalIndent(current, "", "  ")
	if err != nil {
		return errx.Internal("encode the install manifest: %v", err)
	}
	return writeFile(guard, filepath.Join(skillDir, manifestName), data)
}

func writeFile(guard, target string, data []byte) error {
	if err := assertNoSymlink(guard, target); err != nil {
		return err
	}
	dir := filepath.Dir(target)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return translateFS("create", dir, err)
	}
	if info, err := os.Lstat(target); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return symlinkError(target)
	}
	tmp, err := os.CreateTemp(dir, ".jira-skill-*.tmp")
	if err != nil {
		return translateFS("create temporary file in", dir, err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return translateFS("write", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return translateFS("close", tmpName, err)
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		return translateFS("chmod", tmpName, err)
	}
	if err := os.Rename(tmpName, target); err != nil {
		return translateFS("replace", target, err)
	}
	return nil
}

func withLock(ctx context.Context, guard, lockBase string, fn func() error) error {
	lockPath := lockBase + lockSuffix
	if err := assertNoSymlink(guard, lockPath); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("wait for skill installer lock: %w", err)
	}
	err := lockfile.With(lockBase, func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		return fn()
	})
	var timeout *lockfile.TimeoutError
	if errors.As(err, &timeout) {
		return &errx.Error{
			Code:    errx.CodeUsage,
			Reason:  "INSTALL_LOCKED",
			Message: fmt.Sprintf("another jira-cli process is changing %s", filepath.Dir(lockPath)),
			Hint:    "wait for the other skill operation to finish and retry",
		}
	}
	return err
}

func assertNoSymlink(base, target string) error {
	rel, err := filepath.Rel(base, target)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return errx.Internal("resolve %s under %s", target, base)
	}
	if err := assertPrivateAncestors(base); err != nil {
		return err
	}
	current := base
	if info, err := os.Lstat(current); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return symlinkError(current)
		}
		if isSharedWritable(info) {
			return sharedWritableError(current)
		}
	}
	for _, part := range strings.Split(filepath.ToSlash(rel), "/") {
		if part == "" || part == "." {
			continue
		}
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return translateFS("inspect", current, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return symlinkError(current)
		}
		if isSharedWritable(info) {
			return sharedWritableError(current)
		}
	}
	return nil
}

func assertPrivateAncestors(base string) error {
	current := filepath.Clean(base)
	foundExisting := false
	for {
		info, err := os.Lstat(current)
		if os.IsNotExist(err) {
			parent := filepath.Dir(current)
			if parent == current {
				return translateFS("inspect", current, err)
			}
			current = parent
			continue
		}
		if err != nil {
			return translateFS("inspect", current, err)
		}
		firstExisting := !foundExisting
		foundExisting = true
		if isSharedWritable(info) && (!isStickyDirectory(info) || firstExisting) {
			return sharedWritableError(current)
		}
		parent := filepath.Dir(current)
		if parent == current {
			return nil
		}
		current = parent
	}
}

func symlinkError(target string) error {
	return &errx.Error{
		Code:    errx.CodeUsage,
		Reason:  "DEST_IS_SYMLINK",
		Message: fmt.Sprintf("%s is a symlink", target),
		Hint:    "install to a real directory; jira-cli will not write through links it did not create",
	}
}

func sharedWritableError(target string) error {
	return &errx.Error{
		Code:    errx.CodeUsage,
		Reason:  "DEST_SHARED_WRITABLE",
		Message: fmt.Sprintf("%s is writable by another OS user", target),
		Hint:    "remove group/world write permission or choose a private destination",
	}
}

func sum(data []byte) string {
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:])
}

func inSync(files []FileResult, applied bool) bool {
	for _, file := range files {
		if file.Status == StatusCurrent || applied && file.Applied {
			continue
		}
		return false
	}
	return true
}

func uninstallInSync(files []FileResult, applied bool) bool {
	for _, file := range files {
		if file.Status == StatusAbsent || applied && file.Applied {
			continue
		}
		return false
	}
	return true
}

func translateFS(operation, target string, err error) error {
	for _, condition := range []error{syscall.EROFS, syscall.ENOSPC, syscall.ENOTDIR, syscall.EISDIR} {
		if errors.Is(err, condition) {
			return &errx.Error{
				Code:    errx.CodeUsage,
				Reason:  "WRITE_DENIED",
				Message: fmt.Sprintf("cannot %s %s: %v", operation, target, err),
				Hint:    "check that the destination is a writable directory with space available",
			}
		}
	}
	if os.IsPermission(err) {
		return &errx.Error{
			Code:    errx.CodeUsage,
			Reason:  "WRITE_DENIED",
			Message: fmt.Sprintf("cannot %s %s: permission denied", operation, target),
			Hint:    "check directory permissions or pass --dest with a writable location",
		}
	}
	return errx.Internal("cannot %s %s: %v", operation, target, err)
}
