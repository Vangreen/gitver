package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const starredRepoFixture = `{"data":{"viewer":{"starredRepositories":{"nodes":[{"nameWithOwner":"owner/repo","description":"Example","owner":{"avatarUrl":"https://example.com/avatar"}}],"pageInfo":{"hasNextPage":false}}}}}`

func TestEmptyRepositoryRecheck(t *testing.T) {
	now := time.Now().UTC()
	old := now.AddDate(0, 0, -8)
	for _, test := range []struct {
		name          string
		exclusion     Exclusion
		body          string
		status        int
		wantChecks    int
		wantExcluded  bool
		wantRelease   bool
		wantRechecked bool
	}{
		{name: "legacy empty repository recovers", exclusion: Exclusion{Repo: "Owner/Repo", Reason: "no_releases", CreatedAt: old}, body: fmt.Sprintf(`[{"tag_name":"v1","published_at":%q}]`, now.Format(time.RFC3339)), wantChecks: 1, wantRelease: true},
		{name: "still empty", exclusion: Exclusion{Repo: "owner/repo", Reason: "no_releases", CreatedAt: old, CheckedAt: old}, body: `[]`, wantChecks: 1, wantExcluded: true, wantRechecked: true},
		{name: "failed recheck", exclusion: Exclusion{Repo: "owner/repo", Reason: "no_releases", CreatedAt: old, CheckedAt: old}, status: http.StatusNotFound, body: `{}`, wantChecks: 1, wantExcluded: true},
		{name: "recently checked", exclusion: Exclusion{Repo: "owner/repo", Reason: "no_releases", CreatedAt: old, CheckedAt: now}, wantExcluded: true},
		{name: "recent legacy exclusion", exclusion: Exclusion{Repo: "owner/repo", Reason: "no_releases", CreatedAt: now}, wantExcluded: true},
		{name: "manual exclusion never expires", exclusion: Exclusion{Repo: "owner/repo", Reason: "manual", CreatedAt: old}, wantExcluded: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			checks := 0
			a := &app{client: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
				if request.URL.String() == githubGraphQL {
					return mockResponse(http.StatusOK, starredRepoFixture), nil
				}
				checks++
				status := test.status
				if status == 0 {
					status = http.StatusOK
				}
				return mockResponse(status, test.body), nil
			})}}
			result, err := a.fetchReleases(t.Context(), []Exclusion{test.exclusion}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if checks != test.wantChecks || (len(result.Exclusions) == 1) != test.wantExcluded || (len(result.Releases) == 1) != test.wantRelease {
				t.Fatalf("checks=%d exclusions=%+v releases=%+v", checks, result.Exclusions, result.Releases)
			}
			if test.wantExcluded {
				if !result.Exclusions[0].CreatedAt.Equal(test.exclusion.CreatedAt) {
					t.Error("rechecking must preserve the original exclusion date")
				}
				if test.wantRechecked {
					if result.Exclusions[0].CheckedAt.Before(now) {
						t.Error("successful empty check must reset the retry interval")
					}
				} else if !result.Exclusions[0].CheckedAt.Equal(test.exclusion.CheckedAt) {
					t.Error("skipped or failed checks must not reset the interval")
				}
			}
		})
	}
}

func TestScanRetainsRecentReleasesOnRepositoryFailure(t *testing.T) {
	a := &app{client: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.String() == githubGraphQL {
			return mockResponse(http.StatusOK, starredRepoFixture), nil
		}
		return mockResponse(http.StatusNotFound, `{}`), nil
	})}}
	previous := []Release{
		{Repo: "OWNER/REPO", Version: "recent", PublishedAt: time.Now().Add(-time.Hour)},
		{Repo: "owner/repo", Version: "old", PublishedAt: time.Now().AddDate(0, 0, -15)},
	}
	result, err := a.fetchReleases(t.Context(), nil, previous)
	if err != nil || len(result.Releases) != 1 || result.Releases[0].Version != "recent" || len(result.Exclusions) != 0 || a.lastError == "" {
		t.Fatalf("failed repository handling: result=%+v err=%v warning=%s", result, err, a.lastError)
	}
}

func TestScanCancellationStopsQueuedRequests(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	calls := 0
	a := &app{client: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls++
		cancel()
		return mockResponse(http.StatusOK, starredRepoFixture), nil
	})}}
	if _, err := a.fetchReleases(ctx, nil, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancelled scan, got %v", err)
	}
	if calls != 1 {
		t.Fatalf("cancelled scan dispatched additional HTTP requests: %d", calls)
	}
}

func TestScanKeepsConcurrentExclusionEdits(t *testing.T) {
	for _, action := range []string{"exclude", "restore", "restore-auto"} {
		t.Run(action, func(t *testing.T) {
			entered := make(chan struct{})
			resume := make(chan struct{})
			var resumeOnce sync.Once
			unblock := func() { resumeOnce.Do(func() { close(resume) }) }
			t.Cleanup(unblock)
			a := &app{token: "test", csrfToken: "csrf", cachePath: filepath.Join(t.TempDir(), "releases.json")}
			if action != "exclude" {
				a.cache.Exclusions = []Exclusion{{Repo: "owner/repo", Reason: "manual", CreatedAt: time.Now()}}
				if action == "restore-auto" {
					a.cache.Exclusions[0].Reason = "no_releases"
					a.cache.Exclusions[0].CreatedAt = time.Now().AddDate(0, 0, -8)
				}
			}
			a.client = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
				if request.URL.String() == githubGraphQL {
					close(entered)
					<-resume
					return mockResponse(http.StatusOK, starredRepoFixture), nil
				}
				if action == "restore-auto" {
					return mockResponse(http.StatusOK, `[]`), nil
				}
				return mockResponse(http.StatusOK, fmt.Sprintf(`[{"tag_name":"v1","published_at":%q}]`, time.Now().UTC().Format(time.RFC3339))), nil
			})}
			a.startSync()
			select {
			case <-entered:
			case <-time.After(2 * time.Second):
				t.Fatal("scan did not start")
			}
			form := url.Values{"csrf": {"csrf"}, "repo": {"Owner/Repo"}}
			request := httptest.NewRequest(http.MethodPost, "/exclusions", strings.NewReader(form.Encode()))
			request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			response := httptest.NewRecorder()
			if action == "exclude" {
				a.addExclusion(response, request)
			} else {
				a.removeExclusion(response, request)
			}
			if response.Code != http.StatusSeeOther {
				t.Fatalf("edit failed: %d %s", response.Code, response.Body.String())
			}
			unblock()
			deadline := time.Now().Add(2 * time.Second)
			for {
				a.mu.RLock()
				syncing := a.syncing
				a.mu.RUnlock()
				if !syncing {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("scan did not finish")
				}
				time.Sleep(time.Millisecond)
			}
			a.mu.RLock()
			defer a.mu.RUnlock()
			if containsExclusion(a.cache.Exclusions, "owner/repo") != (action == "exclude") || len(a.cache.Releases) != 0 {
				t.Fatalf("concurrent %s was overwritten: %+v", action, a.cache)
			}
			if a.lastError != "" {
				t.Fatalf("scan error: %s", a.lastError)
			}
			persisted := &app{cachePath: a.cachePath}
			if err := persisted.loadCache(); err != nil {
				t.Fatal(err)
			}
			if containsExclusion(persisted.cache.Exclusions, "owner/repo") != (action == "exclude") {
				t.Fatal("concurrent edit was not persisted")
			}
		})
	}
}

func TestManualExclusionPinsAnAutomaticExclusion(t *testing.T) {
	a := &app{
		csrfToken: "csrf", cachePath: filepath.Join(t.TempDir(), "releases.json"),
		cache: cache{Exclusions: []Exclusion{{Repo: "Owner/Repo", Reason: "no_releases", CreatedAt: time.Now().AddDate(0, 0, -8)}}},
	}
	form := url.Values{"csrf": {"csrf"}, "repo": {"owner/repo"}}
	request := httptest.NewRequest(http.MethodPost, "/exclusions", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response := httptest.NewRecorder()
	a.addExclusion(response, request)
	if response.Code != http.StatusSeeOther || len(a.cache.Exclusions) != 1 || a.cache.Exclusions[0].Reason != "manual" {
		t.Fatalf("manual exclusion must not expire: %+v, status=%d", a.cache.Exclusions, response.Code)
	}
}

func TestLegacyCacheLoadsWithoutAPICache(t *testing.T) {
	data := []byte(`{"repo_count":12,"releases":[{"repo":"owner/repo","version":"v1"}],"exclusions":[{"repo":"owner/empty","reason":"no_releases","created_at":"2026-01-01T00:00:00Z"}]}`)
	var restored cache
	if err := json.Unmarshal(data, &restored); err != nil || restored.RepoCount != 12 || len(restored.Releases) != 1 || len(restored.Exclusions) != 1 || !restored.Exclusions[0].CheckedAt.IsZero() {
		t.Fatalf("legacy cache compatibility failed: %+v, %v", restored, err)
	}
}
