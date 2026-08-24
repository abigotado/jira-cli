package profile

import (
	"errors"
	"strings"
	"testing"
)

func TestProfileValidate(t *testing.T) {
	tests := []struct {
		name    string
		profile Profile
		wantErr bool
	}{
		{
			name:    "classic profile is valid",
			profile: Profile{Name: "work", Site: "https://tenant.atlassian.net", Email: "user@example.com", TokenKind: TokenKindClassic},
		},
		{
			name:    "scoped profile requires a bounded cloud id",
			profile: Profile{Name: "work", Site: "https://tenant.atlassian.net", Email: "user@example.com", TokenKind: TokenKindScoped, CloudID: "123e4567-e89b-12d3-a456-426614174000"},
		},
		{
			name:    "classic profile rejects cloud id",
			profile: Profile{Name: "work", Site: "https://tenant.atlassian.net", Email: "user@example.com", TokenKind: TokenKindClassic, CloudID: "cloud"},
			wantErr: true,
		},
		{
			name:    "scoped profile rejects missing cloud id",
			profile: Profile{Name: "work", Site: "https://tenant.atlassian.net", Email: "user@example.com", TokenKind: TokenKindScoped},
			wantErr: true,
		},
		{
			name:    "cloud id rejects punctuation",
			profile: Profile{Name: "work", Site: "https://tenant.atlassian.net", Email: "user@example.com", TokenKind: TokenKindScoped, CloudID: "cloud/id"},
			wantErr: true,
		},
		{
			name:    "cloud id rejects excessive length",
			profile: Profile{Name: "work", Site: "https://tenant.atlassian.net", Email: "user@example.com", TokenKind: TokenKindScoped, CloudID: strings.Repeat("a", maxCloudIDLength+1)},
			wantErr: true,
		},
		{
			name:    "email rejects display name",
			profile: Profile{Name: "work", Site: "https://tenant.atlassian.net", Email: "User <user@example.com>", TokenKind: TokenKindClassic},
			wantErr: true,
		},
		{
			name:    "email rejects invalid domain label",
			profile: Profile{Name: "work", Site: "https://tenant.atlassian.net", Email: "user@invalid_domain.example", TokenKind: TokenKindClassic},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.profile.Validate()
			if (err != nil) != tt.wantErr {
				t.Fatalf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
			if err != nil && !errors.Is(err, ErrInvalidProfile) {
				t.Fatalf("Validate() error = %v, want ErrInvalidProfile", err)
			}
		})
	}
}

func TestProfileValidateSite(t *testing.T) {
	tests := []struct {
		name  string
		site  string
		valid bool
	}{
		{name: "exact tenant URL", site: "https://example.atlassian.net", valid: true},
		{name: "http", site: "http://tenant.atlassian.net"},
		{name: "userinfo", site: "https://user@tenant.atlassian.net"},
		{name: "port", site: "https://tenant.atlassian.net:443"},
		{name: "empty port", site: "https://tenant.atlassian.net:"},
		{name: "path", site: "https://tenant.atlassian.net/rest"},
		{name: "trailing slash is a path", site: "https://tenant.atlassian.net/"},
		{name: "query", site: "https://tenant.atlassian.net?x=1"},
		{name: "empty query marker", site: "https://tenant.atlassian.net?"},
		{name: "fragment", site: "https://tenant.atlassian.net#x"},
		{name: "empty fragment marker", site: "https://tenant.atlassian.net#"},
		{name: "missing tenant", site: "https://atlassian.net"},
		{name: "nested tenant", site: "https://one.two.atlassian.net"},
		{name: "lookalike suffix", site: "https://tenant.atlassian.net.example.com"},
		{name: "localhost", site: "https://localhost"},
		{name: "IP address", site: "https://127.0.0.1"},
		{name: "uppercase host", site: "https://Tenant.atlassian.net"},
		{name: "invalid tenant label", site: "https://-tenant.atlassian.net"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := Profile{Name: "work", Site: tt.site, Email: "user@example.com", TokenKind: TokenKindClassic}
			err := p.Validate()
			if (err == nil) != tt.valid {
				t.Fatalf("Validate() error = %v, valid %v", err, tt.valid)
			}
		})
	}
}

func TestRequireName(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		wantErr error
	}{
		{name: "explicit valid name", value: "work"},
		{name: "missing profile", wantErr: ErrProfileRequired},
		{name: "invalid profile", value: "work account", wantErr: ErrInvalidProfile},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := RequireName(tt.value)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("RequireName() error = %v, want %v", err, tt.wantErr)
			}
		})
	}
}
