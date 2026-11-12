package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (transport roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return transport(request)
}

func mockResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Status: fmt.Sprintf("%d %s", status, http.StatusText(status)), Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
}

func TestGitHubDoesNotCacheResponses(t *testing.T) {
	directory := t.TempDir()
	calls := 0
	application := &app{cachePath: filepath.Join(directory, "releases.json"), client: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls++
		if request.Header.Get("If-None-Match") != "" || request.Header.Get("If-Modified-Since") != "" {
			t.Error("requests must not use conditional cache validators")
		}
		response := mockResponse(http.StatusOK, fmt.Sprintf(`[{"tag_name":"v%d"}]`, calls))
		response.Header.Set("ETag", fmt.Sprintf(`"v%d"`, calls))
		response.Header.Set("Last-Modified", time.Now().UTC().Format(http.TimeFormat))
		return response, nil
	})}}
	for scan := 1; scan <= 2; scan++ {
		body, err := application.githubRequest(t.Context(), http.MethodGet, githubAPI+"/repos/owner/repo/releases", nil)
		if err != nil || string(body) != fmt.Sprintf(`[{"tag_name":"v%d"}]`, scan) {
			t.Fatalf("expected a fresh response on request %d: %s, %v", scan, body, err)
		}
	}
	files, err := os.ReadDir(directory)
	if err != nil || len(files) != 0 {
		t.Fatalf("API requests must not create cache files: %v, %v", files, err)
	}
	if calls != 2 || application.metrics.Requests != 2 {
		t.Fatalf("both requests must contact GitHub: calls=%d metrics=%+v", calls, application.metrics)
	}
}

func TestGitHubRetryDelay(t *testing.T) {
	now := time.Unix(1_800_000_000, 0).UTC()
	for _, test := range []struct {
		name       string
		retryAfter string
		remaining  string
		reset      string
		want       time.Duration
	}{
		{name: "seconds", retryAfter: "12", want: 12 * time.Second},
		{name: "date", retryAfter: now.Add(20 * time.Second).Format(http.TimeFormat), want: 20 * time.Second},
		{name: "reset", remaining: "0", reset: fmt.Sprint(now.Unix() + 30), want: 31 * time.Second},
		{name: "longest", retryAfter: "60", remaining: "0", reset: fmt.Sprint(now.Unix() + 30), want: time.Minute},
		{name: "invalid", retryAfter: "bad", want: time.Minute},
		{name: "past", retryAfter: now.Add(-time.Second).Format(http.TimeFormat), want: time.Minute},
	} {
		t.Run(test.name, func(t *testing.T) {
			headers := make(http.Header)
			headers.Set("Retry-After", test.retryAfter)
			headers.Set("X-RateLimit-Remaining", test.remaining)
			headers.Set("X-RateLimit-Reset", test.reset)
			if got := githubRetryDelay(headers, now, time.Minute); got != test.want {
				t.Fatalf("delay = %s, want %s", got, test.want)
			}
		})
	}
}

func TestGitHubRateLimitPausesOtherRequests(t *testing.T) {
	for _, status := range []int{http.StatusForbidden, http.StatusTooManyRequests, http.StatusOK} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var calls atomic.Int32
			a := &app{client: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				calls.Add(1)
				response := mockResponse(status, `[]`)
				if status == http.StatusTooManyRequests {
					response.Header.Set("Retry-After", "60")
				} else {
					response.Header.Set("X-RateLimit-Remaining", "0")
					response.Header.Set("X-RateLimit-Reset", fmt.Sprint(time.Now().Add(time.Minute).Unix()))
				}
				return response, nil
			})}}
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
			defer cancel()
			_, err := a.githubRequest(ctx, http.MethodGet, githubAPI, nil)
			if status == http.StatusOK && err != nil {
				t.Fatal(err)
			}
			if status != http.StatusOK && !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("expected cancellable rate-limit wait, got %v", err)
			}
			otherCtx, otherCancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
			defer otherCancel()
			endpoint := githubAPI
			if status == http.StatusTooManyRequests {
				endpoint = githubGraphQL
			}
			if _, err := a.githubRequest(otherCtx, http.MethodPost, endpoint, []byte(`{}`)); !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("other workers must share the pause: %v", err)
			}
			if calls.Load() != 1 {
				t.Fatalf("requests during pause: %d", calls.Load())
			}
		})
	}
}

func TestPrimaryRateLimitsRemainSeparate(t *testing.T) {
	a := &app{client: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return mockResponse(http.StatusOK, `{}`), nil
	})}}
	a.pauseGitHub(time.Hour, "core")
	if _, err := a.githubRequest(t.Context(), http.MethodPost, githubGraphQL, []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := a.githubRequest(ctx, http.MethodGet, githubAPI, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled primary-limit wait: %v", err)
	}
}

func TestGitHubRetriesAndPermanentForbidden(t *testing.T) {
	for _, limited := range []bool{false, true} {
		t.Run(fmt.Sprint(limited), func(t *testing.T) {
			var calls atomic.Int32
			var application app
			application.client = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				if calls.Add(1) == 1 {
					if limited {
						response := mockResponse(http.StatusTooManyRequests, `{"message":"slow down"}`)
						response.Header.Set("Retry-After", "1")
						return response, nil
					}
					return mockResponse(http.StatusForbidden, `{"message":"Resource not accessible by integration"}`), nil
				}
				return mockResponse(http.StatusOK, `[]`), nil
			})}
			_, err := application.githubRequest(t.Context(), http.MethodGet, githubAPI, nil)
			if limited {
				if err != nil || calls.Load() != 2 || application.metrics.Retries != 1 {
					t.Fatalf("retry did not recover: calls=%d metrics=%+v err=%v", calls.Load(), application.metrics, err)
				}
			} else if err == nil || calls.Load() != 1 {
				t.Fatalf("permission errors must not be retried: calls=%d err=%v", calls.Load(), err)
			}
		})
	}
}
