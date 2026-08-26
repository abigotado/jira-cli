// Package errx defines typed errors carrying jira-cli's stable exit contract.
//
// This package deliberately imports nothing else from this module. Candidate
// is defined here rather than borrowing a Jira model so errx stays at the
// bottom of the dependency graph.
package errx

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Candidate identifies one possible match for an ambiguous or misspelled
// Jira entity.
type Candidate struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Kind string `json:"kind"`
}

// Error contains the stable fields used to build an error envelope.
type Error struct {
	// Code determines the process exit status.
	Code Code
	// Reason is the stable SCREAMING_SNAKE_CASE error.code.
	Reason string
	// Message is a safe human-readable explanation.
	Message string
	// Hint states the caller's next recovery action.
	Hint string
	// Candidates lists exact choices for an ambiguous lookup.
	Candidates []Candidate
	// DidYouMean lists near matches for a failed lookup.
	DidYouMean []Candidate
	// RetryAfter carries a server-advertised backoff.
	RetryAfter time.Duration

	wrapped error
}

func (e *Error) Error() string {
	if e.Message == "" {
		return e.Reason
	}
	return e.Message
}

// Unwrap exposes the underlying cause.
func (e *Error) Unwrap() error { return e.wrapped }

// Wrap attaches an underlying cause without changing contract fields.
func (e *Error) Wrap(err error) *Error {
	e.wrapped = err
	return e
}

// WithHint replaces the machine-oriented recovery hint.
func (e *Error) WithHint(format string, args ...any) *Error {
	e.Hint = fmt.Sprintf(format, args...)
	return e
}

// Translate maps expected standard-library errors onto the public contract.
func Translate(err error) error {
	if err == nil {
		return nil
	}
	var typed *Error
	if errors.As(err, &typed) {
		return err
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return Retryable("TIMEOUT", 0, "the command exceeded its time budget").
			WithHint("raise --timeout, then retry").Wrap(err)
	case errors.Is(err, context.Canceled):
		return Retryable("CANCELED", 0, "the command was interrupted before it finished").
			WithHint("re-run the command").Wrap(err)
	default:
		return err
	}
}

// ExitCode reports the process status for err. Untyped errors are internal
// failures: allowing a raw transport error to look retryable would leak layer
// details and could make an agent repeat a non-idempotent write.
func ExitCode(err error) Code {
	if err == nil {
		return CodeOK
	}
	var typed *Error
	if errors.As(err, &typed) {
		return typed.Code
	}
	return CodeInternal
}

// Usage reports invalid flags or input.
func Usage(format string, args ...any) *Error {
	return &Error{Code: CodeUsage, Reason: "USAGE", Message: fmt.Sprintf(format, args...), Hint: "check the flags against --help"}
}

// ProfileRequired reports that implicit profile selection was unsafe.
func ProfileRequired() *Error {
	return &Error{
		Code:    CodeUsage,
		Reason:  "PROFILE_REQUIRED",
		Message: "an explicit profile is required for every network command",
		Hint:    "re-run with --profile NAME",
	}
}

// Internal reports a defect in jira-cli.
func Internal(format string, args ...any) *Error {
	return &Error{Code: CodeInternal, Reason: "INTERNAL", Message: fmt.Sprintf(format, args...), Hint: "this is a bug in jira-cli; do not retry unchanged"}
}

// NotFound reports that no Jira entity of kind matched query.
func NotFound(kind, query string, didYouMean []Candidate) *Error {
	hint := fmt.Sprintf("verify the %s key or name and --profile", kind)
	if len(didYouMean) > 0 {
		hint = "re-run with one of the values in did_you_mean"
	}
	return &Error{
		Code: CodeNotFound, Reason: "NOT_FOUND_" + upper(kind),
		Message: fmt.Sprintf("no %s matches %q", kind, query), Hint: hint,
		DidYouMean: didYouMean,
	}
}

// Ambiguous reports that query matched more than one Jira entity.
func Ambiguous(kind, query string, candidates []Candidate) *Error {
	return &Error{
		Code: CodeAmbiguous, Reason: "AMBIGUOUS_" + upper(kind),
		Message:    fmt.Sprintf("%q matches %d %ss", query, len(candidates), kind),
		Hint:       fmt.Sprintf("re-run with --%s-id using an id from candidates", flagKind(kind)),
		Candidates: candidates,
	}
}

// Inexact refuses a partial match for a mutation.
func Inexact(kind, query string, candidates []Candidate) *Error {
	return &Error{
		Code: CodeUsage, Reason: "INEXACT_" + upper(kind),
		Message:    fmt.Sprintf("%q is not the exact name of a %s; a write will not use a partial match", query, kind),
		Hint:       fmt.Sprintf("re-run with the exact name or --%s-id from candidates", flagKind(kind)),
		Candidates: candidates,
	}
}

// CreateFieldsUnsupported reports create metadata that cannot accept the
// bounded jira-cli payload. Only canonical field IDs are exposed, with a
// strict count and length bound; Jira field names and raw metadata never
// become part of the public error.
func CreateFieldsUnsupported(fieldIDs []string, provideDescription bool) *Error {
	const (
		maxReportedIDs  = 8
		maxFieldIDBytes = 255
	)
	unique := make(map[string]struct{}, len(fieldIDs))
	for _, fieldID := range fieldIDs {
		if fieldID != "" {
			unique[fieldID] = struct{}{}
		}
	}
	ordered := make([]string, 0, len(unique))
	for fieldID := range unique {
		ordered = append(ordered, fieldID)
	}
	sort.Strings(ordered)

	reported := make([]string, 0, min(len(ordered), maxReportedIDs))
	omitted := 0
	for _, fieldID := range ordered {
		if len(reported) == maxReportedIDs || len(fieldID) > maxFieldIDBytes || !safeFieldID(fieldID) {
			omitted++
			continue
		}
		reported = append(reported, fieldID)
	}
	message := "Jira create metadata contains unsupported fields"
	if len(reported) > 0 {
		message += ": " + strings.Join(reported, ", ")
	}
	if omitted > 0 {
		message += fmt.Sprintf(" (%d field IDs omitted)", omitted)
	}
	hint := "choose or configure a standard issue type so unsupported fields are optional or have Jira defaults"
	if provideDescription && len(ordered) == 1 && ordered[0] == "description" {
		hint = "re-run with --description to supply the required description field"
	}
	return &Error{
		Code:    CodeUsage,
		Reason:  "CREATE_FIELDS_UNSUPPORTED",
		Message: message,
		Hint:    hint,
	}
}

// Auth reports missing, rejected, or expired Jira credentials.
func Auth(reason, format string, args ...any) *Error {
	return &Error{Code: CodeAuth, Reason: reason, Message: fmt.Sprintf(format, args...), Hint: "run 'jira-cli auth login --profile NAME' or rotate the API token"}
}

// Retryable reports a rate limit or transient transport failure.
func Retryable(reason string, retryAfter time.Duration, format string, args ...any) *Error {
	hint := "back off and retry if the operation is safe to repeat"
	if retryAfter > 0 {
		hint = fmt.Sprintf("retry after %s if the operation is safe to repeat", retryAfter)
	}
	return &Error{Code: CodeRetryable, Reason: reason, Message: fmt.Sprintf(format, args...), Hint: hint, RetryAfter: retryAfter}
}

// ConfirmRequired reports an unconfirmed write.
func ConfirmRequired(action string) *Error {
	return &Error{Code: CodeConfirm, Reason: "CONFIRMATION_REQUIRED", Message: fmt.Sprintf("%s was not confirmed", action), Hint: "obtain approval, then re-run with --yes"}
}

// Permission reports a Jira permission or API-token scope denial.
func Permission(reason, format string, args ...any) *Error {
	if reason == "" {
		reason = "PERMISSION_DENIED"
	}
	return &Error{Code: CodePermission, Reason: reason, Message: fmt.Sprintf(format, args...), Hint: "request the Jira permission or API token scope; do not retry unchanged"}
}

// Conflict reports a stale-state or Jira write conflict.
func Conflict(reason, format string, args ...any) *Error {
	if reason == "" {
		reason = "CONFLICT"
	}
	return &Error{Code: CodeConflict, Reason: reason, Message: fmt.Sprintf(format, args...), Hint: "re-read the issue and available transitions before deciding whether to retry"}
}

// WriteOutcomeUnknown reports that a write was dispatched but its result
// could not be established safely. The caller must reconcile instead of
// automatically repeating the request.
func WriteOutcomeUnknown(operation string) *Error {
	action := strings.TrimSpace(operation)
	if action == "" {
		action = "write"
	}
	return &Error{
		Code:    CodeConflict,
		Reason:  "WRITE_OUTCOME_UNKNOWN",
		Message: fmt.Sprintf("the outcome of %s could not be established", action),
		Hint:    fmt.Sprintf("re-read Jira to reconcile %s before deciding whether to retry", action),
	}
}

// PayloadTooLarge reports a Jira endpoint's bounded payload rejection.
func PayloadTooLarge(operation string) *Error {
	action := strings.TrimSpace(operation)
	if action == "" {
		action = "request"
	}
	return &Error{
		Code:    CodeUsage,
		Reason:  "PAYLOAD_TOO_LARGE",
		Message: fmt.Sprintf("Jira rejected the %s payload as too large", action),
		Hint:    "shorten the plain-text fields and retry",
	}
}

func upper(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z':
			b.WriteByte(c - ('a' - 'A'))
		case c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
			b.WriteByte(c)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

func flagKind(kind string) string {
	return strings.ReplaceAll(strings.ToLower(kind), "_", "-")
}

func safeFieldID(value string) bool {
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
