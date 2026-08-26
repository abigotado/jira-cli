package cli

import (
	"context"
	"regexp"
	"strings"

	"github.com/abigotado/jira-cli/internal/errx"
	"github.com/abigotado/jira-cli/internal/jira"
	"github.com/abigotado/jira-cli/internal/profile"
	"github.com/abigotado/jira-cli/internal/writepolicy"
	"github.com/spf13/cobra"
)

var (
	exactProjectKeyPattern = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,31}$`)
	exactIssueKeyPattern   = regexp.MustCompile(`^([A-Z][A-Z0-9_]{0,31})-[1-9][0-9]*$`)
)

func (a *App) newAuthAllowProjectsCommand() *cobra.Command {
	command := commandGroup("allow-projects", "Manage the identity-bound local write allowlist")
	command.AddCommand(
		a.newAuthAllowProjectsShowCommand(),
		a.newAuthAllowProjectsSetCommand(),
		a.newAuthAllowProjectsClearCommand(),
	)
	return command
}

func (a *App) newAuthAllowProjectsShowCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "show",
		Short: "Show the current profile's non-secret write policy",
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			if a.dryRun {
				return errx.Usage("--dry-run is not supported by auth allow-projects show")
			}
			if a.profileName == "" {
				return errx.ProfileRequired()
			}
			if err := a.out.Validate(writePolicyView{}); err != nil {
				return err
			}
			if a.registry == nil || a.policies == nil {
				return errx.Internal("profile or write policy registry is unavailable")
			}
			err := a.registry.WithProfileLock(cmd.Context(), a.profileName, func() error {
				selected, err := a.registry.Get(cmd.Context(), a.profileName)
				if err != nil {
					return translateLocal(err, a.profileName)
				}
				return a.policies.WithPolicyLock(cmd.Context(), a.profileName, func() error {
					policy, err := a.policies.Get(cmd.Context(), a.profileName)
					if err != nil {
						return translateWritePolicy(err, a.profileName, "")
					}
					state := "bound"
					if policy.Identity != writepolicy.IdentityFor(selected) {
						state = "stale"
					}
					return a.out.Success(newWritePolicyView(policy, state, false, false))
				})
			})
			return translateLocalLockBoundary(err)
		},
	}
}

func (a *App) newAuthAllowProjectsSetCommand() *cobra.Command {
	var projects []string
	command := &cobra.Command{
		Use:   "set",
		Short: "Replace the exact projects allowed for writes",
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			if a.profileName == "" {
				return errx.ProfileRequired()
			}
			if err := a.out.Validate(writePolicyView{}); err != nil {
				return err
			}
			canonical, err := writepolicy.CanonicalProjects(projects)
			if err != nil {
				return translateWritePolicy(err, a.profileName, "")
			}
			if !a.dryRun && !a.assumeYes {
				return errx.ConfirmRequired("auth allow-projects set")
			}
			if a.registry == nil || a.policies == nil {
				return errx.Internal("profile or write policy registry is unavailable")
			}
			err = a.registry.WithProfileLock(cmd.Context(), a.profileName, func() error {
				selected, err := a.registry.Get(cmd.Context(), a.profileName)
				if err != nil {
					return translateLocal(err, a.profileName)
				}
				return a.policies.WithPolicyLock(cmd.Context(), a.profileName, func() error {
					policy := writepolicy.Policy{Profile: selected.Name, Identity: writepolicy.IdentityFor(selected), Projects: canonical}
					if !a.dryRun {
						policy, err = a.policies.Set(cmd.Context(), selected, canonical)
						if err != nil {
							return translateWritePolicyLockBoundary(err, selected.Name)
						}
					}
					return a.out.Success(newWritePolicyView(policy, "bound", a.dryRun, !a.dryRun))
				})
			})
			return translateLocalLockBoundary(err)
		},
	}
	command.Flags().StringArrayVar(&projects, "project", nil, "exact Jira project key to allow (repeatable)")
	return command
}

func (a *App) newAuthAllowProjectsClearCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "clear",
		Short: "Remove the current profile's local write policy",
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			if a.profileName == "" {
				return errx.ProfileRequired()
			}
			if err := a.out.Validate(writePolicyView{}); err != nil {
				return err
			}
			if !a.dryRun && !a.assumeYes {
				return errx.ConfirmRequired("auth allow-projects clear")
			}
			if a.registry == nil || a.policies == nil {
				return errx.Internal("profile or write policy registry is unavailable")
			}
			err := a.registry.WithProfileLock(cmd.Context(), a.profileName, func() error {
				selected, err := a.registry.Get(cmd.Context(), a.profileName)
				if err != nil {
					return translateLocal(err, a.profileName)
				}
				return a.policies.WithPolicyLock(cmd.Context(), a.profileName, func() error {
					if !a.dryRun {
						if err := a.policies.Clear(cmd.Context(), selected.Name); err != nil {
							return translateWritePolicyLockBoundary(err, selected.Name)
						}
					}
					policy := writepolicy.Policy{Profile: selected.Name, Identity: writepolicy.IdentityFor(selected), Projects: []string{}}
					return a.out.Success(newWritePolicyView(policy, "cleared", a.dryRun, !a.dryRun))
				})
			})
			return translateLocalLockBoundary(err)
		},
	}
}

func (a *App) newIssueTypesCommand() *cobra.Command {
	var projectKey string
	var limit int
	var cursor string
	command := &cobra.Command{
		Use:   "types",
		Short: "List supported standard issue types for one exact project",
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := requireExactProjectKey(projectKey); err != nil {
				return err
			}
			if err := validateLimit(limit); err != nil {
				return err
			}
			startAt, err := decodeOffsetCursor(cursor, "issue type")
			if err != nil {
				return err
			}
			reader, _, err := a.client(cmd.Context())
			if err != nil {
				return err
			}
			client, ok := reader.(jiraMutationClient)
			if !ok {
				return errx.Internal("Jira client does not implement issue type discovery")
			}
			project, err := exactProject(cmd.Context(), client, projectKey)
			if err != nil {
				return err
			}
			page, err := client.IssueTypes(cmd.Context(), project.ID, jira.IssueTypePageOptions{StartAt: startAt, MaxResults: limit})
			if err != nil {
				return err
			}
			truncated := page.StartAt+len(page.Values) < page.Total
			next := ""
			if truncated {
				nextOffset := page.StartAt + len(page.Values)
				if nextOffset <= startAt {
					return errx.Internal("Jira issue type pagination did not advance")
				}
				next = encodeOffsetCursor(nextOffset)
			}
			return a.out.SuccessPage(asIssueTypeViews(standardIssueTypes(page.Values)), truncated, next)
		},
	}
	flags := command.Flags()
	flags.StringVar(&projectKey, "project", "", "exact uppercase Jira project key")
	flags.IntVar(&limit, "limit", 50, "maximum issue types in this page (1-100)")
	flags.StringVar(&cursor, "cursor", "", "opaque cursor from meta.next_cursor")
	return command
}

func (a *App) newIssuesCreateCommand() *cobra.Command {
	var projectKey, issueTypeID, summary, description string
	var labels []string
	command := &cobra.Command{
		Use:   "create",
		Short: "Create one bounded issue in an allowed project",
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := requireExactProjectKey(projectKey); err != nil {
				return err
			}
			if err := requireNumericFlag(issueTypeID, "--issue-type-id"); err != nil {
				return err
			}
			if err := jira.ValidateSummary(summary); err != nil {
				return err
			}
			if err := jira.ValidateLabels(labels); err != nil {
				return err
			}
			if _, err := jira.NewPlainTextDocument(description, "description"); err != nil && cmd.Flags().Changed("description") {
				return err
			}
			input := jira.CreateIssueRequest{IssueTypeID: issueTypeID, Summary: summary, Labels: append([]string(nil), labels...)}
			if cmd.Flags().Changed("description") {
				input.Description = &description
			}
			return a.runMutation(cmd.Context(), projectKey, "issues.create", func(client jiraMutationClient, _ profile.Profile) error {
				receipt := mutationReceipt{Action: "issues.create", DryRun: a.dryRun, Applied: !a.dryRun, RemoteChecks: remoteCheckState(a.dryRun), Project: projectKey, IssueTypeID: issueTypeID}
				if a.dryRun {
					return a.out.Success(receipt)
				}
				project, err := exactProject(cmd.Context(), client, projectKey)
				if err != nil {
					return err
				}
				input.ProjectID = project.ID
				created, err := client.CreateIssue(cmd.Context(), input)
				if err != nil {
					return err
				}
				createdProject, keyErr := projectFromIssueKey(created.Key)
				if keyErr != nil || createdProject != projectKey || requireNumericFlag(created.ID, "Jira issue ID") != nil {
					return errx.WriteOutcomeUnknown("issues.create")
				}
				verified, err := verifyIssueProject(cmd.Context(), client, created.ID, projectKey, "issues.create")
				if err != nil {
					return err
				}
				receipt.IssueKey, receipt.IssueID, receipt.Self = verified.Key, verified.ID, created.Self
				return a.out.Success(receipt)
			})
		},
	}
	flags := command.Flags()
	flags.StringVar(&projectKey, "project", "", "exact uppercase Jira project key")
	flags.StringVar(&issueTypeID, "issue-type-id", "", "exact numeric issue type ID")
	flags.StringVar(&summary, "summary", "", "bounded one-line issue summary")
	flags.StringVar(&description, "description", "", "bounded plain-text description")
	flags.StringArrayVar(&labels, "label", nil, "bounded Jira label (repeatable, maximum 100)")
	return command
}

func (a *App) newIssuesEditCommand() *cobra.Command {
	var summary, description string
	var clearDescription bool
	command := &cobra.Command{
		Use:   "edit ISSUE_KEY",
		Short: "Edit bounded fields on one exact allowed issue",
		Args:  usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			projectKey, err := projectFromIssueKey(args[0])
			if err != nil {
				return err
			}
			summaryChanged, descriptionChanged := cmd.Flags().Changed("summary"), cmd.Flags().Changed("description")
			if !summaryChanged && !descriptionChanged && !clearDescription {
				return errx.Usage("issues edit requires --summary, --description, or --clear-description")
			}
			if descriptionChanged && clearDescription {
				return errx.Usage("--description and --clear-description cannot be combined")
			}
			if summaryChanged {
				if err := jira.ValidateSummary(summary); err != nil {
					return err
				}
			}
			if descriptionChanged {
				if _, err := jira.NewPlainTextDocument(description, "description"); err != nil {
					return err
				}
			}
			changed := make([]string, 0, 2)
			if summaryChanged {
				changed = append(changed, "summary")
			}
			if descriptionChanged || clearDescription {
				changed = append(changed, "description")
			}
			return a.runMutation(cmd.Context(), projectKey, "issues.edit", func(client jiraMutationClient, _ profile.Profile) error {
				receipt := mutationReceipt{Action: "issues.edit", DryRun: a.dryRun, Applied: !a.dryRun, RemoteChecks: remoteCheckState(a.dryRun), Project: projectKey, IssueKey: args[0], ChangedFields: changed}
				if a.dryRun {
					return a.out.Success(receipt)
				}
				issue, err := exactIssue(cmd.Context(), client, args[0])
				if err != nil {
					return err
				}
				input := jira.EditIssueRequest{ClearDescription: clearDescription}
				if summaryChanged {
					input.Summary = &summary
				}
				if descriptionChanged {
					input.Description = &description
				}
				if err := client.EditIssue(cmd.Context(), issue.ID, input); err != nil {
					return err
				}
				if _, err := verifyIssueProject(cmd.Context(), client, issue.ID, projectKey, "issues.edit"); err != nil {
					return err
				}
				receipt.IssueID = issue.ID
				return a.out.Success(receipt)
			})
		},
	}
	flags := command.Flags()
	flags.StringVar(&summary, "summary", "", "replacement bounded one-line summary")
	flags.StringVar(&description, "description", "", "replacement bounded plain-text description")
	flags.BoolVar(&clearDescription, "clear-description", false, "set the description to null")
	return command
}

func (a *App) newIssuesTransitionCommand() *cobra.Command {
	var transitionID string
	command := &cobra.Command{
		Use:   "transition ISSUE_KEY",
		Short: "Apply one exact currently available transition",
		Args:  usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			projectKey, err := projectFromIssueKey(args[0])
			if err != nil {
				return err
			}
			if err := requireNumericFlag(transitionID, "--transition-id"); err != nil {
				return err
			}
			return a.runMutation(cmd.Context(), projectKey, "issues.transition", func(client jiraMutationClient, _ profile.Profile) error {
				receipt := mutationReceipt{Action: "issues.transition", DryRun: a.dryRun, Applied: !a.dryRun, RemoteChecks: remoteCheckState(a.dryRun), Project: projectKey, IssueKey: args[0], TransitionID: transitionID}
				if a.dryRun {
					return a.out.Success(receipt)
				}
				issue, err := exactIssue(cmd.Context(), client, args[0])
				if err != nil {
					return err
				}
				transitions, err := client.Transitions(cmd.Context(), issue.ID)
				if err != nil {
					return err
				}
				found := false
				for _, transition := range transitions {
					if transition.ID == transitionID {
						found = true
						break
					}
				}
				if !found {
					return errx.NotFound("transition", transitionID, nil).
						WithHint("re-run 'jira-cli issues transitions %s --profile NAME' and choose an exact numeric transition ID", args[0])
				}
				if err := client.TransitionIssue(cmd.Context(), issue.ID, transitionID); err != nil {
					return err
				}
				if _, err := verifyIssueProject(cmd.Context(), client, issue.ID, projectKey, "issues.transition"); err != nil {
					return err
				}
				receipt.IssueID = issue.ID
				return a.out.Success(receipt)
			})
		},
	}
	command.Flags().StringVar(&transitionID, "transition-id", "", "exact numeric transition ID")
	return command
}

func (a *App) newCommentsAddCommand() *cobra.Command {
	var issueKey, body string
	command := &cobra.Command{
		Use:   "add",
		Short: "Add one bounded plain-text comment to an allowed issue",
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			projectKey, err := projectFromIssueKey(issueKey)
			if err != nil {
				return err
			}
			if strings.TrimSpace(body) == "" {
				return errx.Usage("--body must not be empty")
			}
			if _, err := jira.NewPlainTextDocument(body, "body"); err != nil {
				return err
			}
			return a.runMutation(cmd.Context(), projectKey, "comments.add", func(client jiraMutationClient, _ profile.Profile) error {
				receipt := mutationReceipt{Action: "comments.add", DryRun: a.dryRun, Applied: !a.dryRun, RemoteChecks: remoteCheckState(a.dryRun), Project: projectKey, IssueKey: issueKey}
				if a.dryRun {
					return a.out.Success(receipt)
				}
				issue, err := exactIssue(cmd.Context(), client, issueKey)
				if err != nil {
					return err
				}
				comment, err := client.AddComment(cmd.Context(), issue.ID, body)
				if err != nil {
					return err
				}
				if requireNumericFlag(comment.ID, "Jira comment ID") != nil {
					return errx.WriteOutcomeUnknown("comments.add")
				}
				if _, err := verifyIssueProject(cmd.Context(), client, issue.ID, projectKey, "comments.add"); err != nil {
					return err
				}
				receipt.IssueID, receipt.CommentID = issue.ID, comment.ID
				return a.out.Success(receipt)
			})
		},
	}
	command.Flags().StringVar(&issueKey, "issue", "", "exact uppercase Jira issue key")
	command.Flags().StringVar(&body, "body", "", "bounded plain-text comment body")
	return command
}

func exactProject(ctx context.Context, client jiraMutationClient, key string) (jira.Project, error) {
	project, err := client.Project(ctx, key)
	if err != nil {
		return jira.Project{}, err
	}
	if project.Key != key {
		return jira.Project{}, errx.Inexact("project", key, nil).
			WithHint("re-run 'jira-cli projects get %s --profile NAME' and use the exact uppercase returned key", key)
	}
	if err := requireNumericFlag(project.ID, "Jira project ID"); err != nil {
		return jira.Project{}, errx.Internal("Jira returned a non-numeric project ID")
	}
	return project, nil
}

func exactIssue(ctx context.Context, client jiraMutationClient, key string) (jira.Issue, error) {
	issue, err := client.Issue(ctx, key, []string{})
	if err != nil {
		return jira.Issue{}, err
	}
	if issue.Key != key {
		return jira.Issue{}, errx.Inexact("issue", key, nil).
			WithHint("re-run 'jira-cli issues get %s --profile NAME' and use the exact returned issue key", key)
	}
	if err := requireNumericFlag(issue.ID, "Jira issue ID"); err != nil {
		return jira.Issue{}, errx.Internal("Jira returned a non-numeric issue ID")
	}
	return issue, nil
}

func verifyIssueProject(ctx context.Context, client jiraMutationClient, issueID, projectKey, action string) (jira.Issue, error) {
	issue, err := client.Issue(ctx, issueID, []string{})
	if err != nil {
		return jira.Issue{}, errx.WriteOutcomeUnknown(action).Wrap(err)
	}
	returnedProject, keyErr := projectFromIssueKey(issue.Key)
	if keyErr != nil || returnedProject != projectKey || issue.ID != issueID {
		return jira.Issue{}, errx.WriteOutcomeUnknown(action)
	}
	return issue, nil
}

func requireExactProjectKey(key string) error {
	if !exactProjectKeyPattern.MatchString(key) {
		return errx.Usage("--project must be an exact uppercase Jira project key")
	}
	return nil
}

func projectFromIssueKey(key string) (string, error) {
	matches := exactIssueKeyPattern.FindStringSubmatch(key)
	if len(matches) != 2 {
		return "", errx.Usage("issue key must be exact uppercase PROJECT-NUMBER form")
	}
	return matches[1], nil
}

func requireNumericFlag(value, name string) error {
	if value == "" || len(value) > 32 {
		return errx.Usage("%s must be a numeric ID", name)
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			return errx.Usage("%s must be a numeric ID", name)
		}
	}
	return nil
}

func translateWritePolicyLockBoundary(err error, profileName string) error {
	if writepolicy.WasCommitted(err) {
		return translateWritePolicy(err, profileName, "")
	}
	if isLockTimeout(err) {
		return errx.LocalLockBusy().Wrap(err)
	}
	return translateWritePolicy(err, profileName, "")
}

func remoteCheckState(dryRun bool) string {
	if dryRun {
		return "not_performed"
	}
	return "verified"
}

func standardIssueTypes(values []jira.IssueType) []jira.IssueType {
	supported := make([]jira.IssueType, 0, len(values))
	for _, value := range values {
		if !value.Subtask {
			supported = append(supported, value)
		}
	}
	return supported
}
