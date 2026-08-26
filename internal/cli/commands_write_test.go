package cli

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/abigotado/jira-cli/internal/auth"
	"github.com/abigotado/jira-cli/internal/errx"
	"github.com/abigotado/jira-cli/internal/jira"
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

func TestCommittedWritePolicyErrorRequiresReconciliation(t *testing.T) {
	err := translateWritePolicy(&writepolicy.CommitError{Err: errors.New("directory sync failed")}, "work", "")
	var typed *errx.Error
	if !errors.As(err, &typed) || typed.Code != errx.CodeConflict || typed.Reason != "WRITE_POLICY_OUTCOME_UNKNOWN" {
		t.Fatalf("error = %#v, want WRITE_POLICY_OUTCOME_UNKNOWN", typed)
	}
	if !strings.Contains(typed.Hint, "auth allow-projects show --profile work") {
		t.Fatalf("hint = %q, want reconciliation command", typed.Hint)
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
	}{
		{name: "missing yes", args: []string{"--profile", "work", "issues", "create", "--project", "WL", "--issue-type-id", "1", "--summary", "safe"}, wantCode: errx.CodeConfirm, wantReason: "CONFIRMATION_REQUIRED"},
		{name: "missing profile", args: []string{"--yes", "issues", "create", "--project", "WL", "--issue-type-id", "1", "--summary", "safe"}, wantCode: errx.CodeUsage, wantReason: "PROFILE_REQUIRED"},
		{name: "lowercase project is rejected", args: []string{"--profile", "work", "--yes", "issues", "create", "--project", "wl", "--issue-type-id", "1", "--summary", "safe"}, wantCode: errx.CodeUsage, wantReason: "USAGE"},
		{name: "moved or case-changed issue key is rejected locally", args: []string{"--profile", "work", "--yes", "issues", "edit", "wl-1", "--summary", "safe"}, wantCode: errx.CodeUsage, wantReason: "USAGE"},
		{name: "nonnumeric issue type is rejected", args: []string{"--profile", "work", "--yes", "issues", "create", "--project", "WL", "--issue-type-id", "Task", "--summary", "safe"}, wantCode: errx.CodeUsage, wantReason: "USAGE"},
		{name: "invalid output field is rejected", args: []string{"--profile", "work", "--yes", "--fields", "authorization", "comments", "add", "--issue", "WL-1", "--body", "safe"}, wantCode: errx.CodeUsage, wantReason: "USAGE"},
		{name: "raw output with fields is rejected", args: []string{"--profile", "work", "--yes", "--output", "raw", "--fields", "issue_id", "comments", "add", "--issue", "WL-1", "--body", "safe"}, wantCode: errx.CodeUsage, wantReason: "USAGE"},
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
			app.policies.(*fakePolicyRegistry).requireErr = test.policyErr
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
		{name: "create", args: []string{"issues", "create", "--project", "WL", "--issue-type-id", "1", "--summary", "safe"}},
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
			name: "create", args: []string{"issues", "create", "--project", "WL", "--issue-type-id", "456", "--summary", "New issue", "--description", description},
			client: &fakeJira{project: jira.Project{ID: "123", Key: "WL"}, issue: jira.Issue{ID: "10001", Key: "WL-1"}, created: jira.Issue{ID: "10001", Key: "WL-1", Self: "https://safe.invalid/10001"}},
			check: func(t *testing.T, client *fakeJira, receipt map[string]any) {
				if client.projectCalls != 1 || client.typeCalls != 0 || client.createCalls != 1 || client.issueCalls != 1 {
					t.Fatalf("create preflight/calls = %#v", client)
				}
				want := jira.CreateIssueRequest{ProjectID: "123", IssueTypeID: "456", Summary: "New issue", Description: &description}
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
	mu         sync.Mutex
	policy     writepolicy.Policy
	requireErr error
	gets       int
	sets       int
	clears     int
}

func (r *fakePolicyRegistry) WithPolicyLock(_ context.Context, _ string, fn func() error) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return fn()
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
	canonical, err := writepolicy.CanonicalProjects(projects)
	if err != nil {
		return writepolicy.Policy{}, err
	}
	r.policy = writepolicy.Policy{Profile: value.Name, Identity: writepolicy.IdentityFor(value), Projects: canonical}
	return r.policy, nil
}

func (r *fakePolicyRegistry) Clear(context.Context, string) error {
	r.clears++
	r.policy = writepolicy.Policy{}
	return nil
}

var _ writePolicyRegistry = (*fakePolicyRegistry)(nil)
var _ jiraMutationClient = (*fakeJira)(nil)
