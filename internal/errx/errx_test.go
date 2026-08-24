package errx

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestExitCode(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want Code
	}{
		{"nil is ok", nil, CodeOK},
		{"internal", Internal("boom"), CodeInternal},
		{"usage", Usage("bad flag"), CodeUsage},
		{"not found", NotFound("issue", "WL-1", nil), CodeNotFound},
		{"ambiguous", Ambiguous("project", "W", nil), CodeAmbiguous},
		{"auth", Auth("TOKEN_REJECTED", "bad token"), CodeAuth},
		{"retryable", Retryable("RATE_LIMITED", 0, "slow down"), CodeRetryable},
		{"confirm", ConfirmRequired("transition issue"), CodeConfirm},
		{"permission", Permission("SCOPE_DENIED", "missing scope"), CodePermission},
		{"conflict", Conflict("STALE_ISSUE", "issue changed"), CodeConflict},
		{"untyped is internal", errors.New("raw"), CodeInternal},
		{"wrapped typed keeps code", fmt.Errorf("request: %w", Permission("DENIED", "no")), CodePermission},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ExitCode(tt.err); got != tt.want {
				t.Errorf("ExitCode() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestExitCodeValuesAreFrozen(t *testing.T) {
	frozen := map[Code]string{
		0: "OK", 1: "INTERNAL", 2: "USAGE", 3: "NOT_FOUND", 4: "AMBIGUOUS",
		5: "AUTH", 6: "RETRYABLE", 7: "CONFIRMATION_REQUIRED",
		8: "PERMISSION_DENIED", 9: "CONFLICT",
	}
	got := Codes()
	if len(got) != len(frozen) {
		t.Fatalf("Codes() returned %d entries, want %d", len(got), len(frozen))
	}
	for _, info := range got {
		if want := frozen[info.Code]; info.Name != want {
			t.Errorf("code %d = %q, want %q", info.Code, info.Name, want)
		}
		if info.NextMove == "" {
			t.Errorf("code %d has no recovery action", info.Code)
		}
	}
}

func TestCodesReturnsCopy(t *testing.T) {
	first := Codes()
	first[0].Name = "MUTATED"
	if Codes()[0].Name == "MUTATED" {
		t.Fatal("Codes exposed mutable contract state")
	}
}

func TestJiraSpecificHints(t *testing.T) {
	tests := []struct {
		name     string
		err      *Error
		wantCode Code
		wantHint string
	}{
		{"profile", ProfileRequired(), CodeUsage, "--profile"},
		{"issue not found", NotFound("issue", "WL-404", nil), CodeNotFound, "--profile"},
		{"permission", Permission("", "forbidden"), CodePermission, "permission"},
		{"conflict", Conflict("", "conflict"), CodeConflict, "re-read"},
		{"auth", Auth("TOKEN_EXPIRED", "expired"), CodeAuth, "rotate"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.err.Code != tt.wantCode {
				t.Errorf("code = %d, want %d", tt.err.Code, tt.wantCode)
			}
			if !contains(tt.err.Hint, tt.wantHint) {
				t.Errorf("hint %q does not contain %q", tt.err.Hint, tt.wantHint)
			}
		})
	}
}

func TestLookupErrorsCarryRecoveryData(t *testing.T) {
	candidates := []Candidate{{ID: "10000", Name: "Work", Kind: "project"}}
	notFound := NotFound("project", "Wrok", candidates)
	if len(notFound.DidYouMean) != 1 || !contains(notFound.Hint, "did_you_mean") {
		t.Errorf("not found recovery = %+v", notFound)
	}
	ambiguous := Ambiguous("project", "W", candidates)
	if len(ambiguous.Candidates) != 1 || !contains(ambiguous.Hint, "--project-id") {
		t.Errorf("ambiguous recovery = %+v", ambiguous)
	}
}

func TestRetryableCarriesRetryAfter(t *testing.T) {
	err := Retryable("RATE_LIMITED", 3*time.Second, "slow down")
	if err.RetryAfter != 3*time.Second || !contains(err.Hint, "3s") {
		t.Errorf("retryable error = %+v", err)
	}
}

func TestTranslateContextErrors(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		wantCode   Code
		wantReason string
	}{
		{"deadline", context.DeadlineExceeded, CodeRetryable, "TIMEOUT"},
		{"wrapped deadline", fmt.Errorf("get: %w", context.DeadlineExceeded), CodeRetryable, "TIMEOUT"},
		{"canceled", context.Canceled, CodeRetryable, "CANCELED"},
		{"nil", nil, CodeOK, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Translate(tt.err)
			if ExitCode(got) != tt.wantCode {
				t.Errorf("exit code = %d, want %d", ExitCode(got), tt.wantCode)
			}
			if tt.wantReason == "" {
				return
			}
			var typed *Error
			if !errors.As(got, &typed) || typed.Reason != tt.wantReason {
				t.Errorf("Translate() = %#v, want reason %s", got, tt.wantReason)
			}
			if !errors.Is(got, tt.err) {
				t.Error("Translate dropped the cause")
			}
		})
	}
}

func TestTranslateLeavesTypedAndUnrelatedErrorsAlone(t *testing.T) {
	typed := Auth("TOKEN_REJECTED", "bad")
	if got := Translate(typed); got != error(typed) {
		t.Errorf("typed error changed: %v", got)
	}
	raw := errors.New("other")
	if got := Translate(raw); got != raw {
		t.Errorf("unrelated error changed: %v", got)
	}
}

func TestErrorUnwrap(t *testing.T) {
	cause := errors.New("dial failed")
	err := Retryable("NETWORK", 0, "request failed").Wrap(cause)
	if !errors.Is(err, cause) {
		t.Error("wrapped cause is not visible")
	}
}

func contains(s, substr string) bool {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
