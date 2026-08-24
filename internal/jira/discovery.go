package jira

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"time"

	"github.com/abigotado/jira-cli/internal/errx"
)

const tenantInfoPath = "/_edge/tenant_info"

var cloudIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)

// DiscoverCloudID resolves a validated Jira Cloud site to its Atlassian cloud
// ID. The fixed discovery endpoint is public, so no Authorization header is
// attached. Redirects are refused even when the supplied HTTP client would
// normally follow them.
func DiscoverCloudID(ctx context.Context, siteURL string, httpClient *http.Client) (string, error) {
	baseURL, err := endpoint(Config{SiteURL: siteURL}, Credential{TokenKind: TokenKindClassic})
	if err != nil {
		return "", err
	}

	client := httpClient
	if client == nil {
		client = &http.Client{}
	}
	clone := *client
	clone.CheckRedirect = refuseRedirect
	clone.Jar = nil

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+tenantInfoPath, nil)
	if err != nil {
		return "", errx.Internal("could not build Jira cloud ID discovery request")
	}
	request.Header.Set("Accept", "application/json")
	response, err := clone.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return "", errx.Translate(ctx.Err())
		}
		return "", errx.Retryable("NETWORK", 0, "could not reach Jira cloud ID discovery")
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maxDrainBody))
		_ = response.Body.Close()
	}()

	body, tooLarge, readErr := readBounded(response.Body, maxResponseBody)
	if readErr != nil {
		return "", errx.Internal("could not read Jira cloud ID discovery response")
	}
	if response.StatusCode == http.StatusTooManyRequests {
		delay := parseRetryAfter(response.Header.Get("Retry-After"), time.Now())
		return "", errx.Retryable("RATE_LIMITED", delay, "Jira cloud ID discovery was rate limited")
	}
	if response.StatusCode >= http.StatusInternalServerError {
		return "", errx.Retryable("SERVER_ERROR", 0, "Jira cloud ID discovery is temporarily unavailable")
	}
	if response.StatusCode != http.StatusOK {
		return "", errx.Internal("Jira cloud ID discovery returned unexpected HTTP status %d", response.StatusCode)
	}
	if tooLarge {
		return "", errx.Internal("Jira cloud ID discovery response exceeds the %d-byte safety limit", maxResponseBody)
	}

	var payload struct {
		CloudID string `json:"cloudId"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(body), &payload); err != nil {
		return "", errx.Internal("Jira cloud ID discovery returned invalid JSON")
	}
	if !cloudIDPattern.MatchString(payload.CloudID) {
		return "", errx.Internal("Jira cloud ID discovery returned an invalid cloud ID")
	}
	return payload.CloudID, nil
}
