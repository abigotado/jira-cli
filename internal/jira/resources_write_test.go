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
	"strconv"
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

func TestCreateIssueUsesExactPreflightSequenceAndPayload(t *testing.T) {
	description := "line one\nстрока два"
	type expectedRequest struct {
		method string
		path   string
		query  string
	}
	wantRequests := []expectedRequest{
		{method: http.MethodGet, path: "/rest/api/3/issue/createmeta/123/issuetypes", query: "maxResults=100"},
		{method: http.MethodGet, path: "/rest/api/3/issue/createmeta/123/issuetypes/456", query: "maxResults=100&startAt=0"},
		{method: http.MethodGet, path: "/rest/api/3/issue/createmeta/123/issuetypes/456", query: "maxResults=100&startAt=2"},
		{method: http.MethodPost, path: "/rest/api/3/issue", query: ""},
	}
	responses := []string{
		`{"startAt":0,"maxResults":100,"total":1,"issueTypes":[{"id":"456","name":"Task","subtask":false}]}`,
		createFieldPageJSON(t, 0, 2, 4, []IssueCreateField{
			createField("project", true, false, "set"),
			createField("issuetype", true, false, "set"),
		}),
		createFieldPageJSON(t, 2, 2, 4, []IssueCreateField{
			createField("summary", true, false, "set"),
			createField("description", true, false, "set"),
		}),
		`{"id":"10001","key":"WL-7","self":"https://safe.invalid/10001"}`,
	}
	requestIndex := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if requestIndex >= len(wantRequests) {
			t.Errorf("unexpected extra request: %s %s", request.Method, request.URL.String())
			writer.WriteHeader(http.StatusInternalServerError)
			return
		}
		want := wantRequests[requestIndex]
		if request.Method != want.method || request.URL.Path != want.path || request.URL.RawQuery != want.query {
			t.Errorf("request[%d] = %s %s?%s, want %s %s?%s", requestIndex, request.Method, request.URL.Path, request.URL.RawQuery, want.method, want.path, want.query)
		}
		if request.Method == http.MethodPost {
			var body map[string]any
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				t.Errorf("decode create body: %v", err)
			} else {
				fields := body["fields"].(map[string]any)
				if fields["summary"] != "New issue" || fields["project"].(map[string]any)["id"] != "123" || fields["issuetype"].(map[string]any)["id"] != "456" {
					t.Errorf("fields = %#v", fields)
				}
				assertADFText(t, fields["description"], description)
			}
			writer.WriteHeader(http.StatusCreated)
		}
		_, _ = io.WriteString(writer, responses[requestIndex])
		requestIndex++
	}))
	defer server.Close()

	created, err := newTestClient(t, server).CreateIssue(context.Background(), CreateIssueRequest{
		ProjectID: "123", IssueTypeID: "456", Summary: "New issue", Description: &description,
	})
	if err != nil {
		t.Fatalf("CreateIssue() error = %v", err)
	}
	if created.ID != "10001" || created.Key != "WL-7" {
		t.Fatalf("created = %#v", created)
	}
	if requestIndex != len(wantRequests) {
		t.Fatalf("requests = %d, want %d", requestIndex, len(wantRequests))
	}
}

func TestCreateIssueAcceptsOnlySupportedBoundedMetadata(t *testing.T) {
	description := "bounded description"
	tests := []struct {
		name             string
		input            CreateIssueRequest
		pages            []string
		wantCode         errx.Code
		wantReason       string
		wantHintContains string
		wantPosts        int
	}{
		{
			name:  "provided project issue type and summary are accepted",
			input: CreateIssueRequest{ProjectID: "123", IssueTypeID: "456", Summary: "New issue"},
			pages: []string{createFieldPageJSON(t, 0, 100, 3, []IssueCreateField{
				createField("project", true, false, "set"),
				createField("issuetype", true, false, "set"),
				createField("summary", true, false, "set"),
			})},
			wantPosts: 1,
		},
		{
			name:  "required custom field with Jira default is accepted",
			input: CreateIssueRequest{ProjectID: "123", IssueTypeID: "456", Summary: "New issue"},
			pages: []string{createFieldPageJSON(t, 0, 100, 2, []IssueCreateField{
				createField("summary", true, false, "set"),
				createField("customfield_10000", true, true, "set"),
			})},
			wantPosts: 1,
		},
		{
			name:  "supplied description with set operation is accepted",
			input: CreateIssueRequest{ProjectID: "123", IssueTypeID: "456", Summary: "New issue", Description: &description},
			pages: []string{createFieldPageJSON(t, 0, 100, 2, []IssueCreateField{
				createField("summary", true, false, "set"),
				createField("description", true, false, "set"),
			})},
			wantPosts: 1,
		},
		{
			name:             "required omitted description without default is rejected",
			input:            CreateIssueRequest{ProjectID: "123", IssueTypeID: "456", Summary: "New issue"},
			pages:            []string{createFieldPageJSON(t, 0, 100, 2, []IssueCreateField{createField("summary", true, false, "set"), createField("description", true, false, "set")})},
			wantCode:         errx.CodeUsage,
			wantReason:       "CREATE_FIELDS_UNSUPPORTED",
			wantHintContains: "--description",
		},
		{
			name:             "supplied description lacking exact set is rejected",
			input:            CreateIssueRequest{ProjectID: "123", IssueTypeID: "456", Summary: "New issue", Description: &description},
			pages:            []string{createFieldPageJSON(t, 0, 100, 2, []IssueCreateField{createField("summary", true, false, "set"), createField("description", false, false, "add")})},
			wantCode:         errx.CodeUsage,
			wantReason:       "CREATE_FIELDS_UNSUPPORTED",
			wantHintContains: "standard issue type",
		},
		{
			name:             "supplied description missing from metadata is rejected",
			input:            CreateIssueRequest{ProjectID: "123", IssueTypeID: "456", Summary: "New issue", Description: &description},
			pages:            []string{createFieldPageJSON(t, 0, 100, 1, []IssueCreateField{createField("summary", true, false, "set")})},
			wantCode:         errx.CodeUsage,
			wantReason:       "CREATE_FIELDS_UNSUPPORTED",
			wantHintContains: "standard issue type",
		},
		{
			name:             "later page required blocker prevents write",
			input:            CreateIssueRequest{ProjectID: "123", IssueTypeID: "456", Summary: "New issue"},
			pages:            []string{createFieldPageJSON(t, 0, 1, 2, []IssueCreateField{createField("summary", true, false, "set")}), createFieldPageJSON(t, 1, 1, 2, []IssueCreateField{createField("customfield_20000", true, false, "set")})},
			wantCode:         errx.CodeUsage,
			wantReason:       "CREATE_FIELDS_UNSUPPORTED",
			wantHintContains: "standard issue type",
		},
		{
			name:             "missing summary metadata prevents write",
			input:            CreateIssueRequest{ProjectID: "123", IssueTypeID: "456", Summary: "New issue"},
			pages:            []string{createFieldPageJSON(t, 0, 100, 1, []IssueCreateField{createField("project", true, false, "set")})},
			wantCode:         errx.CodeUsage,
			wantReason:       "CREATE_FIELDS_UNSUPPORTED",
			wantHintContains: "standard issue type",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			metadataCalls := 0
			postCalls := 0
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				switch request.URL.Path {
				case "/rest/api/3/issue/createmeta/123/issuetypes":
					_, _ = io.WriteString(writer, `{"startAt":0,"maxResults":100,"total":1,"issueTypes":[{"id":"456","name":"Task","subtask":false}]}`)
				case "/rest/api/3/issue/createmeta/123/issuetypes/456":
					if metadataCalls >= len(test.pages) {
						t.Errorf("unexpected metadata page %d", metadataCalls)
						writer.WriteHeader(http.StatusInternalServerError)
						return
					}
					_, _ = io.WriteString(writer, test.pages[metadataCalls])
					metadataCalls++
				case "/rest/api/3/issue":
					postCalls++
					writer.WriteHeader(http.StatusCreated)
					_, _ = io.WriteString(writer, `{"id":"10001","key":"WL-1"}`)
				default:
					t.Errorf("unexpected request: %s %s", request.Method, request.URL.Path)
					writer.WriteHeader(http.StatusNotFound)
				}
			}))
			defer server.Close()

			_, err := newTestClient(t, server).CreateIssue(context.Background(), test.input)
			if got := errx.ExitCode(err); got != test.wantCode {
				t.Fatalf("exit code = %d, want %d (err=%v)", got, test.wantCode, err)
			}
			if test.wantReason != "" {
				var typed *errx.Error
				if !errors.As(err, &typed) || typed.Reason != test.wantReason {
					t.Fatalf("error = %#v, want reason %s", typed, test.wantReason)
				}
				if !strings.Contains(typed.Hint, test.wantHintContains) {
					t.Fatalf("hint = %q, want %q", typed.Hint, test.wantHintContains)
				}
			}
			if metadataCalls != len(test.pages) || postCalls != test.wantPosts {
				t.Fatalf("calls = metadata:%d post:%d, want metadata:%d post:%d", metadataCalls, postCalls, len(test.pages), test.wantPosts)
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
		_, _ = writer.Write([]byte(`{"startAt":10,"maxResults":5,"total":20,"issueTypes":[{"id":"100","name":"Task","subtask":false}]}`))
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

func TestIssueTypeDiscoveryRejectsAmbiguousWireDataBeforeCreatePreflightOrWrite(t *testing.T) {
	const malformedSentinel = "ISSUE_TYPE_METADATA_SENTINEL"
	standard := `{"id":"456","name":"Task","subtask":false}`
	tests := []struct {
		name      string
		pages     []string
		wantCalls int
	}{
		{name: "omitted subtask", pages: []string{`{"startAt":0,"maxResults":100,"total":1,"issueTypes":[{"id":"456","name":"Task"}]}`}, wantCalls: 1},
		{name: "null subtask", pages: []string{`{"startAt":0,"maxResults":100,"total":1,"issueTypes":[{"id":"456","name":"Task","subtask":null}]}`}, wantCalls: 1},
		{name: "omitted startAt", pages: []string{`{"maxResults":100,"total":1,"issueTypes":[` + standard + `]}`}, wantCalls: 1},
		{name: "null startAt", pages: []string{`{"startAt":null,"maxResults":100,"total":1,"issueTypes":[` + standard + `]}`}, wantCalls: 1},
		{name: "omitted maxResults", pages: []string{`{"startAt":0,"total":1,"issueTypes":[` + standard + `]}`}, wantCalls: 1},
		{name: "null maxResults", pages: []string{`{"startAt":0,"maxResults":null,"total":1,"issueTypes":[` + standard + `]}`}, wantCalls: 1},
		{name: "omitted total", pages: []string{`{"startAt":0,"maxResults":100,"issueTypes":[` + standard + `]}`}, wantCalls: 1},
		{name: "null total", pages: []string{`{"startAt":0,"maxResults":100,"total":null,"issueTypes":[` + standard + `]}`}, wantCalls: 1},
		{name: "omitted issueTypes", pages: []string{`{"startAt":0,"maxResults":100,"total":1}`}, wantCalls: 1},
		{name: "null issueTypes", pages: []string{`{"startAt":0,"maxResults":100,"total":1,"issueTypes":null}`}, wantCalls: 1},
		{name: "mismatched startAt", pages: []string{`{"startAt":1,"maxResults":100,"total":1,"issueTypes":[]}`}, wantCalls: 1},
		{name: "zero page size", pages: []string{`{"startAt":0,"maxResults":0,"total":1,"issueTypes":[` + standard + `]}`}, wantCalls: 1},
		{name: "page size exceeds cap", pages: []string{`{"startAt":0,"maxResults":101,"total":1,"issueTypes":[` + standard + `]}`}, wantCalls: 1},
		{name: "negative total", pages: []string{`{"startAt":0,"maxResults":100,"total":-1,"issueTypes":[]}`}, wantCalls: 1},
		{name: "total exceeds cap", pages: []string{`{"startAt":0,"maxResults":100,"total":10001,"issueTypes":[` + standard + `]}`}, wantCalls: 1},
		{name: "page makes no progress", pages: []string{`{"startAt":0,"maxResults":100,"total":1,"issueTypes":[]}`}, wantCalls: 1},
		{name: "page overruns total", pages: []string{`{"startAt":0,"maxResults":100,"total":1,"issueTypes":[` + standard + `,{"id":"789","name":"Bug","subtask":false}]}`}, wantCalls: 1},
		{name: "duplicate IDs on one page", pages: []string{`{"startAt":0,"maxResults":100,"total":2,"issueTypes":[` + standard + `,{"id":"456","name":"Duplicate","subtask":false}]}`}, wantCalls: 1},
		{name: "missing ID", pages: []string{`{"startAt":0,"maxResults":100,"total":1,"issueTypes":[{"name":"Task","subtask":false}]}`}, wantCalls: 1},
		{name: "null ID", pages: []string{`{"startAt":0,"maxResults":100,"total":1,"issueTypes":[{"id":null,"name":"Task","subtask":false}]}`}, wantCalls: 1},
		{name: "non numeric ID", pages: []string{`{"startAt":0,"maxResults":100,"total":1,"issueTypes":[{"id":"Task","name":"Task","subtask":false}]}`}, wantCalls: 1},
		{
			name: "total changes after matching ID",
			pages: []string{
				`{"startAt":0,"maxResults":1,"total":2,"issueTypes":[` + standard + `]}`,
				`{"startAt":1,"maxResults":1,"total":3,"issueTypes":[{"id":"789","name":"Bug","subtask":false}]}`,
			},
			wantCalls: 2,
		},
		{
			name: "duplicate ID appears after matching ID",
			pages: []string{
				`{"startAt":0,"maxResults":1,"total":2,"issueTypes":[` + standard + `]}`,
				`{"startAt":1,"maxResults":1,"total":2,"issueTypes":[{"id":"456","name":"Duplicate","subtask":false}]}`,
			},
			wantCalls: 2,
		},
		{
			name: "malformed later page after matching ID",
			pages: []string{
				`{"startAt":0,"maxResults":1,"total":2,"issueTypes":[` + standard + `]}`,
				"not-json-" + malformedSentinel,
			},
			wantCalls: 2,
		},
	}
	operations := []struct {
		name string
		call func(context.Context, *Client) error
	}{
		{name: "ValidateIssueType", call: func(ctx context.Context, client *Client) error {
			return client.ValidateIssueType(ctx, "123", "456")
		}},
		{name: "CreateIssue", call: func(ctx context.Context, client *Client) error {
			_, err := client.CreateIssue(ctx, CreateIssueRequest{ProjectID: "123", IssueTypeID: "456", Summary: "safe"})
			return err
		}},
	}
	for _, test := range tests {
		for _, operation := range operations {
			t.Run(test.name+"/"+operation.name, func(t *testing.T) {
				typeCalls := 0
				fieldMetadataCalls := 0
				postCalls := 0
				var logs bytes.Buffer
				server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
					switch request.URL.Path {
					case "/rest/api/3/issue/createmeta/123/issuetypes":
						if typeCalls >= len(test.pages) {
							t.Errorf("unexpected issue-type page %d", typeCalls)
							writer.WriteHeader(http.StatusInternalServerError)
							return
						}
						_, _ = io.WriteString(writer, test.pages[typeCalls])
						typeCalls++
					case "/rest/api/3/issue/createmeta/123/issuetypes/456":
						fieldMetadataCalls++
						_, _ = io.WriteString(writer, createFieldPageJSON(t, 0, 100, 1, []IssueCreateField{createField("summary", true, false, "set")}))
					case "/rest/api/3/issue":
						postCalls++
						writer.WriteHeader(http.StatusCreated)
						_, _ = io.WriteString(writer, `{"id":"10001","key":"WL-1"}`)
					default:
						t.Errorf("unexpected request: %s %s", request.Method, request.URL.Path)
						writer.WriteHeader(http.StatusNotFound)
					}
				}))
				defer server.Close()
				client := newTestClient(t, server, WithLogger(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))))
				err := operation.call(context.Background(), client)
				var typed *errx.Error
				if !errors.As(err, &typed) || typed.Code != errx.CodeInternal || typed.Reason != "INTERNAL" {
					t.Fatalf("error = %#v, want INTERNAL", typed)
				}
				if typeCalls != test.wantCalls || fieldMetadataCalls != 0 || postCalls != 0 {
					t.Fatalf("calls = types:%d fields:%d post:%d, want types:%d fields:0 post:0", typeCalls, fieldMetadataCalls, postCalls, test.wantCalls)
				}
				combined := err.Error() + logs.String()
				for _, sentinel := range []string{malformedSentinel, testToken, testEmail, "Authorization"} {
					if strings.Contains(combined, sentinel) {
						t.Fatalf("output leaked %q: %s", sentinel, combined)
					}
				}
			})
		}
	}
}

func TestValidateIssueTypeScansEveryValidPageBeforeDeciding(t *testing.T) {
	tests := []struct {
		name      string
		firstType string
		wantCode  errx.Code
	}{
		{name: "standard target on first page succeeds after final page", firstType: `{"id":"456","name":"Task","subtask":false}`},
		{name: "subtask target on first page is rejected after final page", firstType: `{"id":"456","name":"Subtask","subtask":true}`, wantCode: errx.CodeUsage},
		{name: "absent target is not found after final page", firstType: `{"id":"111","name":"Story","subtask":false}`, wantCode: errx.CodeNotFound},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				startAt := request.URL.Query().Get("startAt")
				switch calls {
				case 0:
					if startAt != "" {
						t.Errorf("first startAt = %q, want omitted zero", startAt)
					}
					_, _ = io.WriteString(writer, `{"startAt":0,"maxResults":1,"total":2,"issueTypes":[`+test.firstType+`]}`)
				case 1:
					if startAt != "1" {
						t.Errorf("second startAt = %q, want 1", startAt)
					}
					_, _ = io.WriteString(writer, `{"startAt":1,"maxResults":1,"total":2,"issueTypes":[{"id":"789","name":"Bug","subtask":false}]}`)
				default:
					t.Errorf("unexpected extra page %d", calls)
					writer.WriteHeader(http.StatusInternalServerError)
				}
				calls++
			}))
			defer server.Close()
			err := newTestClient(t, server).ValidateIssueType(context.Background(), "123", "456")
			if got := errx.ExitCode(err); got != test.wantCode {
				t.Fatalf("exit code = %d, want %d (err=%v)", got, test.wantCode, err)
			}
			if calls != 2 {
				t.Fatalf("calls = %d, want all 2 pages", calls)
			}
		})
	}
}

func TestCreateIssueRejectsSubtaskBeforeFieldMetadataOrWrite(t *testing.T) {
	calls := 0
	posts := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		calls++
		if request.Method == http.MethodPost {
			posts++
			writer.WriteHeader(http.StatusCreated)
			return
		}
		if request.Method != http.MethodGet || request.URL.Path != "/rest/api/3/issue/createmeta/123/issuetypes" {
			t.Errorf("unexpected request = %s %s", request.Method, request.URL.Path)
			writer.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = writer.Write([]byte(`{"startAt":0,"maxResults":100,"total":1,"issueTypes":[{"id":"456","name":"Subtask","subtask":true}]}`))
	}))
	defer server.Close()
	_, err := newTestClient(t, server).CreateIssue(context.Background(), CreateIssueRequest{ProjectID: "123", IssueTypeID: "456", Summary: "safe"})
	if errx.ExitCode(err) != errx.CodeUsage || calls != 1 || posts != 0 {
		t.Fatalf("error = %v calls = %d posts = %d, want usage after one read and no write", err, calls, posts)
	}
}

func TestCreateIssueRejectsMalformedCreateMetadataWithoutWrite(t *testing.T) {
	const metadataSentinel = "CREATE_METADATA_BODY_SENTINEL"
	tests := []struct {
		name          string
		page          func(int) string
		wantMetaCalls int
	}{
		{name: "missing fields array", page: func(int) string { return `{"startAt":0,"maxResults":100,"total":1}` }, wantMetaCalls: 1},
		{name: "missing required pointer", page: func(int) string {
			return `{"startAt":0,"maxResults":100,"total":1,"fields":[{"fieldId":"summary","operations":["set"],"hasDefaultValue":false}]}`
		}, wantMetaCalls: 1},
		{name: "empty metadata", page: func(int) string { return `{"startAt":0,"maxResults":100,"total":0,"fields":[]}` }, wantMetaCalls: 1},
		{name: "malformed metadata", page: func(int) string { return "not-json-" + metadataSentinel }, wantMetaCalls: 1},
		{name: "startAt mismatch", page: func(int) string {
			return createFieldPageJSON(t, 1, 100, 1, []IssueCreateField{createField("summary", true, false, "set")})
		}, wantMetaCalls: 1},
		{name: "unstable total", page: func(call int) string {
			if call == 0 {
				return createFieldPageJSON(t, 0, 1, 2, []IssueCreateField{createField("summary", true, false, "set")})
			}
			return createFieldPageJSON(t, 1, 1, 3, []IssueCreateField{createField("project", true, false, "set")})
		}, wantMetaCalls: 2},
		{name: "negative total", page: func(int) string {
			return createFieldPageJSON(t, 0, 100, -1, []IssueCreateField{createField("summary", true, false, "set")})
		}, wantMetaCalls: 1},
		{name: "page overruns total", page: func(int) string {
			return createFieldPageJSON(t, 0, 100, 1, []IssueCreateField{createField("summary", true, false, "set"), createField("project", true, false, "set")})
		}, wantMetaCalls: 1},
		{name: "page makes no progress", page: func(int) string { return createFieldPageJSON(t, 0, 100, 1, []IssueCreateField{}) }, wantMetaCalls: 1},
		{name: "field total exceeds cap", page: func(int) string {
			return createFieldPageJSON(t, 0, 100, 10_001, []IssueCreateField{createField("summary", true, false, "set")})
		}, wantMetaCalls: 1},
		{name: "page count exceeds cap", page: func(call int) string {
			return createFieldPageJSON(t, call, 1, 101, []IssueCreateField{createField("field_"+strconv.Itoa(call), false, false, "set")})
		}, wantMetaCalls: 100},
		{name: "missing field ID", page: func(int) string {
			return createFieldPageJSON(t, 0, 100, 1, []IssueCreateField{createField("", true, false, "set")})
		}, wantMetaCalls: 1},
		{name: "oversized field ID", page: func(int) string {
			return createFieldPageJSON(t, 0, 100, 1, []IssueCreateField{createField(strings.Repeat("f", 256), true, false, "set")})
		}, wantMetaCalls: 1},
		{name: "duplicate field ID", page: func(int) string {
			return createFieldPageJSON(t, 0, 100, 2, []IssueCreateField{createField("summary", true, false, "set"), createField("summary", false, false, "set")})
		}, wantMetaCalls: 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			metaCalls := 0
			postCalls := 0
			var logs bytes.Buffer
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				switch request.URL.Path {
				case "/rest/api/3/issue/createmeta/123/issuetypes":
					_, _ = io.WriteString(writer, `{"startAt":0,"maxResults":100,"total":1,"issueTypes":[{"id":"456","name":"Task","subtask":false}]}`)
				case "/rest/api/3/issue/createmeta/123/issuetypes/456":
					_, _ = io.WriteString(writer, test.page(metaCalls))
					metaCalls++
				case "/rest/api/3/issue":
					postCalls++
					writer.WriteHeader(http.StatusCreated)
				default:
					t.Errorf("unexpected request: %s %s", request.Method, request.URL.Path)
					writer.WriteHeader(http.StatusNotFound)
				}
			}))
			defer server.Close()
			client := newTestClient(t, server, WithLogger(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))))
			_, err := client.CreateIssue(context.Background(), CreateIssueRequest{ProjectID: "123", IssueTypeID: "456", Summary: "safe"})
			var typed *errx.Error
			if !errors.As(err, &typed) || typed.Code != errx.CodeInternal || typed.Reason != "INTERNAL" {
				t.Fatalf("error = %#v, want INTERNAL", typed)
			}
			if metaCalls != test.wantMetaCalls || postCalls != 0 {
				t.Fatalf("calls = metadata:%d post:%d, want metadata:%d post:0", metaCalls, postCalls, test.wantMetaCalls)
			}
			combined := err.Error() + logs.String()
			for _, sentinel := range []string{metadataSentinel, testToken, testEmail, "Authorization"} {
				if strings.Contains(combined, sentinel) {
					t.Fatalf("output leaked %q: %s", sentinel, combined)
				}
			}
		})
	}
}

func TestCreateIssueRetriesMetadataReadBeforeSingleWrite(t *testing.T) {
	metadataCalls := 0
	postCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/rest/api/3/issue/createmeta/123/issuetypes":
			_, _ = io.WriteString(writer, `{"startAt":0,"maxResults":100,"total":1,"issueTypes":[{"id":"456","name":"Task","subtask":false}]}`)
		case "/rest/api/3/issue/createmeta/123/issuetypes/456":
			metadataCalls++
			if metadataCalls == 1 {
				writer.WriteHeader(http.StatusInternalServerError)
				return
			}
			_, _ = io.WriteString(writer, createFieldPageJSON(t, 0, 100, 1, []IssueCreateField{createField("summary", true, false, "set")}))
		case "/rest/api/3/issue":
			postCalls++
			writer.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(writer, `{"id":"10001","key":"WL-1"}`)
		default:
			t.Errorf("unexpected request: %s %s", request.Method, request.URL.Path)
			writer.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	_, err := newTestClient(t, server).CreateIssue(context.Background(), CreateIssueRequest{ProjectID: "123", IssueTypeID: "456", Summary: "safe"})
	if err != nil {
		t.Fatalf("CreateIssue() error = %v", err)
	}
	if metadataCalls != 2 || postCalls != 1 {
		t.Fatalf("calls = metadata:%d post:%d, want metadata:2 post:1", metadataCalls, postCalls)
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
			writeCalls := 0
			var logs bytes.Buffer
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				switch request.URL.Path {
				case "/rest/api/3/issue/createmeta/1/issuetypes":
					_, _ = io.WriteString(writer, `{"startAt":0,"maxResults":100,"total":1,"issueTypes":[{"id":"2","name":"Task","subtask":false}]}`)
				case "/rest/api/3/issue/createmeta/1/issuetypes/2":
					_, _ = io.WriteString(writer, createFieldPageJSON(t, 0, 100, 1, []IssueCreateField{createField("summary", true, false, "set")}))
				case "/rest/api/3/issue":
					writeCalls++
					writer.WriteHeader(test.status)
					test.body(writer)
				default:
					t.Errorf("unexpected request: %s %s", request.Method, request.URL.Path)
					writer.WriteHeader(http.StatusNotFound)
				}
			}))
			defer server.Close()
			client := newTestClient(t, server, WithLogger(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))))
			_, err := client.CreateIssue(context.Background(), CreateIssueRequest{ProjectID: "1", IssueTypeID: "2", Summary: "safe summary"})
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
			if writeCalls != 1 {
				t.Fatalf("write calls = %d, want exactly 1", writeCalls)
			}
			combined := err.Error() + logs.String()
			for _, sentinel := range []string{upstream, testToken, testEmail, "Authorization"} {
				if strings.Contains(combined, sentinel) {
					t.Fatalf("output leaked %q: %s", sentinel, combined)
				}
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

func createField(fieldID string, required, hasDefault bool, operations ...string) IssueCreateField {
	return IssueCreateField{
		FieldID: fieldID, Operations: operations,
		HasDefaultValue: boolPointer(hasDefault), Required: boolPointer(required),
	}
}

func createFieldPageJSON(t *testing.T, startAt, maxResults, total int, fields []IssueCreateField) string {
	t.Helper()
	payload, err := json.Marshal(IssueCreateFieldPage{
		StartAt: intPointer(startAt), MaxResults: intPointer(maxResults), Total: intPointer(total), Fields: fields,
	})
	if err != nil {
		t.Fatalf("marshal create field page: %v", err)
	}
	return string(payload)
}

func boolPointer(value bool) *bool { return &value }

func intPointer(value int) *int { return &value }

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
