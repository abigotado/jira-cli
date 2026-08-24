// Package auth manages Jira API tokens without exposing them to configuration,
// logs, or process output.
package auth

import (
	"context"
	"errors"
	"fmt"
	"io"
)

const (
	// KeychainService is the generic-password service used by jira-cli.
	KeychainService = "jira-cli"
	// MaxTokenBytes bounds interactive and piped token input.
	MaxTokenBytes = 8192
)

var (
	// ErrNotFound means no token exists for the exact profile account.
	ErrNotFound = errors.New("credential not found")
	// ErrUnsupported means this build has no supported native credential store.
	ErrUnsupported = errors.New("native credential store is unsupported on this platform")
	// ErrInteractionNotAllowed means the Keychain operation would require UI.
	ErrInteractionNotAllowed = errors.New("credential store requires user interaction")
	// ErrInvalidToken means credential input is empty or malformed.
	ErrInvalidToken = errors.New("invalid token")
	// ErrOverwriteConfirmationRequired prevents an implicit credential overwrite.
	ErrOverwriteConfirmationRequired = errors.New("credential overwrite requires confirmation")
)

// Credential contains one Jira API token.
//
// It intentionally implements neither String nor Format: callers must never
// render this value into diagnostics or machine output.
type Credential struct {
	Token string `json:"-"`
}

// Format ensures accidental fmt formatting never renders the token.
func (Credential) Format(state fmt.State, _ rune) {
	// fmt.Formatter cannot return a write error to its caller.
	_, _ = io.WriteString(state, "<redacted>")
}

// Validate checks a credential without including its value in any error.
func (c Credential) Validate() error {
	return ValidateToken(c.Token)
}

// CredentialStore persists one token under an exact profile account.
type CredentialStore interface {
	Load(ctx context.Context, profileName string) (Credential, error)
	Save(ctx context.Context, profileName string, credential Credential) error
	Delete(ctx context.Context, profileName string) error
}

// StatusError wraps a platform status code without credential material.
type StatusError struct {
	Operation string
	Status    int64
}

// Error returns a safe diagnostic containing only operation and status code.
func (e *StatusError) Error() string {
	return fmt.Sprintf("keychain %s failed with OSStatus %d", e.Operation, e.Status)
}

// ValidateToken rejects unsafe or ambiguous token input.
func ValidateToken(token string) error {
	if token == "" {
		return fmt.Errorf("%w: token is empty", ErrInvalidToken)
	}
	if len(token) > MaxTokenBytes {
		return fmt.Errorf("%w: token exceeds %d bytes", ErrInvalidToken, MaxTokenBytes)
	}
	for _, ch := range token {
		if ch == '\x00' || ch == '\r' || ch == '\n' {
			return fmt.Errorf("%w: token contains a prohibited control character", ErrInvalidToken)
		}
	}
	return nil
}
