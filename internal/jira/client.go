// Package jira is a hardened Jira Cloud REST v3 client.
//
// Credentials are passed in as values so this package stays independent of
// credential storage. Request URLs never contain credentials, redirects are
// refused, and upstream response bodies are not surfaced in errors.
package jira

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/abigotado/jira-cli/internal/errx"
)

const (
	maxAttempts     = 3
	maxResponseBody = 4 << 20
	maxDrainBody    = 64 << 10
)

var (
	classicHostPattern    = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?\.atlassian\.net$`)
	errTransitionRejected = errors.New("Jira rejected the transition")
)

// TokenKind selects the Atlassian endpoint required by an API token.
type TokenKind string

const (
	// TokenKindClassic uses the tenant's *.atlassian.net endpoint.
	TokenKindClassic TokenKind = "classic"
	// TokenKindScoped uses api.atlassian.com with the credential's cloud ID.
	TokenKindScoped TokenKind = "scoped"
)

// Config contains non-secret Jira connection configuration.
type Config struct {
	SiteURL string
}

// Credential contains one Jira account's authentication material.
//
// Formatting is always redacted so an accidental diagnostic cannot expose the
// token. Callers must still avoid logging credential values at all.
type Credential struct {
	Email     string
	Token     string
	TokenKind TokenKind
	CloudID   string
}

// Format redacts the complete credential, including for %#v and %+v.
func (Credential) Format(state fmt.State, _ rune) {
	_, _ = io.WriteString(state, "<redacted>")
}

// Client talks to Jira Cloud REST API v3.
type Client struct {
	baseURL string
	cred    Credential
	http    *http.Client
	log     *slog.Logger
	sleep   func(context.Context, time.Duration) error
	now     func() time.Time
}

// Option customizes a Client without weakening production URL validation.
type Option func(*Client)

// WithHTTPClient replaces the transport while retaining redirect refusal.
func WithHTTPClient(httpClient *http.Client) Option {
	return func(client *Client) {
		if httpClient == nil {
			return
		}
		clone := *httpClient
		clone.CheckRedirect = refuseRedirect
		clone.Jar = nil
		client.http = &clone
	}
}

// WithLogger sets the diagnostic logger. Messages never include credentials,
// query values, request bodies, response bodies, or complete request URLs.
func WithLogger(logger *slog.Logger) Option {
	return func(client *Client) {
		if logger != nil {
			client.log = logger
		}
	}
}

// WithSleep replaces retry sleeping, primarily for deterministic tests.
func WithSleep(sleep func(context.Context, time.Duration) error) Option {
	return func(client *Client) {
		if sleep != nil {
			client.sleep = sleep
		}
	}
}

// New validates the endpoint selection and constructs a client.
func New(config Config, credential Credential, options ...Option) (*Client, error) {
	baseURL, err := endpoint(config, credential)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(credential.Email) == "" {
		return nil, errx.Usage("Jira account email is required")
	}
	if credential.Token == "" {
		return nil, errx.Auth("MISSING_TOKEN", "Jira API token is missing")
	}

	client := &Client{
		baseURL: baseURL,
		cred:    credential,
		http: &http.Client{
			CheckRedirect: refuseRedirect,
		},
		log:   slog.New(discardHandler{}),
		sleep: sleepContext,
		now:   time.Now,
	}
	for _, option := range options {
		option(client)
	}
	return client, nil
}

func endpoint(config Config, credential Credential) (string, error) {
	switch credential.TokenKind {
	case TokenKindClassic:
		raw := config.SiteURL
		parsed, err := url.Parse(raw)
		if err != nil || parsed.Scheme != "https" || parsed.Opaque != "" || parsed.User != nil || parsed.Port() != "" ||
			parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || parsed.Path != "" || parsed.Hostname() != strings.ToLower(parsed.Hostname()) ||
			!classicHostPattern.MatchString(parsed.Hostname()) || raw != "https://"+parsed.Host {
			return "", errx.Usage("classic Jira site URL must be https://<tenant>.atlassian.net")
		}
		return raw, nil

	case TokenKindScoped:
		if !cloudIDPattern.MatchString(credential.CloudID) {
			return "", errx.Usage("cloud ID is required for a scoped Jira API token")
		}
		return "https://api.atlassian.com/ex/jira/" + url.PathEscape(credential.CloudID), nil

	default:
		return "", errx.Usage("unsupported Jira API token kind %q", credential.TokenKind)
	}
}

func refuseRedirect(_ *http.Request, _ []*http.Request) error {
	return http.ErrUseLastResponse
}

type request struct {
	method     string
	path       string
	query      url.Values
	body       any
	policy     requestPolicy
	operation  string
	notFound   string
	wantStatus int
}

type requestPolicy uint8

const (
	requestPolicyUnspecified requestPolicy = iota
	requestPolicyRead
	requestPolicyWrite
)

func (client *Client) do(ctx context.Context, request request, out any) error {
	if request.policy != requestPolicyRead && request.policy != requestPolicyWrite {
		return errx.Internal("Jira request has an invalid retry policy")
	}
	if request.policy == requestPolicyWrite && (request.wantStatus < http.StatusOK || request.wantStatus >= http.StatusMultipleChoices) {
		return errx.Internal("Jira write request has no exact expected status")
	}
	var payload []byte
	if request.body != nil {
		var err error
		payload, err = json.Marshal(request.body)
		if err != nil {
			return errx.Internal("could not encode Jira request")
		}
	}

	var lastErr error
	attempts := maxAttempts
	if request.policy != requestPolicyRead {
		attempts = 1
	}
	for attempt := 1; attempt <= attempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return errx.Translate(err)
		}
		response, err := client.send(ctx, request, payload)
		if err != nil {
			if request.policy == requestPolicyWrite {
				return errx.WriteOutcomeUnknown(request.operation).Wrap(err)
			}
			if ctx.Err() != nil {
				return errx.Translate(ctx.Err())
			}
			lastErr = errx.Retryable("NETWORK", 0, "could not reach Jira")
			if attempt == attempts {
				return lastErr
			}
			if err := client.sleep(ctx, retryBackoff(attempt)); err != nil {
				return translateSleepError(ctx, err)
			}
			continue
		}

		retryAfter, retry, translated := client.handle(response, request, out)
		if translated == nil {
			return nil
		}
		lastErr = translated
		if !retry || request.policy != requestPolicyRead || attempt == attempts {
			return translated
		}
		delay := retryAfter
		if delay <= 0 {
			delay = retryBackoff(attempt)
		}
		client.log.Debug("retrying Jira request",
			"method", request.method,
			"attempt", attempt,
			"delay", delay,
		)
		if err := client.sleep(ctx, delay); err != nil {
			return translateSleepError(ctx, err)
		}
	}
	return lastErr
}

func (client *Client) send(ctx context.Context, request request, payload []byte) (*http.Response, error) {
	fullURL := client.baseURL + request.path
	if len(request.query) > 0 {
		fullURL += "?" + request.query.Encode()
	}
	var body io.Reader
	if payload != nil {
		body = bytes.NewReader(payload)
	}
	httpRequest, err := http.NewRequestWithContext(ctx, request.method, fullURL, body)
	if err != nil {
		return nil, errors.New("invalid Jira request")
	}
	httpRequest.Header.Set("Accept", "application/json")
	if payload != nil {
		httpRequest.Header.Set("Content-Type", "application/json")
	}
	// SetBasicAuth constructs the header in memory. It is never logged or
	// copied into an error value.
	httpRequest.SetBasicAuth(client.cred.Email, client.cred.Token)
	client.log.Debug("Jira request", "method", request.method)
	return client.http.Do(httpRequest)
}

func (client *Client) handle(response *http.Response, request request, out any) (time.Duration, bool, error) {
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maxDrainBody))
		_ = response.Body.Close()
	}()

	body, tooLarge, readErr := readBounded(response.Body, maxResponseBody)
	if readErr != nil {
		if request.policy == requestPolicyWrite {
			return 0, false, errx.WriteOutcomeUnknown(request.operation).Wrap(readErr)
		}
		return 0, false, errx.Internal("could not read Jira response")
	}
	if tooLarge {
		if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
			return client.translateStatus(response, request, nil)
		}
		if request.policy == requestPolicyWrite {
			return 0, false, errx.WriteOutcomeUnknown(request.operation)
		}
		return 0, false, errx.Internal("Jira response exceeds the %d-byte safety limit", maxResponseBody)
	}

	if response.StatusCode >= http.StatusOK && response.StatusCode < http.StatusMultipleChoices {
		if request.policy == requestPolicyWrite && response.StatusCode != request.wantStatus {
			return 0, false, errx.WriteOutcomeUnknown(request.operation)
		}
		if out == nil {
			return 0, false, nil
		}
		if len(bytes.TrimSpace(body)) == 0 {
			if request.policy == requestPolicyWrite {
				return 0, false, errx.WriteOutcomeUnknown(request.operation)
			}
			return 0, false, nil
		}
		if err := json.Unmarshal(body, out); err != nil {
			if request.policy == requestPolicyWrite {
				return 0, false, errx.WriteOutcomeUnknown(request.operation).Wrap(err)
			}
			return 0, false, errx.Internal("Jira returned an invalid JSON response")
		}
		return 0, false, nil
	}

	return client.translateStatus(response, request, body)
}

func (client *Client) translateStatus(response *http.Response, request request, body []byte) (time.Duration, bool, error) {
	switch response.StatusCode {
	case http.StatusUnauthorized:
		return 0, false, errx.Auth("AUTHENTICATION_FAILED", "Jira rejected the account credentials")

	case http.StatusForbidden:
		if isAuthenticationDenied(response.Header, body) {
			return 0, false, errx.Auth("AUTHENTICATION_DENIED", "Jira denied authentication; complete any required browser challenge and retry")
		}
		return 0, false, errx.Permission("PERMISSION_DENIED", "the Jira account does not have permission for this operation")

	case http.StatusNotFound:
		if request.policy == requestPolicyWrite {
			return 0, false, writeConflict(request.operation)
		}
		kind := request.notFound
		if kind == "" {
			kind = resourceKind(request.path)
		}
		return 0, false, errx.NotFound(kind, "requested", nil)

	case http.StatusConflict, http.StatusPreconditionFailed:
		return 0, false, writeConflict(request.operation)

	case http.StatusRequestEntityTooLarge:
		return 0, false, errx.PayloadTooLarge(request.operation)

	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		if request.operation == "issues.transition" {
			return 0, false, errTransitionRejected
		}
		return 0, false, errx.Usage("Jira rejected the request parameters")

	case http.StatusTooManyRequests:
		delay := parseRetryAfter(response.Header.Get("Retry-After"), client.now())
		return delay, true, errx.Retryable("RATE_LIMITED", delay, "Jira rate limit reached")

	default:
		if request.policy == requestPolicyWrite {
			return 0, false, errx.WriteOutcomeUnknown(request.operation)
		}
		if response.StatusCode >= http.StatusInternalServerError {
			return 0, true, errx.Retryable("SERVER_ERROR", 0, "Jira is temporarily unavailable")
		}
		return 0, false, errx.Internal("Jira returned unexpected HTTP status %d", response.StatusCode)
	}
}

func writeConflict(operation string) *errx.Error {
	switch operation {
	case "issues.create":
		return errx.Conflict("ISSUE_CREATE_CONFLICT", "Jira rejected issue creation because related state changed")
	case "issues.edit":
		return errx.Conflict("ISSUE_EDIT_CONFLICT", "Jira rejected the edit because the issue changed")
	case "issues.transition":
		return errx.Conflict("TRANSITION_CONFLICT", "Jira rejected the transition because workflow state changed")
	case "comments.add":
		return errx.Conflict("COMMENT_CONFLICT", "Jira rejected the comment because the issue changed")
	default:
		return errx.Conflict("CONFLICT", "Jira rejected the request because the resource changed")
	}
}

func isAuthenticationDenied(header http.Header, body []byte) bool {
	reason := strings.ToUpper(strings.TrimSpace(header.Get("X-Seraph-LoginReason")))
	if reason == "AUTHENTICATION_DENIED" || reason == "AUTHENTICATION_FAILED" {
		return true
	}
	normalizedBody := strings.ToUpper(string(body))
	return strings.Contains(normalizedBody, "CAPTCHA") || strings.Contains(normalizedBody, "AUTHENTICATION_DENIED")
}

func readBounded(reader io.Reader, limit int64) ([]byte, bool, error) {
	body, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, false, err
	}
	if int64(len(body)) > limit {
		return nil, true, nil
	}
	return body, false, nil
}

func retryBackoff(attempt int) time.Duration {
	return time.Duration(1<<(attempt-1)) * 250 * time.Millisecond
}

func parseRetryAfter(value string, now time.Time) time.Duration {
	trimmed := strings.TrimSpace(value)
	if seconds, err := strconv.Atoi(trimmed); err == nil && seconds > 0 {
		return time.Duration(seconds) * time.Second
	}
	when, err := http.ParseTime(trimmed)
	if err != nil || !when.After(now) {
		return 0
	}
	return when.Sub(now)
}

func sleepContext(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func translateSleepError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return errx.Translate(ctx.Err())
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return errx.Translate(err)
	}
	return errx.Internal("Jira retry delay failed")
}

func resourceKind(path string) string {
	switch {
	case strings.Contains(path, "/issue/"):
		return "issue"
	case strings.Contains(path, "/project/"):
		return "project"
	default:
		return "resource"
	}
}

type discardHandler struct{}

func (discardHandler) Enabled(context.Context, slog.Level) bool   { return false }
func (discardHandler) Handle(context.Context, slog.Record) error  { return nil }
func (handler discardHandler) WithAttrs([]slog.Attr) slog.Handler { return handler }
func (handler discardHandler) WithGroup(string) slog.Handler      { return handler }
