package jira

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/abigotado/jira-cli/internal/errx"
)

func TestNewPlainTextDocumentPreservesBoundedPlainText(t *testing.T) {
	tests := []struct {
		name       string
		value      string
		wantText   string
		wantLeaves int
		wantCode   errx.Code
	}{
		{name: "unicode and newlines are preserved", value: "Привет 👋\nsecond line", wantText: "Привет 👋\nsecond line", wantLeaves: 3},
		{name: "CRLF and CR become deterministic line breaks", value: "one\r\ntwo\rthree", wantText: "one\ntwo\nthree", wantLeaves: 5},
		{name: "blank lines are preserved", value: "one\n\nthree", wantText: "one\n\nthree", wantLeaves: 4},
		{name: "empty document is one empty paragraph", value: "", wantLeaves: 0},
		{name: "maximum rune count is accepted", value: strings.Repeat("界", maxBodyRunes), wantText: strings.Repeat("界", maxBodyRunes), wantLeaves: 1},
		{name: "one rune beyond limit is rejected", value: strings.Repeat("界", maxBodyRunes+1), wantCode: errx.CodeUsage},
		{name: "NUL is rejected", value: "before\x00after", wantCode: errx.CodeUsage},
		{name: "invalid UTF-8 is rejected", value: string([]byte{0xff}), wantCode: errx.CodeUsage},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			document, err := NewPlainTextDocument(test.value, "body")
			if test.wantCode != 0 {
				if got := errx.ExitCode(err); got != test.wantCode {
					t.Fatalf("exit code = %d, want %d (err=%v)", got, test.wantCode, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("NewPlainTextDocument() error = %v", err)
			}
			if document.Version != 1 || document.Type != "doc" || len(document.Content) != 1 {
				t.Fatalf("document = %#v", document)
			}
			if len(document.Content[0].Content) != test.wantLeaves {
				t.Fatalf("leaves = %#v, want %d", document.Content[0].Content, test.wantLeaves)
			}
			if test.wantLeaves > 0 && plainTextFromADF(document) != test.wantText {
				t.Fatalf("text changed: %q", plainTextFromADF(document))
			}
		})
	}
}

func TestValidateSummaryUsesRuneBoundsAndSingleLineContract(t *testing.T) {
	tests := []struct {
		name     string
		summary  string
		wantCode errx.Code
	}{
		{name: "unicode at rune limit is accepted", summary: strings.Repeat("界", maxSummaryRunes)},
		{name: "one rune beyond limit is rejected", summary: strings.Repeat("界", maxSummaryRunes+1), wantCode: errx.CodeUsage},
		{name: "leading whitespace is rejected", summary: " summary", wantCode: errx.CodeUsage},
		{name: "newline is rejected", summary: "one\ntwo", wantCode: errx.CodeUsage},
		{name: "tab is rejected", summary: "one\ttwo", wantCode: errx.CodeUsage},
		{name: "Unicode next line is rejected", summary: "one\u0085two", wantCode: errx.CodeUsage},
		{name: "Unicode line separator is rejected", summary: "one\u2028two", wantCode: errx.CodeUsage},
		{name: "Unicode paragraph separator is rejected", summary: "one\u2029two", wantCode: errx.CodeUsage},
		{name: "empty is rejected", wantCode: errx.CodeUsage},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := ValidateSummary(test.summary)
			if got := errx.ExitCode(err); got != test.wantCode {
				t.Fatalf("exit code = %d, want %d (err=%v, runes=%d)", got, test.wantCode, err, utf8.RuneCountInString(test.summary))
			}
		})
	}
}

func TestWriteResourcesUseNumericIDRoutesAndExactPayloads(t *testing.T) {
	description := "line one\nстрока два"
	summary := "Updated summary"
	tests := []struct {
		name       string
		method     string
		path       string
		status     int
		response   string
		call       func(context.Context, *Client) error
		assertBody func(*testing.T, map[string]any)
	}{
		{
			name: "create uses project and issue type IDs with ADF description", method: http.MethodPost, path: "/rest/api/3/issue", status: http.StatusCreated, response: `{"id":"10001","key":"WL-7","self":"https://safe.invalid/10001"}`,
			call: func(ctx context.Context, client *Client) error {
				created, err := client.CreateIssue(ctx, CreateIssueRequest{ProjectID: "123", IssueTypeID: "456", Summary: "New issue", Description: &description})
				if err == nil && (created.ID != "10001" || created.Key != "WL-7") {
					t.Fatalf("created = %#v", created)
				}
				return err
			},
			assertBody: func(t *testing.T, body map[string]any) {
				fields := body["fields"].(map[string]any)
				if fields["summary"] != "New issue" || fields["project"].(map[string]any)["id"] != "123" || fields["issuetype"].(map[string]any)["id"] != "456" {
					t.Fatalf("fields = %#v", fields)
				}
				assertADFText(t, fields["description"], description)
			},
		},
		{
			name: "edit uses numeric issue ID and typed fields", method: http.MethodPut, path: "/rest/api/3/issue/10001", status: http.StatusNoContent, response: "",
			call: func(ctx context.Context, client *Client) error {
				return client.EditIssue(ctx, "10001", EditIssueRequest{Summary: &summary, Description: &description})
			},
			assertBody: func(t *testing.T, body map[string]any) {
				fields := body["fields"].(map[string]any)
				if fields["summary"] != summary {
					t.Fatalf("summary = %#v", fields["summary"])
				}
				assertADFText(t, fields["description"], description)
			},
		},
		{
			name: "clear description sends JSON null", method: http.MethodPut, path: "/rest/api/3/issue/10001", status: http.StatusNoContent, response: "",
			call: func(ctx context.Context, client *Client) error {
				return client.EditIssue(ctx, "10001", EditIssueRequest{ClearDescription: true})
			},
			assertBody: func(t *testing.T, body map[string]any) {
				fields := body["fields"].(map[string]any)
				value, exists := fields["description"]
				if !exists || value != nil {
					t.Fatalf("description = %#v, exists=%v", value, exists)
				}
			},
		},
		{
			name: "transition uses numeric issue and transition IDs", method: http.MethodPost, path: "/rest/api/3/issue/10001/transitions", status: http.StatusNoContent, response: "",
			call: func(ctx context.Context, client *Client) error { return client.TransitionIssue(ctx, "10001", "31") },
			assertBody: func(t *testing.T, body map[string]any) {
				if body["transition"].(map[string]any)["id"] != "31" {
					t.Fatalf("body = %#v", body)
				}
			},
		},
		{
			name: "comment uses numeric issue ID and ADF body", method: http.MethodPost, path: "/rest/api/3/issue/10001/comment", status: http.StatusCreated, response: `{"id":"900"}`,
			call: func(ctx context.Context, client *Client) error {
				comment, err := client.AddComment(ctx, "10001", description)
				if err == nil && comment.ID != "900" {
					t.Fatalf("comment = %#v", comment)
				}
				return err
			},
			assertBody: func(t *testing.T, body map[string]any) { assertADFText(t, body["body"], description) },
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				calls++
				if request.Method != test.method || request.URL.Path != test.path {
					t.Errorf("request = %s %s, want %s %s", request.Method, request.URL.Path, test.method, test.path)
				}
				var body map[string]any
				if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
					t.Fatalf("decode body: %v", err)
				}
				test.assertBody(t, body)
				writer.Header().Set("Content-Type", "application/json")
				writer.WriteHeader(test.status)
				_, _ = writer.Write([]byte(test.response))
			}))
			defer server.Close()
			if err := test.call(context.Background(), newTestClient(t, server)); err != nil {
				t.Fatalf("call error = %v", err)
			}
			if calls != 1 {
				t.Fatalf("write calls = %d, want exactly 1", calls)
			}
		})
	}
}

func TestWriteInputValidationMakesNoRequest(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls++ }))
	defer server.Close()
	client := newTestClient(t, server)
	summary := "valid"
	description := "valid"
	tests := []struct {
		name string
		call func() error
	}{
		{name: "create rejects nonnumeric project ID", call: func() error {
			_, err := client.CreateIssue(context.Background(), CreateIssueRequest{ProjectID: "WL", IssueTypeID: "1", Summary: summary})
			return err
		}},
		{name: "create rejects nonnumeric type ID", call: func() error {
			_, err := client.CreateIssue(context.Background(), CreateIssueRequest{ProjectID: "1", IssueTypeID: "Task", Summary: summary})
			return err
		}},
		{name: "edit requires changed field", call: func() error { return client.EditIssue(context.Background(), "1", EditIssueRequest{}) }},
		{name: "edit rejects description and clear", call: func() error {
			return client.EditIssue(context.Background(), "1", EditIssueRequest{Description: &description, ClearDescription: true})
		}},
		{name: "transition rejects nonnumeric ID", call: func() error { return client.TransitionIssue(context.Background(), "1", "Done") }},
		{name: "comment rejects empty body", call: func() error { _, err := client.AddComment(context.Background(), "1", " \n "); return err }},
		{name: "issue types reject negative pagination", call: func() error {
			_, err := client.IssueTypes(context.Background(), "1", IssueTypePageOptions{StartAt: -1})
			return err
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := test.call(); err == nil || errx.ExitCode(err) != errx.CodeUsage {
				t.Fatalf("error = %v, want usage", err)
			}
		})
	}
	if calls != 0 {
		t.Fatalf("HTTP calls = %d, want 0", calls)
	}
}

func TestIssueTypesUsesOffsetPagination(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet || request.URL.Path != "/rest/api/3/issue/createmeta/123/issuetypes" {
			t.Errorf("request = %s %s", request.Method, request.URL.Path)
		}
		if request.URL.Query().Get("startAt") != "10" || request.URL.Query().Get("maxResults") != "5" {
			t.Errorf("query = %s", request.URL.RawQuery)
		}
		_, _ = writer.Write([]byte(`{"startAt":10,"maxResults":5,"total":20,"issueTypes":[{"id":"100","name":"Task"}]}`))
	}))
	defer server.Close()
	page, err := newTestClient(t, server).IssueTypes(context.Background(), "123", IssueTypePageOptions{StartAt: 10, MaxResults: 5})
	if err != nil || page.StartAt != 10 || page.Total != 20 || len(page.Values) != 1 {
		t.Fatalf("IssueTypes() = %#v, %v", page, err)
	}
}

func TestValidateIssueTypeUsesSpecificNotFoundContract(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte(`{"startAt":0,"maxResults":100,"total":0,"issueTypes":[]}`))
	}))
	defer server.Close()
	err := newTestClient(t, server).ValidateIssueType(context.Background(), "123", "456")
	var typed *errx.Error
	if !errors.As(err, &typed) || typed.Code != errx.CodeNotFound || typed.Reason != "NOT_FOUND_ISSUE_TYPE" {
		t.Fatalf("error = %#v, want NOT_FOUND_ISSUE_TYPE", typed)
	}
}

func TestValidateIssueTypeRejectsSubtaskWithoutWrite(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		calls++
		if request.Method != http.MethodGet || request.URL.Path != "/rest/api/3/issue/createmeta/123/issuetypes" {
			t.Fatalf("unexpected request = %s %s", request.Method, request.URL.Path)
		}
		_, _ = writer.Write([]byte(`{"startAt":0,"maxResults":100,"total":1,"issueTypes":[{"id":"456","name":"Subtask","subtask":true}]}`))
	}))
	defer server.Close()
	err := newTestClient(t, server).ValidateIssueType(context.Background(), "123", "456")
	if errx.ExitCode(err) != errx.CodeUsage || calls != 1 {
		t.Fatalf("error = %v calls = %d, want usage after one read", err, calls)
	}
}

func TestWriteFailuresAreNeverRetriedAndHaveSafeOutcome(t *testing.T) {
	const upstream = "UPSTREAM_BODY_SENTINEL"
	tests := []struct {
		name       string
		status     int
		body       func(http.ResponseWriter)
		wantCode   errx.Code
		wantReason string
	}{
		{name: "409 is operation conflict", status: http.StatusConflict, body: func(w http.ResponseWriter) { _, _ = io.WriteString(w, upstream) }, wantCode: errx.CodeConflict, wantReason: "ISSUE_CREATE_CONFLICT"},
		{name: "412 is operation conflict", status: http.StatusPreconditionFailed, body: func(w http.ResponseWriter) { _, _ = io.WriteString(w, upstream) }, wantCode: errx.CodeConflict, wantReason: "ISSUE_CREATE_CONFLICT"},
		{name: "413 is bounded usage failure", status: http.StatusRequestEntityTooLarge, body: func(w http.ResponseWriter) { _, _ = io.WriteString(w, upstream) }, wantCode: errx.CodeUsage, wantReason: "PAYLOAD_TOO_LARGE"},
		{name: "429 is not retried", status: http.StatusTooManyRequests, body: func(w http.ResponseWriter) { _, _ = io.WriteString(w, upstream) }, wantCode: errx.CodeRetryable, wantReason: "RATE_LIMITED"},
		{name: "500 has unknown write outcome", status: http.StatusInternalServerError, body: func(w http.ResponseWriter) { _, _ = io.WriteString(w, upstream) }, wantCode: errx.CodeConflict, wantReason: "WRITE_OUTCOME_UNKNOWN"},
		{name: "empty 2xx has unknown write outcome", status: http.StatusCreated, body: func(http.ResponseWriter) {}, wantCode: errx.CodeConflict, wantReason: "WRITE_OUTCOME_UNKNOWN"},
		{name: "unexpected 2xx has unknown write outcome", status: http.StatusOK, body: func(w http.ResponseWriter) { _, _ = io.WriteString(w, `{"id":"10001","key":"WL-1"}`) }, wantCode: errx.CodeConflict, wantReason: "WRITE_OUTCOME_UNKNOWN"},
		{name: "invalid 2xx has unknown write outcome", status: http.StatusCreated, body: func(w http.ResponseWriter) { _, _ = io.WriteString(w, "not-json-"+upstream) }, wantCode: errx.CodeConflict, wantReason: "WRITE_OUTCOME_UNKNOWN"},
		{name: "oversized 2xx has unknown write outcome", status: http.StatusCreated, body: func(w http.ResponseWriter) { _, _ = io.WriteString(w, strings.Repeat("x", maxResponseBody+1)) }, wantCode: errx.CodeConflict, wantReason: "WRITE_OUTCOME_UNKNOWN"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				calls++
				writer.WriteHeader(test.status)
				test.body(writer)
			}))
			defer server.Close()
			_, err := newTestClient(t, server).CreateIssue(context.Background(), CreateIssueRequest{ProjectID: "1", IssueTypeID: "2", Summary: "safe summary"})
			if err == nil {
				t.Fatal("expected error")
			}
			if got := errx.ExitCode(err); got != test.wantCode {
				t.Fatalf("exit code = %d, want %d (err=%v)", got, test.wantCode, err)
			}
			var typed *errx.Error
			if !errors.As(err, &typed) || typed.Reason != test.wantReason {
				t.Fatalf("reason = %#v, want %q", typed, test.wantReason)
			}
			if calls != 1 {
				t.Fatalf("calls = %d, want 1", calls)
			}
			if combined := err.Error(); strings.Contains(combined, upstream) || strings.Contains(combined, testToken) || strings.Contains(combined, testEmail) {
				t.Fatalf("error leaked sentinel: %s", combined)
			}
		})
	}
}

func TestTransitionStaleStatusesAreConflictsAndNeverRetried(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusNotFound} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				calls++
				writer.WriteHeader(status)
			}))
			defer server.Close()
			err := newTestClient(t, server).TransitionIssue(context.Background(), "10001", "31")
			var typed *errx.Error
			if !errors.As(err, &typed) || typed.Code != errx.CodeConflict || typed.Reason != "TRANSITION_CONFLICT" {
				t.Fatalf("error = %#v, want TRANSITION_CONFLICT", typed)
			}
			if calls != 1 {
				t.Fatalf("calls = %d, want 1", calls)
			}
		})
	}
}

func TestDispatchedWriteTransportAndTruncatedResponsesAreOutcomeUnknown(t *testing.T) {
	const bodySentinel = "WRITE_BODY_SENTINEL"
	tests := []struct {
		name      string
		transport writeRoundTripFunc
	}{
		{
			name: "transport failure",
			transport: func(*http.Request) (*http.Response, error) {
				return nil, errors.New("TRANSPORT_SENTINEL")
			},
		},
		{
			name: "timeout after dispatch",
			transport: func(*http.Request) (*http.Response, error) {
				return nil, context.DeadlineExceeded
			},
		},
		{
			name: "truncated success response",
			transport: func(*http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusCreated,
					Header:     make(http.Header),
					Body:       &failingBody{prefix: []byte(`{"id":"1"`), err: io.ErrUnexpectedEOF},
				}, nil
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			var logs bytes.Buffer
			transport := test.transport
			client, err := New(
				Config{SiteURL: "https://example.atlassian.net"},
				Credential{Email: testEmail, Token: testToken, TokenKind: TokenKindClassic},
				WithHTTPClient(&http.Client{Transport: writeRoundTripFunc(func(request *http.Request) (*http.Response, error) {
					calls++
					return transport(request)
				})}),
				WithLogger(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))),
			)
			if err != nil {
				t.Fatal(err)
			}
			_, err = client.AddComment(context.Background(), "10001", bodySentinel)
			if errx.ExitCode(err) != errx.CodeConflict {
				t.Fatalf("exit code = %d, want %d (err=%v)", errx.ExitCode(err), errx.CodeConflict, err)
			}
			var typed *errx.Error
			if !errors.As(err, &typed) || typed.Reason != "WRITE_OUTCOME_UNKNOWN" {
				t.Fatalf("error = %#v", typed)
			}
			if calls != 1 {
				t.Fatalf("calls = %d, want exactly 1", calls)
			}
			combined := err.Error() + logs.String()
			for _, sentinel := range []string{testToken, testEmail, "Authorization", bodySentinel, "TRANSPORT_SENTINEL"} {
				if strings.Contains(combined, sentinel) {
					t.Fatalf("output leaked %q: %s", sentinel, combined)
				}
			}
		})
	}
}

func assertADFText(t *testing.T, raw any, want string) {
	t.Helper()
	document := raw.(map[string]any)
	if document["version"] != float64(1) || document["type"] != "doc" {
		t.Fatalf("ADF document = %#v", document)
	}
	paragraphs := document["content"].([]any)
	leaves := paragraphs[0].(map[string]any)["content"].([]any)
	var got strings.Builder
	for _, rawLeaf := range leaves {
		leaf := rawLeaf.(map[string]any)
		switch leaf["type"] {
		case "text":
			got.WriteString(leaf["text"].(string))
		case "hardBreak":
			got.WriteByte('\n')
		default:
			t.Fatalf("unexpected ADF leaf = %#v", leaf)
		}
	}
	if !reflect.DeepEqual(got.String(), want) {
		t.Fatalf("ADF text = %q, want %q", got.String(), want)
	}
}

func plainTextFromADF(document ADFDocument) string {
	var text strings.Builder
	for _, leaf := range document.Content[0].Content {
		if leaf.Type == "hardBreak" {
			text.WriteByte('\n')
		} else {
			text.WriteString(leaf.Text)
		}
	}
	return text.String()
}

type writeRoundTripFunc func(*http.Request) (*http.Response, error)

func (function writeRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

type failingBody struct {
	prefix []byte
	err    error
}

func (body *failingBody) Read(target []byte) (int, error) {
	if len(body.prefix) > 0 {
		count := copy(target, body.prefix)
		body.prefix = body.prefix[count:]
		return count, nil
	}
	return 0, body.err
}

func (*failingBody) Close() error { return nil }
