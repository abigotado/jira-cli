package profile

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"

	"github.com/abigotado/jira-cli/internal/lockfile"
)

const (
	registryFilename = "profiles.json"
	maxRegistryBytes = 1 << 20
	maxProfiles      = 1024
)

// Registry persists profiles in a strict, non-secret JSON file.
type Registry struct {
	path    string
	openDir func(string) (*os.File, error)
}

// NewRegistry creates a registry at an explicit path.
func NewRegistry(path string) *Registry {
	return &Registry{path: path}
}

// NewDefaultRegistry creates the default user-scoped registry.
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

// WithProfileLock serializes a complete credential+metadata transaction for
// one profile across jira-cli processes. The lock is distinct from the
// registry file's short read-modify-write lock.
func (r *Registry) WithProfileLock(ctx context.Context, name string, fn func() error) error {
	if err := RequireName(name); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if r == nil || r.path == "" {
		return errors.New("profile registry path is empty")
	}
	return lockfile.With(r.path+".profile-"+name, func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		return fn()
	})
}

// List returns all registered profiles sorted by name.
func (r *Registry) List(ctx context.Context) ([]Profile, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	profiles, err := r.read()
	if err != nil {
		return nil, err
	}
	sort.Slice(profiles, func(i, j int) bool { return profiles[i].Name < profiles[j].Name })
	return profiles, nil
}

// Get returns one explicitly named profile.
func (r *Registry) Get(ctx context.Context, name string) (Profile, error) {
	if err := RequireName(name); err != nil {
		return Profile{}, err
	}
	profiles, err := r.List(ctx)
	if err != nil {
		return Profile{}, err
	}
	for _, candidate := range profiles {
		if candidate.Name == name {
			return candidate, nil
		}
	}
	return Profile{}, fmt.Errorf("%w: %s", ErrNotFound, name)
}

// Add registers p without overwriting an existing profile.
func (r *Registry) Add(ctx context.Context, p Profile) error {
	if err := p.Validate(); err != nil {
		return err
	}
	return r.mutate(ctx, func(profiles []Profile) ([]Profile, error) {
		for _, candidate := range profiles {
			if candidate.Name == p.Name {
				return nil, fmt.Errorf("%w: %s", ErrAlreadyExists, p.Name)
			}
		}
		return append(profiles, p), nil
	})
}

// Put registers p, replacing metadata for the same profile name.
func (r *Registry) Put(ctx context.Context, p Profile) error {
	if err := p.Validate(); err != nil {
		return err
	}
	return r.mutate(ctx, func(profiles []Profile) ([]Profile, error) {
		for i := range profiles {
			if profiles[i].Name == p.Name {
				profiles[i] = p
				return profiles, nil
			}
		}
		return append(profiles, p), nil
	})
}

// Remove deletes one profile from the registry.
func (r *Registry) Remove(ctx context.Context, name string) error {
	if err := RequireName(name); err != nil {
		return err
	}
	return r.mutate(ctx, func(profiles []Profile) ([]Profile, error) {
		for i, candidate := range profiles {
			if candidate.Name == name {
				return append(profiles[:i:i], profiles[i+1:]...), nil
			}
		}
		return nil, fmt.Errorf("%w: %s", ErrNotFound, name)
	})
}

func (r *Registry) mutate(ctx context.Context, change func([]Profile) ([]Profile, error)) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if r == nil || r.path == "" {
		return errors.New("profile registry path is empty")
	}
	if err := ensureRegistryDir(filepath.Dir(r.path)); err != nil {
		return err
	}
	return lockfile.With(r.path, func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		profiles, err := r.read()
		if err != nil {
			return err
		}
		profiles, err = change(profiles)
		if err != nil {
			return err
		}
		if len(profiles) > maxProfiles {
			return fmt.Errorf("profile registry cannot contain more than %d profiles", maxProfiles)
		}
		for _, p := range profiles {
			if err := p.Validate(); err != nil {
				return fmt.Errorf("%w: %v", ErrCorruptRegistry, err)
			}
		}
		sort.Slice(profiles, func(i, j int) bool { return profiles[i].Name < profiles[j].Name })
		return r.write(profiles)
	})
}

func (r *Registry) read() ([]Profile, error) {
	if r == nil || r.path == "" {
		return nil, errors.New("profile registry path is empty")
	}
	if err := validateRegistryDir(filepath.Dir(r.path)); err != nil {
		return nil, err
	}
	info, err := os.Lstat(r.path)
	if errors.Is(err, os.ErrNotExist) {
		return []Profile{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("inspect profile registry: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		return nil, fmt.Errorf("%w: %s must be a regular 0600 file", ErrInsecurePermissions, r.path)
	}
	if info.Size() > maxRegistryBytes {
		return nil, fmt.Errorf("%w: file exceeds %d bytes", ErrCorruptRegistry, maxRegistryBytes)
	}
	raw, err := os.ReadFile(r.path)
	if err != nil {
		return nil, fmt.Errorf("read profile registry: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var profiles []Profile
	if err := decoder.Decode(&profiles); err != nil {
		return nil, fmt.Errorf("%w: decode: %v", ErrCorruptRegistry, err)
	}
	if profiles == nil {
		return nil, fmt.Errorf("%w: top-level JSON value must be an array", ErrCorruptRegistry)
	}
	if len(profiles) > maxProfiles {
		return nil, fmt.Errorf("%w: more than %d profiles", ErrCorruptRegistry, maxProfiles)
	}
	if err := requireJSONEOF(decoder); err != nil {
		return nil, err
	}
	seen := make(map[string]struct{}, len(profiles))
	for _, p := range profiles {
		if err := p.Validate(); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrCorruptRegistry, err)
		}
		if _, exists := seen[p.Name]; exists {
			return nil, fmt.Errorf("%w: duplicate profile %q", ErrCorruptRegistry, p.Name)
		}
		seen[p.Name] = struct{}{}
	}
	return profiles, nil
}

func requireJSONEOF(decoder *json.Decoder) error {
	var extra json.RawMessage
	err := decoder.Decode(&extra)
	if !errors.Is(err, io.EOF) {
		if err == nil {
			return fmt.Errorf("%w: multiple JSON values", ErrCorruptRegistry)
		}
		return fmt.Errorf("%w: trailing data: %v", ErrCorruptRegistry, err)
	}
	return nil
}

func ensureRegistryDir(dir string) error {
	info, err := os.Lstat(dir)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("create profile registry directory: %w", err)
		}
		if err := os.Chmod(dir, 0o700); err != nil {
			return fmt.Errorf("secure profile registry directory: %w", err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect profile registry directory: %w", err)
	}
	if !info.IsDir() || info.Mode().Perm() != 0o700 {
		return fmt.Errorf("%w: %s must be a 0700 directory", ErrInsecurePermissions, dir)
	}
	return nil
}

func validateRegistryDir(dir string) error {
	info, err := os.Lstat(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect profile registry directory: %w", err)
	}
	if !info.IsDir() || info.Mode().Perm() != 0o700 {
		return fmt.Errorf("%w: %s must be a 0700 directory", ErrInsecurePermissions, dir)
	}
	return nil
}

func (r *Registry) write(profiles []Profile) error {
	dir := filepath.Dir(r.path)
	raw, err := json.MarshalIndent(profiles, "", "  ")
	if err != nil {
		return fmt.Errorf("encode profile registry: %w", err)
	}
	raw = append(raw, '\n')
	tmp, err := os.CreateTemp(dir, ".profiles-*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary profile registry: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		// A successful rename makes this a harmless ErrNotExist. On failure it
		// prevents a partial registry from being mistaken for user data.
		_ = os.Remove(tmpName)
	}()
	if err := tmp.Chmod(0o600); err != nil {
		closeErr := tmp.Close()
		return errors.Join(fmt.Errorf("secure temporary profile registry: %w", err), closeErr)
	}
	if _, err := tmp.Write(raw); err != nil {
		closeErr := tmp.Close()
		return errors.Join(fmt.Errorf("write temporary profile registry: %w", err), closeErr)
	}
	if err := tmp.Sync(); err != nil {
		closeErr := tmp.Close()
		return errors.Join(fmt.Errorf("sync temporary profile registry: %w", err), closeErr)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temporary profile registry: %w", err)
	}
	if err := os.Rename(tmpName, r.path); err != nil {
		return fmt.Errorf("replace profile registry: %w", err)
	}
	openDir := r.openDir
	if openDir == nil {
		openDir = os.Open
	}
	directory, err := openDir(dir)
	if err != nil {
		return &CommitError{Err: fmt.Errorf("open profile registry directory for sync: %w", err)}
	}
	syncErr := directory.Sync()
	closeErr := directory.Close()
	if syncErr != nil || closeErr != nil {
		return &CommitError{Err: errors.Join(
			wrapIfError("sync profile registry directory", syncErr),
			wrapIfError("close profile registry directory", closeErr),
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
