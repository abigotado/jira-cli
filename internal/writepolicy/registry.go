// Package writepolicy stores the local, non-secret project allowlist used as
// the pre-dispatch target-selection rail for every Jira mutation.
package writepolicy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/abigotado/jira-cli/internal/lockfile"
	"github.com/abigotado/jira-cli/internal/profile"
)

const (
	registryVersion  = 1
	registryFilename = "write-policies.json"
	maxRegistryBytes = 1 << 20
	maxPolicies      = 1024
	maxProjects      = 256
)

var projectKeyPattern = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,31}$`)

var (
	// ErrNotFound means a profile has no write policy.
	ErrNotFound = errors.New("write policy not found")
	// ErrStale means the policy belongs to different profile metadata.
	ErrStale = errors.New("write policy identity is stale")
	// ErrProjectDenied means the project is not locally allowed for writes.
	ErrProjectDenied = errors.New("project is not allowed for writes")
	// ErrInvalid means policy input is invalid.
	ErrInvalid = errors.New("invalid write policy")
	// ErrCorruptRegistry means the on-disk registry failed strict validation.
	ErrCorruptRegistry = errors.New("write policy registry is corrupt")
	// ErrInsecurePermissions means the registry is accessible too broadly.
	ErrInsecurePermissions = errors.New("write policy registry has insecure permissions")
)

// CommitError reports that the atomic rename completed but a following
// directory durability check failed. Callers must reconcile the persisted
// policy instead of assuming the mutation did not apply.
type CommitError struct {
	Err error
}

func (e *CommitError) Error() string {
	return fmt.Sprintf("write policy committed but durability check failed: %v", e.Err)
}

func (e *CommitError) Unwrap() error { return e.Err }

// WasCommitted identifies a post-rename write-policy failure.
func WasCommitted(err error) bool {
	var committed *CommitError
	return errors.As(err, &committed)
}

// Identity binds an allowlist to the exact non-secret Jira account metadata.
type Identity struct {
	Site      string `json:"site"`
	Email     string `json:"email"`
	TokenKind string `json:"token_kind"`
	CloudID   string `json:"cloud_id,omitempty"`
}

// IdentityFor returns the canonical policy identity for a profile.
func IdentityFor(value profile.Profile) Identity {
	return Identity{
		Site:      value.Site,
		Email:     strings.ToLower(value.Email),
		TokenKind: string(value.TokenKind),
		CloudID:   value.CloudID,
	}
}

// Policy is one profile's identity-bound project allowlist.
type Policy struct {
	Profile  string   `json:"profile"`
	Identity Identity `json:"identity"`
	Projects []string `json:"projects"`
}

type registryFile struct {
	Version  int      `json:"version"`
	Policies []Policy `json:"policies"`
}

// Registry persists policies in a strict atomic 0600 JSON file.
type Registry struct {
	path    string
	openDir func(string) (*os.File, error)
}

// NewRegistry creates a registry at an explicit path.
func NewRegistry(path string) *Registry { return &Registry{path: path} }

// NewDefaultRegistry creates the user-scoped registry beside profiles.json.
func NewDefaultRegistry() (*Registry, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return nil, fmt.Errorf("locate user config directory: %w", err)
	}
	return NewRegistry(filepath.Join(dir, "jira-cli", registryFilename)), nil
}

// Path returns the registry path.
func (r *Registry) Path() string {
	if r == nil {
		return ""
	}
	return r.path
}

// WithPolicyLock serializes operations for one profile. Callers that also
// need the profile lock must acquire it first.
func (r *Registry) WithPolicyLock(ctx context.Context, name string, fn func() error) error {
	if err := profile.RequireName(name); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if r == nil || r.path == "" {
		return errors.New("write policy registry path is empty")
	}
	return lockfile.With(r.path+".profile-"+name, func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		return fn()
	})
}

// Get returns one policy without interpreting its identity.
func (r *Registry) Get(ctx context.Context, name string) (Policy, error) {
	if err := profile.RequireName(name); err != nil {
		return Policy{}, err
	}
	if err := ctx.Err(); err != nil {
		return Policy{}, err
	}
	file, err := r.read()
	if err != nil {
		return Policy{}, err
	}
	for _, candidate := range file.Policies {
		if candidate.Profile == name {
			return candidate, nil
		}
	}
	return Policy{}, fmt.Errorf("%w: %s", ErrNotFound, name)
}

// GetBound returns a policy only when it belongs to the exact current profile.
func (r *Registry) GetBound(ctx context.Context, value profile.Profile) (Policy, error) {
	policy, err := r.Get(ctx, value.Name)
	if err != nil {
		return Policy{}, err
	}
	if policy.Identity != IdentityFor(value) {
		return Policy{}, fmt.Errorf("%w: %s", ErrStale, value.Name)
	}
	return policy, nil
}

// RequireProject returns the bound policy when project is explicitly allowed.
func (r *Registry) RequireProject(ctx context.Context, value profile.Profile, project string) (Policy, error) {
	policy, err := r.GetBound(ctx, value)
	if err != nil {
		return Policy{}, err
	}
	for _, allowed := range policy.Projects {
		if allowed == project {
			return policy, nil
		}
	}
	return Policy{}, fmt.Errorf("%w: %s", ErrProjectDenied, project)
}

// Set replaces one profile's policy after canonicalizing project keys.
func (r *Registry) Set(ctx context.Context, value profile.Profile, projects []string) (Policy, error) {
	canonical, err := CanonicalProjects(projects)
	if err != nil {
		return Policy{}, err
	}
	policy := Policy{Profile: value.Name, Identity: IdentityFor(value), Projects: canonical}
	err = r.mutate(ctx, func(file registryFile) (registryFile, error) {
		for index := range file.Policies {
			if file.Policies[index].Profile == value.Name {
				file.Policies[index] = policy
				return file, nil
			}
		}
		file.Policies = append(file.Policies, policy)
		return file, nil
	})
	return policy, err
}

// Clear removes one profile's policy. Missing policy is an idempotent success.
func (r *Registry) Clear(ctx context.Context, name string) error {
	if err := profile.RequireName(name); err != nil {
		return err
	}
	return r.mutate(ctx, func(file registryFile) (registryFile, error) {
		for index, candidate := range file.Policies {
			if candidate.Profile == name {
				file.Policies = append(file.Policies[:index:index], file.Policies[index+1:]...)
				break
			}
		}
		return file, nil
	})
}

// CanonicalProjects validates, uppercases, sorts, and deduplicates keys.
func CanonicalProjects(projects []string) ([]string, error) {
	if len(projects) == 0 || len(projects) > maxProjects {
		return nil, fmt.Errorf("%w: provide 1-%d project keys", ErrInvalid, maxProjects)
	}
	seen := make(map[string]struct{}, len(projects))
	canonical := make([]string, 0, len(projects))
	for _, value := range projects {
		key := strings.ToUpper(strings.TrimSpace(value))
		if !projectKeyPattern.MatchString(key) {
			return nil, fmt.Errorf("%w: project key %q must contain only uppercase letters, digits, or underscore", ErrInvalid, value)
		}
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		canonical = append(canonical, key)
	}
	sort.Strings(canonical)
	return canonical, nil
}

func (r *Registry) mutate(ctx context.Context, change func(registryFile) (registryFile, error)) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if r == nil || r.path == "" {
		return errors.New("write policy registry path is empty")
	}
	if err := ensureDir(filepath.Dir(r.path)); err != nil {
		return err
	}
	return lockfile.With(r.path, func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		file, err := r.read()
		if err != nil {
			return err
		}
		file, err = change(file)
		if err != nil {
			return err
		}
		if len(file.Policies) > maxPolicies {
			return fmt.Errorf("write policy registry cannot contain more than %d policies", maxPolicies)
		}
		sort.Slice(file.Policies, func(i, j int) bool { return file.Policies[i].Profile < file.Policies[j].Profile })
		for _, policy := range file.Policies {
			if err := validatePolicy(policy); err != nil {
				return fmt.Errorf("%w: %v", ErrCorruptRegistry, err)
			}
		}
		return r.write(file)
	})
}

func (r *Registry) read() (registryFile, error) {
	empty := registryFile{Version: registryVersion, Policies: []Policy{}}
	if r == nil || r.path == "" {
		return empty, errors.New("write policy registry path is empty")
	}
	if err := validateDir(filepath.Dir(r.path)); err != nil {
		return empty, err
	}
	info, err := os.Lstat(r.path)
	if errors.Is(err, os.ErrNotExist) {
		return empty, nil
	}
	if err != nil {
		return empty, fmt.Errorf("inspect write policy registry: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		return empty, fmt.Errorf("%w: %s must be a regular 0600 file", ErrInsecurePermissions, r.path)
	}
	if info.Size() > maxRegistryBytes {
		return empty, fmt.Errorf("%w: file exceeds %d bytes", ErrCorruptRegistry, maxRegistryBytes)
	}
	raw, err := os.ReadFile(r.path)
	if err != nil {
		return empty, fmt.Errorf("read write policy registry: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var file registryFile
	if err := decoder.Decode(&file); err != nil {
		return empty, fmt.Errorf("%w: decode: %v", ErrCorruptRegistry, err)
	}
	if file.Version != registryVersion || file.Policies == nil {
		return empty, fmt.Errorf("%w: unsupported version or null policies", ErrCorruptRegistry)
	}
	var extra json.RawMessage
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return empty, fmt.Errorf("%w: trailing data", ErrCorruptRegistry)
	}
	if len(file.Policies) > maxPolicies {
		return empty, fmt.Errorf("%w: too many policies", ErrCorruptRegistry)
	}
	seen := make(map[string]struct{}, len(file.Policies))
	for _, policy := range file.Policies {
		if err := validatePolicy(policy); err != nil {
			return empty, fmt.Errorf("%w: %v", ErrCorruptRegistry, err)
		}
		if _, exists := seen[policy.Profile]; exists {
			return empty, fmt.Errorf("%w: duplicate profile %q", ErrCorruptRegistry, policy.Profile)
		}
		seen[policy.Profile] = struct{}{}
	}
	return file, nil
}

func validatePolicy(policy Policy) error {
	if policy.Identity.Email != strings.ToLower(policy.Identity.Email) {
		return fmt.Errorf("%w: policy identity is incomplete or non-canonical", ErrInvalid)
	}
	identityProfile := profile.Profile{
		Name: policy.Profile, Site: policy.Identity.Site, Email: policy.Identity.Email,
		TokenKind: profile.TokenKind(policy.Identity.TokenKind), CloudID: policy.Identity.CloudID,
	}
	if err := identityProfile.Validate(); err != nil {
		return err
	}
	canonical, err := CanonicalProjects(policy.Projects)
	if err != nil {
		return err
	}
	if len(canonical) != len(policy.Projects) {
		return fmt.Errorf("%w: project keys must be unique", ErrInvalid)
	}
	for index := range canonical {
		if canonical[index] != policy.Projects[index] {
			return fmt.Errorf("%w: project keys must be sorted and canonical", ErrInvalid)
		}
	}
	return nil
}

func ensureDir(dir string) error {
	info, err := os.Lstat(dir)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("create write policy directory: %w", err)
		}
		if err := os.Chmod(dir, 0o700); err != nil {
			return fmt.Errorf("secure write policy directory: %w", err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect write policy directory: %w", err)
	}
	if !info.IsDir() || info.Mode().Perm() != 0o700 {
		return fmt.Errorf("%w: %s must be a 0700 directory", ErrInsecurePermissions, dir)
	}
	return nil
}

func validateDir(dir string) error {
	info, err := os.Lstat(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect write policy directory: %w", err)
	}
	if !info.IsDir() || info.Mode().Perm() != 0o700 {
		return fmt.Errorf("%w: %s must be a 0700 directory", ErrInsecurePermissions, dir)
	}
	return nil
}

func (r *Registry) write(file registryFile) error {
	raw, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return fmt.Errorf("encode write policy registry: %w", err)
	}
	raw = append(raw, '\n')
	if len(raw) > maxRegistryBytes {
		return fmt.Errorf("%w: encoded registry exceeds %d bytes", ErrInvalid, maxRegistryBytes)
	}
	dir := filepath.Dir(r.path)
	tmp, err := os.CreateTemp(dir, ".write-policies-*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary write policy registry: %w", err)
	}
	tmpName := tmp.Name()
	// Best-effort cleanup is safe because tmpName is returned by CreateTemp in
	// the already validated policy directory.
	defer func() { _ = os.Remove(tmpName) }()
	if err := tmp.Chmod(0o600); err != nil {
		return errors.Join(fmt.Errorf("secure temporary write policy registry: %w", err), wrapIfError("close temporary write policy registry", tmp.Close()))
	}
	if _, err := tmp.Write(raw); err != nil {
		return errors.Join(fmt.Errorf("write temporary write policy registry: %w", err), wrapIfError("close temporary write policy registry", tmp.Close()))
	}
	if err := tmp.Sync(); err != nil {
		return errors.Join(fmt.Errorf("sync temporary write policy registry: %w", err), wrapIfError("close temporary write policy registry", tmp.Close()))
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temporary write policy registry: %w", err)
	}
	if err := os.Rename(tmpName, r.path); err != nil {
		return fmt.Errorf("replace write policy registry: %w", err)
	}
	openDir := r.openDir
	if openDir == nil {
		openDir = os.Open
	}
	directory, err := openDir(dir)
	if err != nil {
		return &CommitError{Err: fmt.Errorf("open write policy directory for sync: %w", err)}
	}
	syncErr := directory.Sync()
	closeErr := directory.Close()
	if syncErr != nil || closeErr != nil {
		return &CommitError{Err: errors.Join(
			wrapIfError("sync write policy directory", syncErr),
			wrapIfError("close write policy directory", closeErr),
		)}
	}
	return nil
}

func wrapIfError(operation string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", operation, err)
}
