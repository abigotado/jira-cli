package jira

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

func TestReadResourceRoutesAndPagination(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.EscapedPath() {
		case "/rest/api/3/project/search":
			if got := request.URL.Query().Get("startAt"); got != "20" {
				t.Errorf("projects startAt = %q, want 20", got)
			}
			if got := request.URL.Query().Get("maxResults"); got != "10" {
				t.Errorf("projects maxResults = %q, want 10", got)
			}
			if got := request.URL.Query().Get("query"); got != "Wallet" {
				t.Errorf("projects query = %q, want Wallet", got)
			}
			_, _ = writer.Write([]byte(`{"startAt":20,"maxResults":10,"total":31,"isLast":false,"values":[{"id":"1","key":"WL","name":"Wallet"}]}`))

		case "/rest/api/3/project/WL%2F1":
			_, _ = writer.Write([]byte(`{"id":"1","key":"WL-1","name":"Wallet"}`))

		case "/rest/api/3/issue/WL%2F2":
			if got := request.URL.Query().Get("fields"); got != "summary,status" {
				t.Errorf("issue fields = %q, want summary,status", got)
			}
			_, _ = writer.Write([]byte(`{"id":"2","key":"WL-2","fields":{"summary":"Fix"}}`))

		case "/rest/api/3/issue/WL%2F2/transitions":
			_, _ = writer.Write([]byte(`{"transitions":[{"id":"31","name":"Done","to":{"id":"3","name":"Done"}}]}`))

		case "/rest/api/3/issue/WL%2F2/comment":
			if got := request.URL.Query().Get("startAt"); got != "5" {
				t.Errorf("comments startAt = %q, want 5", got)
			}
			if got := request.URL.Query().Get("maxResults"); got != "2" {
				t.Errorf("comments maxResults = %q, want 2", got)
			}
			_, _ = writer.Write([]byte(`{"startAt":5,"maxResults":2,"total":8,"comments":[{"id":"9","body":{"type":"doc"}}]}`))

		default:
			t.Errorf("unexpected route %s", request.URL.EscapedPath())
			writer.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	client := newTestClient(t, server)
	ctx := context.Background()

	projects, err := client.Projects(ctx, ProjectPageOptions{StartAt: 20, MaxResults: 10, Query: "Wallet"})
	if err != nil {
		t.Fatalf("Projects() error = %v", err)
	}
	if projects.Total != 31 || projects.IsLast || len(projects.Values) != 1 {
		t.Errorf("Projects() = %+v", projects)
	}

	project, err := client.Project(ctx, "WL/1")
	if err != nil {
		t.Fatalf("Project() error = %v", err)
	}
	if project.Key != "WL-1" {
		t.Errorf("Project().Key = %q", project.Key)
	}

	issue, err := client.Issue(ctx, "WL/2", []string{"summary", "status"})
	if err != nil {
		t.Fatalf("Issue() error = %v", err)
	}
	if issue.Key != "WL-2" || string(issue.Fields["summary"]) != `"Fix"` {
		t.Errorf("Issue() = %+v", issue)
	}

	transitions, err := client.Transitions(ctx, "WL/2")
	if err != nil {
		t.Fatalf("Transitions() error = %v", err)
	}
	if len(transitions) != 1 || transitions[0].To.Name != "Done" {
		t.Errorf("Transitions() = %+v", transitions)
	}

	comments, err := client.Comments(ctx, "WL/2", CommentPageOptions{StartAt: 5, MaxResults: 2})
	if err != nil {
		t.Fatalf("Comments() error = %v", err)
	}
	if comments.Total != 8 || len(comments.Comments) != 1 || comments.Comments[0].ID != "9" {
		t.Errorf("Comments() = %+v", comments)
	}
}

func TestSearchUsesEnhancedJQLTokenPagination(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.URL.Path != "/rest/api/3/search/jql" {
			t.Errorf("request = %s %s", request.Method, request.URL.Path)
		}
		if request.URL.RawQuery != "" {
			t.Errorf("search query string = %q, want empty", request.URL.RawQuery)
		}
		var body SearchRequest
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		want := SearchRequest{
			JQL:           "project in (WL, FL) ORDER BY updated DESC",
			Fields:        []string{"summary", "status"},
			MaxResults:    50,
			NextPageToken: "page-token-1",
		}
		if !reflect.DeepEqual(body, want) {
			t.Errorf("search body = %+v, want %+v", body, want)
		}
		_, _ = writer.Write([]byte(`{"issues":[{"id":"1","key":"WL-1"}],"nextPageToken":"page-token-2","isLast":false}`))
	}))
	defer server.Close()
	client := newTestClient(t, server)

	page, err := client.Search(context.Background(), SearchRequest{
		JQL:           "project in (WL, FL) ORDER BY updated DESC",
		Fields:        []string{"summary", "status"},
		MaxResults:    50,
		NextPageToken: "page-token-1",
	})
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if page.NextPageToken != "page-token-2" || page.IsLast || len(page.Issues) != 1 {
		t.Errorf("Search() = %+v", page)
	}
}

func TestExplicitEmptyIssueFieldsArePreserved(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/rest/api/3/issue/WL-1":
			if !request.URL.Query().Has("fields") || request.URL.Query().Get("fields") != "" {
				t.Errorf("issue query = %q, want explicit empty fields", request.URL.RawQuery)
			}
			_, _ = writer.Write([]byte(`{"id":"1","key":"WL-1"}`))
		case "/rest/api/3/search/jql":
			var body map[string]any
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			fields, ok := body["fields"].([]any)
			if !ok || len(fields) != 0 {
				t.Errorf("search fields = %#v, want []", body["fields"])
			}
			_, _ = writer.Write([]byte(`{"issues":[],"isLast":true}`))
		default:
			t.Errorf("unexpected path %s", request.URL.Path)
		}
	}))
	defer server.Close()
	client := newTestClient(t, server)
	if _, err := client.Issue(context.Background(), "WL-1", []string{}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Search(context.Background(), SearchRequest{JQL: "project = WL", Fields: []string{}}); err != nil {
		t.Fatal(err)
	}
}

func TestInputValidationAvoidsRequests(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls++ }))
	defer server.Close()
	client := newTestClient(t, server)
	ctx := context.Background()

	tests := []struct {
		name string
		call func() error
	}{
		{"empty project", func() error { _, err := client.Project(ctx, " "); return err }},
		{"empty issue", func() error { _, err := client.Issue(ctx, "", nil); return err }},
		{"empty JQL", func() error { _, err := client.Search(ctx, SearchRequest{}); return err }},
		{"negative project page", func() error { _, err := client.Projects(ctx, ProjectPageOptions{StartAt: -1}); return err }},
		{"negative comment page", func() error { _, err := client.Comments(ctx, "WL-1", CommentPageOptions{MaxResults: -1}); return err }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := test.call(); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
	if calls != 0 {
		t.Errorf("server calls = %d, want 0", calls)
	}
}
