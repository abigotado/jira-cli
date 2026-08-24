package jira

import "encoding/json"

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
