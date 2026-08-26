package jira

import (
	"encoding/json"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/abigotado/jira-cli/internal/errx"
)

// User is the stable subset returned by /myself.
type User struct {
	AccountID    string `json:"accountId"`
	DisplayName  string `json:"displayName"`
	EmailAddress string `json:"emailAddress,omitempty"`
	Active       bool   `json:"active"`
}

// Project is the stable subset needed to identify a Jira project.
type Project struct {
	ID          string `json:"id"`
	Key         string `json:"key"`
	Name        string `json:"name"`
	ProjectType string `json:"projectTypeKey,omitempty"`
	Simplified  bool   `json:"simplified,omitempty"`
}

// ProjectPage is an offset-based Jira project page.
type ProjectPage struct {
	StartAt    int       `json:"startAt"`
	MaxResults int       `json:"maxResults"`
	Total      int       `json:"total"`
	IsLast     bool      `json:"isLast"`
	Values     []Project `json:"values"`
}

// Issue contains stable identity fields plus exactly the requested Jira
// fields. Dynamic field values remain JSON so callers do not lose data.
type Issue struct {
	ID     string                     `json:"id"`
	Key    string                     `json:"key"`
	Self   string                     `json:"self,omitempty"`
	Fields map[string]json.RawMessage `json:"fields,omitempty"`
}

// SearchPage is a token-paginated enhanced JQL response.
type SearchPage struct {
	Issues        []Issue `json:"issues"`
	NextPageToken string  `json:"nextPageToken,omitempty"`
	IsLast        bool    `json:"isLast,omitempty"`
}

// Transition describes one transition currently available for an issue.
type Transition struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	To   Status `json:"to"`
}

// Status is a Jira workflow status.
type Status struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type transitionsResponse struct {
	Transitions []Transition `json:"transitions"`
}

// Comment is the stable subset of an issue comment. Body is Atlassian
// Document Format and remains raw JSON for lossless machine output.
type Comment struct {
	ID      string          `json:"id"`
	Author  User            `json:"author"`
	Body    json.RawMessage `json:"body"`
	Created string          `json:"created"`
	Updated string          `json:"updated"`
}

// CommentPage is an offset-based Jira comment page.
type CommentPage struct {
	StartAt    int       `json:"startAt"`
	MaxResults int       `json:"maxResults"`
	Total      int       `json:"total"`
	Comments   []Comment `json:"comments"`
}

// ProjectPageOptions controls Jira's offset-based project pagination.
type ProjectPageOptions struct {
	StartAt    int
	MaxResults int
	Query      string
}

// SearchRequest is the enhanced JQL request body.
type SearchRequest struct {
	JQL           string   `json:"jql"`
	Fields        []string `json:"fields"`
	MaxResults    int      `json:"maxResults,omitempty"`
	NextPageToken string   `json:"nextPageToken,omitempty"`
}

// CommentPageOptions controls offset-based issue comment pagination.
type CommentPageOptions struct {
	StartAt    int
	MaxResults int
}

// IssueType is the stable subset of a Jira issue type used for safe create
// discovery.
type IssueType struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Subtask     bool   `json:"subtask"`
}

// IssueTypePage is an offset-based create-metadata issue type page.
type IssueTypePage struct {
	StartAt    int         `json:"startAt"`
	MaxResults int         `json:"maxResults"`
	Total      int         `json:"total"`
	Values     []IssueType `json:"issueTypes"`
}

// IssueTypePageOptions controls issue type pagination.
type IssueTypePageOptions struct {
	StartAt    int
	MaxResults int
}

// ADFDocument is the deliberately narrow Atlassian Document Format accepted
// by jira-cli: one deterministic paragraph containing plain text only.
type ADFDocument struct {
	Version int       `json:"version"`
	Type    string    `json:"type"`
	Content []ADFNode `json:"content"`
}

// ADFNode is a paragraph in the narrow plain-text document model.
type ADFNode struct {
	Type    string      `json:"type"`
	Content []ADFInline `json:"content,omitempty"`
}

// ADFInline is either an unformatted text leaf or a hard line break.
type ADFInline struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

const (
	maxSummaryRunes = 255
	maxBodyRunes    = 32767
)

// NewPlainTextDocument validates and converts bounded plain text to ADF.
func NewPlainTextDocument(value, field string) (ADFDocument, error) {
	if !utf8.ValidString(value) || strings.ContainsRune(value, '\x00') {
		return ADFDocument{}, errx.Usage("--%s must be valid text without NUL bytes", field)
	}
	if utf8.RuneCountInString(value) > maxBodyRunes {
		return ADFDocument{}, errx.Usage("--%s cannot exceed %d characters", field, maxBodyRunes)
	}
	normalized := strings.ReplaceAll(value, "\r\n", "\n")
	normalized = strings.ReplaceAll(normalized, "\r", "\n")
	paragraph := ADFNode{Type: "paragraph"}
	if normalized != "" {
		lines := strings.Split(normalized, "\n")
		paragraph.Content = make([]ADFInline, 0, len(lines)*2-1)
		for index, line := range lines {
			if line != "" {
				paragraph.Content = append(paragraph.Content, ADFInline{Type: "text", Text: line})
			}
			if index < len(lines)-1 {
				paragraph.Content = append(paragraph.Content, ADFInline{Type: "hardBreak"})
			}
		}
	}
	return ADFDocument{Version: 1, Type: "doc", Content: []ADFNode{paragraph}}, nil
}

// CreateIssueRequest contains the bounded fields jira-cli supports creating.
type CreateIssueRequest struct {
	ProjectID   string
	IssueTypeID string
	Summary     string
	Description *string
}

// EditIssueRequest contains the bounded fields jira-cli supports editing.
type EditIssueRequest struct {
	Summary          *string
	Description      *string
	ClearDescription bool
}

// ValidateSummary validates Jira's bounded one-line summary field.
func ValidateSummary(summary string) error {
	if strings.TrimSpace(summary) == "" || summary != strings.TrimSpace(summary) || !utf8.ValidString(summary) || containsLineOrControl(summary) {
		return errx.Usage("--summary must be non-empty single-line text without surrounding whitespace")
	}
	if utf8.RuneCountInString(summary) > maxSummaryRunes {
		return errx.Usage("--summary cannot exceed %d characters", maxSummaryRunes)
	}
	return nil
}

func containsLineOrControl(value string) bool {
	for _, character := range value {
		if unicode.IsControl(character) || unicode.Is(unicode.Zl, character) || unicode.Is(unicode.Zp, character) {
			return true
		}
	}
	return false
}
