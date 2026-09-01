package cli

import (
	"github.com/abigotado/jira-cli/internal/jira"
	"github.com/abigotado/jira-cli/internal/output"
	"github.com/abigotado/jira-cli/internal/writepolicy"
)

type issueTypeView jira.IssueType

func (view issueTypeView) Fields() []output.Field {
	return []output.Field{
		{Name: "id", Value: view.ID, Raw: view.ID},
		{Name: "name", Value: view.Name, Raw: view.Name},
		{Name: "description", Value: view.Description, Raw: view.Description, OnRequest: true},
		{Name: "subtask", Raw: view.Subtask},
	}
}

func asIssueTypeViews(values []jira.IssueType) []issueTypeView {
	views := make([]issueTypeView, len(values))
	for index, value := range values {
		views[index] = issueTypeView(value)
	}
	return views
}

type mutationReceipt struct {
	Action        string   `json:"action"`
	DryRun        bool     `json:"dry_run"`
	Applied       bool     `json:"applied"`
	RemoteChecks  string   `json:"remote_checks"`
	Project       string   `json:"project"`
	IssueKey      string   `json:"issue_key,omitempty"`
	IssueID       string   `json:"issue_id,omitempty"`
	IssueTypeID   string   `json:"issue_type_id,omitempty"`
	TransitionID  string   `json:"transition_id,omitempty"`
	CommentID     string   `json:"comment_id,omitempty"`
	ChangedFields []string `json:"changed_fields,omitempty"`
	Self          string   `json:"self,omitempty"`
}

func (view mutationReceipt) Fields() []output.Field {
	return []output.Field{
		{Name: "action", Value: view.Action, Raw: view.Action},
		{Name: "dry_run", Raw: view.DryRun},
		{Name: "applied", Raw: view.Applied},
		{Name: "remote_checks", Value: view.RemoteChecks, Raw: view.RemoteChecks},
		{Name: "project", Value: view.Project, Raw: view.Project},
		{Name: "issue_key", Value: view.IssueKey, Raw: omitEmpty(view.IssueKey), OnRequest: view.IssueKey == ""},
		{Name: "issue_id", Value: view.IssueID, Raw: omitEmpty(view.IssueID), OnRequest: view.IssueID == ""},
		{Name: "issue_type_id", Value: view.IssueTypeID, Raw: omitEmpty(view.IssueTypeID), OnRequest: view.IssueTypeID == ""},
		{Name: "transition_id", Value: view.TransitionID, Raw: omitEmpty(view.TransitionID), OnRequest: view.TransitionID == ""},
		{Name: "comment_id", Value: view.CommentID, Raw: omitEmpty(view.CommentID), OnRequest: view.CommentID == ""},
		{Name: "changed_fields", Raw: view.ChangedFields, OnRequest: len(view.ChangedFields) == 0},
		{Name: "self", Value: view.Self, Raw: omitEmpty(view.Self), OnRequest: view.Self == ""},
	}
}

type writePolicyView struct {
	Profile   string   `json:"profile"`
	Site      string   `json:"site"`
	Email     string   `json:"email"`
	TokenKind string   `json:"token_kind"`
	CloudID   string   `json:"cloud_id,omitempty"`
	Projects  []string `json:"projects"`
	State     string   `json:"state"`
	DryRun    bool     `json:"dry_run"`
	Applied   bool     `json:"applied"`
}

func newWritePolicyView(policy writepolicy.Policy, state string, dryRun, applied bool) writePolicyView {
	projects := append([]string{}, policy.Projects...)
	return writePolicyView{
		Profile: policy.Profile, Site: policy.Identity.Site, Email: policy.Identity.Email,
		TokenKind: policy.Identity.TokenKind, CloudID: policy.Identity.CloudID,
		Projects: projects, State: state, DryRun: dryRun, Applied: applied,
	}
}

func (view writePolicyView) Fields() []output.Field {
	return []output.Field{
		{Name: "profile", Value: view.Profile, Raw: view.Profile},
		{Name: "site", Value: view.Site, Raw: view.Site},
		{Name: "email", Value: view.Email, Raw: view.Email},
		{Name: "token_kind", Value: view.TokenKind, Raw: view.TokenKind},
		{Name: "cloud_id", Value: view.CloudID, Raw: omitEmpty(view.CloudID), OnRequest: view.CloudID == ""},
		{Name: "projects", Raw: view.Projects},
		{Name: "state", Value: view.State, Raw: view.State},
		{Name: "dry_run", Raw: view.DryRun},
		{Name: "applied", Raw: view.Applied},
	}
}

func omitEmpty(value string) any {
	if value == "" {
		return nil
	}
	return value
}
