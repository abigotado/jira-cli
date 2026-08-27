package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/abigotado/jira-cli/internal/auth"
	"github.com/abigotado/jira-cli/internal/errx"
	"github.com/abigotado/jira-cli/internal/jira"
	"github.com/abigotado/jira-cli/internal/lockfile"
	"github.com/abigotado/jira-cli/internal/output"
	"github.com/abigotado/jira-cli/internal/profile"
	"github.com/abigotado/jira-cli/internal/writepolicy"
)

func TestAuthAllowProjectsCommandsStayLocalAndCanonical(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		wantCode   errx.Code
		wantReason string
		wantSets   int
		wantClears int
		wantGet    int
		wantPolicy []string
	}{
		{name: "show reads policy without confirmation", args: []string{"--profile", "work", "auth", "allow-projects", "show"}, wantCode: errx.CodeOK, wantGet: 1},
		{name: "set requires confirmation", args: []string{"--profile", "work", "auth", "allow-projects", "set", "--project", "WL"}, wantCode: errx.CodeConfirm, wantReason: "CONFIRMATION_REQUIRED"},
		{name: "set canonicalizes and persists", args: []string{"--profile", "work", "--yes", "auth", "allow-projects", "set", "--project", " wl ", "--project", "FL", "--project", "wl"}, wantCode: errx.CodeOK, wantSets: 1, wantPolicy: []string{"FL", "WL"}},
		{name: "set dry run canonicalizes without persistence or confirmation", args: []string{"--profile", "work", "--dry-run", "auth", "allow-projects", "set", "--project", "fl", "--project", "WL"}, wantCode: errx.CodeOK, wantPolicy: []string{"FL", "WL"}},
		{name: "clear requires confirmation", args: []string{"--profile", "work", "auth", "allow-projects", "clear"}, wantCode: errx.CodeConfirm, wantReason: "CONFIRMATION_REQUIRED"},
		{name: "clear persists", args: []string{"--profile", "work", "--yes", "auth", "allow-projects", "clear"}, wantCode: errx.CodeOK, wantClears: 1, wantPolicy: []string{}},
		{name: "clear dry run does not persist", args: []string{"--profile", "work", "--dry-run", "auth", "allow-projects", "clear"}, wantCode: errx.CodeOK, wantPolicy: []string{}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := &fakeStore{credential: auth.Credential{Token: "KEYCHAIN_TOKEN_SENTINEL"}}
			app, stdout, stderr := testApp(store, &fakeJira{})
			policies := app.policies.(*fakePolicyRegistry)
			code := app.Run(context.Background(), app.NewRootCommand(), append([]string{"--output", "json"}, test.args...))
			if code != test.wantCode {
				t.Fatalf("code = %d, want %d, stdout=%s stderr=%s", code, test.wantCode, stdout.String(), stderr.String())
			}
			if test.wantReason != "" {
				assertErrorReason(t, stdout.Bytes(), test.wantReason)
			}
			if store.loads != 0 || store.saves != 0 || store.deletes != 0 {
				t.Fatalf("allow-projects touched credential store: %#v", store)
			}
			if policies.sets != test.wantSets || policies.clears != test.wantClears || policies.gets != test.wantGet {
				t.Fatalf("policy calls = set:%d clear:%d get:%d", policies.sets, policies.clears, policies.gets)
			}
			if test.wantPolicy != nil && code == errx.CodeOK {
				var envelope output.Envelope
				if err := json.Unmarshal(stdout.Bytes(), &envelope); err != nil {
					t.Fatal(err)
				}
				data := envelope.Data.(map[string]any)
				gotRaw, ok := data["projects"].([]any)
				if !ok {
					t.Fatalf("projects = %#v, want JSON array", data["projects"])
				}
				got := make([]string, len(gotRaw))
				for index := range gotRaw {
					got[index] = gotRaw[index].(string)
				}
				if !reflect.DeepEqual(got, test.wantPolicy) {
					t.Fatalf("projects = %#v, want %#v", got, test.wantPolicy)
				}
			}
			if strings.Contains(stdout.String()+stderr.String(), store.credential.Token) {
				t.Fatal("credential leaked")
			}
		})
	}
}

func TestPostLockReleaseFailureEmitsOnlyOneFailureEnvelope(t *testing.T) {
	releaseErr := errors.New("POST_LOCK_RELEASE_SENTINEL")
	tests := []struct {
		name       string
		args       []string
		client     *fakeJira
		wantCode   errx.Code
		wantReason string
		configure  func(*fakeRegistry, *fakePolicyRegistry)
		assertDone func(*testing.T, *fakeJira, *fakePolicyRegistry)
	}{
		{
			name:       "remote mutation profile lock release fails",
			args:       []string{"--yes", "comments", "add", "--issue", "WL-1", "--body", "safe"},
			client:     &fakeJira{issue: jira.Issue{ID: "10001", Key: "WL-1"}, comment: jira.Comment{ID: "900"}},
			wantCode:   errx.CodeConflict,
			wantReason: "WRITE_OUTCOME_UNKNOWN",
			configure: func(registry *fakeRegistry, _ *fakePolicyRegistry) {
				registry.postLockErr = releaseErr
			},
			assertDone: func(t *testing.T, client *fakeJira, _ *fakePolicyRegistry) {
				if client.commentCalls != 1 {
					t.Fatalf("comment calls = %d, want completed callback", client.commentCalls)
				}
			},
		},
		{
			name:       "remote mutation policy lock release fails",
			args:       []string{"--yes", "comments", "add", "--issue", "WL-1", "--body", "safe"},
			client:     &fakeJira{issue: jira.Issue{ID: "10001", Key: "WL-1"}, comment: jira.Comment{ID: "900"}},
			wantCode:   errx.CodeConflict,
			wantReason: "WRITE_OUTCOME_UNKNOWN",
			configure: func(_ *fakeRegistry, policies *fakePolicyRegistry) {
				policies.postLockErr = releaseErr
			},
			assertDone: func(t *testing.T, client *fakeJira, _ *fakePolicyRegistry) {
				if client.commentCalls != 1 {
					t.Fatalf("comment calls = %d, want completed callback", client.commentCalls)
				}
			},
		},
		{
			name:       "remote mutation dry run profile lock release fails",
			args:       []string{"--dry-run", "comments", "add", "--issue", "WL-1", "--body", "safe"},
			client:     &fakeJira{},
			wantCode:   errx.CodeInternal,
			wantReason: "INTERNAL",
			configure: func(registry *fakeRegistry, _ *fakePolicyRegistry) {
				registry.postLockErr = releaseErr
			},
			assertDone: func(t *testing.T, client *fakeJira, policies *fakePolicyRegistry) {
				if client.totalMutationCalls() != 0 || policies.requires != 1 || policies.lockCallbacks != 1 {
					t.Fatalf("dry-run callback = mutation calls:%d policy requires:%d lock callbacks:%d", client.totalMutationCalls(), policies.requires, policies.lockCallbacks)
				}
			},
		},
		{
			name:       "remote mutation dry run policy lock release fails",
			args:       []string{"--dry-run", "comments", "add", "--issue", "WL-1", "--body", "safe"},
			client:     &fakeJira{},
			wantCode:   errx.CodeInternal,
			wantReason: "INTERNAL",
			configure: func(_ *fakeRegistry, policies *fakePolicyRegistry) {
				policies.postLockErr = releaseErr
			},
			assertDone: func(t *testing.T, client *fakeJira, policies *fakePolicyRegistry) {
				if client.totalMutationCalls() != 0 || policies.requires != 1 || policies.lockCallbacks != 1 {
					t.Fatalf("dry-run callback = mutation calls:%d policy requires:%d lock callbacks:%d", client.totalMutationCalls(), policies.requires, policies.lockCallbacks)
				}
			},
		},
		{
			name:       "allow-projects show profile lock release fails",
			args:       []string{"auth", "allow-projects", "show"},
			client:     &fakeJira{},
			wantCode:   errx.CodeInternal,
			wantReason: "INTERNAL",
			configure: func(registry *fakeRegistry, _ *fakePolicyRegistry) {
				registry.postLockErr = releaseErr
			},
			assertDone: func(t *testing.T, _ *fakeJira, policies *fakePolicyRegistry) {
				if policies.gets != 1 {
					t.Fatalf("policy gets = %d, want completed callback", policies.gets)
				}
			},
		},
		{
			name:       "allow-projects show policy lock release fails",
			args:       []string{"auth", "allow-projects", "show"},
			client:     &fakeJira{},
			wantCode:   errx.CodeInternal,
			wantReason: "INTERNAL",
			configure: func(_ *fakeRegistry, policies *fakePolicyRegistry) {
				policies.postLockErr = releaseErr
			},
			assertDone: func(t *testing.T, _ *fakeJira, policies *fakePolicyRegistry) {
				if policies.gets != 1 {
					t.Fatalf("policy gets = %d, want completed callback", policies.gets)
				}
			},
		},
		{
			name:       "allow-projects set profile lock release fails",
			args:       []string{"--yes", "auth", "allow-projects", "set", "--project", "WL"},
			client:     &fakeJira{},
			wantCode:   errx.CodeConflict,
			wantReason: "WRITE_POLICY_OUTCOME_UNKNOWN",
			configure: func(registry *fakeRegistry, _ *fakePolicyRegistry) {
				registry.postLockErr = releaseErr
			},
			assertDone: func(t *testing.T, _ *fakeJira, policies *fakePolicyRegistry) {
				if policies.sets != 1 {
					t.Fatalf("policy sets = %d, want completed callback", policies.sets)
				}
			},
		},
		{
			name:       "allow-projects set policy lock release fails",
			args:       []string{"--yes", "auth", "allow-projects", "set", "--project", "WL"},
			client:     &fakeJira{},
			wantCode:   errx.CodeConflict,
			wantReason: "WRITE_POLICY_OUTCOME_UNKNOWN",
			configure: func(_ *fakeRegistry, policies *fakePolicyRegistry) {
				policies.postLockErr = releaseErr
			},
			assertDone: func(t *testing.T, _ *fakeJira, policies *fakePolicyRegistry) {
				if policies.sets != 1 {
					t.Fatalf("policy sets = %d, want completed callback", policies.sets)
				}
			},
		},
		{
			name:       "allow-projects set dry run profile lock release fails",
			args:       []string{"--dry-run", "auth", "allow-projects", "set", "--project", "WL"},
			client:     &fakeJira{},
			wantCode:   errx.CodeInternal,
			wantReason: "INTERNAL",
			configure: func(registry *fakeRegistry, _ *fakePolicyRegistry) {
				registry.postLockErr = releaseErr
			},
			assertDone: func(t *testing.T, _ *fakeJira, policies *fakePolicyRegistry) {
				if policies.sets != 0 || policies.lockCallbacks != 1 {
					t.Fatalf("dry-run callback = policy sets:%d lock callbacks:%d", policies.sets, policies.lockCallbacks)
				}
			},
		},
		{
			name:       "allow-projects set dry run policy lock release fails",
			args:       []string{"--dry-run", "auth", "allow-projects", "set", "--project", "WL"},
			client:     &fakeJira{},
			wantCode:   errx.CodeInternal,
			wantReason: "INTERNAL",
			configure: func(_ *fakeRegistry, policies *fakePolicyRegistry) {
				policies.postLockErr = releaseErr
			},
			assertDone: func(t *testing.T, _ *fakeJira, policies *fakePolicyRegistry) {
				if policies.sets != 0 || policies.lockCallbacks != 1 {
					t.Fatalf("dry-run callback = policy sets:%d lock callbacks:%d", policies.sets, policies.lockCallbacks)
				}
			},
		},
		{
			name:       "allow-projects clear profile lock release fails",
			args:       []string{"--yes", "auth", "allow-projects", "clear"},
			client:     &fakeJira{},
			wantCode:   errx.CodeConflict,
			wantReason: "WRITE_POLICY_OUTCOME_UNKNOWN",
			configure: func(registry *fakeRegistry, _ *fakePolicyRegistry) {
				registry.postLockErr = releaseErr
			},
			assertDone: func(t *testing.T, _ *fakeJira, policies *fakePolicyRegistry) {
				if policies.clears != 1 {
					t.Fatalf("policy clears = %d, want completed callback", policies.clears)
				}
			},
		},
		{
			name:       "allow-projects clear policy lock release fails",
			args:       []string{"--yes", "auth", "allow-projects", "clear"},
			client:     &fakeJira{},
			wantCode:   errx.CodeConflict,
			wantReason: "WRITE_POLICY_OUTCOME_UNKNOWN",
			configure: func(_ *fakeRegistry, policies *fakePolicyRegistry) {
				policies.postLockErr = releaseErr
			},
			assertDone: func(t *testing.T, _ *fakeJira, policies *fakePolicyRegistry) {
				if policies.clears != 1 {
					t.Fatalf("policy clears = %d, want completed callback", policies.clears)
				}
			},
		},
		{
			name:       "allow-projects clear dry run profile lock release fails",
			args:       []string{"--dry-run", "auth", "allow-projects", "clear"},
			client:     &fakeJira{},
			wantCode:   errx.CodeInternal,
			wantReason: "INTERNAL",
			configure: func(registry *fakeRegistry, _ *fakePolicyRegistry) {
				registry.postLockErr = releaseErr
			},
			assertDone: func(t *testing.T, _ *fakeJira, policies *fakePolicyRegistry) {
				if policies.clears != 0 || policies.lockCallbacks != 1 {
					t.Fatalf("dry-run callback = policy clears:%d lock callbacks:%d", policies.clears, policies.lockCallbacks)
				}
			},
		},
		{
			name:       "allow-projects clear dry run policy lock release fails",
			args:       []string{"--dry-run", "auth", "allow-projects", "clear"},
			client:     &fakeJira{},
			wantCode:   errx.CodeInternal,
			wantReason: "INTERNAL",
			configure: func(_ *fakeRegistry, policies *fakePolicyRegistry) {
				policies.postLockErr = releaseErr
			},
			assertDone: func(t *testing.T, _ *fakeJira, policies *fakePolicyRegistry) {
				if policies.clears != 0 || policies.lockCallbacks != 1 {
					t.Fatalf("dry-run callback = policy clears:%d lock callbacks:%d", policies.clears, policies.lockCallbacks)
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := &fakeStore{credential: auth.Credential{Token: "KEYCHAIN_TOKEN_SENTINEL"}}
			app, stdout, stderr := testApp(store, test.client)
			registry := app.registry.(*fakeRegistry)
			policies := app.policies.(*fakePolicyRegistry)
			test.configure(registry, policies)

			code := app.Run(context.Background(), app.NewRootCommand(), append([]string{"--output", "json", "--profile", "work"}, test.args...))

			if code != test.wantCode {
				t.Fatalf("code = %d, want %d stdout=%s stderr=%s", code, test.wantCode, stdout.String(), stderr.String())
			}
			test.assertDone(t, test.client, policies)
			assertSingleFailureEnvelope(t, stdout.Bytes(), test.wantReason)
			if strings.Contains(stdout.String()+stderr.String(), releaseErr.Error()) {
				t.Fatalf("lock release details leaked: stdout=%s stderr=%s", stdout.String(), stderr.String())
			}
		})
	}
}

func TestSuccessOutputFailureAfterCompletedCallbackEmitsOnlyReconciliationFailure(t *testing.T) {
	writeErr := errors.New("SUCCESS_OUTPUT_WRITE_SENTINEL")
	tests := []struct {
		name       string
		args       []string
		client     *fakeJira
		wantCode   errx.Code
		wantReason string
		assertDone func(*testing.T, *fakeJira, *fakePolicyRegistry)
	}{
		{
			name:       "real remote mutation",
			args:       []string{"--yes", "comments", "add", "--issue", "WL-1", "--body", "safe"},
			client:     &fakeJira{issue: jira.Issue{ID: "10001", Key: "WL-1"}, comment: jira.Comment{ID: "900"}},
			wantCode:   errx.CodeConflict,
			wantReason: "WRITE_OUTCOME_UNKNOWN",
			assertDone: func(t *testing.T, client *fakeJira, _ *fakePolicyRegistry) {
				if client.commentCalls != 1 {
					t.Fatalf("comment calls = %d, want applied mutation", client.commentCalls)
				}
			},
		},
		{
			name:       "real policy set",
			args:       []string{"--yes", "auth", "allow-projects", "set", "--project", "WL"},
			client:     &fakeJira{},
			wantCode:   errx.CodeConflict,
			wantReason: "WRITE_POLICY_OUTCOME_UNKNOWN",
			assertDone: func(t *testing.T, _ *fakeJira, policies *fakePolicyRegistry) {
				if policies.sets != 1 {
					t.Fatalf("policy sets = %d, want applied mutation", policies.sets)
				}
			},
		},
		{
			name:       "real policy clear",
			args:       []string{"--yes", "auth", "allow-projects", "clear"},
			client:     &fakeJira{},
			wantCode:   errx.CodeConflict,
			wantReason: "WRITE_POLICY_OUTCOME_UNKNOWN",
			assertDone: func(t *testing.T, _ *fakeJira, policies *fakePolicyRegistry) {
				if policies.clears != 1 {
					t.Fatalf("policy clears = %d, want applied mutation", policies.clears)
				}
			},
		},
		{
			name:       "remote mutation dry run",
			args:       []string{"--dry-run", "comments", "add", "--issue", "WL-1", "--body", "safe"},
			client:     &fakeJira{},
			wantCode:   errx.CodeInternal,
			wantReason: "INTERNAL",
			assertDone: func(t *testing.T, client *fakeJira, policies *fakePolicyRegistry) {
				if client.totalMutationCalls() != 0 || policies.requires != 1 {
					t.Fatalf("dry-run callback = mutation calls:%d policy requires:%d", client.totalMutationCalls(), policies.requires)
				}
			},
		},
		{
			name:       "policy set dry run",
			args:       []string{"--dry-run", "auth", "allow-projects", "set", "--project", "WL"},
			client:     &fakeJira{},
			wantCode:   errx.CodeInternal,
			wantReason: "INTERNAL",
			assertDone: func(t *testing.T, _ *fakeJira, policies *fakePolicyRegistry) {
				if policies.sets != 0 || policies.lockCallbacks != 1 {
					t.Fatalf("dry-run callback = policy sets:%d lock callbacks:%d", policies.sets, policies.lockCallbacks)
				}
			},
		},
		{
			name:       "policy clear dry run",
			args:       []string{"--dry-run", "auth", "allow-projects", "clear"},
			client:     &fakeJira{},
			wantCode:   errx.CodeInternal,
			wantReason: "INTERNAL",
			assertDone: func(t *testing.T, _ *fakeJira, policies *fakePolicyRegistry) {
				if policies.clears != 0 || policies.lockCallbacks != 1 {
					t.Fatalf("dry-run callback = policy clears:%d lock callbacks:%d", policies.clears, policies.lockCallbacks)
				}
			},
		},
		{
			name:       "policy show",
			args:       []string{"auth", "allow-projects", "show"},
			client:     &fakeJira{},
			wantCode:   errx.CodeInternal,
			wantReason: "INTERNAL",
			assertDone: func(t *testing.T, _ *fakeJira, policies *fakePolicyRegistry) {
				if policies.gets != 1 || policies.lockCallbacks != 1 {
					t.Fatalf("show callback = policy gets:%d lock callbacks:%d", policies.gets, policies.lockCallbacks)
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := &fakeStore{credential: auth.Credential{Token: "KEYCHAIN_TOKEN_SENTINEL"}}
			app, _, stderr := testApp(store, test.client)
			stdout := &failFirstWriteBuffer{writeErr: writeErr}
			app.stdout = stdout
			policies := app.policies.(*fakePolicyRegistry)

			code := app.Run(context.Background(), app.NewRootCommand(), append([]string{"--output", "json", "--profile", "work"}, test.args...))

			if code != test.wantCode {
				t.Fatalf("code = %d, want %d stdout=%s stderr=%s", code, test.wantCode, stdout.String(), stderr.String())
			}
			test.assertDone(t, test.client, policies)
			if stdout.writes != 2 {
				t.Fatalf("stdout writes = %d, want failed success plus accepted failure", stdout.writes)
			}
			assertSingleFailureEnvelope(t, stdout.Bytes(), test.wantReason)
			combined := stdout.String() + stderr.String()
			for _, sentinel := range []string{writeErr.Error(), store.credential.Token} {
				if strings.Contains(combined, sentinel) {
					t.Fatalf("output leaked %q: %s", sentinel, combined)
				}
			}
		})
	}
}

func TestMutationOutputBoundaryFailureKeepsWriteOutcomeUnknown(t *testing.T) {
	writeErr := errors.New("OUTPUT_WRITER_TOKEN_SENTINEL OUTPUT_WRITER_PATH_SENTINEL")
	tests := []struct {
		name          string
		newOutput     func() *outputBoundaryBuffer
		outputStarted bool
	}{
		{
			name: "partial first write does not append a second envelope",
			newOutput: func() *outputBoundaryBuffer {
				return &outputBoundaryBuffer{writeErr: writeErr, firstWriteSize: 1}
			},
			outputStarted: true,
		},
		{
			name: "persistent zero byte failure leaves stdout empty",
			newOutput: func() *outputBoundaryBuffer {
				return &outputBoundaryBuffer{writeErr: writeErr, failEveryWrite: true}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := &fakeStore{credential: auth.Credential{Token: "KEYCHAIN_TOKEN_SENTINEL"}}
			client := &fakeJira{issue: jira.Issue{ID: "10001", Key: "WL-1"}, comment: jira.Comment{ID: "900"}}
			app, _, stderr := testApp(store, client)
			stdout := test.newOutput()
			app.stdout = stdout

			code := app.Run(context.Background(), app.NewRootCommand(), []string{"--output", "json", "--profile", "work", "--yes", "comments", "add", "--issue", "WL-1", "--body", "safe"})

			if code != errx.CodeConflict {
				t.Fatalf("code = %d, want %d (write outcome unknown) stdout=%q stderr=%q", code, errx.CodeConflict, stdout.String(), stderr.String())
			}
			if client.commentCalls != 1 {
				t.Fatalf("comment calls = %d, want exactly one applied mutation", client.commentCalls)
			}
			if test.outputStarted {
				if stdout.writes != 1 {
					t.Fatalf("stdout writes = %d, want no second envelope after output started", stdout.writes)
				}
				if strings.Contains(stdout.String(), `"ok":false`) {
					t.Fatalf("stdout appended failure envelope after partial output: %q", stdout.String())
				}
			} else if stdout.String() != "" {
				t.Fatalf("persistent output failure produced stdout: %q", stdout.String())
			}

			combined := stdout.String() + stderr.String()
			for _, sentinel := range []string{"OUTPUT_WRITER_TOKEN_SENTINEL", "OUTPUT_WRITER_PATH_SENTINEL", store.credential.Token} {
				if strings.Contains(combined, sentinel) {
					t.Fatalf("output leaked %q: stdout=%q stderr=%q", sentinel, stdout.String(), stderr.String())
				}
			}
		})
	}
}

func TestPostCallbackLockTimeoutForNonMutatingPathsStaysInternal(t *testing.T) {
	timeout := &lockfile.TimeoutError{Path: "POST_CALLBACK_TIMEOUT_PATH_SENTINEL", Timeout: time.Second}
	releaseErr := fmt.Errorf("POST_CALLBACK_RELEASE_SENTINEL: %w", timeout)
	tests := []struct {
		name       string
		args       []string
		configure  func(*fakeRegistry, *fakePolicyRegistry)
		assertDone func(*testing.T, *fakeJira, *fakePolicyRegistry)
	}{
		{
			name: "remote dry run profile lock",
			args: []string{"--dry-run", "comments", "add", "--issue", "WL-1", "--body", "safe"},
			configure: func(registry *fakeRegistry, _ *fakePolicyRegistry) {
				registry.postLockErr = releaseErr
			},
			assertDone: assertRemoteDryRunCallback,
		},
		{
			name: "remote dry run policy lock",
			args: []string{"--dry-run", "comments", "add", "--issue", "WL-1", "--body", "safe"},
			configure: func(_ *fakeRegistry, policies *fakePolicyRegistry) {
				policies.postLockErr = releaseErr
			},
			assertDone: assertRemoteDryRunCallback,
		},
		{
			name: "policy set dry run profile lock",
			args: []string{"--dry-run", "auth", "allow-projects", "set", "--project", "WL"},
			configure: func(registry *fakeRegistry, _ *fakePolicyRegistry) {
				registry.postLockErr = releaseErr
			},
			assertDone: assertPolicySetDryRunCallback,
		},
		{
			name: "policy set dry run policy lock",
			args: []string{"--dry-run", "auth", "allow-projects", "set", "--project", "WL"},
			configure: func(_ *fakeRegistry, policies *fakePolicyRegistry) {
				policies.postLockErr = releaseErr
			},
			assertDone: assertPolicySetDryRunCallback,
		},
		{
			name: "policy clear dry run profile lock",
			args: []string{"--dry-run", "auth", "allow-projects", "clear"},
			configure: func(registry *fakeRegistry, _ *fakePolicyRegistry) {
				registry.postLockErr = releaseErr
			},
			assertDone: assertPolicyClearDryRunCallback,
		},
		{
			name: "policy clear dry run policy lock",
			args: []string{"--dry-run", "auth", "allow-projects", "clear"},
			configure: func(_ *fakeRegistry, policies *fakePolicyRegistry) {
				policies.postLockErr = releaseErr
			},
			assertDone: assertPolicyClearDryRunCallback,
		},
		{
			name: "policy show profile lock",
			args: []string{"auth", "allow-projects", "show"},
			configure: func(registry *fakeRegistry, _ *fakePolicyRegistry) {
				registry.postLockErr = releaseErr
			},
			assertDone: assertPolicyShowCallback,
		},
		{
			name: "policy show policy lock",
			args: []string{"auth", "allow-projects", "show"},
			configure: func(_ *fakeRegistry, policies *fakePolicyRegistry) {
				policies.postLockErr = releaseErr
			},
			assertDone: assertPolicyShowCallback,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := &fakeStore{credential: auth.Credential{Token: "KEYCHAIN_TOKEN_SENTINEL"}}
			client := &fakeJira{}
			app, stdout, stderr := testApp(store, client)
			registry := app.registry.(*fakeRegistry)
			policies := app.policies.(*fakePolicyRegistry)
			test.configure(registry, policies)

			code := app.Run(context.Background(), app.NewRootCommand(), append([]string{"--output", "json", "--profile", "work"}, test.args...))

			if code != errx.CodeInternal {
				t.Fatalf("code = %d, want %d stdout=%s stderr=%s", code, errx.CodeInternal, stdout.String(), stderr.String())
			}
			test.assertDone(t, client, policies)
			assertSingleFailureEnvelope(t, stdout.Bytes(), "INTERNAL")
			combined := stdout.String() + stderr.String()
			for _, sentinel := range []string{"LOCAL_LOCK_BUSY", "POST_CALLBACK_RELEASE_SENTINEL", timeout.Path, store.credential.Token} {
				if strings.Contains(combined, sentinel) {
					t.Fatalf("post-callback timeout leaked or was misclassified as %q: %s", sentinel, combined)
				}
			}
		})
	}
}

func assertRemoteDryRunCallback(t *testing.T, client *fakeJira, policies *fakePolicyRegistry) {
	t.Helper()
	if client.totalMutationCalls() != 0 || policies.requires != 1 || policies.lockCallbacks != 1 {
		t.Fatalf("dry-run callback = mutation calls:%d policy requires:%d lock callbacks:%d", client.totalMutationCalls(), policies.requires, policies.lockCallbacks)
	}
}

func assertPolicySetDryRunCallback(t *testing.T, _ *fakeJira, policies *fakePolicyRegistry) {
	t.Helper()
	if policies.sets != 0 || policies.lockCallbacks != 1 {
		t.Fatalf("dry-run callback = policy sets:%d lock callbacks:%d", policies.sets, policies.lockCallbacks)
	}
}

func assertPolicyClearDryRunCallback(t *testing.T, _ *fakeJira, policies *fakePolicyRegistry) {
	t.Helper()
	if policies.clears != 0 || policies.lockCallbacks != 1 {
		t.Fatalf("dry-run callback = policy clears:%d lock callbacks:%d", policies.clears, policies.lockCallbacks)
	}
}

func assertPolicyShowCallback(t *testing.T, _ *fakeJira, policies *fakePolicyRegistry) {
	t.Helper()
	if policies.gets != 1 || policies.lockCallbacks != 1 {
		t.Fatalf("show callback = policy gets:%d lock callbacks:%d", policies.gets, policies.lockCallbacks)
	}
}

func TestCommittedWritePolicyErrorRequiresReconciliation(t *testing.T) {
	tests := []struct {
		name string
		err  error
	}{
		{name: "durability error", err: errors.New("directory sync failed")},
		{name: "timeout-shaped durability error", err: &lockfile.TimeoutError{Path: "/private/sentinel", Timeout: time.Second}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := translateWritePolicyLockBoundary(&writepolicy.CommitError{Err: test.err}, "work")
			var typed *errx.Error
			if !errors.As(err, &typed) || typed.Code != errx.CodeConflict || typed.Reason != "WRITE_POLICY_OUTCOME_UNKNOWN" {
				t.Fatalf("error = %#v, want WRITE_POLICY_OUTCOME_UNKNOWN", typed)
			}
			if !strings.Contains(typed.Hint, "auth allow-projects show --profile work") {
				t.Fatalf("hint = %q, want reconciliation command", typed.Hint)
			}
			if strings.Contains(typed.Message+typed.Hint, "/private/sentinel") {
				t.Fatal("lock path leaked")
			}
		})
	}
}

func TestLocalPolicyMutationsValidateOutputBeforeChangingState(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{name: "unknown projected field", args: []string{"--profile", "work", "--yes", "--fields", "authorization", "auth", "allow-projects", "set", "--project", "WL"}},
		{name: "raw output with fields", args: []string{"--profile", "work", "--yes", "--output", "raw", "--fields", "projects", "auth", "allow-projects", "clear"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := &fakeStore{credential: auth.Credential{Token: "KEYCHAIN_TOKEN_SENTINEL"}}
			app, stdout, stderr := testApp(store, &fakeJira{})
			policies := app.policies.(*fakePolicyRegistry)
			code := app.Run(context.Background(), app.NewRootCommand(), append([]string{"--output", "json"}, test.args...))
			if code != errx.CodeUsage {
				t.Fatalf("code = %d, want %d stdout=%s stderr=%s", code, errx.CodeUsage, stdout.String(), stderr.String())
			}
			if policies.gets != 0 || policies.sets != 0 || policies.clears != 0 || policies.requires != 0 {
				t.Fatalf("invalid output mutated or read policy: %#v", policies)
			}
			if store.loads != 0 || store.saves != 0 || store.deletes != 0 {
				t.Fatalf("invalid output touched credentials: %#v", store)
			}
		})
	}
}

func TestProfileRequiredPrecedesProjectionValidation(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{name: "remote mutation", args: []string{"--yes", "--fields", "authorization", "comments", "add", "--issue", "WL-1", "--body", "safe"}},
		{name: "policy show", args: []string{"--fields", "authorization", "auth", "allow-projects", "show"}},
		{name: "policy set", args: []string{"--yes", "--fields", "authorization", "auth", "allow-projects", "set", "--project", "WL"}},
		{name: "policy clear", args: []string{"--yes", "--fields", "authorization", "auth", "allow-projects", "clear"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := &fakeStore{credential: auth.Credential{Token: "KEYCHAIN_TOKEN_SENTINEL"}}
			client := &fakeJira{}
			app, stdout, _ := testApp(store, client)
			policies := app.policies.(*fakePolicyRegistry)
			code := app.Run(context.Background(), app.NewRootCommand(), append([]string{"--output", "json"}, test.args...))
			if code != errx.CodeUsage {
				t.Fatalf("code = %d, want usage stdout=%s", code, stdout.String())
			}
			assertErrorReason(t, stdout.Bytes(), "PROFILE_REQUIRED")
			if store.loads != 0 || client.totalCalls() != 0 || policies.gets != 0 || policies.sets != 0 || policies.clears != 0 || policies.requires != 0 {
				t.Fatalf("profile precedence crossed a boundary: store=%#v client=%#v policies=%#v", store, client, policies)
			}
		})
	}
}

func TestMutationValidationAndSafetyRailsRunBeforeCredentialOrJira(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		policyErr  error
		wantCode   errx.Code
		wantReason string
		wantHint   string
		noPolicy   bool
	}{
		{name: "missing yes", args: []string{"--profile", "work", "issues", "create", "--project", "WL", "--issue-type-id", "1", "--summary", "safe"}, wantCode: errx.CodeConfirm, wantReason: "CONFIRMATION_REQUIRED"},
		{name: "missing profile", args: []string{"--yes", "issues", "create", "--project", "WL", "--issue-type-id", "1", "--summary", "safe"}, wantCode: errx.CodeUsage, wantReason: "PROFILE_REQUIRED"},
		{name: "lowercase project is rejected", args: []string{"--profile", "work", "--yes", "issues", "create", "--project", "wl", "--issue-type-id", "1", "--summary", "safe"}, wantCode: errx.CodeUsage, wantReason: "USAGE"},
		{name: "moved or case-changed issue key is rejected locally", args: []string{"--profile", "work", "--yes", "issues", "edit", "wl-1", "--summary", "safe"}, wantCode: errx.CodeUsage, wantReason: "USAGE"},
		{name: "nonnumeric issue type is rejected", args: []string{"--profile", "work", "--yes", "issues", "create", "--project", "WL", "--issue-type-id", "Task", "--summary", "safe"}, wantCode: errx.CodeUsage, wantReason: "USAGE"},
		{name: "invalid label is rejected", args: []string{"--profile", "work", "--yes", "issues", "create", "--project", "WL", "--issue-type-id", "1", "--summary", "safe", "--label", "two words"}, wantCode: errx.CodeUsage, wantReason: "USAGE", noPolicy: true},
		{name: "invalid output field is rejected", args: []string{"--profile", "work", "--yes", "--fields", "authorization", "comments", "add", "--issue", "WL-1", "--body", "safe"}, wantCode: errx.CodeUsage, wantReason: "USAGE", noPolicy: true},
		{name: "raw output with fields is rejected", args: []string{"--profile", "work", "--yes", "--output", "raw", "--fields", "issue_id", "comments", "add", "--issue", "WL-1", "--body", "safe"}, wantCode: errx.CodeUsage, wantReason: "USAGE", noPolicy: true},
		{name: "missing write policy", args: []string{"--profile", "work", "--yes", "comments", "add", "--issue", "WL-1", "--body", "safe"}, policyErr: writepolicy.ErrNotFound, wantCode: errx.CodePermission, wantReason: "WRITE_POLICY_MISSING"},
		{name: "missing write policy is checked before confirmation", args: []string{"--profile", "work", "comments", "add", "--issue", "WL-1", "--body", "safe"}, policyErr: writepolicy.ErrNotFound, wantCode: errx.CodePermission, wantReason: "WRITE_POLICY_MISSING"},
		{name: "stale write policy", args: []string{"--profile", "work", "--yes", "comments", "add", "--issue", "WL-1", "--body", "safe"}, policyErr: writepolicy.ErrStale, wantCode: errx.CodePermission, wantReason: "WRITE_POLICY_STALE"},
		{name: "disallowed project", args: []string{"--profile", "work", "--yes", "comments", "add", "--issue", "WL-1", "--body", "safe"}, policyErr: writepolicy.ErrProjectDenied, wantCode: errx.CodePermission, wantReason: "PROJECT_NOT_ALLOWED", wantHint: "complete intended --project list"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := &fakeStore{credential: auth.Credential{Token: "KEYCHAIN_TOKEN_SENTINEL"}}
			client := &fakeJira{}
			app, stdout, stderr := testApp(store, client)
			policies := app.policies.(*fakePolicyRegistry)
			policies.requireErr = test.policyErr
			code := app.Run(context.Background(), app.NewRootCommand(), append([]string{"--output", "json"}, test.args...))
			if code != test.wantCode {
				t.Fatalf("code = %d, want %d stdout=%s stderr=%s", code, test.wantCode, stdout.String(), stderr.String())
			}
			assertErrorReason(t, stdout.Bytes(), test.wantReason)
			if test.wantHint != "" {
				assertErrorHintContains(t, stdout.Bytes(), test.wantHint)
			}
			if store.loads != 0 || client.totalMutationCalls() != 0 || client.projectCalls != 0 || client.issueCalls != 0 {
				t.Fatalf("unsafe prevalidation effects: loads=%d client=%#v", store.loads, client)
			}
			if test.noPolicy && policies.requires != 0 {
				t.Fatalf("invalid local input reached write policy: requires=%d", policies.requires)
			}
			if strings.Contains(stdout.String()+stderr.String(), store.credential.Token) {
				t.Fatal("credential leaked")
			}
		})
	}
}

func TestMutationDryRunIsLocalOnly(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{name: "create", args: []string{"issues", "create", "--project", "WL", "--issue-type-id", "1", "--summary", "safe", "--label", "DRY_RUN_LABEL_SENTINEL", "--label", "Second"}},
		{name: "edit", args: []string{"issues", "edit", "WL-1", "--clear-description"}},
		{name: "transition", args: []string{"issues", "transition", "WL-1", "--transition-id", "31"}},
		{name: "comment", args: []string{"comments", "add", "--issue", "WL-1", "--body", "safe"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := &fakeStore{credential: auth.Credential{Token: "KEYCHAIN_TOKEN_SENTINEL"}}
			client := &fakeJira{}
			app, stdout, _ := testApp(store, client)
			code := app.Run(context.Background(), app.NewRootCommand(), append([]string{"--output", "json", "--profile", "work", "--dry-run"}, test.args...))
			if code != errx.CodeOK {
				t.Fatalf("code = %d stdout=%s", code, stdout.String())
			}
			if store.loads != 0 || client.totalCalls() != 0 {
				t.Fatalf("dry run reached credential/Jira: loads=%d calls=%d", store.loads, client.totalCalls())
			}
			var envelope output.Envelope
			if err := json.Unmarshal(stdout.Bytes(), &envelope); err != nil {
				t.Fatal(err)
			}
			data := envelope.Data.(map[string]any)
			if data["dry_run"] != true || data["applied"] != false || data["remote_checks"] != "not_performed" {
				t.Fatalf("receipt = %#v", data)
			}
			if strings.Contains(stdout.String(), "DRY_RUN_LABEL_SENTINEL") {
				t.Fatalf("dry-run receipt leaked label values: %s", stdout.String())
			}
		})
	}
}

func TestIssueTypesReturnsAdvancingOffsetCursor(t *testing.T) {
	client := &fakeJira{
		project: jira.Project{ID: "123", Key: "WL"},
		issueTypes: jira.IssueTypePage{
			StartAt: 5, MaxResults: 2, Total: 9,
			Values: []jira.IssueType{{ID: "100", Name: "Task"}, {ID: "101", Name: "Subtask", Subtask: true}, {ID: "102", Name: "Bug"}},
		},
	}
	store := &fakeStore{credential: auth.Credential{Token: "KEYCHAIN_TOKEN_SENTINEL"}}
	app, stdout, _ := testApp(store, client)
	code := app.Run(context.Background(), app.NewRootCommand(), []string{
		"--output", "json", "--profile", "work", "issues", "types", "--project", "WL", "--limit", "2", "--cursor", encodeOffsetCursor(5),
	})
	if code != errx.CodeOK {
		t.Fatalf("code = %d stdout=%s", code, stdout.String())
	}
	if store.loads != 1 || client.projectCalls != 1 || client.typeCalls != 1 || client.issueTypeProjectID != "123" {
		t.Fatalf("preflight = loads:%d client:%#v", store.loads, client)
	}
	if client.issueTypeOptions.StartAt != 5 || client.issueTypeOptions.MaxResults != 2 {
		t.Fatalf("options = %#v", client.issueTypeOptions)
	}
	var envelope output.Envelope
	if err := json.Unmarshal(stdout.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Meta == nil || envelope.Meta.NextCursor != encodeOffsetCursor(8) || envelope.Meta.Truncated == nil || !*envelope.Meta.Truncated || envelope.Meta.Count == nil || *envelope.Meta.Count != 2 {
		t.Fatalf("meta = %#v", envelope.Meta)
	}
	if rows, ok := envelope.Data.([]any); !ok || len(rows) != 2 {
		t.Fatalf("data = %#v, want two standard issue types", envelope.Data)
	}
}

func TestMutationCommandsPerformExactPreflightAndWriteOnce(t *testing.T) {
	description := "first\nвторая"
	tests := []struct {
		name   string
		args   []string
		client *fakeJira
		check  func(*testing.T, *fakeJira, map[string]any)
	}{
		{
			name: "create", args: []string{"issues", "create", "--project", "WL", "--issue-type-id", "456", "--summary", "New issue", "--description", description, "--label", "Zulu", "--label", "alpha", "--label", "Música"},
			client: &fakeJira{project: jira.Project{ID: "123", Key: "WL"}, issue: jira.Issue{ID: "10001", Key: "WL-1"}, created: jira.Issue{ID: "10001", Key: "WL-1", Self: "https://safe.invalid/10001"}},
			check: func(t *testing.T, client *fakeJira, receipt map[string]any) {
				if client.projectCalls != 1 || client.typeCalls != 0 || client.createCalls != 1 || client.issueCalls != 1 {
					t.Fatalf("create preflight/calls = %#v", client)
				}
				want := jira.CreateIssueRequest{ProjectID: "123", IssueTypeID: "456", Summary: "New issue", Description: &description, Labels: []string{"Zulu", "alpha", "Música"}}
				if !reflect.DeepEqual(client.createInput, want) {
					t.Fatalf("create input = %#v", client.createInput)
				}
				if receipt["issue_key"] != "WL-1" || receipt["issue_id"] != "10001" {
					t.Fatalf("receipt = %#v", receipt)
				}
			},
		},
		{
			name: "edit clear", args: []string{"issues", "edit", "WL-1", "--clear-description"}, client: &fakeJira{issue: jira.Issue{ID: "10001", Key: "WL-1"}},
			check: func(t *testing.T, client *fakeJira, receipt map[string]any) {
				if client.issueCalls != 2 || client.editCalls != 1 || client.editIssueID != "10001" || !client.editInput.ClearDescription {
					t.Fatalf("edit preflight/calls = %#v", client)
				}
				if receipt["issue_id"] != "10001" {
					t.Fatalf("receipt = %#v", receipt)
				}
			},
		},
		{
			name: "transition available ID", args: []string{"issues", "transition", "WL-1", "--transition-id", "31"}, client: &fakeJira{issue: jira.Issue{ID: "10001", Key: "WL-1"}, transitions: []jira.Transition{{ID: "31", Name: "Done"}}},
			check: func(t *testing.T, client *fakeJira, receipt map[string]any) {
				if client.issueCalls != 2 || client.transitionCalls != 1 || client.transitionIssueID != "10001" || client.transitionID != "31" {
					t.Fatalf("transition preflight/calls = %#v", client)
				}
				if receipt["transition_id"] != "31" {
					t.Fatalf("receipt = %#v", receipt)
				}
			},
		},
		{
			name: "comment", args: []string{"comments", "add", "--issue", "WL-1", "--body", description}, client: &fakeJira{issue: jira.Issue{ID: "10001", Key: "WL-1"}, comment: jira.Comment{ID: "900"}},
			check: func(t *testing.T, client *fakeJira, receipt map[string]any) {
				if client.issueCalls != 2 || client.commentCalls != 1 || client.commentIssueID != "10001" || client.commentBody != description {
					t.Fatalf("comment preflight/calls = %#v", client)
				}
				if receipt["comment_id"] != "900" {
					t.Fatalf("receipt = %#v", receipt)
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := &fakeStore{credential: auth.Credential{Token: "KEYCHAIN_TOKEN_SENTINEL"}}
			app, stdout, stderr := testApp(store, test.client)
			code := app.Run(context.Background(), app.NewRootCommand(), append([]string{"--output", "json", "--profile", "work", "--yes"}, test.args...))
			if code != errx.CodeOK {
				t.Fatalf("code = %d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
			}
			if store.loads != 1 || test.client.totalMutationCalls() != 1 {
				t.Fatalf("loads=%d mutation calls=%d", store.loads, test.client.totalMutationCalls())
			}
			var envelope output.Envelope
			if err := json.Unmarshal(stdout.Bytes(), &envelope); err != nil {
				t.Fatal(err)
			}
			receipt := envelope.Data.(map[string]any)
			if receipt["applied"] != true || receipt["dry_run"] != false || receipt["remote_checks"] != "verified" || receipt["project"] != "WL" {
				t.Fatalf("receipt = %#v", receipt)
			}
			if envelope.Meta == nil || envelope.Meta.Profile != "work" || envelope.Meta.Site != "https://example.atlassian.net" {
				t.Fatalf("mutation context = %#v", envelope.Meta)
			}
			test.check(t, test.client, receipt)
			if strings.Contains(stdout.String()+stderr.String(), store.credential.Token) {
				t.Fatal("credential leaked")
			}
		})
	}
}

func TestMutationRejectsInexactRemoteIdentityBeforeWrite(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		client     *fakeJira
		wantReason string
		wantHint   string
	}{
		{name: "project moved or case changed", args: []string{"issues", "create", "--project", "WL", "--issue-type-id", "1", "--summary", "safe"}, client: &fakeJira{project: jira.Project{ID: "123", Key: "FL"}}, wantReason: "INEXACT_PROJECT", wantHint: "projects get WL"},
		{name: "issue moved or case changed", args: []string{"issues", "edit", "WL-1", "--summary", "safe"}, client: &fakeJira{issue: jira.Issue{ID: "10001", Key: "FL-1"}}, wantReason: "INEXACT_ISSUE", wantHint: "issues get WL-1"},
		{name: "nonnumeric issue ID", args: []string{"comments", "add", "--issue", "WL-1", "--body", "safe"}, client: &fakeJira{issue: jira.Issue{ID: "WL-1", Key: "WL-1"}}, wantReason: "INTERNAL"},
		{name: "transition ID unavailable", args: []string{"issues", "transition", "WL-1", "--transition-id", "31"}, client: &fakeJira{issue: jira.Issue{ID: "10001", Key: "WL-1"}, transitions: []jira.Transition{{ID: "21"}}}, wantReason: "NOT_FOUND_TRANSITION", wantHint: "issues transitions WL-1"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			app, stdout, _ := testApp(&fakeStore{credential: auth.Credential{Token: "KEYCHAIN_TOKEN_SENTINEL"}}, test.client)
			code := app.Run(context.Background(), app.NewRootCommand(), append([]string{"--output", "json", "--profile", "work", "--yes"}, test.args...))
			if code == errx.CodeOK {
				t.Fatalf("unexpected success: %s", stdout.String())
			}
			assertErrorReason(t, stdout.Bytes(), test.wantReason)
			if test.wantHint != "" {
				assertErrorHintContains(t, stdout.Bytes(), test.wantHint)
			}
			if test.client.totalMutationCalls() != 0 {
				t.Fatalf("mutation calls = %d, want 0", test.client.totalMutationCalls())
			}
		})
	}
}

func TestMutationRejectsIncompleteWriteReceiptsAsUnknown(t *testing.T) {
	tests := []struct {
		name   string
		args   []string
		client *fakeJira
	}{
		{name: "create response missing identity", args: []string{"issues", "create", "--project", "WL", "--issue-type-id", "1", "--summary", "safe"}, client: &fakeJira{project: jira.Project{ID: "123", Key: "WL"}}},
		{name: "create response moved project", args: []string{"issues", "create", "--project", "WL", "--issue-type-id", "1", "--summary", "safe"}, client: &fakeJira{project: jira.Project{ID: "123", Key: "WL"}, created: jira.Issue{ID: "10001", Key: "FL-1"}}},
		{name: "comment response missing identity", args: []string{"comments", "add", "--issue", "WL-1", "--body", "safe"}, client: &fakeJira{issue: jira.Issue{ID: "10001", Key: "WL-1"}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			app, stdout, _ := testApp(&fakeStore{credential: auth.Credential{Token: "KEYCHAIN_TOKEN_SENTINEL"}}, test.client)
			code := app.Run(context.Background(), app.NewRootCommand(), append([]string{"--output", "json", "--profile", "work", "--yes"}, test.args...))
			if code != errx.CodeConflict {
				t.Fatalf("code = %d, want %d stdout=%s", code, errx.CodeConflict, stdout.String())
			}
			assertErrorReason(t, stdout.Bytes(), "WRITE_OUTCOME_UNKNOWN")
		})
	}
}

func TestCreatePropagatesClientPreflightFailure(t *testing.T) {
	client := &fakeJira{
		project:   jira.Project{ID: "123", Key: "WL"},
		createErr: errx.CreateFieldsUnsupported([]string{"customfield_10000"}, false),
	}
	app, stdout, _ := testApp(&fakeStore{credential: auth.Credential{Token: "KEYCHAIN_TOKEN_SENTINEL"}}, client)
	code := app.Run(context.Background(), app.NewRootCommand(), []string{
		"--output", "json", "--profile", "work", "--yes", "issues", "create", "--project", "WL", "--issue-type-id", "456", "--summary", "safe",
	})
	if code != errx.CodeUsage {
		t.Fatalf("code = %d, want usage stdout=%s", code, stdout.String())
	}
	assertErrorReason(t, stdout.Bytes(), "CREATE_FIELDS_UNSUPPORTED")
	assertErrorHintContains(t, stdout.Bytes(), "standard issue type")
	if client.createCalls != 1 || client.issueCalls != 0 {
		t.Fatalf("calls = create:%d issue:%d, want one client operation and no reconciliation read", client.createCalls, client.issueCalls)
	}
}

func TestMutationDetectsProjectMoveAfterWrite(t *testing.T) {
	client := &fakeJira{
		issueSequence: []jira.Issue{{ID: "10001", Key: "WL-1"}, {ID: "10001", Key: "FL-9"}},
	}
	app, stdout, _ := testApp(&fakeStore{credential: auth.Credential{Token: "KEYCHAIN_TOKEN_SENTINEL"}}, client)
	code := app.Run(context.Background(), app.NewRootCommand(), []string{
		"--output", "json", "--profile", "work", "--yes", "issues", "edit", "WL-1", "--summary", "safe",
	})
	if code != errx.CodeConflict {
		t.Fatalf("code = %d, want %d stdout=%s", code, errx.CodeConflict, stdout.String())
	}
	assertErrorReason(t, stdout.Bytes(), "WRITE_OUTCOME_UNKNOWN")
	if client.editCalls != 1 || client.issueCalls != 2 {
		t.Fatalf("calls = edit:%d issue:%d", client.editCalls, client.issueCalls)
	}
}

func (f *fakeJira) totalMutationCalls() int {
	return f.createCalls + f.editCalls + f.transitionCalls + f.commentCalls
}

func (f *fakeJira) totalCalls() int {
	return f.projectCalls + f.issueCalls + f.typeCalls + f.totalMutationCalls()
}

type fakePolicyRegistry struct {
	mu            sync.Mutex
	policy        writepolicy.Policy
	requireErr    error
	lockErr       error
	postLockErr   error
	setErr        error
	clearErr      error
	gets          int
	sets          int
	clears        int
	requires      int
	lockCallbacks int
}

type failFirstWriteBuffer struct {
	buffer   strings.Builder
	writeErr error
	writes   int
}

type outputBoundaryBuffer struct {
	buffer         strings.Builder
	writeErr       error
	firstWriteSize int
	failEveryWrite bool
	writes         int
}

func (w *outputBoundaryBuffer) Write(value []byte) (int, error) {
	w.writes++
	if w.firstWriteSize > 0 && w.writes == 1 {
		n := min(w.firstWriteSize, len(value))
		if _, err := w.buffer.Write(value[:n]); err != nil {
			return 0, err
		}
		return n, w.writeErr
	}
	if w.failEveryWrite {
		return 0, w.writeErr
	}
	return w.buffer.Write(value)
}

func (w *outputBoundaryBuffer) String() string {
	return w.buffer.String()
}

func (w *failFirstWriteBuffer) Write(value []byte) (int, error) {
	w.writes++
	if w.writes == 1 {
		return 0, w.writeErr
	}
	return w.buffer.Write(value)
}

func (w *failFirstWriteBuffer) Bytes() []byte {
	return []byte(w.buffer.String())
}

func (w *failFirstWriteBuffer) String() string {
	return w.buffer.String()
}

func (r *fakePolicyRegistry) WithPolicyLock(_ context.Context, _ string, fn func() error) error {
	if r.lockErr != nil {
		return r.lockErr
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	err := fn()
	if err == nil {
		r.lockCallbacks++
	}
	if r.postLockErr != nil {
		return errors.Join(err, r.postLockErr)
	}
	return err
}

func (r *fakePolicyRegistry) Get(context.Context, string) (writepolicy.Policy, error) {
	r.gets++
	if r.policy.Profile == "" {
		return writepolicy.Policy{}, writepolicy.ErrNotFound
	}
	return r.policy, nil
}

func (r *fakePolicyRegistry) GetBound(_ context.Context, value profile.Profile) (writepolicy.Policy, error) {
	if r.policy.Identity != writepolicy.IdentityFor(value) {
		return writepolicy.Policy{}, writepolicy.ErrStale
	}
	return r.policy, nil
}

func (r *fakePolicyRegistry) RequireProject(_ context.Context, value profile.Profile, project string) (writepolicy.Policy, error) {
	r.requires++
	if r.requireErr != nil {
		return writepolicy.Policy{}, r.requireErr
	}
	if r.policy.Identity != writepolicy.IdentityFor(value) {
		return writepolicy.Policy{}, writepolicy.ErrStale
	}
	for _, allowed := range r.policy.Projects {
		if allowed == project {
			return r.policy, nil
		}
	}
	return writepolicy.Policy{}, writepolicy.ErrProjectDenied
}

func (r *fakePolicyRegistry) Set(_ context.Context, value profile.Profile, projects []string) (writepolicy.Policy, error) {
	r.sets++
	if r.setErr != nil {
		return writepolicy.Policy{}, r.setErr
	}
	canonical, err := writepolicy.CanonicalProjects(projects)
	if err != nil {
		return writepolicy.Policy{}, err
	}
	r.policy = writepolicy.Policy{Profile: value.Name, Identity: writepolicy.IdentityFor(value), Projects: canonical}
	return r.policy, nil
}

func (r *fakePolicyRegistry) Clear(context.Context, string) error {
	r.clears++
	if r.clearErr != nil {
		return r.clearErr
	}
	r.policy = writepolicy.Policy{}
	return nil
}

var _ writePolicyRegistry = (*fakePolicyRegistry)(nil)
var _ jiraMutationClient = (*fakeJira)(nil)
