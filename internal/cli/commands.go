package cli

import (
	"encoding/base64"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/abigotado/jira-cli/internal/auth"
	"github.com/abigotado/jira-cli/internal/errx"
	"github.com/abigotado/jira-cli/internal/jira"
	"github.com/abigotado/jira-cli/internal/profile"
	"github.com/abigotado/jira-cli/internal/skills"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

var defaultIssueFields = []string{"summary", "status", "assignee", "updated"}

func (a *App) newContractCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "contract",
		Short: "Print the versioned envelope and exit-code contract",
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(_ *cobra.Command, _ []string) error {
			return a.out.Success(errx.Describe())
		},
	}
}

func (a *App) newAuthCommand() *cobra.Command {
	command := commandGroup("auth", "Manage named Jira profiles")
	command.AddCommand(
		a.newAuthLoginCommand(),
		a.newAuthListCommand(),
		a.newAuthStatusCommand(),
		a.newAuthLogoutCommand(),
	)
	return command
}

func (a *App) newAuthLoginCommand() *cobra.Command {
	var site string
	var email string
	var tokenKind string
	var tokenStdin bool
	var expiresAt string
	command := &cobra.Command{
		Use:   "login",
		Short: "Validate and store one profile token in macOS Keychain",
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			if a.dryRun {
				return errx.Usage("--dry-run is not supported by auth login")
			}
			if len(a.fields) > 0 {
				return errx.Usage("--fields is not supported by auth login")
			}
			if a.profileName == "" {
				return errx.ProfileRequired()
			}
			if !tokenStdin {
				return errx.Usage("auth login requires --token-stdin; tokens are never accepted through argv")
			}
			kind := profile.TokenKind(tokenKind)
			candidate := profile.Profile{
				Name: a.profileName, Site: site, Email: email, TokenKind: kind,
			}
			if expiresAt != "" {
				parsed, err := time.Parse(time.DateOnly, expiresAt)
				if err != nil {
					return errx.Usage("--expires-at must use YYYY-MM-DD")
				}
				candidate.ExpiresAt = &parsed
			}

			if kind == profile.TokenKindScoped {
				preflight := candidate
				preflight.TokenKind = profile.TokenKindClassic
				if err := preflight.Validate(); err != nil {
					return translateLocal(err, candidate.Name)
				}
				cloudID, err := a.discoverCloudID(cmd.Context(), candidate.Site)
				if err != nil {
					return err
				}
				candidate.CloudID = cloudID
			}
			if err := candidate.Validate(); err != nil {
				return translateLocal(err, candidate.Name)
			}
			if file, ok := a.stdin.(*os.File); ok && term.IsTerminal(int(file.Fd())) {
				_, _ = fmt.Fprint(a.stderr, "API token: ")
				defer func() { _, _ = fmt.Fprintln(a.stderr) }()
			}
			credential, err := auth.ReadToken(a.stdin)
			if err != nil {
				return translateLocal(err, candidate.Name)
			}
			client, err := a.newJira(candidate, credential, a.log)
			if err != nil {
				return err
			}
			if _, err := client.Myself(cmd.Context()); err != nil {
				return err
			}
			if a.registry == nil {
				return errx.Internal("profile registry is unavailable")
			}
			if err := auth.Login(cmd.Context(), a.store, a.registry, candidate, credential, a.assumeYes); err != nil {
				return translateLocal(err, candidate.Name)
			}
			return a.out.Success(newProfileView(candidate, "verified"))
		},
	}
	flags := command.Flags()
	flags.StringVar(&site, "site", "", "Jira Cloud site, exactly https://<tenant>.atlassian.net")
	flags.StringVar(&email, "email", "", "Atlassian account email")
	flags.StringVar(&tokenKind, "token-kind", string(profile.TokenKindClassic), "API token kind: classic or scoped")
	flags.BoolVar(&tokenStdin, "token-stdin", false, "read one bounded token from stdin without echo")
	flags.StringVar(&expiresAt, "expires-at", "", "optional token expiry date (YYYY-MM-DD)")
	return command
}

func (a *App) newAuthListCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List non-secret profile metadata without reading Keychain",
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			if a.registry == nil {
				return errx.Internal("profile registry is unavailable")
			}
			profiles, err := a.registry.List(cmd.Context())
			if err != nil {
				return translateLocal(err, "")
			}
			views := make([]profileView, len(profiles))
			for index, value := range profiles {
				views[index] = newProfileView(value, "unchecked")
			}
			return a.out.Success(views)
		},
	}
}

func (a *App) newAuthStatusCommand() *cobra.Command {
	var check bool
	command := &cobra.Command{
		Use:   "status",
		Short: "Show one profile and optionally validate its stored credential",
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			state := "unchecked"
			var selected profile.Profile
			if check {
				client, checked, err := a.client(cmd.Context())
				if err != nil {
					return err
				}
				if _, err := client.Myself(cmd.Context()); err != nil {
					return err
				}
				selected = checked
				state = "valid"
			} else {
				var err error
				selected, err = a.requireProfile(cmd.Context())
				if err != nil {
					return err
				}
			}
			return a.out.Success(newProfileView(selected, state))
		},
	}
	command.Flags().BoolVar(&check, "check", false, "read the exact Keychain entry and call Jira /myself")
	return command
}

func (a *App) newAuthLogoutCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "logout",
		Short: "Delete one exact profile credential and its metadata",
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			if a.dryRun {
				return errx.Usage("--dry-run is not supported by auth logout")
			}
			if len(a.fields) > 0 {
				return errx.Usage("--fields is not supported by auth logout")
			}
			if a.profileName == "" {
				return errx.ProfileRequired()
			}
			if !a.assumeYes {
				return errx.ConfirmRequired("auth logout")
			}
			if a.registry == nil {
				return errx.Internal("profile registry is unavailable")
			}
			if err := auth.Logout(cmd.Context(), a.store, a.registry, a.profileName); err != nil {
				return translateLocal(err, a.profileName)
			}
			return a.out.Success(map[string]any{"profile": a.profileName, "removed": true})
		},
	}
}

func (a *App) newSkillsCommand() *cobra.Command {
	command := commandGroup("skills", "Install the canonical Jira Agent Skill")
	command.AddCommand(a.newSkillsActionCommand("install"), a.newSkillsActionCommand("uninstall"))
	return command
}

func (a *App) newSkillsActionCommand(action string) *cobra.Command {
	var providerValue string
	var scopeValue string
	var projectDir string
	var dest string
	command := &cobra.Command{
		Use:   action,
		Short: action + " the Jira Agent Skill for Codex and/or Claude Code",
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			if len(a.fields) > 0 {
				return errx.Usage("--fields is not supported by skills %s", action)
			}
			if providerValue == "" {
				return errx.Usage("--provider is required: codex, claude, or all")
			}
			provider, err := skills.ParseProvider(providerValue)
			if err != nil {
				return err
			}
			scope, err := skills.ParseScope(scopeValue)
			if err != nil {
				return err
			}
			options := skills.Options{
				Provider: provider, Scope: scope, ProjectDir: projectDir, Dest: dest,
				Confirmed: a.assumeYes, DryRun: a.dryRun,
			}
			var results []skills.Result
			if action == "install" {
				results, err = skills.Install(cmd.Context(), options)
			} else {
				results, err = skills.Uninstall(cmd.Context(), options)
			}
			if err != nil {
				return err
			}
			return a.out.Success(results)
		},
	}
	flags := command.Flags()
	flags.StringVar(&providerValue, "provider", "", "target provider: codex, claude, or all")
	flags.StringVar(&scopeValue, "scope", string(skills.ScopeUser), "install scope: user or project")
	flags.StringVar(&projectDir, "project-dir", ".", "project directory for project scope")
	flags.StringVar(&dest, "dest", "", "explicit skills root for one provider")
	return command
}

func (a *App) newMeCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "me",
		Short: "Read the authenticated Jira account",
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, _, err := a.client(cmd.Context())
			if err != nil {
				return err
			}
			user, err := client.Myself(cmd.Context())
			if err != nil {
				return err
			}
			return a.out.Success(userView(user))
		},
	}
}

func (a *App) newProjectsCommand() *cobra.Command {
	command := commandGroup("projects", "Read Jira projects")
	command.AddCommand(a.newProjectsListCommand(), a.newProjectsGetCommand())
	return command
}

func (a *App) newProjectsListCommand() *cobra.Command {
	var limit int
	var cursor string
	var query string
	command := &cobra.Command{
		Use:   "list",
		Short: "Read one project page",
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := validateLimit(limit); err != nil {
				return err
			}
			startAt, err := decodeOffsetCursor(cursor, "project")
			if err != nil {
				return err
			}
			client, _, err := a.client(cmd.Context())
			if err != nil {
				return err
			}
			page, err := client.Projects(cmd.Context(), jira.ProjectPageOptions{StartAt: startAt, MaxResults: limit, Query: query})
			if err != nil {
				return err
			}
			truncated := !page.IsLast && page.StartAt+len(page.Values) < page.Total
			next := ""
			if truncated {
				nextOffset := page.StartAt + len(page.Values)
				if nextOffset <= startAt {
					return errx.Internal("Jira project pagination did not advance")
				}
				next = encodeOffsetCursor(nextOffset)
			}
			return a.out.SuccessPage(asProjectViews(page.Values), truncated, next)
		},
	}
	flags := command.Flags()
	flags.IntVar(&limit, "limit", 50, "maximum projects in this page (1-100)")
	flags.StringVar(&cursor, "cursor", "", "opaque cursor from meta.next_cursor")
	flags.StringVar(&query, "query", "", "optional Jira project name/key query")
	return command
}

func (a *App) newProjectsGetCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "get KEY_OR_ID",
		Short: "Read one exact project",
		Args:  usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _, err := a.client(cmd.Context())
			if err != nil {
				return err
			}
			project, err := client.Project(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			return a.out.Success(projectView(project))
		},
	}
}

func (a *App) newIssuesCommand() *cobra.Command {
	command := commandGroup("issues", "Read Jira issues")
	command.AddCommand(a.newIssuesGetCommand(), a.newIssuesSearchCommand(), a.newIssuesTransitionsCommand())
	return command
}

func (a *App) issueRequestFields() []string {
	if len(a.fields) == 0 {
		return append([]string(nil), defaultIssueFields...)
	}
	fields := make([]string, 0, len(a.fields))
	for _, field := range a.fields {
		switch field {
		case "key", "id", "self":
			continue
		default:
			fields = append(fields, field)
		}
	}
	return fields
}

func (a *App) newIssuesGetCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "get ISSUE_KEY",
		Short: "Read one exact issue",
		Args:  usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _, err := a.client(cmd.Context())
			if err != nil {
				return err
			}
			issue, err := client.Issue(cmd.Context(), args[0], a.issueRequestFields())
			if err != nil {
				return err
			}
			return a.out.Success(issueView{issue: issue, requested: a.issueRequestFields()})
		},
	}
}

func (a *App) newIssuesSearchCommand() *cobra.Command {
	var jql string
	var limit int
	var cursor string
	command := &cobra.Command{
		Use:   "search",
		Short: "Run one enhanced JQL search page",
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			if strings.TrimSpace(jql) == "" {
				return errx.Usage("--jql is required")
			}
			if err := validateLimit(limit); err != nil {
				return err
			}
			client, _, err := a.client(cmd.Context())
			if err != nil {
				return err
			}
			page, err := client.Search(cmd.Context(), jira.SearchRequest{
				JQL: jql, Fields: a.issueRequestFields(), MaxResults: limit, NextPageToken: cursor,
			})
			if err != nil {
				return err
			}
			if page.IsLast && page.NextPageToken != "" {
				return errx.Internal("Jira issue pagination returned a cursor for a final page")
			}
			if !page.IsLast && page.NextPageToken == "" {
				return errx.Internal("Jira issue pagination omitted a cursor for a non-final page")
			}
			truncated := page.NextPageToken != "" && !page.IsLast
			if truncated && cursor != "" && page.NextPageToken == cursor {
				return errx.Internal("Jira issue pagination did not advance")
			}
			return a.out.SuccessPage(asIssueViews(page.Issues, a.issueRequestFields()), truncated, page.NextPageToken)
		},
	}
	flags := command.Flags()
	flags.StringVar(&jql, "jql", "", "bounded Jira Query Language expression")
	flags.IntVar(&limit, "limit", 50, "maximum issues in this page (1-100)")
	flags.StringVar(&cursor, "cursor", "", "opaque nextPageToken from meta.next_cursor")
	return command
}

func (a *App) newIssuesTransitionsCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "transitions ISSUE_KEY",
		Short: "Read currently available transitions for an issue",
		Args:  usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _, err := a.client(cmd.Context())
			if err != nil {
				return err
			}
			transitions, err := client.Transitions(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			return a.out.Success(asTransitionViews(transitions))
		},
	}
}

func (a *App) newCommentsCommand() *cobra.Command {
	command := commandGroup("comments", "Read Jira issue comments")
	command.AddCommand(a.newCommentsListCommand())
	return command
}

func commandGroup(use, short string) *cobra.Command {
	return &cobra.Command{
		Use:   use,
		Short: short,
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			return errx.Usage("%s needs a subcommand", cmd.CommandPath())
		},
	}
}

func (a *App) newCommentsListCommand() *cobra.Command {
	var issue string
	var limit int
	var cursor string
	command := &cobra.Command{
		Use:   "list",
		Short: "Read one comment page",
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			if strings.TrimSpace(issue) == "" {
				return errx.Usage("--issue is required")
			}
			if err := validateLimit(limit); err != nil {
				return err
			}
			startAt, err := decodeOffsetCursor(cursor, "comment")
			if err != nil {
				return err
			}
			client, _, err := a.client(cmd.Context())
			if err != nil {
				return err
			}
			page, err := client.Comments(cmd.Context(), issue, jira.CommentPageOptions{StartAt: startAt, MaxResults: limit})
			if err != nil {
				return err
			}
			truncated := page.StartAt+len(page.Comments) < page.Total
			next := ""
			if truncated {
				nextOffset := page.StartAt + len(page.Comments)
				if nextOffset <= startAt {
					return errx.Internal("Jira comment pagination did not advance")
				}
				next = encodeOffsetCursor(nextOffset)
			}
			return a.out.SuccessPage(asCommentViews(page.Comments), truncated, next)
		},
	}
	flags := command.Flags()
	flags.StringVar(&issue, "issue", "", "exact Jira issue key or ID")
	flags.IntVar(&limit, "limit", 50, "maximum comments in this page (1-100)")
	flags.StringVar(&cursor, "cursor", "", "opaque cursor from meta.next_cursor")
	return command
}

func validateLimit(limit int) error {
	if limit < 1 || limit > 100 {
		return errx.Usage("--limit must be between 1 and 100")
	}
	return nil
}

func encodeOffsetCursor(offset int) string {
	return base64.RawURLEncoding.EncodeToString([]byte("offset:" + strconv.Itoa(offset)))
}

func decodeOffsetCursor(cursor, kind string) (int, error) {
	if cursor == "" {
		return 0, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil || !strings.HasPrefix(string(raw), "offset:") {
		return 0, errx.Usage("invalid %s cursor", kind)
	}
	offset, err := strconv.Atoi(strings.TrimPrefix(string(raw), "offset:"))
	if err != nil || offset < 0 {
		return 0, errx.Usage("invalid %s cursor", kind)
	}
	return offset, nil
}
