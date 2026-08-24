package jira

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/abigotado/jira-cli/internal/errx"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

func routedClient(t *testing.T, server *httptest.Server, inspect func(*http.Request)) *http.Client {
	t.Helper()
	target, err := url.Parse(server.URL)
	if err != nil {
		t.Fatalf("parse test server URL: %v", err)
	}
	return &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if inspect != nil {
			inspect(request)
		}
		clone := request.Clone(request.Context())
		clone.URL.Scheme = target.Scheme
		clone.URL.Host = target.Host
		clone.Host = target.Host
		return server.Client().Transport.RoundTrip(clone)
	})}
}

func TestDiscoverCloudIDUsesFixedUnauthenticatedRoute(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != tenantInfoPath || request.Method != http.MethodGet {
			t.Errorf("request = %s %s", request.Method, request.URL.Path)
		}
		if request.Header.Get("Authorization") != "" {
			t.Error("tenant discovery sent an Authorization header")
		}
		_, _ = writer.Write([]byte(`{"cloudId":"abc-123_DEF"}`))
	}))
	defer server.Close()

	client := routedClient(t, server, func(request *http.Request) {
		if request.URL.String() != "https://example.atlassian.net"+tenantInfoPath {
			t.Errorf("validated discovery URL = %q", request.URL.String())
		}
		if request.Header.Get("Authorization") != "" {
			t.Error("transport received an Authorization header")
		}
	})
	cloudID, err := DiscoverCloudID(context.Background(), "https://example.atlassian.net", client)
	if err != nil {
		t.Fatalf("DiscoverCloudID() error = %v", err)
	}
	if cloudID != "abc-123_DEF" {
		t.Errorf("cloud ID = %q, want abc-123_DEF", cloudID)
	}
}

func TestDiscoverCloudIDRefusesRedirect(t *testing.T) {
	var destinationCalls int
	destination := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		destinationCalls++
	}))
	defer destination.Close()
	source := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		http.Redirect(writer, request, destination.URL, http.StatusFound)
	}))
	defer source.Close()

	_, err := DiscoverCloudID(context.Background(), "https://example.atlassian.net", routedClient(t, source, nil))
	if err == nil {
		t.Fatal("expected redirect refusal error")
	}
	if destinationCalls != 0 {
		t.Errorf("redirect destination calls = %d, want 0", destinationCalls)
	}
}

func TestDiscoverCloudIDValidationAndResponseSafety(t *testing.T) {
	tests := []struct {
		name     string
		siteURL  string
		status   int
		body     func(http.ResponseWriter)
		wantCode errx.Code
	}{
		{
			name:     "site URL is strictly validated",
			siteURL:  "https://example.atlassian.net.invalid",
			wantCode: errx.CodeUsage,
		},
		{
			name:    "invalid cloud id is rejected",
			siteURL: "https://example.atlassian.net",
			body: func(writer http.ResponseWriter) {
				_, _ = writer.Write([]byte(`{"cloudId":"bad/id"}`))
			},
			wantCode: errx.CodeInternal,
		},
		{
			name:    "non JSON body is not exposed",
			siteURL: "https://example.atlassian.net",
			body: func(writer http.ResponseWriter) {
				_, _ = writer.Write([]byte("UPSTREAM_BODY_SENTINEL"))
			},
			wantCode: errx.CodeInternal,
		},
		{
			name:    "oversized body is refused",
			siteURL: "https://example.atlassian.net",
			body: func(writer http.ResponseWriter) {
				_, _ = io.CopyN(writer, strings.NewReader(strings.Repeat("x", maxResponseBody+1)), maxResponseBody+1)
			},
			wantCode: errx.CodeInternal,
		},
		{
			name:     "rate limit is retryable",
			siteURL:  "https://example.atlassian.net",
			status:   http.StatusTooManyRequests,
			wantCode: errx.CodeRetryable,
		},
		{
			name:     "server error is retryable",
			siteURL:  "https://example.atlassian.net",
			status:   http.StatusServiceUnavailable,
			wantCode: errx.CodeRetryable,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var calls int
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				calls++
				if test.status != 0 {
					writer.WriteHeader(test.status)
				}
				if test.body != nil {
					test.body(writer)
				}
			}))
			defer server.Close()

			_, err := DiscoverCloudID(context.Background(), test.siteURL, routedClient(t, server, nil))
			if got := errx.ExitCode(err); got != test.wantCode {
				t.Errorf("exit code = %d, want %d (err=%v)", got, test.wantCode, err)
			}
			if err != nil && strings.Contains(err.Error(), "UPSTREAM_BODY_SENTINEL") {
				t.Errorf("error leaked upstream body: %v", err)
			}
			if test.wantCode == errx.CodeUsage && calls != 0 {
				t.Errorf("invalid site made %d HTTP calls, want 0", calls)
			}
		})
	}
}

func TestDiscoverCloudIDTransportErrorIsGeneric(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("UPSTREAM_TRANSPORT_SENTINEL")
	})}
	_, err := DiscoverCloudID(context.Background(), "https://example.atlassian.net", client)
	if err == nil {
		t.Fatal("expected transport error")
	}
	if got := errx.ExitCode(err); got != errx.CodeRetryable {
		t.Errorf("exit code = %d, want %d", got, errx.CodeRetryable)
	}
	if strings.Contains(err.Error(), "UPSTREAM_TRANSPORT_SENTINEL") {
		t.Errorf("error leaked transport detail: %v", err)
	}
}
