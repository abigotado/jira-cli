package jira

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/abigotado/jira-cli/internal/errx"
)

const (
	issueTypePageSize = 100
	maxIssueTypePages = 100
	maxIssueTypes     = issueTypePageSize * maxIssueTypePages
)

// IssueTypes returns issue types that can be created in one exact project.
func (client *Client) IssueTypes(ctx context.Context, projectID string, options IssueTypePageOptions) (IssueTypePage, error) {
	if err := requireNumericID(projectID, "project"); err != nil {
		return IssueTypePage{}, err
	}
	if options.StartAt < 0 || options.MaxResults < 0 || options.MaxResults > issueTypePageSize {
		return IssueTypePage{}, errx.Usage("issue type pagination values must use a nonnegative offset and a limit no greater than %d", issueTypePageSize)
	}
	query := make(url.Values)
	if options.StartAt > 0 {
		query.Set("startAt", strconv.Itoa(options.StartAt))
	}
	if options.MaxResults > 0 {
		query.Set("maxResults", strconv.Itoa(options.MaxResults))
	}
	var wire issueTypePageWire
	err := client.do(ctx, request{
		method:   http.MethodGet,
		path:     "/rest/api/3/issue/createmeta/" + url.PathEscape(projectID) + "/issuetypes",
		query:    query,
		policy:   requestPolicyRead,
		notFound: "project",
	}, &wire)
	if err != nil {
		return IssueTypePage{}, err
	}
	return validatedIssueTypePage(wire, options.StartAt)
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
	startAt := 0
	total := -1
	foundStandard := false
	foundSubtask := false
	seen := make(map[string]struct{})
	for pageNumber := 0; pageNumber < maxIssueTypePages; pageNumber++ {
		page, err := client.IssueTypes(ctx, projectID, IssueTypePageOptions{StartAt: startAt, MaxResults: issueTypePageSize})
		if err != nil {
			return err
		}
		if total < 0 {
			total = page.Total
		} else if page.Total != total {
			return errx.Internal("Jira issue type pagination changed its total")
		}
		for _, issueType := range page.Values {
			if _, exists := seen[issueType.ID]; exists {
				return errx.Internal("Jira issue type metadata contains a duplicate issue type ID")
			}
			seen[issueType.ID] = struct{}{}
			if issueType.ID == issueTypeID {
				if issueType.Subtask {
					foundSubtask = true
				} else {
					foundStandard = true
				}
			}
		}
		next := page.StartAt + len(page.Values)
		if next == total {
			if foundSubtask {
				return errx.Usage("subtask issue types are not supported; choose a standard issue type")
			}
			if foundStandard {
				return nil
			}
			return errx.NotFound("issue type", issueTypeID, nil)
		}
		startAt = next
		if pageNumber == maxIssueTypePages-1 {
			return errx.Internal("Jira issue type pagination exceeded the safety limit")
		}
	}
	return errx.Internal("Jira issue type pagination exceeded the safety limit")
}

func validatedIssueTypePage(wire issueTypePageWire, requestedStartAt int) (IssueTypePage, error) {
	if wire.StartAt == nil || wire.MaxResults == nil || wire.Total == nil || wire.Values == nil {
		return IssueTypePage{}, errx.Internal("Jira issue type metadata is incomplete")
	}
	if *wire.StartAt != requestedStartAt {
		return IssueTypePage{}, errx.Internal("Jira issue type pagination returned an unexpected offset")
	}
	if *wire.MaxResults <= 0 || *wire.MaxResults > issueTypePageSize {
		return IssueTypePage{}, errx.Internal("Jira issue type pagination returned an invalid page size")
	}
	if *wire.Total < 0 || *wire.Total > maxIssueTypes {
		return IssueTypePage{}, errx.Internal("Jira issue type pagination returned an invalid total")
	}
	if *wire.StartAt > *wire.Total || len(wire.Values) > *wire.MaxResults || len(wire.Values) > issueTypePageSize {
		return IssueTypePage{}, errx.Internal("Jira issue type pagination overran its total")
	}
	next := *wire.StartAt + len(wire.Values)
	if next > *wire.Total || (*wire.StartAt < *wire.Total && next <= *wire.StartAt) {
		return IssueTypePage{}, errx.Internal("Jira issue type pagination did not make valid progress")
	}

	values := make([]IssueType, 0, len(wire.Values))
	seen := make(map[string]struct{}, len(wire.Values))
	for _, issueType := range wire.Values {
		if issueType.ID == nil || !validNumericMetadataID(*issueType.ID) || issueType.Subtask == nil {
			return IssueTypePage{}, errx.Internal("Jira issue type metadata is incomplete or malformed")
		}
		if _, exists := seen[*issueType.ID]; exists {
			return IssueTypePage{}, errx.Internal("Jira issue type metadata contains a duplicate issue type ID")
		}
		seen[*issueType.ID] = struct{}{}
		values = append(values, IssueType{
			ID: *issueType.ID, Name: issueType.Name, Description: issueType.Description, Subtask: *issueType.Subtask,
		})
	}
	return IssueTypePage{
		StartAt: *wire.StartAt, MaxResults: *wire.MaxResults, Total: *wire.Total, Values: values,
	}, nil
}

func validNumericMetadataID(value string) bool {
	if value == "" || len(value) > 32 {
		return false
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			return false
		}
	}
	return true
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
	if err := ValidateLabels(input.Labels); err != nil {
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
		Labels      []string     `json:"labels,omitempty"`
	}
	payload := fields{
		Project: identity{ID: input.ProjectID}, IssueType: identity{ID: input.IssueTypeID}, Summary: input.Summary,
		Labels: append([]string(nil), input.Labels...),
	}
	if input.Description != nil {
		document, err := NewPlainTextDocument(*input.Description, "description")
		if err != nil {
			return Issue{}, err
		}
		payload.Description = &document
	}
	if err := client.ValidateIssueType(ctx, input.ProjectID, input.IssueTypeID); err != nil {
		return Issue{}, err
	}
	if err := client.validateCreateFields(ctx, input); err != nil {
		return Issue{}, err
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

func (client *Client) validateCreateFields(ctx context.Context, input CreateIssueRequest) error {
	const (
		pageSize      = 100
		maxPages      = 100
		maxFields     = 10_000
		maxFieldIDLen = 255
		maxOperation  = 64
	)

	provided := map[string]bool{
		"project":   true,
		"issuetype": true,
		"summary":   true,
	}
	if input.Description != nil {
		provided["description"] = true
	}
	if len(input.Labels) > 0 {
		provided["labels"] = true
	}

	seen := make(map[string]struct{})
	unsupported := make(map[string]struct{})
	provideDescription := false
	provideLabels := false
	startAt := 0
	total := -1
	for pageNumber := 0; pageNumber < maxPages; pageNumber++ {
		query := make(url.Values)
		query.Set("startAt", strconv.Itoa(startAt))
		query.Set("maxResults", strconv.Itoa(pageSize))
		var page IssueCreateFieldPage
		if err := client.do(ctx, request{
			method: http.MethodGet,
			path:   "/rest/api/3/issue/createmeta/" + url.PathEscape(input.ProjectID) + "/issuetypes/" + url.PathEscape(input.IssueTypeID),
			query:  query, policy: requestPolicyRead, notFound: "issue type",
		}, &page); err != nil {
			return err
		}
		if page.StartAt == nil || page.MaxResults == nil || page.Total == nil || page.Fields == nil {
			return errx.Internal("Jira create-field metadata is incomplete")
		}
		if *page.StartAt != startAt {
			return errx.Internal("Jira create-field pagination returned an unexpected offset")
		}
		if *page.MaxResults <= 0 || *page.MaxResults > pageSize {
			return errx.Internal("Jira create-field pagination returned an invalid page size")
		}
		if *page.Total < 0 || *page.Total > maxFields {
			return errx.Internal("Jira create-field pagination returned an invalid total")
		}
		if total < 0 {
			total = *page.Total
			if total == 0 {
				return errx.Internal("Jira create-field metadata is empty")
			}
		} else if *page.Total != total {
			return errx.Internal("Jira create-field pagination changed its total")
		}
		if startAt >= total || len(page.Fields) == 0 || len(page.Fields) > *page.MaxResults || len(page.Fields) > pageSize {
			return errx.Internal("Jira create-field pagination did not make valid progress")
		}
		next := startAt + len(page.Fields)
		if next <= startAt || next > total || next > maxFields {
			return errx.Internal("Jira create-field pagination overran its total")
		}

		for _, field := range page.Fields {
			if field.FieldID == "" || len(field.FieldID) > maxFieldIDLen || !validMetadataIdentifier(field.FieldID) {
				return errx.Internal("Jira create-field metadata contains an invalid field ID")
			}
			if _, exists := seen[field.FieldID]; exists {
				return errx.Internal("Jira create-field metadata contains a duplicate field ID")
			}
			seen[field.FieldID] = struct{}{}
			if field.Required == nil || field.Operations == nil {
				return errx.Internal("Jira create-field metadata is incomplete")
			}
			supportsSet := false
			for _, operation := range field.Operations {
				if operation == "" || len(operation) > maxOperation || !validMetadataIdentifier(operation) {
					return errx.Internal("Jira create-field metadata contains an invalid operation")
				}
				if operation == "set" {
					supportsSet = true
				}
			}
			hasDefault := field.HasDefaultValue != nil && *field.HasDefaultValue
			if *field.Required && !provided[field.FieldID] && !hasDefault {
				unsupported[field.FieldID] = struct{}{}
				if field.FieldID == "description" && supportsSet {
					provideDescription = true
				}
				if field.FieldID == "labels" && supportsSet {
					provideLabels = true
				}
			}
			if (field.FieldID == "summary" ||
				field.FieldID == "description" && input.Description != nil ||
				field.FieldID == "labels" && len(input.Labels) > 0) && !supportsSet {
				unsupported[field.FieldID] = struct{}{}
			}
		}

		startAt = next
		if startAt == total {
			break
		}
		if pageNumber == maxPages-1 {
			return errx.Internal("Jira create-field pagination exceeded the safety limit")
		}
	}
	if startAt != total {
		return errx.Internal("Jira create-field pagination ended before its total")
	}
	if _, found := seen["summary"]; !found {
		unsupported["summary"] = struct{}{}
	}
	if input.Description != nil {
		if _, found := seen["description"]; !found {
			unsupported["description"] = struct{}{}
		}
	}
	if len(input.Labels) > 0 {
		if _, found := seen["labels"]; !found {
			unsupported["labels"] = struct{}{}
		}
	}
	if len(unsupported) == 0 {
		return nil
	}
	fieldIDs := make([]string, 0, len(unsupported))
	for fieldID := range unsupported {
		fieldIDs = append(fieldIDs, fieldID)
	}
	return errx.CreateFieldsUnsupported(fieldIDs, provideDescription, provideLabels)
}

func validMetadataIdentifier(value string) bool {
	for _, character := range value {
		switch {
		case character >= 'a' && character <= 'z':
		case character >= 'A' && character <= 'Z':
		case character >= '0' && character <= '9':
		case character == '_', character == '-', character == '.', character == ':':
		default:
			return false
		}
	}
	return value != ""
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
