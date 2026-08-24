// Package profile stores non-secret Jira Cloud connection metadata.
//
// Profile selection is deliberately explicit for every invocation. This
// package has no active or default profile state.
package profile

import (
	"errors"
	"fmt"
	"net"
	"net/mail"
	"net/url"
	"regexp"
	"strings"
	"time"
)

const (
	maxNameLength    = 64
	maxCloudIDLength = 128
)

var (
	// ErrProfileRequired means a caller did not explicitly select a profile.
	ErrProfileRequired = errors.New("a profile is required for every invocation")
	// ErrInvalidProfile means profile metadata failed validation.
	ErrInvalidProfile = errors.New("invalid profile")
	// ErrNotFound means the requested profile is not registered.
	ErrNotFound = errors.New("profile not found")
	// ErrAlreadyExists means Add would overwrite an existing profile.
	ErrAlreadyExists = errors.New("profile already exists")
	// ErrCorruptRegistry means the registry cannot be decoded or validated.
	ErrCorruptRegistry = errors.New("profile registry is corrupt")
	// ErrInsecurePermissions means registry metadata is accessible too broadly.
	ErrInsecurePermissions = errors.New("profile registry has insecure permissions")

	namePattern    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
	cloudIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]*$`)
)

// CommitError reports that a registry mutation reached its atomic rename but
// a subsequent durability step failed. Callers must treat the requested
// metadata state as committed and must not compensate it as a pre-commit
// failure.
type CommitError struct {
	Err error
}

func (e *CommitError) Error() string {
	return fmt.Sprintf("profile metadata committed but durability check failed: %v", e.Err)
}

func (e *CommitError) Unwrap() error {
	return e.Err
}

// WasCommitted identifies a post-rename registry failure.
func WasCommitted(err error) bool {
	var committed *CommitError
	return errors.As(err, &committed)
}

// TokenKind identifies the Jira API token authentication scheme.
type TokenKind string

const (
	// TokenKindClassic is a standard Atlassian API token used with the site URL.
	TokenKindClassic TokenKind = "classic"
	// TokenKindScoped is a scoped Atlassian API token used with a cloud ID.
	TokenKindScoped TokenKind = "scoped"
)

// Profile contains only non-secret Jira Cloud connection metadata.
type Profile struct {
	Name      string     `json:"name"`
	Site      string     `json:"site"`
	Email     string     `json:"email"`
	TokenKind TokenKind  `json:"token_kind"`
	CloudID   string     `json:"cloud_id,omitempty"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

// RequireName validates the mandatory per-invocation profile selection.
func RequireName(name string) error {
	if name == "" {
		return ErrProfileRequired
	}
	return ValidateName(name)
}

// ValidateName validates a profile name for exact use as a Keychain account.
func ValidateName(name string) error {
	if name == "" || len(name) > maxNameLength || !namePattern.MatchString(name) {
		return fmt.Errorf("%w: name must be 1-%d ASCII letters, digits, dot, dash, or underscore and start with a letter or digit", ErrInvalidProfile, maxNameLength)
	}
	return nil
}

// Validate checks all profile metadata.
func (p Profile) Validate() error {
	if err := ValidateName(p.Name); err != nil {
		return err
	}
	if err := validateSite(p.Site); err != nil {
		return err
	}
	if err := validateEmail(p.Email); err != nil {
		return err
	}
	switch p.TokenKind {
	case TokenKindClassic:
		if p.CloudID != "" {
			return fmt.Errorf("%w: cloud_id is only valid for scoped tokens", ErrInvalidProfile)
		}
	case TokenKindScoped:
		if err := validateCloudID(p.CloudID); err != nil {
			return err
		}
	default:
		return fmt.Errorf("%w: token_kind must be %q or %q", ErrInvalidProfile, TokenKindClassic, TokenKindScoped)
	}
	if p.ExpiresAt != nil && p.ExpiresAt.IsZero() {
		return fmt.Errorf("%w: expires_at must be a valid timestamp", ErrInvalidProfile)
	}
	return nil
}

func validateSite(raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("%w: parse site: %v", ErrInvalidProfile, err)
	}
	if parsed.Scheme != "https" || parsed.Opaque != "" || parsed.User != nil || parsed.Port() != "" || strings.Contains(parsed.Host, ":") || parsed.Path != "" || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || raw != "https://"+parsed.Host {
		return fmt.Errorf("%w: site must be exactly https://<tenant>.atlassian.net with no credentials, port, path, query, or fragment", ErrInvalidProfile)
	}
	host := parsed.Hostname()
	const suffix = ".atlassian.net"
	if host == "" || host != strings.ToLower(host) || net.ParseIP(host) != nil || !strings.HasSuffix(host, suffix) {
		return fmt.Errorf("%w: site host must be a lowercase Atlassian Cloud tenant", ErrInvalidProfile)
	}
	tenant := strings.TrimSuffix(host, suffix)
	if tenant == "" || strings.Contains(tenant, ".") || !validDNSLabel(tenant) {
		return fmt.Errorf("%w: site must contain exactly one valid tenant label before atlassian.net", ErrInvalidProfile)
	}
	return nil
}

func validDNSLabel(label string) bool {
	if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
		return false
	}
	for _, ch := range label {
		if (ch < 'a' || ch > 'z') && (ch < '0' || ch > '9') && ch != '-' {
			return false
		}
	}
	return true
}

func validateEmail(email string) error {
	if email == "" || len(email) > 254 || strings.TrimSpace(email) != email || strings.ContainsAny(email, "\x00\r\n\t ") || strings.Count(email, "@") != 1 {
		return fmt.Errorf("%w: email must be a plain address without whitespace", ErrInvalidProfile)
	}
	address, err := mail.ParseAddress(email)
	if err != nil || address.Address != email {
		return fmt.Errorf("%w: email must be a plain RFC 5322 address", ErrInvalidProfile)
	}
	parts := strings.SplitN(email, "@", 2)
	if parts[0] == "" || !validEmailDomain(parts[1]) {
		return fmt.Errorf("%w: email must contain a local part and domain", ErrInvalidProfile)
	}
	return nil
}

func validEmailDomain(domain string) bool {
	labels := strings.Split(strings.ToLower(domain), ".")
	if len(labels) < 2 {
		return false
	}
	for _, label := range labels {
		if !validDNSLabel(label) {
			return false
		}
	}
	return true
}

func validateCloudID(cloudID string) error {
	if cloudID == "" || len(cloudID) > maxCloudIDLength || !cloudIDPattern.MatchString(cloudID) {
		return fmt.Errorf("%w: cloud_id must be 1-%d ASCII letters, digits, dash, or underscore", ErrInvalidProfile, maxCloudIDLength)
	}
	return nil
}
