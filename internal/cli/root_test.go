package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
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

func TestNetworkCommandRequiresExplicitProfileWithoutReadingCredential(t *testing.T) {
	t.Parallel()

	store := &fakeStore{credential: auth.Credential{Token: "must-not-appear"}}
	app, stdout, _ := testApp(store, &fakeJira{})
	code := app.Run(context.Background(), app.NewRootCommand(), []string{"--output", "json", "me"})

	if code != errx.CodeUsage {
		t.Fatalf("code = %d, want %d", code, errx.CodeUsage)
	}
	if store.loads != 0 {
		t.Fatalf("credential loads = %d, want 0", store.loads)
	}
	assertErrorReason(t, stdout.Bytes(), "PROFILE_REQUIRED")
	if strings.Contains(stdout.String(), store.credential.Token) {
		t.Fatal("stdout contains credential")
	}
}

func TestIssuesSearchPreservesOpaqueCursorAndContext(t *testing.T) {
	t.Parallel()

	reader := &fakeJira{searchPage: jira.SearchPage{
		Issues:        []jira.Issue{{ID: "10001", Key: "WL-42"}},
		NextPageToken: "opaque+/cursor==",
	}}
	app, stdout, _ := testApp(&fakeStore{credential: auth.Credential{Token: "sentinel-token"}}, reader)
	code := app.Run(context.Background(), app.NewRootCommand(), []string{
		"--output", "json", "--profile", "work", "issues", "search", "--jql", "project = WL", "--limit", "1",
	})

	if code != errx.CodeOK {
		t.Fatalf("code = %d, stdout = %s", code, stdout.String())
	}
	if reader.searchRequest.JQL != "project = WL" || reader.searchRequest.MaxResults != 1 {
		t.Fatalf("search request = %#v", reader.searchRequest)
	}
	var envelope output.Envelope
	if err := json.Unmarshal(stdout.Bytes(), &envelope); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	if envelope.Meta == nil || envelope.Meta.Profile != "work" || envelope.Meta.Site != "https://example.atlassian.net" {
		t.Fatalf("meta = %#v", envelope.Meta)
	}
	if envelope.Meta.NextCursor != reader.searchPage.NextPageToken || envelope.Meta.Truncated == nil || !*envelope.Meta.Truncated {
		t.Fatalf("pagination meta = %#v", envelope.Meta)
	}
	if strings.Contains(stdout.String(), "sentinel-token") {
		t.Fatal("stdout contains credential")
	}
}

func TestAuthListDoesNotReadKeychain(t *testing.T) {
	t.Parallel()

	store := &fakeStore{credential: auth.Credential{Token: "sentinel-token"}}
	app, stdout, _ := testApp(store, &fakeJira{})
	code := app.Run(context.Background(), app.NewRootCommand(), []string{"--output", "json", "auth", "list"})

	if code != errx.CodeOK {
		t.Fatalf("code = %d, stdout = %s", code, stdout.String())
	}
	if store.loads != 0 {
		t.Fatalf("credential loads = %d, want 0", store.loads)
	}
	if strings.Contains(stdout.String(), store.credential.Token) {
		t.Fatal("stdout contains credential")
	}
}

func TestHiddenJSONAliasAndHelpKeepStreamsPredictable(t *testing.T) {
	t.Parallel()

	app, stdout, stderr := testApp(&fakeStore{}, &fakeJira{})
	code := app.Run(context.Background(), app.NewRootCommand(), []string{"--json", "version"})
	if code != errx.CodeOK || !strings.Contains(stdout.String(), `"ok":true`) {
		t.Fatalf("JSON alias: code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}

	app, stdout, stderr = testApp(&fakeStore{}, &fakeJira{})
	code = app.Run(context.Background(), app.NewRootCommand(), []string{"--help"})
	if code != errx.CodeOK || !strings.Contains(stdout.String(), "Usage:") || stderr.Len() != 0 {
		t.Fatalf("help: code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

func TestCommandGroupWithoutLeafReturnsUsageEnvelope(t *testing.T) {
	t.Parallel()

	for _, group := range []string{"auth", "skills", "projects", "issues", "comments"} {
		t.Run(group, func(t *testing.T) {
			app, stdout, _ := testApp(&fakeStore{}, &fakeJira{})
			code := app.Run(context.Background(), app.NewRootCommand(), []string{"--output", "json", group})
			if code != errx.CodeUsage {
				t.Fatalf("code = %d, stdout = %s", code, stdout.String())
			}
			assertErrorReason(t, stdout.Bytes(), "USAGE")
		})
	}
}

func TestLocalMutationFlagsFailBeforeCredentialStore(t *testing.T) {
	t.Parallel()

	tests := [][]string{
		{"--output", "json", "--profile", "work", "--dry-run", "--yes", "auth", "logout"},
		{"--output", "json", "--profile", "work", "--fields", "profile", "--yes", "auth", "logout"},
	}
	for _, args := range tests {
		store := &fakeStore{credential: auth.Credential{Token: "sentinel-token"}}
		app, stdout, _ := testApp(store, &fakeJira{})
		if code := app.Run(context.Background(), app.NewRootCommand(), args); code != errx.CodeUsage {
			t.Fatalf("args=%v code=%d stdout=%s", args, code, stdout.String())
		}
		if store.deletes != 0 || store.saves != 0 || store.loads != 0 {
			t.Fatalf("args=%v mutated or read store: %#v", args, store)
		}
	}
}

func TestPanicIsSanitizedIntoOneInternalEnvelope(t *testing.T) {
	t.Parallel()

	const sentinel = "PANIC_TOKEN_SENTINEL"
	app, stdout, stderr := testApp(&fakeStore{credential: auth.Credential{Token: "stored-sentinel"}}, &fakeJira{panicValue: sentinel})
	code := app.Run(context.Background(), app.NewRootCommand(), []string{"--output", "json", "--profile", "work", "me"})
	if code != errx.CodeInternal {
		t.Fatalf("code = %d, want %d", code, errx.CodeInternal)
	}
	assertErrorReason(t, stdout.Bytes(), "INTERNAL")
	combined := stdout.String() + stderr.String()
	if strings.Contains(combined, sentinel) || strings.Contains(combined, "goroutine") {
		t.Fatalf("panic details leaked: %s", combined)
	}
}

func TestRepeatedSearchCursorFailsInsteadOfLooping(t *testing.T) {
	t.Parallel()

	reader := &fakeJira{searchPage: jira.SearchPage{NextPageToken: "same", IsLast: false}}
	app, stdout, _ := testApp(&fakeStore{credential: auth.Credential{Token: "sentinel-token"}}, reader)
	code := app.Run(context.Background(), app.NewRootCommand(), []string{
		"--output", "json", "--profile", "work", "issues", "search", "--jql", "project = WL", "--cursor", "same",
	})
	if code != errx.CodeInternal {
		t.Fatalf("code = %d, stdout = %s", code, stdout.String())
	}
	assertErrorReason(t, stdout.Bytes(), "INTERNAL")
}

func TestFinalSearchPageCannotExposeNextCursor(t *testing.T) {
	t.Parallel()

	reader := &fakeJira{searchPage: jira.SearchPage{NextPageToken: "contradictory", IsLast: true}}
	app, stdout, _ := testApp(&fakeStore{credential: auth.Credential{Token: "sentinel-token"}}, reader)
	code := app.Run(context.Background(), app.NewRootCommand(), []string{
		"--output", "json", "--profile", "work", "issues", "search", "--jql", "project = WL",
	})
	if code != errx.CodeInternal {
		t.Fatalf("code = %d, stdout = %s", code, stdout.String())
	}
	assertErrorReason(t, stdout.Bytes(), "INTERNAL")
}

func TestNonFinalSearchPageRequiresNextCursor(t *testing.T) {
	t.Parallel()

	reader := &fakeJira{searchPage: jira.SearchPage{IsLast: false}}
	app, stdout, _ := testApp(&fakeStore{credential: auth.Credential{Token: "sentinel-token"}}, reader)
	code := app.Run(context.Background(), app.NewRootCommand(), []string{
		"--output", "json", "--profile", "work", "issues", "search", "--jql", "project = WL",
	})
	if code != errx.CodeInternal {
		t.Fatalf("code = %d, stdout = %s", code, stdout.String())
	}
	assertErrorReason(t, stdout.Bytes(), "INTERNAL")
}

func TestExpiredProfileFailsBeforeKeychainLoad(t *testing.T) {
	t.Parallel()

	expires := time.Date(2026, time.August, 24, 0, 0, 0, 0, time.UTC)
	store := &fakeStore{credential: auth.Credential{Token: "sentinel-token"}}
	app, stdout, _ := testApp(store, &fakeJira{})
	app.now = func() time.Time { return expires }
	app.registry.(*fakeRegistry).profiles[0].ExpiresAt = &expires
	code := app.Run(context.Background(), app.NewRootCommand(), []string{"--output", "json", "--profile", "work", "me"})
	if code != errx.CodeAuth {
		t.Fatalf("code = %d, stdout = %s", code, stdout.String())
	}
	if store.loads != 0 {
		t.Fatalf("credential loads = %d, want 0", store.loads)
	}
	assertErrorReason(t, stdout.Bytes(), "TOKEN_EXPIRED")
}

func TestReadAndWriteCommandsShareExpiredProfileBoundary(t *testing.T) {
	expires := time.Date(2026, time.August, 24, 0, 0, 0, 0, time.UTC)
	tests := []struct {
		name string
		args []string
	}{
		{name: "read", args: []string{"--profile", "work", "me"}},
		{name: "write", args: []string{"--profile", "work", "--yes", "comments", "add", "--issue", "WL-1", "--body", "safe"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := &fakeStore{credential: auth.Credential{Token: "KEYCHAIN_TOKEN_SENTINEL"}}
			client := &fakeJira{}
			app, stdout, _ := testApp(store, client)
			app.now = func() time.Time { return expires }
			app.registry.(*fakeRegistry).profiles[0].ExpiresAt = &expires
			code := app.Run(context.Background(), app.NewRootCommand(), append([]string{"--output", "json"}, test.args...))
			if code != errx.CodeAuth {
				t.Fatalf("code = %d, want %d stdout=%s", code, errx.CodeAuth, stdout.String())
			}
			assertErrorReason(t, stdout.Bytes(), "TOKEN_EXPIRED")
			if store.loads != 0 || client.totalCalls() != 0 {
				t.Fatalf("expired profile crossed credential/Jira boundary: loads=%d calls=%d", store.loads, client.totalCalls())
			}
		})
	}
}

func TestReadAndWriteCommandsShareCredentialLoadBoundary(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{name: "read", args: []string{"--profile", "work", "me"}},
		{name: "write", args: []string{"--profile", "work", "--yes", "comments", "add", "--issue", "WL-1", "--body", "safe"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := &fakeStore{}
			client := &fakeJira{}
			app, stdout, _ := testApp(store, client)
			code := app.Run(context.Background(), app.NewRootCommand(), append([]string{"--output", "json"}, test.args...))
			if code != errx.CodeAuth {
				t.Fatalf("code = %d, want %d stdout=%s", code, errx.CodeAuth, stdout.String())
			}
			assertErrorReason(t, stdout.Bytes(), "CREDENTIAL_NOT_FOUND")
			if store.loads != 1 || client.totalCalls() != 0 {
				t.Fatalf("credential boundary = loads:%d Jira calls:%d, want loads:1 calls:0", store.loads, client.totalCalls())
			}
		})
	}
}

func TestLocalLockTimeoutIsRetryableBeforeCredentialsOrJira(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		profileErr error
		policyErr  error
		setErr     error
		clearErr   error
		wantSets   int
		wantClears int
	}{
		{name: "read profile lock", args: []string{"--profile", "work", "me"}, profileErr: &lockfile.TimeoutError{Path: "PROFILE_LOCK_PATH_SENTINEL", Timeout: time.Second}},
		{name: "write profile lock", args: []string{"--profile", "work", "--yes", "comments", "add", "--issue", "WL-1", "--body", "safe"}, profileErr: &lockfile.TimeoutError{Path: "PROFILE_LOCK_PATH_SENTINEL", Timeout: time.Second}},
		{name: "write policy lock", args: []string{"--profile", "work", "--yes", "comments", "add", "--issue", "WL-1", "--body", "safe"}, policyErr: &lockfile.TimeoutError{Path: "POLICY_LOCK_PATH_SENTINEL", Timeout: time.Second}},
		{name: "set profile lock", args: []string{"--profile", "work", "--yes", "auth", "allow-projects", "set", "--project", "WL"}, profileErr: &lockfile.TimeoutError{Path: "PROFILE_LOCK_PATH_SENTINEL", Timeout: time.Second}},
		{name: "set policy lock", args: []string{"--profile", "work", "--yes", "auth", "allow-projects", "set", "--project", "WL"}, policyErr: &lockfile.TimeoutError{Path: "POLICY_LOCK_PATH_SENTINEL", Timeout: time.Second}},
		{name: "set local mutation lock", args: []string{"--profile", "work", "--yes", "auth", "allow-projects", "set", "--project", "WL"}, setErr: &lockfile.TimeoutError{Path: "SET_LOCK_PATH_SENTINEL", Timeout: time.Second}, wantSets: 1},
		{name: "clear profile lock", args: []string{"--profile", "work", "--yes", "auth", "allow-projects", "clear"}, profileErr: &lockfile.TimeoutError{Path: "PROFILE_LOCK_PATH_SENTINEL", Timeout: time.Second}},
		{name: "clear policy lock", args: []string{"--profile", "work", "--yes", "auth", "allow-projects", "clear"}, policyErr: &lockfile.TimeoutError{Path: "POLICY_LOCK_PATH_SENTINEL", Timeout: time.Second}},
		{name: "clear local mutation lock", args: []string{"--profile", "work", "--yes", "auth", "allow-projects", "clear"}, clearErr: &lockfile.TimeoutError{Path: "CLEAR_LOCK_PATH_SENTINEL", Timeout: time.Second}, wantClears: 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := &fakeStore{credential: auth.Credential{Token: "KEYCHAIN_TOKEN_SENTINEL"}}
			client := &fakeJira{}
			app, stdout, stderr := testApp(store, client)
			app.registry.(*fakeRegistry).lockErr = test.profileErr
			policies := app.policies.(*fakePolicyRegistry)
			policies.lockErr = test.policyErr
			policies.setErr = test.setErr
			policies.clearErr = test.clearErr
			beforePolicy := strings.Join(policies.policy.Projects, ",")
			code := app.Run(context.Background(), app.NewRootCommand(), append([]string{"--output", "json"}, test.args...))
			if code != errx.CodeRetryable {
				t.Fatalf("code = %d, want %d stdout=%s", code, errx.CodeRetryable, stdout.String())
			}
			assertErrorReason(t, stdout.Bytes(), "LOCAL_LOCK_BUSY")
			if store.loads != 0 || client.totalCalls() != 0 {
				t.Fatalf("lock timeout crossed credential/Jira boundary: loads=%d calls=%d", store.loads, client.totalCalls())
			}
			if policies.sets != test.wantSets || policies.clears != test.wantClears {
				t.Fatalf("policy mutation calls = set:%d clear:%d, want set:%d clear:%d", policies.sets, policies.clears, test.wantSets, test.wantClears)
			}
			if got := strings.Join(policies.policy.Projects, ","); got != beforePolicy {
				t.Fatalf("lock timeout changed policy from %q to %q", beforePolicy, got)
			}
			combined := stdout.String() + stderr.String()
			for _, sentinel := range []string{"PROFILE_LOCK_PATH_SENTINEL", "POLICY_LOCK_PATH_SENTINEL", "SET_LOCK_PATH_SENTINEL", "CLEAR_LOCK_PATH_SENTINEL", store.credential.Token} {
				if strings.Contains(combined, sentinel) {
					t.Fatalf("output leaked %q: %s", sentinel, combined)
				}
			}
		})
	}
}

func TestPostCredentialTransactionTimeoutIsNotReportedAsLockBusy(t *testing.T) {
	timeout := &lockfile.TimeoutError{Path: "POST_OPERATION_LOCK_PATH_SENTINEL", Timeout: time.Second}
	tests := []struct {
		name             string
		args             []string
		stdin            string
		configure        func(*fakeStore, *fakeRegistry)
		assertFinalState func(*testing.T, *fakeStore, *fakeRegistry)
	}{
		{
			name:  "wrapped timeout after login credential save",
			args:  []string{"--profile", "work", "--yes", "auth", "login", "--site", "https://example.atlassian.net", "--email", "user@example.com", "--token-stdin"},
			stdin: "NEW_LOGIN_TOKEN_SENTINEL\n",
			configure: func(_ *fakeStore, registry *fakeRegistry) {
				registry.postLockErr = fmt.Errorf("release profile lock: %w", timeout)
			},
			assertFinalState: func(t *testing.T, store *fakeStore, registry *fakeRegistry) {
				if store.saves != 1 || store.credential.Token != "NEW_LOGIN_TOKEN_SENTINEL" {
					t.Fatalf("login state = saves:%d credential:%v, want saved replacement", store.saves, store.credential)
				}
				if len(registry.profiles) != 1 {
					t.Fatalf("profiles = %#v, want existing coherent profile", registry.profiles)
				}
			},
		},
		{
			name:  "joined timeout after login credential save",
			args:  []string{"--profile", "work", "--yes", "auth", "login", "--site", "https://example.atlassian.net", "--email", "user@example.com", "--token-stdin"},
			stdin: "NEW_LOGIN_TOKEN_SENTINEL\n",
			configure: func(_ *fakeStore, registry *fakeRegistry) {
				registry.postLockErr = errors.Join(errors.New("release profile lock failed"), timeout)
			},
			assertFinalState: func(t *testing.T, store *fakeStore, _ *fakeRegistry) {
				if store.saves != 1 || store.credential.Token != "NEW_LOGIN_TOKEN_SENTINEL" {
					t.Fatalf("login state = saves:%d credential:%v, want saved replacement", store.saves, store.credential)
				}
			},
		},
		{
			name: "wrapped timeout after logout delete and metadata removal",
			args: []string{"--profile", "work", "--yes", "auth", "logout"},
			configure: func(_ *fakeStore, registry *fakeRegistry) {
				registry.postLockErr = fmt.Errorf("release profile lock: %w", timeout)
			},
			assertFinalState: func(t *testing.T, store *fakeStore, registry *fakeRegistry) {
				if store.deletes != 1 || store.credential.Token != "" || len(registry.profiles) != 0 {
					t.Fatalf("logout state = deletes:%d credential:%v profiles:%#v, want applied removal", store.deletes, store.credential, registry.profiles)
				}
			},
		},
		{
			name: "joined timeout from failed logout compensation",
			args: []string{"--profile", "work", "--yes", "auth", "logout"},
			configure: func(store *fakeStore, registry *fakeRegistry) {
				registry.removeErr = errors.New("profile metadata removal failed")
				store.saveErr = fmt.Errorf("restore credential: %w", timeout)
			},
			assertFinalState: func(t *testing.T, store *fakeStore, registry *fakeRegistry) {
				if store.deletes != 1 || store.saves != 1 || store.credential.Token != "" {
					t.Fatalf("compensation state = deletes:%d saves:%d credential:%v, want explicit failed restore", store.deletes, store.saves, store.credential)
				}
				if len(registry.profiles) != 1 {
					t.Fatalf("profile metadata changed despite pre-commit failure: %#v", registry.profiles)
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := &fakeStore{credential: auth.Credential{Token: "OLD_CREDENTIAL_SENTINEL"}}
			app, stdout, stderr := testApp(store, &fakeJira{})
			registry := app.registry.(*fakeRegistry)
			test.configure(store, registry)
			app.stdin = bytes.NewBufferString(test.stdin)
			code := app.Run(context.Background(), app.NewRootCommand(), append([]string{"--output", "json"}, test.args...))
			if code != errx.CodeInternal {
				t.Fatalf("code = %d, want %d stdout=%s stderr=%s", code, errx.CodeInternal, stdout.String(), stderr.String())
			}
			assertErrorReason(t, stdout.Bytes(), "INTERNAL")
			if strings.Contains(stdout.String(), "LOCAL_LOCK_BUSY") {
				t.Fatalf("post-operation timeout was misreported as retryable: %s", stdout.String())
			}
			test.assertFinalState(t, store, registry)
			combined := stdout.String() + stderr.String()
			for _, sentinel := range []string{"POST_OPERATION_LOCK_PATH_SENTINEL", "NEW_LOGIN_TOKEN_SENTINEL", "OLD_CREDENTIAL_SENTINEL"} {
				if strings.Contains(combined, sentinel) {
					t.Fatalf("output leaked %q: %s", sentinel, combined)
				}
			}
		})
	}
}

func TestRawIssueOutputsPreserveModeledData(t *testing.T) {
	t.Parallel()

	reader := &fakeJira{
		issue:      jira.Issue{ID: "7", Key: "WL-7"},
		searchPage: jira.SearchPage{Issues: []jira.Issue{{ID: "8", Key: "FL-8"}}, IsLast: true},
	}
	for _, args := range [][]string{
		{"--output", "raw", "--profile", "work", "issues", "get", "WL-7"},
		{"--output", "raw", "--profile", "work", "issues", "search", "--jql", "project = FL"},
	} {
		app, stdout, _ := testApp(&fakeStore{credential: auth.Credential{Token: "sentinel-token"}}, reader)
		if code := app.Run(context.Background(), app.NewRootCommand(), args); code != errx.CodeOK {
			t.Fatalf("args=%v code=%d stdout=%s", args, code, stdout.String())
		}
		if stdout.String() == "{}\n" || stdout.String() == "[{}]\n" {
			t.Fatalf("args=%v lost raw issue data", args)
		}
	}
}

func TestEmptyIssuePageAcceptsRequestedDynamicField(t *testing.T) {
	t.Parallel()

	app, stdout, _ := testApp(
		&fakeStore{credential: auth.Credential{Token: "sentinel-token"}},
		&fakeJira{searchPage: jira.SearchPage{Issues: []jira.Issue{}, IsLast: true}},
	)
	code := app.Run(context.Background(), app.NewRootCommand(), []string{
		"--output", "json", "--profile", "work", "--fields", "key,customfield_12345",
		"issues", "search", "--jql", "project = WL",
	})
	if code != errx.CodeOK {
		t.Fatalf("code=%d stdout=%s", code, stdout.String())
	}
}

func TestClientSnapshotWaitsForProfileTransactionLock(t *testing.T) {
	store := &fakeStore{credential: auth.Credential{Token: "sentinel-token"}}
	app, _, _ := testApp(store, &fakeJira{})
	registry := app.registry.(*fakeRegistry)
	entered := make(chan struct{})
	release := make(chan struct{})
	holderDone := make(chan error, 1)
	go func() {
		holderDone <- registry.WithProfileLock(context.Background(), "work", func() error {
			close(entered)
			<-release
			return nil
		})
	}()
	<-entered
	commandDone := make(chan errx.Code, 1)
	go func() {
		commandDone <- app.Run(context.Background(), app.NewRootCommand(), []string{"--output", "json", "--profile", "work", "me"})
	}()
	time.Sleep(30 * time.Millisecond)
	if store.loads != 0 {
		t.Fatalf("credential loaded before coherent profile lock released")
	}
	close(release)
	if err := <-holderDone; err != nil {
		t.Fatal(err)
	}
	if code := <-commandDone; code != errx.CodeOK {
		t.Fatalf("code=%d", code)
	}
}

func testApp(store *fakeStore, reader *fakeJira) (*App, *bytes.Buffer, *bytes.Buffer) {
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	app := &App{
		registry: &fakeRegistry{profiles: []profile.Profile{{
			Name: "work", Site: "https://example.atlassian.net", Email: "user@example.com", TokenKind: profile.TokenKindClassic,
		}}},
		policies: &fakePolicyRegistry{policy: writepolicy.Policy{
			Profile: "work",
			Identity: writepolicy.Identity{
				Site: "https://example.atlassian.net", Email: "user@example.com", TokenKind: string(profile.TokenKindClassic),
			},
			Projects: []string{"FL", "WL"},
		}},
		store:  store,
		stdin:  bytes.NewBuffer(nil),
		stdout: stdout,
		stderr: stderr,
		now:    time.Now,
	}
	app.newJira = func(profile.Profile, auth.Credential, *slog.Logger) (jiraReader, error) { return reader, nil }
	app.discoverCloudID = func(context.Context, string) (string, error) { return "cloud-id", nil }
	return app, stdout, stderr
}

func assertErrorReason(t *testing.T, raw []byte, reason string) {
	t.Helper()
	var envelope output.Envelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatalf("decode envelope: %v\n%s", err, raw)
	}
	if envelope.Error == nil || envelope.Error.Code != reason {
		t.Fatalf("error = %#v, want reason %q", envelope.Error, reason)
	}
}

func assertErrorHintContains(t *testing.T, raw []byte, want string) {
	t.Helper()
	var envelope output.Envelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatalf("decode envelope: %v\n%s", err, raw)
	}
	if !strings.Contains(envelope.Hint, want) {
		t.Fatalf("hint = %q, want substring %q", envelope.Hint, want)
	}
}

func assertSingleFailureEnvelope(t *testing.T, raw []byte, reason string) {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 1 {
		t.Fatalf("stdout contains %d envelopes, want exactly one failure envelope:\n%s", len(lines), raw)
	}
	var envelope output.Envelope
	if err := json.Unmarshal([]byte(lines[0]), &envelope); err != nil {
		t.Fatalf("decode envelope: %v\n%s", err, raw)
	}
	if envelope.OK || envelope.Data != nil || envelope.Error == nil || envelope.Error.Code != reason {
		t.Fatalf("envelope = %#v, want only failure reason %q", envelope, reason)
	}
}

type fakeRegistry struct {
	mu          sync.Mutex
	profiles    []profile.Profile
	lockErr     error
	postLockErr error
	addErr      error
	putErr      error
	removeErr   error
}

func (r *fakeRegistry) List(context.Context) ([]profile.Profile, error) {
	return append([]profile.Profile(nil), r.profiles...), nil
}
func (r *fakeRegistry) WithProfileLock(_ context.Context, _ string, fn func() error) error {
	if r.lockErr != nil {
		return r.lockErr
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	err := fn()
	if r.postLockErr != nil {
		return errors.Join(err, r.postLockErr)
	}
	return err
}
func (r *fakeRegistry) Get(_ context.Context, name string) (profile.Profile, error) {
	for _, candidate := range r.profiles {
		if candidate.Name == name {
			return candidate, nil
		}
	}
	return profile.Profile{}, profile.ErrNotFound
}
func (r *fakeRegistry) Add(_ context.Context, value profile.Profile) error {
	if r.addErr != nil {
		return r.addErr
	}
	r.profiles = append(r.profiles, value)
	return nil
}
func (r *fakeRegistry) Put(_ context.Context, value profile.Profile) error {
	if r.putErr != nil {
		return r.putErr
	}
	for index := range r.profiles {
		if r.profiles[index].Name == value.Name {
			r.profiles[index] = value
			return nil
		}
	}
	r.profiles = append(r.profiles, value)
	return nil
}
func (r *fakeRegistry) Remove(_ context.Context, name string) error {
	if r.removeErr != nil {
		return r.removeErr
	}
	for index := range r.profiles {
		if r.profiles[index].Name == name {
			r.profiles = append(r.profiles[:index], r.profiles[index+1:]...)
			return nil
		}
	}
	return profile.ErrNotFound
}

type fakeStore struct {
	credential auth.Credential
	saveErr    error
	deleteErr  error
	loads      int
	saves      int
	deletes    int
}

func (s *fakeStore) Load(context.Context, string) (auth.Credential, error) {
	s.loads++
	if err := s.credential.Validate(); err != nil {
		return auth.Credential{}, auth.ErrNotFound
	}
	return s.credential, nil
}
func (s *fakeStore) Save(_ context.Context, _ string, credential auth.Credential) error {
	s.saves++
	if s.saveErr != nil {
		return s.saveErr
	}
	s.credential = credential
	return nil
}
func (s *fakeStore) Delete(context.Context, string) error {
	s.deletes++
	if s.deleteErr != nil {
		return s.deleteErr
	}
	if s.credential.Token == "" {
		return auth.ErrNotFound
	}
	s.credential = auth.Credential{}
	return nil
}

type fakeJira struct {
	searchPage         jira.SearchPage
	searchRequest      jira.SearchRequest
	panicValue         any
	issue              jira.Issue
	issueSequence      []jira.Issue
	project            jira.Project
	issueTypes         jira.IssueTypePage
	issueTypeProjectID string
	issueTypeOptions   jira.IssueTypePageOptions
	transitions        []jira.Transition
	created            jira.Issue
	comment            jira.Comment
	projectCalls       int
	issueCalls         int
	typeCalls          int
	createCalls        int
	editCalls          int
	transitionCalls    int
	commentCalls       int
	createInput        jira.CreateIssueRequest
	createErr          error
	editIssueID        string
	editInput          jira.EditIssueRequest
	transitionIssueID  string
	transitionID       string
	commentIssueID     string
	commentBody        string
}

func (f *fakeJira) Myself(context.Context) (jira.User, error) {
	if f.panicValue != nil {
		panic(f.panicValue)
	}
	return jira.User{}, nil
}
func (*fakeJira) Projects(context.Context, jira.ProjectPageOptions) (jira.ProjectPage, error) {
	return jira.ProjectPage{}, nil
}
func (f *fakeJira) Project(context.Context, string) (jira.Project, error) {
	f.projectCalls++
	return f.project, nil
}
func (f *fakeJira) Issue(context.Context, string, []string) (jira.Issue, error) {
	f.issueCalls++
	if len(f.issueSequence) > 0 {
		index := f.issueCalls - 1
		if index >= len(f.issueSequence) {
			index = len(f.issueSequence) - 1
		}
		return f.issueSequence[index], nil
	}
	return f.issue, nil
}
func (f *fakeJira) Search(_ context.Context, request jira.SearchRequest) (jira.SearchPage, error) {
	f.searchRequest = request
	return f.searchPage, nil
}
func (f *fakeJira) Transitions(context.Context, string) ([]jira.Transition, error) {
	return f.transitions, nil
}
func (*fakeJira) Comments(context.Context, string, jira.CommentPageOptions) (jira.CommentPage, error) {
	return jira.CommentPage{}, nil
}

func (f *fakeJira) IssueTypes(_ context.Context, projectID string, options jira.IssueTypePageOptions) (jira.IssueTypePage, error) {
	f.typeCalls++
	f.issueTypeProjectID, f.issueTypeOptions = projectID, options
	return f.issueTypes, nil
}

func (f *fakeJira) CreateIssue(_ context.Context, input jira.CreateIssueRequest) (jira.Issue, error) {
	f.createCalls++
	f.createInput = input
	return f.created, f.createErr
}

func (f *fakeJira) EditIssue(_ context.Context, issueID string, input jira.EditIssueRequest) error {
	f.editCalls++
	f.editIssueID, f.editInput = issueID, input
	return nil
}

func (f *fakeJira) TransitionIssue(_ context.Context, issueID, transitionID string) error {
	f.transitionCalls++
	f.transitionIssueID, f.transitionID = issueID, transitionID
	return nil
}

func (f *fakeJira) AddComment(_ context.Context, issueID, body string) (jira.Comment, error) {
	f.commentCalls++
	f.commentIssueID, f.commentBody = issueID, body
	return f.comment, nil
}
