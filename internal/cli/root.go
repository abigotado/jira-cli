// Package cli wires jira-cli's command tree. It contains no HTTP or Keychain
// implementation; those boundaries are injected through internal packages.
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/abigotado/jira-cli/internal/auth"
	"github.com/abigotado/jira-cli/internal/errx"
	"github.com/abigotado/jira-cli/internal/jira"
	"github.com/abigotado/jira-cli/internal/lockfile"
	"github.com/abigotado/jira-cli/internal/output"
	"github.com/abigotado/jira-cli/internal/profile"
	"github.com/abigotado/jira-cli/internal/writepolicy"
	"github.com/spf13/cobra"
)

const defaultTimeout = 30 * time.Second

type profileRegistry interface {
	WithProfileLock(context.Context, string, func() error) error
	List(context.Context) ([]profile.Profile, error)
	Get(context.Context, string) (profile.Profile, error)
	Add(context.Context, profile.Profile) error
	Put(context.Context, profile.Profile) error
	Remove(context.Context, string) error
}

type jiraReader interface {
	Myself(context.Context) (jira.User, error)
	Projects(context.Context, jira.ProjectPageOptions) (jira.ProjectPage, error)
	Project(context.Context, string) (jira.Project, error)
	Issue(context.Context, string, []string) (jira.Issue, error)
	Search(context.Context, jira.SearchRequest) (jira.SearchPage, error)
	Transitions(context.Context, string) ([]jira.Transition, error)
	Comments(context.Context, string, jira.CommentPageOptions) (jira.CommentPage, error)
}

type jiraMutationClient interface {
	jiraReader
	IssueTypes(context.Context, string, jira.IssueTypePageOptions) (jira.IssueTypePage, error)
	CreateIssue(context.Context, jira.CreateIssueRequest) (jira.Issue, error)
	EditIssue(context.Context, string, jira.EditIssueRequest) error
	TransitionIssue(context.Context, string, string) error
	AddComment(context.Context, string, string) (jira.Comment, error)
}

var _ jiraMutationClient = (*jira.Client)(nil)

type writePolicyRegistry interface {
	WithPolicyLock(context.Context, string, func() error) error
	Get(context.Context, string) (writepolicy.Policy, error)
	GetBound(context.Context, profile.Profile) (writepolicy.Policy, error)
	RequireProject(context.Context, profile.Profile, string) (writepolicy.Policy, error)
	Set(context.Context, profile.Profile, []string) (writepolicy.Policy, error)
	Clear(context.Context, string) error
}

// App contains only per-invocation state and injectable boundaries.
type App struct {
	registry profileRegistry
	policies writePolicyRegistry
	store    auth.CredentialStore

	newJira         func(profile.Profile, auth.Credential, *slog.Logger) (jiraReader, error)
	discoverCloudID func(context.Context, string) (string, error)
	now             func() time.Time

	stdin  io.Reader
	stdout io.Writer
	stderr io.Writer

	profileName string
	format      string
	jsonAlias   bool
	fields      []string
	timeout     time.Duration
	verbose     bool
	assumeYes   bool
	dryRun      bool

	out     *output.Writer
	log     *slog.Logger
	cancels []context.CancelFunc
}

// NewApp builds an App with production boundaries.
func NewApp() *App {
	registry, registryErr := profile.NewDefaultRegistry()
	policies, policyErr := writepolicy.NewDefaultRegistry()
	app := &App{
		store:  auth.KeychainStore{},
		stdin:  os.Stdin,
		stdout: os.Stdout,
		stderr: os.Stderr,
		now:    time.Now,
	}
	if registryErr == nil {
		app.registry = registry
	}
	if policyErr == nil {
		app.policies = policies
	}
	app.newJira = func(p profile.Profile, credential auth.Credential, logger *slog.Logger) (jiraReader, error) {
		return jira.New(
			jira.Config{SiteURL: p.Site},
			jira.Credential{
				Email:     p.Email,
				Token:     credential.Token,
				TokenKind: jira.TokenKind(p.TokenKind),
				CloudID:   p.CloudID,
			},
			jira.WithLogger(logger),
		)
	}
	app.discoverCloudID = func(ctx context.Context, site string) (string, error) {
		return jira.DiscoverCloudID(ctx, site, nil)
	}
	return app
}

//go:generate go run github.com/abigotado/jira-cli/tools/gencommands

// NewRootCommand assembles the public command surface.
func (a *App) NewRootCommand() *cobra.Command {
	root := &cobra.Command{
		Use:           "jira-cli",
		Short:         "Use Jira Cloud safely from the command line and AI agents",
		SilenceUsage:  true,
		SilenceErrors: true,
		Args: usageArgs(func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 {
				return fmt.Errorf("unknown command %q for %q", args[0], cmd.CommandPath())
			}
			return nil
		}),
		RunE: func(cmd *cobra.Command, _ []string) error {
			return errx.Usage("%s needs a command", cmd.CommandPath())
		},
	}
	root.SetOut(a.stdout)
	root.SetErr(a.stderr)

	flags := root.PersistentFlags()
	flags.StringVar(&a.profileName, "profile", "", "named Jira profile (required for every network command)")
	flags.StringVarP(&a.format, "output", "o", "", "output format: text, json, or raw")
	flags.BoolVar(&a.jsonAlias, "json", false, "emit the JSON envelope")
	_ = flags.MarkHidden("json")
	flags.StringSliceVar(&a.fields, "fields", nil, "comma-separated fields to request and emit")
	flags.DurationVar(&a.timeout, "timeout", defaultTimeout, "abort the command after this duration")
	flags.BoolVarP(&a.verbose, "verbose", "v", false, "write redacted request activity to stderr")
	flags.BoolVar(&a.assumeYes, "yes", false, "confirm a supported mutation or local overwrite")
	flags.BoolVar(&a.dryRun, "dry-run", false, "preview a supported change without applying it or contacting Jira")

	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error {
		return errx.Usage("%v", err)
	})
	root.PersistentPreRunE = a.setup
	root.AddCommand(
		a.newVersionCommand(),
		a.newContractCommand(),
		a.newAuthCommand(),
		a.newSkillsCommand(),
		a.newMeCommand(),
		a.newProjectsCommand(),
		a.newIssuesCommand(),
		a.newCommentsCommand(),
	)
	return root
}

func (a *App) runMutation(ctx context.Context, projectKey, action string, fn func(jiraMutationClient, profile.Profile) (mutationReceipt, error)) error {
	if a.profileName == "" {
		return errx.ProfileRequired()
	}
	if err := a.out.Validate(mutationReceipt{}); err != nil {
		return err
	}
	if a.registry == nil || a.policies == nil {
		return errx.Internal("profile or write policy registry is unavailable")
	}
	var receipt mutationReceipt
	callbackCompleted := false
	mutationApplied := false
	err := a.registry.WithProfileLock(ctx, a.profileName, func() error {
		selected, err := a.registry.Get(ctx, a.profileName)
		if err != nil {
			return translateLocal(err, a.profileName)
		}
		return a.policies.WithPolicyLock(ctx, a.profileName, func() error {
			if _, err := a.policies.RequireProject(ctx, selected, projectKey); err != nil {
				return translateWritePolicy(err, selected.Name, projectKey)
			}
			if !a.dryRun && !a.assumeYes {
				return errx.ConfirmRequired(action)
			}
			a.out.WithContext(selected.Name, selected.Site)
			if a.dryRun {
				receipt, err = fn(nil, selected)
				callbackCompleted = err == nil
				return err
			}
			reader, err := a.loadClientLocked(ctx, selected)
			if err != nil {
				return err
			}
			client, ok := reader.(jiraMutationClient)
			if !ok {
				return errx.Internal("Jira client does not implement mutation operations")
			}
			receipt, err = fn(client, selected)
			callbackCompleted = err == nil
			mutationApplied = callbackCompleted
			return err
		})
	})
	if err != nil {
		if mutationApplied {
			return errx.WriteOutcomeUnknown(action).Wrap(err)
		}
		if callbackCompleted {
			return protectedWorkFailure(err)
		}
		return translateLocalLockBoundary(err)
	}
	if err := a.out.Success(receipt); err != nil {
		if mutationApplied {
			return errx.WriteOutcomeUnknown(action).Wrap(err)
		}
		return protectedWorkFailure(err)
	}
	return nil
}

func (a *App) setup(cmd *cobra.Command, _ []string) error {
	if a.jsonAlias && a.format != "" && a.format != string(output.FormatJSON) {
		return errx.Usage("--json cannot be combined with --output %s", a.format)
	}
	format := defaultFormat(a.stdout)
	if a.jsonAlias {
		format = output.FormatJSON
	} else if a.format != "" {
		parsed, err := output.ParseFormat(a.format)
		if err != nil {
			return err
		}
		format = parsed
	}
	if err := output.ValidateFormat(format, a.fields); err != nil {
		return err
	}
	a.out = &output.Writer{Format: format, Fields: a.fields, Out: a.stdout, Err: a.stderr}
	level := slog.LevelWarn
	if a.verbose {
		level = slog.LevelDebug
	}
	a.log = slog.New(slog.NewTextHandler(a.stderr, &slog.HandlerOptions{Level: level}))
	if a.timeout <= 0 {
		return errx.Usage("--timeout must be greater than zero")
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), a.timeout)
	a.cancels = append(a.cancels, cancel)
	cmd.SetContext(ctx)
	return nil
}

func defaultFormat(writer io.Writer) output.Format {
	if file, ok := writer.(*os.File); ok {
		return output.DefaultFormat(file)
	}
	return output.FormatJSON
}

func (a *App) requireProfile(ctx context.Context) (profile.Profile, error) {
	if a.profileName == "" {
		return profile.Profile{}, errx.ProfileRequired()
	}
	if a.registry == nil {
		return profile.Profile{}, errx.Internal("profile registry is unavailable")
	}
	selected, err := a.registry.Get(ctx, a.profileName)
	if err != nil {
		return profile.Profile{}, translateLocal(err, a.profileName)
	}
	return selected, nil
}

func (a *App) client(ctx context.Context) (jiraReader, profile.Profile, error) {
	if a.profileName == "" {
		return nil, profile.Profile{}, errx.ProfileRequired()
	}
	if a.registry == nil {
		return nil, profile.Profile{}, errx.Internal("profile registry is unavailable")
	}
	var selected profile.Profile
	var client jiraReader
	err := a.registry.WithProfileLock(ctx, a.profileName, func() error {
		var loadErr error
		selected, loadErr = a.registry.Get(ctx, a.profileName)
		if loadErr != nil {
			return translateLocal(loadErr, a.profileName)
		}
		client, loadErr = a.loadClientLocked(ctx, selected)
		return loadErr
	})
	if err != nil {
		return nil, profile.Profile{}, translateLocalLockBoundary(err)
	}
	return client, selected, nil
}

// loadClientLocked loads credentials and constructs a Jira client while the
// caller holds the selected profile's lock.
func (a *App) loadClientLocked(ctx context.Context, selected profile.Profile) (jiraReader, error) {
	if selected.ExpiresAt != nil && !a.now().Before(*selected.ExpiresAt) {
		return nil, errx.Auth("TOKEN_EXPIRED", "the API token for profile %q is expired", selected.Name)
	}
	credential, err := a.store.Load(ctx, selected.Name)
	if err != nil {
		return nil, translateLocal(err, selected.Name)
	}
	client, err := a.newJira(selected, credential, a.log)
	if err != nil {
		return nil, err
	}
	a.out.WithContext(selected.Name, selected.Site)
	return client, nil
}

func usageArgs(validator cobra.PositionalArgs) cobra.PositionalArgs {
	if validator == nil {
		validator = cobra.ArbitraryArgs
	}
	return func(cmd *cobra.Command, args []string) error {
		if err := validator(cmd, args); err != nil {
			var typed *errx.Error
			if errors.As(err, &typed) {
				return err
			}
			return errx.Usage("%v", err)
		}
		return nil
	}
}

func translateLocal(err error, name string) error {
	if err == nil {
		return nil
	}
	var typed *errx.Error
	if errors.As(err, &typed) {
		return err
	}
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return errx.Translate(err)
	case errors.Is(err, profile.ErrProfileRequired):
		return errx.ProfileRequired()
	case errors.Is(err, profile.ErrInvalidProfile):
		return errx.Usage("profile metadata is invalid")
	case errors.Is(err, profile.ErrNotFound):
		return errx.NotFound("profile", name, nil)
	case errors.Is(err, profile.ErrAlreadyExists), errors.Is(err, auth.ErrOverwriteConfirmationRequired):
		return errx.ConfirmRequired("auth login overwrite")
	case errors.Is(err, profile.ErrCorruptRegistry), errors.Is(err, profile.ErrInsecurePermissions):
		return errx.Internal("profile registry cannot be used safely")
	case errors.Is(err, auth.ErrNotFound):
		return errx.Auth("CREDENTIAL_NOT_FOUND", "no stored credential exists for profile %q", name)
	case errors.Is(err, auth.ErrUnsupported):
		return errx.Auth("KEYCHAIN_UNSUPPORTED", "this build has no supported native credential store")
	case errors.Is(err, auth.ErrInteractionNotAllowed):
		return errx.Auth("KEYCHAIN_INTERACTION_REQUIRED", "Keychain access for profile %q requires user interaction", name)
	case errors.Is(err, auth.ErrInvalidToken):
		return errx.Usage("API token input is invalid")
	default:
		return errx.Internal("local operation failed without exposing credential details")
	}
}

func translateWritePolicy(err error, profileName, projectKey string) error {
	if err == nil {
		return nil
	}
	var typed *errx.Error
	if errors.As(err, &typed) {
		return err
	}
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return errx.Translate(err)
	case writepolicy.WasCommitted(err):
		return writePolicyOutcomeUnknown(profileName).Wrap(err)
	case errors.Is(err, writepolicy.ErrNotFound):
		return errx.Permission("WRITE_POLICY_MISSING", "profile %q has no local write allowlist", profileName).
			WithHint("run 'jira-cli auth allow-projects set --profile NAME --project KEY --yes'")
	case errors.Is(err, writepolicy.ErrStale):
		return errx.Permission("WRITE_POLICY_STALE", "profile %q write allowlist belongs to different account metadata", profileName).
			WithHint("review the current account, then reset its allowlist with auth allow-projects set")
	case errors.Is(err, writepolicy.ErrProjectDenied):
		return errx.Permission("PROJECT_NOT_ALLOWED", "project %q is not in profile %q local write allowlist", projectKey, profileName).
			WithHint("run 'jira-cli auth allow-projects show --profile %s', then use set --yes with the complete intended --project list", profileName)
	case errors.Is(err, writepolicy.ErrInvalid), errors.Is(err, profile.ErrInvalidProfile):
		return errx.Usage("write policy input is invalid")
	case errors.Is(err, writepolicy.ErrCorruptRegistry), errors.Is(err, writepolicy.ErrInsecurePermissions):
		return errx.Internal("write policy registry cannot be used safely")
	default:
		return errx.Internal("local write policy operation failed")
	}
}

func writePolicyOutcomeUnknown(profileName string) *errx.Error {
	return errx.Conflict("WRITE_POLICY_OUTCOME_UNKNOWN", "the local write policy may have been updated despite a durability-check failure").
		WithHint("run 'jira-cli auth allow-projects show --profile %s' before deciding whether to repeat the change", profileName)
}

func protectedWorkFailure(err error) error {
	return errx.Internal("command could not finish safely after protected work completed").Wrap(err)
}

func isLockTimeout(err error) bool {
	var timeout *lockfile.TimeoutError
	return errors.As(err, &timeout)
}

// translateLocalLockBoundary is used only when a CLI-owned lock acquisition
// failed before its protected credential or Jira operation could begin.
func translateLocalLockBoundary(err error) error {
	if err == nil {
		return nil
	}
	var typed *errx.Error
	if errors.As(err, &typed) {
		return err
	}
	if isLockTimeout(err) {
		return errx.LocalLockBusy().Wrap(err)
	}
	return err
}

// Run executes one command tree and returns its process status.
func (a *App) Run(ctx context.Context, root *cobra.Command, args []string) (code errx.Code) {
	defer func() {
		for _, cancel := range a.cancels {
			cancel()
		}
		if recover() != nil {
			if a.out == nil {
				a.out = &output.Writer{Format: defaultFormat(a.stdout), Out: a.stdout, Err: a.stderr}
			}
			code = a.out.Failure(errx.Internal("jira-cli stopped after an unexpected internal failure"))
		}
	}()
	root.SetArgs(args)
	err := root.ExecuteContext(ctx)
	if err == nil {
		return errx.CodeOK
	}
	if a.out == nil {
		format := defaultFormat(a.stdout)
		if a.jsonAlias {
			format = output.FormatJSON
		} else if parsed, parseErr := output.ParseFormat(a.format); a.format != "" && parseErr == nil {
			format = parsed
		}
		a.out = &output.Writer{Format: format, Out: a.stdout, Err: a.stderr}
	}
	return a.out.Failure(translateLocal(err, a.profileName))
}

// Execute runs jira-cli with process streams and signal cancellation.
func Execute(args []string) errx.Code {
	app := NewApp()
	root := app.NewRootCommand()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return app.Run(ctx, root, args)
}
