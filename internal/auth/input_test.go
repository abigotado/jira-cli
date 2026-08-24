package auth

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestReadToken(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		wantToken string
		wantErr   error
	}{
		{name: "token without newline", input: "sentinel-token", wantToken: "sentinel-token"},
		{name: "single line ending", input: "sentinel-token\n", wantToken: "sentinel-token"},
		{name: "CRLF line ending", input: "sentinel-token\r\n", wantToken: "sentinel-token"},
		{name: "empty", wantErr: ErrInvalidToken},
		{name: "only newline", input: "\n", wantErr: ErrInvalidToken},
		{name: "multiline", input: "first\nsecond\n", wantErr: ErrInvalidToken},
		{name: "NUL", input: "before\x00after", wantErr: ErrInvalidToken},
		{name: "too long", input: strings.Repeat("a", MaxTokenBytes+1), wantErr: ErrInvalidToken},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ReadToken(strings.NewReader(tt.input))
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("ReadToken() error = %v, want %v", err, tt.wantErr)
			}
			if got.Token != tt.wantToken {
				t.Fatalf("ReadToken().Token = %q, want %q", got.Token, tt.wantToken)
			}
		})
	}
}

func TestCredentialFormattingAndJSONAreRedacted(t *testing.T) {
	credential := Credential{Token: "non-secret-sentinel"}
	for _, format := range []string{"%v", "%+v", "%#v", "%s", "%q"} {
		if got := fmt.Sprintf(format, credential); strings.Contains(got, credential.Token) {
			t.Fatalf("format %q exposed the credential", format)
		}
	}
	raw, err := json.Marshal(credential)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	if strings.Contains(string(raw), credential.Token) {
		t.Fatal("JSON exposed the credential")
	}
}
