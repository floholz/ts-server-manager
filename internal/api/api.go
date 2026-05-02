// Package api wires HTTP handlers for the manager's endpoints.
//
// Handlers depend on consumer-side interfaces (TSClient, DockerHubClient)
// satisfied by the concrete clients in internal/ts and internal/dockerhub.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/floholz/ts-server-manager/internal/dockerhub"
	"github.com/floholz/ts-server-manager/internal/ts"
)

// TSClient is the subset of *ts.Client the API depends on.
type TSClient interface {
	IsConnected() bool
	Version() (ts.VersionInfo, error)
	ServerInfo() (ts.ServerInfo, error)
}

// DockerHubClient is the subset of *dockerhub.Client the API depends on.
type DockerHubClient interface {
	Check(ctx context.Context, runningVersion string, forceRefresh bool) (dockerhub.Result, error)
}

// Register attaches all routes to mux. Wrap the mux with RecoverAndLog
// (or a chain that includes it) to get request-id and access logging.
func Register(mux *http.ServeMux, tsClient TSClient, hub DockerHubClient, logger *slog.Logger) {
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
	})

	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		if tsClient.IsConnected() {
			writeJSON(w, http.StatusOK, map[string]any{"status": "ready"})
			return
		}
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"status": "not_ready", "ts": "down"})
	})

	mux.HandleFunc("GET /api/server-info", func(w http.ResponseWriter, r *http.Request) {
		info, err := tsClient.ServerInfo()
		if err != nil {
			if errors.Is(err, ts.ErrUnavailable) {
				writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "ts unavailable"})
				return
			}
			logger.Error("server_info", "err", err.Error(), "request_id", requestIDFromContext(r.Context()))
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal error"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"version":        info.Version,
			"platform":       info.Platform,
			"build":          info.Build,
			"sid":            info.SID,
			"server_status":  info.ServerStatus,
			"server_name":    info.ServerName,
			"uptime_seconds": info.UptimeSeconds,
			"clients_online": info.ClientsOnline,
			"clients_max":    info.ClientsMax,
			"login_name":     info.LoginName,
		})
	})

	mux.HandleFunc("GET /api/update-check", func(w http.ResponseWriter, r *http.Request) {
		force, ok := parseRefreshParam(r.URL.Query().Get("refresh"))
		if !ok {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid refresh value"})
			return
		}

		v, err := tsClient.Version()
		if err != nil {
			if errors.Is(err, ts.ErrUnavailable) {
				writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "ts unavailable"})
				return
			}
			logger.Error("update_check.version", "err", err.Error(), "request_id", requestIDFromContext(r.Context()))
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal error"})
			return
		}

		result, err := hub.Check(r.Context(), v.Version, force)
		if err != nil {
			switch {
			case errors.Is(err, dockerhub.ErrUpstreamUnreachable):
				writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "upstream unreachable"})
			case errors.Is(err, dockerhub.ErrUpstreamError):
				writeJSON(w, http.StatusBadGateway, map[string]any{"error": "upstream error"})
			default:
				logger.Error("update_check.hub", "err", err.Error(), "request_id", requestIDFromContext(r.Context()))
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal error"})
			}
			return
		}
		writeJSON(w, http.StatusOK, result)
	})
}

func parseRefreshParam(s string) (force bool, ok bool) {
	switch s {
	case "":
		return false, true
	case "1", "true":
		return true, true
	case "0", "false":
		return false, true
	default:
		return false, false
	}
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
