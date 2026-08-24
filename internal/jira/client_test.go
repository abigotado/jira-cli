package jira

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/abigotado/jira-cli/internal/errx"
)

const (
	testEmail = "agent@example.invalid"
	testToken = "TOKEN_SENTINEL_NEVER_LEAK"
)

func newTestClient(t *testing.T, server *httptest.Server, options ...Option) *Client {
	t.Helper()
	baseOptions := []Option{
		WithHTTPClient(server.Client()),
		WithSleep(func(context.Context, time.Duration) error { return nil }),
	}
	baseOptions = append(baseOptions, options...)
	client, err := New(
		Config{SiteURL: "https://example.atlassian.net"},
		Credential{Email: testEmail, Token: testToken, TokenKind: TokenKindClassic},
		baseOptions...,
	)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	// Production construction has already validated an Atlassian URL. Tests
	// in this package alone redirect it to httptest; there is no public base
	// URL override that callers could use to exfiltrate an auth header.
	client.baseURL = server.URL
	return client
}

func TestNewEndpointSelection(t *testing.T) {
	tests := []struct {
		name       string
		config     Config
		credential Credential
		wantBase   string
		wantCode   errx.Code
	}{
		{
			name:       "classic uses validated tenant host",
			config:     Config{SiteURL: "https://team-1.atlassian.net"},
			credential: Credential{Email: testEmail, Token: testToken, TokenKind: TokenKindClassic},
			wantBase:   "https://team-1.atlassian.net",
		},
		{
			name:       "scoped fixes Atlassian API base",
			config:     Config{SiteURL: "https://ignored.atlassian.net"},
			credential: Credential{Email: testEmail, Token: testToken, TokenKind: TokenKindScoped, CloudID: "cloud-id_1"},
			wantBase:   "https://api.atlassian.com/ex/jira/cloud-id_1",
		},
		{
			name:       "classic rejects uppercase host and trailing slash",
			config:     Config{SiteURL: "https://Team-1.atlassian.net/"},
			credential: Credential{Email: testEmail, Token: testToken, TokenKind: TokenKindClassic},
			wantCode:   errx.CodeUsage,
		},
		{
			name:       "scoped rejects path-like cloud id",
			config:     Config{SiteURL: "https://ignored.atlassian.net"},
			credential: Credential{Email: testEmail, Token: testToken, TokenKind: TokenKindScoped, CloudID: "cloud/id"},
			wantCode:   errx.CodeUsage,
		},
		{
			name:       "classic rejects plain HTTP",
			config:     Config{SiteURL: "http://example.atlassian.net"},
			credential: Credential{Email: testEmail, Token: testToken, TokenKind: TokenKindClassic},
			wantCode:   errx.CodeUsage,
		},
		{
			name:       "classic rejects deceptive suffix",
			config:     Config{SiteURL: "https://example.atlassian.net.invalid"},
			credential: Credential{Email: testEmail, Token: testToken, TokenKind: TokenKindClassic},
			wantCode:   errx.CodeUsage,
		},
		{
			name:       "classic rejects credentials in URL",
			config:     Config{SiteURL: "https://user@example.atlassian.net"},
			credential: Credential{Email: testEmail, Token: testToken, TokenKind: TokenKindClassic},
			wantCode:   errx.CodeUsage,
		},
		{
			name:       "classic rejects port",
			config:     Config{SiteURL: "https://example.atlassian.net:443"},
			credential: Credential{Email: testEmail, Token: testToken, TokenKind: TokenKindClassic},
			wantCode:   errx.CodeUsage,
		},
		{
			name:       "classic rejects invalid DNS label",
			config:     Config{SiteURL: "https://example-.atlassian.net"},
			credential: Credential{Email: testEmail, Token: testToken, TokenKind: TokenKindClassic},
			wantCode:   errx.CodeUsage,
		},
		{
			name:       "classic rejects empty query marker",
			config:     Config{SiteURL: "https://example.atlassian.net?"},
			credential: Credential{Email: testEmail, Token: testToken, TokenKind: TokenKindClassic},
			wantCode:   errx.CodeUsage,
		},
		{
			name:       "classic rejects empty fragment marker",
			config:     Config{SiteURL: "https://example.atlassian.net#"},
			credential: Credential{Email: testEmail, Token: testToken, TokenKind: TokenKindClassic},
			wantCode:   errx.CodeUsage,
		},
		{
			name:       "scoped requires cloud id",
			credential: Credential{Email: testEmail, Token: testToken, TokenKind: TokenKindScoped},
			wantCode:   errx.CodeUsage,
		},
		{
			name:       "missing token is auth",
			config:     Config{SiteURL: "https://example.atlassian.net"},
			credential: Credential{Email: testEmail, TokenKind: TokenKindClassic},
			wantCode:   errx.CodeAuth,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client, err := New(test.config, test.credential)
			if test.wantCode != 0 {
				if got := errx.ExitCode(err); got != test.wantCode {
					t.Fatalf("exit code = %d, want %d (err=%v)", got, test.wantCode, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("New() error = %v", err)
			}
			if client.baseURL != test.wantBase {
				t.Errorf("base URL = %q, want %q", client.baseURL, test.wantBase)
			}
		})
	}
}

func TestCredentialFormattingIsAlwaysRedacted(t *testing.T) {
	credential := Credential{Email: testEmail, Token: testToken, TokenKind: TokenKindClassic}
	if _, ok := any(credential).(fmt.Stringer); ok {
		t.Error("Credential must not implement fmt.Stringer")
	}
	if got := fmt.Sprintf("%+v", credential); got != "<redacted>" {
		t.Errorf("formatted credential = %q, want redaction", got)
	}
}

func TestMyselfUsesFixedRouteAndBasicAuth(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet || request.URL.Path != "/rest/api/3/myself" {
			t.Errorf("request = %s %s", request.Method, request.URL.Path)
		}
		email, token, ok := request.BasicAuth()
		if !ok || email != testEmail || token != testToken {
			t.Errorf("BasicAuth() = (%q, %q, %v)", email, token, ok)
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"accountId":"abc","displayName":"Agent","active":true}`))
	}))
	defer server.Close()

	client := newTestClient(t, server)
	user, err := client.Myself(context.Background())
	if err != nil {
		t.Fatalf("Myself() error = %v", err)
	}
	if user.AccountID != "abc" || user.DisplayName != "Agent" || !user.Active {
		t.Errorf("Myself() = %+v", user)
	}
}

func TestRedirectIsRefusedWithoutForwardingAuthorization(t *testing.T) {
	var destinationCalls int
	destination := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
		destinationCalls++
		if request.Header.Get("Authorization") != "" {
			t.Error("redirect destination received Authorization header")
		}
	}))
	defer destination.Close()

	source := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		http.Redirect(writer, &http.Request{}, destination.URL, http.StatusFound)
	}))
	defer source.Close()

	client := newTestClient(t, source, WithHTTPClient(&http.Client{}))
	_, err := client.Myself(context.Background())
	if err == nil {
		t.Fatal("expected redirect refusal error")
	}
	if destinationCalls != 0 {
		t.Errorf("redirect destination calls = %d, want 0", destinationCalls)
	}
}

func TestResponseBodySafety(t *testing.T) {
	tests := []struct {
		name string
		body func(http.ResponseWriter)
	}{
		{
			name: "oversized body is refused",
			body: func(writer http.ResponseWriter) {
				_, _ = io.CopyN(writer, strings.NewReader(strings.Repeat("x", maxResponseBody+1)), maxResponseBody+1)
			},
		},
		{
			name: "non JSON success is translated",
			body: func(writer http.ResponseWriter) {
				_, _ = writer.Write([]byte("UPSTREAM_BODY_SENTINEL"))
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				test.body(writer)
			}))
			defer server.Close()

			client := newTestClient(t, server)
			_, err := client.Myself(context.Background())
			if err == nil {
				t.Fatal("expected error")
			}
			if got := errx.ExitCode(err); got != errx.CodeInternal {
				t.Errorf("exit code = %d, want %d", got, errx.CodeInternal)
			}
			if strings.Contains(err.Error(), "UPSTREAM_BODY_SENTINEL") {
				t.Errorf("error leaked upstream body: %v", err)
			}
			if strings.Contains(err.Error(), testToken) {
				t.Errorf("error leaked credential token: %v", err)
			}
		})
	}
}

func TestDynamicPathCannotPutCredentialIntoErrorOrLog(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	client := newTestClient(t, server, WithLogger(logger))
	_, err := client.Issue(context.Background(), testToken, nil)
	if err == nil {
		t.Fatal("expected not-found error")
	}
	combined := err.Error() + logs.String()
	if strings.Contains(combined, testToken) {
		t.Errorf("dynamic path leaked the credential token: %s", combined)
	}
}

func TestStatusMappingDoesNotExposeUpstreamBody(t *testing.T) {
	tests := []struct {
		name     string
		status   int
		header   http.Header
		body     string
		wantCode errx.Code
	}{
		{"401 is auth", http.StatusUnauthorized, nil, "UPSTREAM_BODY_SENTINEL", errx.CodeAuth},
		{"403 is permission", http.StatusForbidden, nil, "UPSTREAM_BODY_SENTINEL", errx.CodePermission},
		{"403 Seraph denial is auth", http.StatusForbidden, http.Header{"X-Seraph-LoginReason": []string{"AUTHENTICATION_DENIED"}}, "UPSTREAM_BODY_SENTINEL", errx.CodeAuth},
		{"403 CAPTCHA body is auth", http.StatusForbidden, nil, `{"reason":"CAPTCHA_CHALLENGE","detail":"UPSTREAM_BODY_SENTINEL"}`, errx.CodeAuth},
		{"404 is not found", http.StatusNotFound, nil, "UPSTREAM_BODY_SENTINEL", errx.CodeNotFound},
		{"409 is conflict", http.StatusConflict, nil, "UPSTREAM_BODY_SENTINEL", errx.CodeConflict},
		{"429 is retryable", http.StatusTooManyRequests, nil, "UPSTREAM_BODY_SENTINEL", errx.CodeRetryable},
		{"500 is retryable", http.StatusInternalServerError, nil, "UPSTREAM_BODY_SENTINEL", errx.CodeRetryable},
		{"400 is usage", http.StatusBadRequest, nil, "UPSTREAM_BODY_SENTINEL", errx.CodeUsage},
		{"422 is usage", http.StatusUnprocessableEntity, nil, "UPSTREAM_BODY_SENTINEL", errx.CodeUsage},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				for name, values := range test.header {
					for _, value := range values {
						writer.Header().Add(name, value)
					}
				}
				writer.WriteHeader(test.status)
				_, _ = writer.Write([]byte(test.body))
			}))
			defer server.Close()

			client := newTestClient(t, server)
			_, err := client.Myself(context.Background())
			if err == nil {
				t.Fatal("expected error")
			}
			if got := errx.ExitCode(err); got != test.wantCode {
				t.Errorf("exit code = %d, want %d (err=%v)", got, test.wantCode, err)
			}
			if strings.Contains(err.Error(), "UPSTREAM_BODY_SENTINEL") {
				t.Errorf("error leaked upstream body: %v", err)
			}
		})
	}
}

func TestRetryAfterAndSafeReadRetry(t *testing.T) {
	var calls int
	var slept []time.Duration
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		calls++
		if calls == 1 {
			writer.Header().Set("Retry-After", "7")
			writer.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = writer.Write([]byte(`{"accountId":"abc"}`))
	}))
	defer server.Close()

	client := newTestClient(t, server, WithSleep(func(_ context.Context, delay time.Duration) error {
		slept = append(slept, delay)
		return nil
	}))
	if _, err := client.Myself(context.Background()); err != nil {
		t.Fatalf("Myself() error = %v", err)
	}
	if calls != 2 {
		t.Errorf("calls = %d, want 2", calls)
	}
	if len(slept) != 1 || slept[0] != 7*time.Second {
		t.Errorf("slept = %v, want [7s]", slept)
	}
}

func TestEnhancedJQLPostIsRetrySafe(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		calls++
		if request.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", request.Method)
		}
		if calls == 1 {
			writer.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = writer.Write([]byte(`{"issues":[],"isLast":true}`))
	}))
	defer server.Close()

	client := newTestClient(t, server)
	if _, err := client.Search(context.Background(), SearchRequest{JQL: "project = WL"}); err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if calls != 2 {
		t.Errorf("calls = %d, want 2", calls)
	}
}

func TestPostDoesNotInheritRetrySafety(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		calls++
		writer.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()

	client := newTestClient(t, server)
	err := client.do(context.Background(), request{
		method: http.MethodPost,
		path:   "/rest/api/3/future-write",
	}, nil)
	if err == nil {
		t.Fatal("expected server error")
	}
	if calls != 1 {
		t.Errorf("calls = %d, want 1; a future POST must opt into retry safety explicitly", calls)
	}
}

type failingRoundTripper struct{ err error }

func (transport failingRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, transport.err
}

func TestSecretsAreAbsentFromTransportErrorsAndLogs(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	client, err := New(
		Config{SiteURL: "https://example.atlassian.net"},
		Credential{Email: testEmail, Token: testToken, TokenKind: TokenKindClassic},
		WithHTTPClient(&http.Client{Transport: failingRoundTripper{err: errors.New(testToken + " " + testEmail)}}),
		WithLogger(logger),
		WithSleep(func(context.Context, time.Duration) error { return nil }),
	)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	_, requestErr := client.Myself(context.Background())
	if requestErr == nil {
		t.Fatal("expected transport error")
	}
	combined := requestErr.Error() + logs.String()
	for _, secret := range []string{testToken, testEmail, "Authorization", "Basic "} {
		if strings.Contains(combined, secret) {
			t.Errorf("error/log output leaked %q: %s", secret, combined)
		}
	}
}

func TestContextCancellationStopsRetry(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		calls++
		writer.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	client := newTestClient(t, server, WithSleep(func(context.Context, time.Duration) error {
		cancel()
		return context.Canceled
	}))
	_, err := client.Myself(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want wrapped context.Canceled", err)
	}
	if calls != 1 {
		t.Errorf("calls = %d, want 1", calls)
	}
}

func TestRetrySleepErrorCannotLeakCredential(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()

	client := newTestClient(t, server, WithSleep(func(context.Context, time.Duration) error {
		return errors.New(testToken)
	}))
	_, err := client.Myself(context.Background())
	if err == nil {
		t.Fatal("expected retry delay error")
	}
	if strings.Contains(err.Error(), testToken) {
		t.Errorf("retry delay error leaked credential token: %v", err)
	}
}

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, time.August, 24, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name  string
		value string
		want  time.Duration
	}{
		{"seconds", " 5 ", 5 * time.Second},
		{"HTTP date", "Mon, 24 Aug 2026 12:00:07 GMT", 7 * time.Second},
		{"past HTTP date", "Mon, 24 Aug 2026 11:59:59 GMT", 0},
		{"zero", "0", 0},
		{"invalid", "later", 0},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := parseRetryAfter(test.value, now); got != test.want {
				t.Errorf("parseRetryAfter(%q) = %v, want %v", test.value, got, test.want)
			}
		})
	}
}

func TestConcurrentLoggingDoesNotLeak(t *testing.T) {
	var buffer lockedBuffer
	logger := slog.New(slog.NewTextHandler(&buffer, &slog.HandlerOptions{Level: slog.LevelDebug}))
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte(`{"accountId":"abc"}`))
	}))
	defer server.Close()
	client := newTestClient(t, server, WithLogger(logger))

	var group sync.WaitGroup
	for range 4 {
		group.Add(1)
		go func() {
			defer group.Done()
			_, _ = client.Myself(context.Background())
		}()
	}
	group.Wait()
	if strings.Contains(buffer.String(), testToken) {
		t.Errorf("logs leaked token: %s", buffer.String())
	}
}

type lockedBuffer struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (buffer *lockedBuffer) Write(value []byte) (int, error) {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return buffer.buffer.Write(value)
}

func (buffer *lockedBuffer) String() string {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return buffer.buffer.String()
}
