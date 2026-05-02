package dockerhub

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// stubServer collects request paths and serves canned responses keyed by path.
type stubServer struct {
	mu        sync.Mutex
	responses map[string]stubResponse
	calls     atomic.Int32
	pathLog   []string
}

type stubResponse struct {
	status int
	body   string
}

func newStub() *stubServer {
	return &stubServer{responses: map[string]stubResponse{}}
}

func (s *stubServer) set(path string, status int, body string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.responses[path] = stubResponse{status: status, body: body}
}

func (s *stubServer) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.calls.Add(1)
		s.mu.Lock()
		s.pathLog = append(s.pathLog, r.URL.Path+"?"+r.URL.RawQuery)
		resp, ok := s.responses[r.URL.Path]
		s.mu.Unlock()
		if !ok {
			http.Error(w, "no stub", http.StatusNotFound)
			return
		}
		w.WriteHeader(resp.status)
		_, _ = w.Write([]byte(resp.body))
	})
}

func tagJSON(name, digest string) string {
	b, _ := json.Marshal(map[string]any{"name": name, "digest": digest})
	return string(b)
}

func tagListJSON(items []map[string]string) string {
	results := make([]map[string]string, 0, len(items))
	for _, it := range items {
		results = append(results, it)
	}
	b, _ := json.Marshal(map[string]any{"results": results})
	return string(b)
}

func newTestClient(t *testing.T, baseURL string, ttl time.Duration) *Client {
	c := New(Config{
		Repo:    "teamspeaksystems/teamspeak6-server",
		BaseURL: baseURL,
		TTL:     ttl,
	})
	t.Cleanup(func() {})
	return c
}

func TestCheck_DigestsMatchNoUpdate(t *testing.T) {
	stub := newStub()
	digest := "sha256:aaa"
	stub.set("/v2/repositories/teamspeaksystems/teamspeak6-server/tags/latest", 200, tagJSON("latest", digest))
	stub.set("/v2/repositories/teamspeaksystems/teamspeak6-server/tags/6.0.0-beta9", 200, tagJSON("6.0.0-beta9", digest))
	srv := httptest.NewServer(stub.handler())
	t.Cleanup(srv.Close)

	c := newTestClient(t, srv.URL, time.Minute)
	r, err := c.Check(context.Background(), "6.0.0-beta9", false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r.UpdateAvailable {
		t.Error("update_available should be false")
	}
	if r.VersionLatest != "6.0.0-beta9" {
		t.Errorf("version_latest: got %q, want %q", r.VersionLatest, "6.0.0-beta9")
	}
	if stub.calls.Load() != 2 {
		t.Errorf("expected 2 calls (no tag list), got %d", stub.calls.Load())
	}
}

func TestCheck_DigestsDiffer_TagListResolvesLatestName(t *testing.T) {
	stub := newStub()
	stub.set("/v2/repositories/teamspeaksystems/teamspeak6-server/tags/latest", 200, tagJSON("latest", "sha256:newer"))
	stub.set("/v2/repositories/teamspeaksystems/teamspeak6-server/tags/6.0.0-beta8", 200, tagJSON("6.0.0-beta8", "sha256:older"))
	stub.set("/v2/repositories/teamspeaksystems/teamspeak6-server/tags/", 200, tagListJSON([]map[string]string{
		{"name": "latest", "digest": "sha256:newer"},
		{"name": "6.0.0-beta9", "digest": "sha256:newer"},
		{"name": "6.0.0-beta8", "digest": "sha256:older"},
	}))
	srv := httptest.NewServer(stub.handler())
	t.Cleanup(srv.Close)

	c := newTestClient(t, srv.URL, time.Minute)
	r, err := c.Check(context.Background(), "6.0.0-beta8", false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !r.UpdateAvailable {
		t.Error("update_available should be true")
	}
	if r.VersionLatest != "6.0.0-beta9" {
		t.Errorf("version_latest: got %q, want %q", r.VersionLatest, "6.0.0-beta9")
	}
	if stub.calls.Load() != 3 {
		t.Errorf("expected 3 calls (tag list on miss), got %d", stub.calls.Load())
	}
}

func TestCheck_RunningVersionNotOnHub(t *testing.T) {
	stub := newStub()
	stub.set("/v2/repositories/teamspeaksystems/teamspeak6-server/tags/latest", 200, tagJSON("latest", "sha256:x"))
	stub.set("/v2/repositories/teamspeaksystems/teamspeak6-server/tags/custom-build", 404, `{"message":"not found"}`)
	srv := httptest.NewServer(stub.handler())
	t.Cleanup(srv.Close)

	c := newTestClient(t, srv.URL, time.Minute)
	r, err := c.Check(context.Background(), "custom-build", false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r.UpdateAvailable {
		t.Error("update_available should be false on 404")
	}
	if r.VersionLatest != "" {
		t.Errorf("version_latest should be empty, got %q", r.VersionLatest)
	}
	if r.Note == "" {
		t.Error("note should be set on 404")
	}
}

func TestCheck_UpstreamUnreachable(t *testing.T) {
	c := New(Config{
		Repo:    "teamspeaksystems/teamspeak6-server",
		BaseURL: "http://127.0.0.1:1", // closed port
		TTL:     time.Minute,
	})
	_, err := c.Check(context.Background(), "6.0.0-beta9", false)
	if !errors.Is(err, ErrUpstreamUnreachable) {
		t.Fatalf("expected ErrUpstreamUnreachable, got %v", err)
	}
}

func TestCheck_Upstream5xx(t *testing.T) {
	stub := newStub()
	stub.set("/v2/repositories/teamspeaksystems/teamspeak6-server/tags/latest", 503, `{"message":"upstream"}`)
	srv := httptest.NewServer(stub.handler())
	t.Cleanup(srv.Close)

	c := newTestClient(t, srv.URL, time.Minute)
	_, err := c.Check(context.Background(), "6.0.0-beta9", false)
	if !errors.Is(err, ErrUpstreamError) {
		t.Fatalf("expected ErrUpstreamError, got %v", err)
	}
}

func TestCheck_CacheHitSkipsHTTP(t *testing.T) {
	stub := newStub()
	stub.set("/v2/repositories/teamspeaksystems/teamspeak6-server/tags/latest", 200, tagJSON("latest", "sha256:x"))
	stub.set("/v2/repositories/teamspeaksystems/teamspeak6-server/tags/6.0.0-beta9", 200, tagJSON("6.0.0-beta9", "sha256:x"))
	srv := httptest.NewServer(stub.handler())
	t.Cleanup(srv.Close)

	c := newTestClient(t, srv.URL, time.Minute)
	if _, err := c.Check(context.Background(), "6.0.0-beta9", false); err != nil {
		t.Fatalf("first call: %v", err)
	}
	first := stub.calls.Load()
	if _, err := c.Check(context.Background(), "6.0.0-beta9", false); err != nil {
		t.Fatalf("second call: %v", err)
	}
	if stub.calls.Load() != first {
		t.Errorf("cached call should not hit HTTP: before=%d after=%d", first, stub.calls.Load())
	}
}

func TestCheck_ForceRefreshBypassesCache(t *testing.T) {
	stub := newStub()
	stub.set("/v2/repositories/teamspeaksystems/teamspeak6-server/tags/latest", 200, tagJSON("latest", "sha256:x"))
	stub.set("/v2/repositories/teamspeaksystems/teamspeak6-server/tags/6.0.0-beta9", 200, tagJSON("6.0.0-beta9", "sha256:x"))
	srv := httptest.NewServer(stub.handler())
	t.Cleanup(srv.Close)

	c := newTestClient(t, srv.URL, time.Minute)
	if _, err := c.Check(context.Background(), "6.0.0-beta9", false); err != nil {
		t.Fatalf("first call: %v", err)
	}
	first := stub.calls.Load()
	if _, err := c.Check(context.Background(), "6.0.0-beta9", true); err != nil {
		t.Fatalf("forced call: %v", err)
	}
	if stub.calls.Load() <= first {
		t.Errorf("forced refresh should re-hit HTTP: before=%d after=%d", first, stub.calls.Load())
	}
}

func TestCheck_TTLExpiry(t *testing.T) {
	stub := newStub()
	stub.set("/v2/repositories/teamspeaksystems/teamspeak6-server/tags/latest", 200, tagJSON("latest", "sha256:x"))
	stub.set("/v2/repositories/teamspeaksystems/teamspeak6-server/tags/6.0.0-beta9", 200, tagJSON("6.0.0-beta9", "sha256:x"))
	srv := httptest.NewServer(stub.handler())
	t.Cleanup(srv.Close)

	c := newTestClient(t, srv.URL, 5*time.Millisecond)
	if _, err := c.Check(context.Background(), "6.0.0-beta9", false); err != nil {
		t.Fatalf("first call: %v", err)
	}
	first := stub.calls.Load()
	time.Sleep(15 * time.Millisecond)
	if _, err := c.Check(context.Background(), "6.0.0-beta9", false); err != nil {
		t.Fatalf("post-ttl call: %v", err)
	}
	if stub.calls.Load() <= first {
		t.Errorf("expired cache should re-hit HTTP: before=%d after=%d", first, stub.calls.Load())
	}
}

// guard against the test file accidentally not compiling on its own.
var _ = strings.TrimSpace
