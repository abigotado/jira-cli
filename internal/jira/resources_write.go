package jira

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/abigotado/jira-cli/internal/errx"
)

// IssueTypes returns issue types that can be created in one exact project.
func (client *Client) IssueTypes(ctx context.Context, projectID string, options IssueTypePageOptions) (IssueTypePage, error) {
	if err := requireNumericID(projectID, "project"); err != nil {
		return IssueTypePage{}, err
	}
	if options.StartAt < 0 || options.MaxResults < 0 {
		return IssueTypePage{}, errx.Usage("issue type pagination values cannot be negative")
	}
	query := make(url.Values)
	if options.StartAt > 0 {
		query.Set("startAt", strconv.Itoa(options.StartAt))
	}
	if options.MaxResults > 0 {
		query.Set("maxResults", strconv.Itoa(options.MaxResults))
	}
	var page IssueTypePage
	err := client.do(ctx, request{
		method: http.MethodGet,
		path:   "/rest/api/3/issue/createmeta/" + url.PathEscape(projectID) + "/issuetypes",
		query:  query,
		policy: requestPolicyRead,
	}, &page)
	return page, err
}

// ValidateIssueType verifies that an exact numeric standard issue type belongs
// to a project. Subtasks are deliberately unsupported because safe creation
// would also require an exact parent contract.
func (client *Client) ValidateIssueType(ctx context.Context, projectID, issueTypeID string) error {
	if err := requireNumericID(projectID, "project"); err != nil {
		return err
	}
	if err := requireNumericID(issueTypeID, "issue type"); err != nil {
		return err
	}
	const maxPages = 100
	startAt := 0
	for pageNumber := 0; pageNumber < maxPages; pageNumber++ {
		page, err := client.IssueTypes(ctx, projectID, IssueTypePageOptions{StartAt: startAt, MaxResults: 100})
		if err != nil {
			return err
		}
		for _, issueType := range page.Values {
			if issueType.ID != issueTypeID {
				continue
			}
			if issueType.Subtask {
				return errx.Usage("subtask issue types are not supported; choose a standard issue type")
			}
			return nil
		}
		next := page.StartAt + len(page.Values)
		if next >= page.Total {
			return errx.NotFound("issue type", issueTypeID, nil)
		}
		if next <= startAt {
			return errx.Internal("Jira issue type pagination did not advance")
		}
		startAt = next
	}
	return errx.Internal("Jira issue type pagination exceeded the safety limit")
}

// CreateIssue creates one issue using only bounded typed fields.
func (client *Client) CreateIssue(ctx context.Context, input CreateIssueRequest) (Issue, error) {
	if err := requireNumericID(input.ProjectID, "project"); err != nil {
		return Issue{}, err
	}
	if err := requireNumericID(input.IssueTypeID, "issue type"); err != nil {
		return Issue{}, err
	}
	if err := ValidateSummary(input.Summary); err != nil {
		return Issue{}, err
	}
	type identity struct {
		ID string `json:"id"`
	}
	type fields struct {
		Project     identity     `json:"project"`
		IssueType   identity     `json:"issuetype"`
		Summary     string       `json:"summary"`
		Description *ADFDocument `json:"description,omitempty"`
	}
	payload := fields{
		Project: identity{ID: input.ProjectID}, IssueType: identity{ID: input.IssueTypeID}, Summary: input.Summary,
	}
	if input.Description != nil {
		document, err := NewPlainTextDocument(*input.Description, "description")
		if err != nil {
			return Issue{}, err
		}
		payload.Description = &document
	}
	var created Issue
	err := client.do(ctx, request{
		method: http.MethodPost, path: "/rest/api/3/issue",
		body: struct {
			Fields fields `json:"fields"`
		}{Fields: payload},
		policy: requestPolicyWrite, operation: "issues.create", wantStatus: http.StatusCreated,
	}, &created)
	return created, err
}

// EditIssue updates only summary and/or description on one numeric issue ID.
func (client *Client) EditIssue(ctx context.Context, issueID string, input EditIssueRequest) error {
	if err := requireNumericID(issueID, "issue"); err != nil {
		return err
	}
	if input.Summary == nil && input.Description == nil && !input.ClearDescription {
		return errx.Usage("issues edit requires at least one changed field")
	}
	if input.Description != nil && input.ClearDescription {
		return errx.Usage("--description and --clear-description cannot be combined")
	}
	fields := make(map[string]any, 2)
	if input.Summary != nil {
		if err := ValidateSummary(*input.Summary); err != nil {
			return err
		}
		fields["summary"] = *input.Summary
	}
	if input.Description != nil {
		document, err := NewPlainTextDocument(*input.Description, "description")
		if err != nil {
			return err
		}
		fields["description"] = document
	} else if input.ClearDescription {
		fields["description"] = nil
	}
	return client.do(ctx, request{
		method: http.MethodPut, path: "/rest/api/3/issue/" + url.PathEscape(issueID),
		body: struct {
			Fields map[string]any `json:"fields"`
		}{Fields: fields},
		policy: requestPolicyWrite, operation: "issues.edit", wantStatus: http.StatusNoContent,
	}, nil)
}

// TransitionIssue applies one exact currently available transition by ID.
func (client *Client) TransitionIssue(ctx context.Context, issueID, transitionID string) error {
	if err := requireNumericID(issueID, "issue"); err != nil {
		return err
	}
	if err := requireNumericID(transitionID, "transition"); err != nil {
		return err
	}
	type transition struct {
		ID string `json:"id"`
	}
	return client.do(ctx, request{
		method: http.MethodPost, path: "/rest/api/3/issue/" + url.PathEscape(issueID) + "/transitions",
		body: struct {
			Transition transition `json:"transition"`
		}{Transition: transition{ID: transitionID}},
		policy: requestPolicyWrite, operation: "issues.transition", wantStatus: http.StatusNoContent,
	}, nil)
}

// AddComment adds one bounded plain-text ADF comment to a numeric issue ID.
func (client *Client) AddComment(ctx context.Context, issueID, body string) (Comment, error) {
	if err := requireNumericID(issueID, "issue"); err != nil {
		return Comment{}, err
	}
	if strings.TrimSpace(body) == "" {
		return Comment{}, errx.Usage("--body must not be empty")
	}
	document, err := NewPlainTextDocument(body, "body")
	if err != nil {
		return Comment{}, err
	}
	var comment Comment
	err = client.do(ctx, request{
		method: http.MethodPost, path: "/rest/api/3/issue/" + url.PathEscape(issueID) + "/comment",
		body: struct {
			Body ADFDocument `json:"body"`
		}{Body: document},
		policy: requestPolicyWrite, operation: "comments.add", wantStatus: http.StatusCreated,
	}, &comment)
	return comment, err
}

func requireNumericID(value, kind string) error {
	if value == "" || len(value) > 32 {
		return errx.Usage("%s ID must be numeric", kind)
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			return errx.Usage("%s ID must be numeric", kind)
		}
	}
	return nil
}
