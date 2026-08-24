package jira

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/abigotado/jira-cli/internal/errx"
)

// Myself returns the authenticated Jira account.
func (client *Client) Myself(ctx context.Context) (User, error) {
	var user User
	err := client.do(ctx, request{
		method:    http.MethodGet,
		path:      "/rest/api/3/myself",
		retrySafe: true,
	}, &user)
	return user, err
}

// Projects returns one offset-based project page.
func (client *Client) Projects(ctx context.Context, options ProjectPageOptions) (ProjectPage, error) {
	if options.StartAt < 0 || options.MaxResults < 0 {
		return ProjectPage{}, errx.Usage("project pagination values cannot be negative")
	}
	query := make(url.Values)
	if options.StartAt > 0 {
		query.Set("startAt", strconv.Itoa(options.StartAt))
	}
	if options.MaxResults > 0 {
		query.Set("maxResults", strconv.Itoa(options.MaxResults))
	}
	if options.Query != "" {
		query.Set("query", options.Query)
	}
	var page ProjectPage
	err := client.do(ctx, request{
		method:    http.MethodGet,
		path:      "/rest/api/3/project/search",
		query:     query,
		retrySafe: true,
	}, &page)
	return page, err
}

// Project returns a project by key or ID.
func (client *Client) Project(ctx context.Context, keyOrID string) (Project, error) {
	if strings.TrimSpace(keyOrID) == "" {
		return Project{}, errx.Usage("project key or ID is required")
	}
	var project Project
	err := client.do(ctx, request{
		method:    http.MethodGet,
		path:      "/rest/api/3/project/" + url.PathEscape(keyOrID),
		retrySafe: true,
	}, &project)
	return project, err
}

// Issue returns an issue with only the requested fields.
func (client *Client) Issue(ctx context.Context, keyOrID string, fields []string) (Issue, error) {
	if strings.TrimSpace(keyOrID) == "" {
		return Issue{}, errx.Usage("issue key or ID is required")
	}
	query := make(url.Values)
	if fields != nil {
		query.Set("fields", strings.Join(fields, ","))
	}
	var issue Issue
	err := client.do(ctx, request{
		method:    http.MethodGet,
		path:      "/rest/api/3/issue/" + url.PathEscape(keyOrID),
		query:     query,
		retrySafe: true,
	}, &issue)
	return issue, err
}

// Search runs Jira's enhanced JQL search. This specific POST is explicitly
// retry-safe because it is read-only; no generic POST method is exposed.
func (client *Client) Search(ctx context.Context, search SearchRequest) (SearchPage, error) {
	if strings.TrimSpace(search.JQL) == "" {
		return SearchPage{}, errx.Usage("JQL is required")
	}
	if search.MaxResults < 0 {
		return SearchPage{}, errx.Usage("search max results cannot be negative")
	}
	var page SearchPage
	err := client.do(ctx, request{
		method:    http.MethodPost,
		path:      "/rest/api/3/search/jql",
		body:      search,
		retrySafe: true,
	}, &page)
	return page, err
}

// Transitions returns the transitions currently available for an issue.
func (client *Client) Transitions(ctx context.Context, issueKeyOrID string) ([]Transition, error) {
	if strings.TrimSpace(issueKeyOrID) == "" {
		return nil, errx.Usage("issue key or ID is required")
	}
	var response transitionsResponse
	err := client.do(ctx, request{
		method:    http.MethodGet,
		path:      "/rest/api/3/issue/" + url.PathEscape(issueKeyOrID) + "/transitions",
		retrySafe: true,
	}, &response)
	return response.Transitions, err
}

// Comments returns one offset-based issue comment page.
func (client *Client) Comments(ctx context.Context, issueKeyOrID string, options CommentPageOptions) (CommentPage, error) {
	if strings.TrimSpace(issueKeyOrID) == "" {
		return CommentPage{}, errx.Usage("issue key or ID is required")
	}
	if options.StartAt < 0 || options.MaxResults < 0 {
		return CommentPage{}, errx.Usage("comment pagination values cannot be negative")
	}
	query := make(url.Values)
	if options.StartAt > 0 {
		query.Set("startAt", strconv.Itoa(options.StartAt))
	}
	if options.MaxResults > 0 {
		query.Set("maxResults", strconv.Itoa(options.MaxResults))
	}
	var page CommentPage
	err := client.do(ctx, request{
		method:    http.MethodGet,
		path:      "/rest/api/3/issue/" + url.PathEscape(issueKeyOrID) + "/comment",
		query:     query,
		retrySafe: true,
	}, &page)
	return page, err
}
