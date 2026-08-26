package errx

import (
	"context"
	"errors"
	"fmt"
	"strings"
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

func TestCreateFieldsUnsupportedIsDeterministicBoundedAndActionable(t *testing.T) {
	const (
		genericHint     = "choose or configure a standard issue type so unsupported fields are optional or have Jira defaults"
		descriptionHint = "re-run with --description to supply the required description field"
		labelsHint      = "re-run with at least one --label to supply the required labels field"
		bothHint        = "re-run with --description and at least one --label to supply the required description and labels fields"
	)
	maxLengthID := strings.Repeat("a", 255)
	tests := []struct {
		name               string
		fieldIDs           []string
		provideDescription bool
		provideLabels      bool
		wantMessage        string
		wantHint           string
		absent             []string
	}{
		{
			name:     "description alone offers the bounded flag",
			fieldIDs: []string{"description"}, provideDescription: true,
			wantMessage: "Jira create metadata contains unsupported fields: description",
			wantHint:    descriptionHint,
		},
		{
			name:     "labels alone offers the repeated bounded flag",
			fieldIDs: []string{"labels"}, provideLabels: true,
			wantMessage: "Jira create metadata contains unsupported fields: labels",
			wantHint:    labelsHint,
		},
		{
			name:     "description and labels offer both bounded flags",
			fieldIDs: []string{"labels", "description"}, provideDescription: true, provideLabels: true,
			wantMessage: "Jira create metadata contains unsupported fields: description, labels",
			wantHint:    bothHint,
		},
		{
			name:     "field IDs are sorted and deduplicated",
			fieldIDs: []string{"zeta", "alpha", "zeta"}, provideDescription: false,
			wantMessage: "Jira create metadata contains unsupported fields: alpha, zeta",
			wantHint:    genericHint,
		},
		{
			name: "only eight safe field IDs are reported",
			fieldIDs: []string{
				"field_09", "field_08", "field_07", "field_06", "field_05", "field_04", "field_03", "field_02", "field_01", "field_00",
				"BAD\nRAW_METADATA_SENTINEL", strings.Repeat("x", 256), "",
			},
			wantMessage: "Jira create metadata contains unsupported fields: field_00, field_01, field_02, field_03, field_04, field_05, field_06, field_07 (4 field IDs omitted)",
			wantHint:    genericHint,
			absent:      []string{"field_08", "field_09", "RAW_METADATA_SENTINEL", strings.Repeat("x", 32)},
		},
		{
			name:        "255 byte ID is accepted and 256 byte ID is omitted",
			fieldIDs:    []string{maxLengthID, strings.Repeat("b", 256)},
			wantMessage: "Jira create metadata contains unsupported fields: " + maxLengthID + " (1 field IDs omitted)",
			wantHint:    genericHint,
			absent:      []string{strings.Repeat("b", 32)},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := CreateFieldsUnsupported(test.fieldIDs, test.provideDescription, test.provideLabels)
			if err.Code != CodeUsage || ExitCode(err) != CodeUsage || err.Reason != "CREATE_FIELDS_UNSUPPORTED" {
				t.Fatalf("contract = code:%d exit:%d reason:%q", err.Code, ExitCode(err), err.Reason)
			}
			if err.Message != test.wantMessage {
				t.Fatalf("message = %q, want %q", err.Message, test.wantMessage)
			}
			if err.Hint != test.wantHint {
				t.Fatalf("hint = %q, want %q", err.Hint, test.wantHint)
			}
			for _, value := range test.absent {
				if strings.Contains(err.Message+err.Hint, value) {
					t.Fatalf("bounded error leaked %q: %#v", value, err)
				}
			}
		})
	}
}

func TestLocalLockBusyHasStableRetryContract(t *testing.T) {
	err := LocalLockBusy()
	if err.Code != CodeRetryable || ExitCode(err) != CodeRetryable || err.Reason != "LOCAL_LOCK_BUSY" {
		t.Fatalf("contract = code:%d exit:%d reason:%q", err.Code, ExitCode(err), err.Reason)
	}
	if err.RetryAfter != 0 || !strings.Contains(err.Hint, "retry") {
		t.Fatalf("retry guidance = %#v", err)
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
