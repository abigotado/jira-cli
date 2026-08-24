package auth

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/abigotado/jira-cli/internal/profile"
)

const compensationTimeout = 3 * time.Second

// ProfileRegistry is the metadata boundary used by login and logout.
type ProfileRegistry interface {
	WithProfileLock(ctx context.Context, name string, fn func() error) error
	Get(ctx context.Context, name string) (profile.Profile, error)
	Add(ctx context.Context, p profile.Profile) error
	Put(ctx context.Context, p profile.Profile) error
	Remove(ctx context.Context, name string) error
}

// Login saves the credential before publishing its profile metadata.
// Existing profiles require overwriteConfirmed to be true.
func Login(
	ctx context.Context,
	store CredentialStore,
	registry ProfileRegistry,
	p profile.Profile,
	credential Credential,
	overwriteConfirmed bool,
) error {
	if store == nil || registry == nil {
		return errors.New("login dependencies are required")
	}
	if err := p.Validate(); err != nil {
		return err
	}
	if err := credential.Validate(); err != nil {
		return err
	}

	return registry.WithProfileLock(ctx, p.Name, func() error {
		return loginLocked(ctx, store, registry, p, credential, overwriteConfirmed)
	})
}

func loginLocked(ctx context.Context, store CredentialStore, registry ProfileRegistry, p profile.Profile, credential Credential, overwriteConfirmed bool) error {
	previousProfile, registryLookupErr := registry.Get(ctx, p.Name)
	profileExists := registryLookupErr == nil
	if registryLookupErr != nil && !errors.Is(registryLookupErr, profile.ErrNotFound) {
		return fmt.Errorf("check existing profile: %w", registryLookupErr)
	}
	previousCredential, credentialLookupErr := store.Load(ctx, p.Name)
	credentialExists := credentialLookupErr == nil
	if credentialLookupErr != nil && !errors.Is(credentialLookupErr, ErrNotFound) {
		return fmt.Errorf("check existing credential: %w", credentialLookupErr)
	}
	if (profileExists || credentialExists) && !overwriteConfirmed {
		return fmt.Errorf("%w: %s", ErrOverwriteConfirmationRequired, p.Name)
	}
	if err := store.Save(ctx, p.Name, credential); err != nil {
		return fmt.Errorf("save credential: %w", err)
	}

	var registryErr error
	if profileExists && profilesEqual(previousProfile, p) {
		return nil
	}
	if profileExists {
		registryErr = registry.Put(ctx, p)
	} else {
		registryErr = registry.Add(ctx, p)
	}
	if registryErr == nil {
		return nil
	}
	if profile.WasCommitted(registryErr) {
		return fmt.Errorf("profile metadata committed with a durability warning: %w", registryErr)
	}
	compensationContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), compensationTimeout)
	defer cancel()
	rollbackErr := rollbackCredential(compensationContext, store, p.Name, previousCredential, credentialExists)
	if rollbackErr != nil {
		return errors.Join(fmt.Errorf("save profile metadata: %w", registryErr), fmt.Errorf("rollback credential: %w", rollbackErr))
	}
	return fmt.Errorf("save profile metadata: %w", registryErr)
}

func profilesEqual(left, right profile.Profile) bool {
	if left.Name != right.Name || left.Site != right.Site || left.Email != right.Email || left.TokenKind != right.TokenKind || left.CloudID != right.CloudID {
		return false
	}
	if left.ExpiresAt == nil || right.ExpiresAt == nil {
		return left.ExpiresAt == nil && right.ExpiresAt == nil
	}
	return left.ExpiresAt.Equal(*right.ExpiresAt)
}

// Logout deletes the credential before removing its public metadata.
func Logout(ctx context.Context, store CredentialStore, registry ProfileRegistry, profileName string) error {
	if store == nil || registry == nil {
		return errors.New("logout dependencies are required")
	}
	if err := profile.RequireName(profileName); err != nil {
		return err
	}
	return registry.WithProfileLock(ctx, profileName, func() error {
		previous, loadErr := store.Load(ctx, profileName)
		credentialExisted := loadErr == nil
		if loadErr != nil && !errors.Is(loadErr, ErrNotFound) {
			return fmt.Errorf("load credential for logout: %w", loadErr)
		}
		if err := store.Delete(ctx, profileName); err != nil {
			return fmt.Errorf("delete credential: %w", err)
		}
		registryErr := registry.Remove(ctx, profileName)
		if registryErr == nil {
			return nil
		}
		if errors.Is(registryErr, profile.ErrNotFound) {
			return nil
		}
		if profile.WasCommitted(registryErr) {
			return fmt.Errorf("profile metadata removal committed with a durability warning: %w", registryErr)
		}
		compensationContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), compensationTimeout)
		defer cancel()
		if credentialExisted {
			if restoreErr := store.Save(compensationContext, profileName, previous); restoreErr != nil {
				return errors.Join(
					fmt.Errorf("remove profile metadata: %w", registryErr),
					fmt.Errorf("restore credential after failed logout: %w", restoreErr),
				)
			}
		}
		return fmt.Errorf("remove profile metadata: %w", registryErr)
	})
}

func rollbackCredential(ctx context.Context, store CredentialStore, profileName string, previous Credential, restore bool) error {
	if restore {
		return store.Save(ctx, profileName, previous)
	}
	return store.Delete(ctx, profileName)
}
