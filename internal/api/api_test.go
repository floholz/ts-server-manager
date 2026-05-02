package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/floholz/ts-server-manager/internal/dockerhub"
	"github.com/floholz/ts-server-manager/internal/ts"
)

type fakeTS struct {
	connected bool
	version   ts.VersionInfo
	versionErr error
	info      ts.ServerInfo
	infoErr   error
}

func (f *fakeTS) IsConnected() bool                     { return f.connected }
func (f *fakeTS) Version() (ts.VersionInfo, error)      { return f.version, f.versionErr }
func (f *fakeTS) ServerInfo() (ts.ServerInfo, error)    { return f.info, f.infoErr }

type fakeHub struct {
	result dockerhub.Result
	err    error
	lastForce bool
}

func (f *fakeHub) Check(_ context.Context, runningVersion string, force bool) (dockerhub.Result, error) {
	f.lastForce = force
	return f.result, f.err
}

func newRouter(t *testing.T, ts TSClient, hub DockerHubClient) http.Handler {
	mux := http.NewServeMux()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	Register(mux, ts, hub, logger)
	return RecoverAndLog(logger)(mux)
}

func doGet(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

func TestHealthz_AlwaysOK(t *testing.T) {
	h := newRouter(t, &fakeTS{connected: false}, &fakeHub{})
	rr := doGet(t, h, "/healthz")
	if rr.Code != http.StatusOK {
		t.Errorf("status: got %d, want 200", rr.Code)
	}
}

func TestReadyz_NotConnected(t *testing.T) {
	h := newRouter(t, &fakeTS{connected: false}, &fakeHub{})
	rr := doGet(t, h, "/readyz")
	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("status: got %d, want 503", rr.Code)
	}
}

func TestReadyz_Connected(t *testing.T) {
	h := newRouter(t, &fakeTS{connected: true}, &fakeHub{})
	rr := doGet(t, h, "/readyz")
	if rr.Code != http.StatusOK {
		t.Errorf("status: got %d, want 200", rr.Code)
	}
}

func TestServerInfo_Success(t *testing.T) {
	info := ts.ServerInfo{
		Version: "6.0.0-beta9", Platform: "Linux", Build: 1700000000,
		SID: 1, ServerStatus: "online", ServerName: "Example TS",
		UptimeSeconds: 12345, ClientsOnline: 3, ClientsMax: 32, LoginName: "serveradmin",
	}
	h := newRouter(t, &fakeTS{connected: true, info: info}, &fakeHub{})
	rr := doGet(t, h, "/api/server-info")
	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got["version"] != "6.0.0-beta9" {
		t.Errorf("version: got %v", got["version"])
	}
	if got["server_name"] != "Example TS" {
		t.Errorf("server_name: got %v", got["server_name"])
	}
}

func TestServerInfo_Unavailable(t *testing.T) {
	h := newRouter(t, &fakeTS{connected: true, infoErr: ts.ErrUnavailable}, &fakeHub{})
	rr := doGet(t, h, "/api/server-info")
	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("status: got %d, want 503", rr.Code)
	}
}

func TestUpdateCheck_Success(t *testing.T) {
	hub := &fakeHub{result: dockerhub.Result{
		VersionRunning:  "6.0.0-beta9",
		VersionLatest:   "6.0.0-beta10",
		UpdateAvailable: true,
		CheckedAt:       time.Now().UTC(),
	}}
	h := newRouter(t, &fakeTS{connected: true, version: ts.VersionInfo{Version: "6.0.0-beta9"}}, hub)
	rr := doGet(t, h, "/api/update-check")
	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d", rr.Code)
	}
	var got map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["update_available"] != true {
		t.Errorf("update_available: got %v", got["update_available"])
	}
}

func TestUpdateCheck_TSDownReturns503(t *testing.T) {
	h := newRouter(t, &fakeTS{connected: false, versionErr: ts.ErrUnavailable}, &fakeHub{})
	rr := doGet(t, h, "/api/update-check")
	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("status: got %d", rr.Code)
	}
}

func TestUpdateCheck_UpstreamUnreachableReturns503(t *testing.T) {
	hub := &fakeHub{err: dockerhub.ErrUpstreamUnreachable}
	h := newRouter(t, &fakeTS{connected: true, version: ts.VersionInfo{Version: "6.0.0-beta9"}}, hub)
	rr := doGet(t, h, "/api/update-check")
	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("status: got %d", rr.Code)
	}
}

func TestUpdateCheck_UpstreamErrorReturns502(t *testing.T) {
	hub := &fakeHub{err: dockerhub.ErrUpstreamError}
	h := newRouter(t, &fakeTS{connected: true, version: ts.VersionInfo{Version: "6.0.0-beta9"}}, hub)
	rr := doGet(t, h, "/api/update-check")
	if rr.Code != http.StatusBadGateway {
		t.Errorf("status: got %d, want 502", rr.Code)
	}
}

func TestUpdateCheck_RefreshParam(t *testing.T) {
	hub := &fakeHub{result: dockerhub.Result{VersionRunning: "6.0.0-beta9", VersionLatest: "6.0.0-beta9"}}
	h := newRouter(t, &fakeTS{connected: true, version: ts.VersionInfo{Version: "6.0.0-beta9"}}, hub)

	doGet(t, h, "/api/update-check?refresh=1")
	if !hub.lastForce {
		t.Error("expected forceRefresh=true when refresh=1")
	}
	doGet(t, h, "/api/update-check")
	if hub.lastForce {
		t.Error("expected forceRefresh=false when refresh missing")
	}
}

func TestUpdateCheck_BadRefreshParam(t *testing.T) {
	hub := &fakeHub{}
	h := newRouter(t, &fakeTS{connected: true, version: ts.VersionInfo{Version: "6.0.0-beta9"}}, hub)
	rr := doGet(t, h, "/api/update-check?refresh=foo")
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status: got %d, want 400", rr.Code)
	}
}

func TestRecoverAndLog_PanicReturnsJSON500(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := RecoverAndLog(logger)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("boom")
	}))
	rr := doGet(t, h, "/anything")
	if rr.Code != http.StatusInternalServerError {
		t.Errorf("status: got %d, want 500", rr.Code)
	}
	if ct := rr.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("content-type: got %q, want application/json", ct)
	}
	var body map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("body is not valid JSON: %v; raw=%q", err, rr.Body.String())
	}
	if body["error"] != "internal error" {
		t.Errorf("error field: got %v", body["error"])
	}
}

// silence unused-import warnings if test set shrinks during refactor.
var _ = errors.New
