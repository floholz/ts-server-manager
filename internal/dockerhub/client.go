// Package dockerhub queries Docker Hub to determine whether a newer
// image tag is available than the one currently running. It compares
// digests rather than version strings, with a small in-memory cache.
package dockerhub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync"
	"time"
)

const defaultBaseURL = "https://hub.docker.com"

type Config struct {
	Repo    string        // e.g. "teamspeaksystems/teamspeak6-server"
	BaseURL string        // overridable for tests; defaults to https://hub.docker.com
	TTL     time.Duration // cache freshness window; <= 0 disables caching
}

type Client struct {
	repo    string
	baseURL string
	ttl     time.Duration
	http    *http.Client

	mu     sync.Mutex
	cached *cachedResult
}

type cachedResult struct {
	runningVersion string
	result         Result
	storedAt       time.Time
}

type Result struct {
	VersionRunning  string    `json:"version_running"`
	VersionLatest   string    `json:"version_latest"`
	UpdateAvailable bool      `json:"update_available"`
	CheckedAt       time.Time `json:"checked_at"`
	Note            string    `json:"note,omitempty"`
}

var (
	ErrUpstreamUnreachable = errors.New("dockerhub: upstream unreachable")
	ErrUpstreamError       = errors.New("dockerhub: upstream error")
)

func New(cfg Config) *Client {
	base := cfg.BaseURL
	if base == "" {
		base = defaultBaseURL
	}
	return &Client{
		repo:    cfg.Repo,
		baseURL: base,
		ttl:     cfg.TTL,
		http:    &http.Client{Timeout: 10 * time.Second},
	}
}

// Check compares the digest of `latest` against the digest of the running
// version's tag. If they differ, it makes one extra call to fetch the
// tag list and resolve the human-readable name of `latest`.
func (c *Client) Check(ctx context.Context, runningVersion string, forceRefresh bool) (Result, error) {
	if !forceRefresh {
		if r, ok := c.cacheGet(runningVersion); ok {
			return r, nil
		}
	}

	digestLatest, _, err := c.fetchTag(ctx, "latest")
	if err != nil {
		return Result{}, err
	}

	digestRunning, status, err := c.fetchTag(ctx, runningVersion)
	if err != nil && status != http.StatusNotFound {
		return Result{}, err
	}

	out := Result{
		VersionRunning: runningVersion,
		CheckedAt:      time.Now().UTC(),
	}

	switch {
	case status == http.StatusNotFound:
		out.VersionLatest = ""
		out.UpdateAvailable = false
		out.Note = "running version not found on docker hub"
	case digestRunning == digestLatest:
		out.VersionLatest = runningVersion
		out.UpdateAvailable = false
	default:
		out.UpdateAvailable = true
		name, err := c.resolveLatestName(ctx, digestLatest)
		if err != nil {
			return Result{}, err
		}
		out.VersionLatest = name
	}

	c.cacheStore(runningVersion, out)
	return out, nil
}

func (c *Client) cacheGet(runningVersion string) (Result, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cached == nil || c.ttl <= 0 {
		return Result{}, false
	}
	if c.cached.runningVersion != runningVersion {
		return Result{}, false
	}
	if time.Since(c.cached.storedAt) > c.ttl {
		return Result{}, false
	}
	return c.cached.result, true
}

func (c *Client) cacheStore(runningVersion string, r Result) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cached = &cachedResult{runningVersion: runningVersion, result: r, storedAt: time.Now()}
}

// fetchTag returns the digest, the HTTP status (so callers can distinguish
// 404 without inspecting error wrapping), and any error.
func (c *Client) fetchTag(ctx context.Context, tag string) (string, int, error) {
	u := fmt.Sprintf("%s/v2/repositories/%s/tags/%s", c.baseURL, c.repo, url.PathEscape(tag))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", 0, fmt.Errorf("build request: %w", err)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("%w: %v", ErrUpstreamUnreachable, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return "", resp.StatusCode, fmt.Errorf("tag %q: 404", tag)
	}
	if resp.StatusCode >= 500 {
		return "", resp.StatusCode, fmt.Errorf("%w: status %d", ErrUpstreamError, resp.StatusCode)
	}
	if resp.StatusCode >= 400 {
		return "", resp.StatusCode, fmt.Errorf("%w: status %d", ErrUpstreamError, resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", resp.StatusCode, fmt.Errorf("%w: read body: %v", ErrUpstreamUnreachable, err)
	}
	var t struct {
		Digest string `json:"digest"`
	}
	if err := json.Unmarshal(body, &t); err != nil {
		return "", resp.StatusCode, fmt.Errorf("%w: parse: %v", ErrUpstreamError, err)
	}
	return t.Digest, resp.StatusCode, nil
}

// resolveLatestName returns the name of the first tag (excluding "latest")
// whose digest matches digestLatest, looking only at the first page of
// 100 tags. Empty string when not found.
func (c *Client) resolveLatestName(ctx context.Context, digestLatest string) (string, error) {
	u := fmt.Sprintf("%s/v2/repositories/%s/tags/?page_size=100", c.baseURL, c.repo)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", fmt.Errorf("build request: %w", err)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrUpstreamUnreachable, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 500 {
		return "", fmt.Errorf("%w: status %d", ErrUpstreamError, resp.StatusCode)
	}
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("%w: status %d", ErrUpstreamError, resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("%w: read body: %v", ErrUpstreamUnreachable, err)
	}
	var page struct {
		Results []struct {
			Name   string `json:"name"`
			Digest string `json:"digest"`
		} `json:"results"`
	}
	if err := json.Unmarshal(body, &page); err != nil {
		return "", fmt.Errorf("%w: parse: %v", ErrUpstreamError, err)
	}
	for _, r := range page.Results {
		if r.Name == "latest" {
			continue
		}
		if r.Digest == digestLatest {
			return r.Name, nil
		}
	}
	return "", nil
}
