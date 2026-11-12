package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type scanMetrics struct {
	Requests int
	Bytes    int64
	Retries  int
}

func (a *app) githubRequest(ctx context.Context, method, endpoint string, payload []byte) ([]byte, error) {
	for attempt := 0; attempt < maxRetries; attempt++ {
		if err := a.waitForGitHub(ctx, endpoint); err != nil {
			return nil, err
		}
		req, err := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(payload))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+a.token)
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
		req.Header.Set("User-Agent", "GitVer/2")
		if payload != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		a.githubMu.Lock()
		a.metrics.Requests++
		if attempt > 0 {
			a.metrics.Retries++
		}
		a.githubMu.Unlock()
		resp, err := a.client.Do(req)
		delay := time.Duration(1<<attempt) * time.Second
		if err == nil {
			const maxBody = 10 << 20
			body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
			resp.Body.Close()
			a.githubMu.Lock()
			a.metrics.Bytes += int64(len(body))
			a.githubMu.Unlock()
			if readErr != nil {
				return nil, fmt.Errorf("read GitHub response: %w", readErr)
			}
			if len(body) > maxBody {
				return nil, errors.New("GitHub response exceeds 10 MiB")
			}
			now := time.Now()
			limited := isRateLimited(resp.StatusCode, resp.Header, body)
			if limited || resp.Header.Get("X-RateLimit-Remaining") == "0" {
				fallback := time.Minute * time.Duration(1<<attempt)
				resource := ""
				if resp.Header.Get("X-RateLimit-Remaining") == "0" {
					resource = githubResource(endpoint)
				}
				a.pauseGitHub(githubRetryDelay(resp.Header, now, fallback), resource)
			}
			if resp.StatusCode == http.StatusOK {
				return body, nil
			}
			if (!limited && !isTransientStatus(resp.StatusCode)) || attempt == maxRetries-1 {
				return nil, fmt.Errorf("GitHub returned %s: %s", resp.Status, strings.TrimSpace(string(body)))
			}
			log.Printf("GitHub returned %s; retrying", resp.Status)
			if limited {
				continue
			}
			delay = githubRetryDelay(resp.Header, now, delay)
		} else if attempt == maxRetries-1 {
			return nil, fmt.Errorf("GitHub request failed: %w", err)
		}
		if err := waitForContext(ctx, delay); err != nil {
			return nil, err
		}
	}
	return nil, errors.New("GitHub request failed after retries")
}

func isTransientStatus(status int) bool {
	return status == http.StatusBadGateway || status == http.StatusServiceUnavailable || status == http.StatusGatewayTimeout || status == http.StatusInternalServerError
}

func isRateLimited(status int, headers http.Header, body []byte) bool {
	if status == http.StatusTooManyRequests {
		return true
	}
	if status != http.StatusForbidden {
		return false
	}
	message := strings.ToLower(string(body))
	return headers.Get("Retry-After") != "" || headers.Get("X-RateLimit-Remaining") == "0" || strings.Contains(message, "secondary rate limit") || strings.Contains(message, "rate limit exceeded")
}

func githubRetryDelay(headers http.Header, now time.Time, fallback time.Duration) time.Duration {
	var delay time.Duration
	if seconds, err := strconv.ParseInt(headers.Get("Retry-After"), 10, 32); err == nil && seconds >= 0 {
		delay = time.Duration(seconds) * time.Second
	} else if until, err := http.ParseTime(headers.Get("Retry-After")); err == nil {
		delay = until.Sub(now)
	}
	if headers.Get("X-RateLimit-Remaining") == "0" {
		if reset, err := strconv.ParseInt(headers.Get("X-RateLimit-Reset"), 10, 64); err == nil {
			delay = max(delay, time.Unix(reset, 0).Add(time.Second).Sub(now))
		}
	}
	if delay <= 0 {
		return fallback
	}
	return delay
}

func githubResource(endpoint string) string {
	if endpoint == githubGraphQL {
		return "graphql"
	}
	return "core"
}

func (a *app) pauseGitHub(delay time.Duration, resource string) {
	a.githubMu.Lock()
	defer a.githubMu.Unlock()
	until := time.Now().Add(delay)
	if resource != "" {
		if a.primaryPauses == nil {
			a.primaryPauses = make(map[string]time.Time)
		}
		if until.After(a.primaryPauses[resource]) {
			a.primaryPauses[resource] = until
		}
	} else if until.After(a.pauseUntil) {
		a.pauseUntil = until
	}
}

func (a *app) waitForGitHub(ctx context.Context, endpoint string) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		a.githubMu.Lock()
		delay := max(time.Until(a.pauseUntil), time.Until(a.primaryPauses[githubResource(endpoint)]))
		a.githubMu.Unlock()
		if delay <= 0 {
			return nil
		}
		if err := waitForContext(ctx, delay); err != nil {
			return err
		}
	}
}

func waitForContext(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (a *app) logScanMetrics(started time.Time) {
	a.githubMu.Lock()
	metrics := a.metrics
	a.githubMu.Unlock()
	log.Printf("scan metrics: duration=%s requests=%d response_bytes=%d retries=%d", time.Since(started).Round(time.Millisecond), metrics.Requests, metrics.Bytes, metrics.Retries)
}
