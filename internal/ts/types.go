// Package ts wraps the TeamSpeak 3 ServerQuery client with a long-lived,
// auto-reconnecting connection plus accessors for /api/server-info and
// /api/update-check.
package ts

import (
	"errors"
	"time"
)

// Config is the connection-time configuration for the long-lived client.
type Config struct {
	Host       string
	Port       string
	User       string
	Password   string
	SID        int

	// DialTimeout bounds the SSH handshake + initial protocol exchange.
	// Defaults to 10 seconds when zero.
	DialTimeout time.Duration

	// KeepAliveInterval is how often the wrapper pings the server with a
	// `version` ServerQuery to detect logical disconnects. Defaults to 3
	// minutes when zero.
	KeepAliveInterval time.Duration

	// BackoffMin and BackoffMax bound the exponential reconnect backoff.
	// Default 1s and 30s.
	BackoffMin time.Duration
	BackoffMax time.Duration
}

// VersionInfo mirrors the TS3 `version` response.
type VersionInfo struct {
	Version  string
	Platform string
	Build    int
}

// ServerInfo is the aggregated payload for /api/server-info.
type ServerInfo struct {
	Version       string
	Platform      string
	Build         int
	SID           int
	ServerStatus  string
	ServerName    string
	UptimeSeconds int64
	ClientsOnline int
	ClientsMax    int
	LoginName     string
}

// ErrUnavailable is returned when the TS connection is down or the
// command failed at the transport layer. The API maps it to HTTP 503.
var ErrUnavailable = errors.New("ts: unavailable")
