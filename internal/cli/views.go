package cli

import (
	"encoding/json"
	"sort"
	"time"

	"github.com/abigotado/jira-cli/internal/jira"
	"github.com/abigotado/jira-cli/internal/output"
	"github.com/abigotado/jira-cli/internal/profile"
)

type profileView struct {
	Name      string  `json:"name"`
	Site      string  `json:"site"`
	Email     string  `json:"email"`
	TokenKind string  `json:"token_kind"`
	CloudID   string  `json:"cloud_id,omitempty"`
	ExpiresAt *string `json:"expires_at,omitempty"`
	State     string  `json:"credential_state,omitempty"`
}

func newProfileView(value profile.Profile, state string) profileView {
	view := profileView{
		Name: value.Name, Site: value.Site, Email: value.Email,
		TokenKind: string(value.TokenKind), CloudID: value.CloudID, State: state,
	}
	if value.ExpiresAt != nil {
		formatted := value.ExpiresAt.Format(time.DateOnly)
		view.ExpiresAt = &formatted
	}
	return view
}

func (view profileView) Fields() []output.Field {
	return []output.Field{
		{Name: "name", Value: view.Name, Raw: view.Name},
		{Name: "site", Value: view.Site, Raw: view.Site},
		{Name: "email", Value: view.Email, Raw: view.Email},
		{Name: "token_kind", Value: view.TokenKind, Raw: view.TokenKind},
		{Name: "cloud_id", Value: view.CloudID, Raw: view.CloudID, OnRequest: true},
		{Name: "expires_at", Raw: view.ExpiresAt, OnRequest: view.ExpiresAt == nil},
		{Name: "credential_state", Value: view.State, Raw: view.State, OnRequest: view.State == ""},
	}
}

type userView jira.User

func (view userView) Fields() []output.Field {
	return []output.Field{
		{Name: "account_id", Value: view.AccountID, Raw: view.AccountID},
		{Name: "display_name", Value: view.DisplayName, Raw: view.DisplayName},
		{Name: "email", Value: view.EmailAddress, Raw: view.EmailAddress, OnRequest: true},
		{Name: "active", Raw: view.Active, OnRequest: true},
	}
}

type projectView jira.Project

func (view projectView) Fields() []output.Field {
	return []output.Field{
		{Name: "key", Value: view.Key, Raw: view.Key},
		{Name: "name", Value: view.Name, Raw: view.Name},
		{Name: "id", Value: view.ID, Raw: view.ID, OnRequest: true},
		{Name: "type", Value: view.ProjectType, Raw: view.ProjectType, OnRequest: true},
		{Name: "simplified", Raw: view.Simplified, OnRequest: true},
	}
}

type transitionView jira.Transition

func (view transitionView) Fields() []output.Field {
	return []output.Field{
		{Name: "id", Value: view.ID, Raw: view.ID},
		{Name: "name", Value: view.Name, Raw: view.Name},
		{Name: "to_status", Value: view.To.Name, Raw: view.To.Name},
		{Name: "to_status_id", Value: view.To.ID, Raw: view.To.ID, OnRequest: true},
	}
}

type commentView jira.Comment

func (view commentView) Fields() []output.Field {
	return []output.Field{
		{Name: "id", Value: view.ID, Raw: view.ID},
		{Name: "author", Value: view.Author.DisplayName, Raw: view.Author.DisplayName},
		{Name: "created", Value: view.Created, Raw: view.Created},
		{Name: "updated", Value: view.Updated, Raw: view.Updated},
		{Name: "body", Raw: rawValue(view.Body), OnRequest: true},
	}
}

type issueView struct {
	issue     jira.Issue
	requested []string
}

func (view issueView) MarshalJSON() ([]byte, error) {
	return json.Marshal(view.issue)
}

func (view issueView) Fields() []output.Field {
	fields := []output.Field{
		{Name: "key", Value: view.issue.Key, Raw: view.issue.Key},
		{Name: "id", Value: view.issue.ID, Raw: view.issue.ID, OnRequest: true},
		{Name: "self", Value: view.issue.Self, Raw: view.issue.Self, OnRequest: true},
	}
	knownOrder := []string{"summary", "status", "assignee", "updated", "description"}
	seen := make(map[string]bool, len(knownOrder))
	for _, name := range knownOrder {
		raw := view.issue.Fields[name]
		seen[name] = true
		fields = append(fields, issueField(name, raw))
	}
	extra := make([]string, 0, len(view.issue.Fields))
	for name := range view.issue.Fields {
		if !seen[name] {
			extra = append(extra, name)
		}
	}
	sort.Strings(extra)
	for _, name := range extra {
		field := issueField(name, view.issue.Fields[name])
		field.OnRequest = true
		fields = append(fields, field)
	}
	for _, name := range view.requested {
		if name == "key" || name == "id" || name == "self" || seen[name] {
			continue
		}
		seen[name] = true
		fields = append(fields, output.Field{Name: name, Raw: nil, OnRequest: true})
	}
	return fields
}

func issueField(name string, raw json.RawMessage) output.Field {
	value := rawValue(raw)
	field := output.Field{Name: name, Raw: value, OnRequest: name == "description"}
	switch name {
	case "summary", "updated":
		if text, ok := value.(string); ok {
			field.Value = text
		}
	case "status":
		field.Value = nestedString(value, "name")
	case "assignee":
		field.Value = nestedString(value, "displayName")
	}
	return field
}

func rawValue(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil
	}
	return value
}

func nestedString(value any, key string) string {
	object, ok := value.(map[string]any)
	if !ok {
		return ""
	}
	text, _ := object[key].(string)
	return text
}

func asProjectViews(values []jira.Project) []projectView {
	views := make([]projectView, len(values))
	for index, value := range values {
		views[index] = projectView(value)
	}
	return views
}

type issueViewCollection struct {
	issues    []jira.Issue
	requested []string
}

func (collection issueViewCollection) RenderRows() []output.Renderable {
	rows := make([]output.Renderable, len(collection.issues))
	for index, value := range collection.issues {
		rows[index] = issueView{issue: value, requested: collection.requested}
	}
	return rows
}

func (collection issueViewCollection) SchemaFields() []output.Field {
	return issueView{requested: collection.requested}.Fields()
}

func (collection issueViewCollection) MarshalJSON() ([]byte, error) {
	return json.Marshal(collection.issues)
}

func asIssueViews(values []jira.Issue, requested []string) issueViewCollection {
	issues := make([]jira.Issue, len(values))
	copy(issues, values)
	return issueViewCollection{issues: issues, requested: append([]string(nil), requested...)}
}

func asTransitionViews(values []jira.Transition) []transitionView {
	views := make([]transitionView, len(values))
	for index, value := range values {
		views[index] = transitionView(value)
	}
	return views
}

func asCommentViews(values []jira.Comment) []commentView {
	views := make([]commentView, len(values))
	for index, value := range values {
		views[index] = commentView(value)
	}
	return views
}

type versionView struct {
	Version    string `json:"version"`
	Commit     string `json:"commit"`
	CommitTime string `json:"commit_time"`
	Go         string `json:"go"`
	OS         string `json:"os"`
	Arch       string `json:"arch"`
}

func (view versionView) Fields() []output.Field {
	return []output.Field{
		{Name: "version", Value: view.Version, Raw: view.Version},
		{Name: "commit", Value: view.Commit, Raw: view.Commit, OnRequest: true},
		{Name: "commit_time", Value: view.CommitTime, Raw: view.CommitTime, OnRequest: true},
		{Name: "go", Value: view.Go, Raw: view.Go, OnRequest: true},
		{Name: "os", Value: view.OS, Raw: view.OS, OnRequest: true},
		{Name: "arch", Value: view.Arch, Raw: view.Arch, OnRequest: true},
	}
}
