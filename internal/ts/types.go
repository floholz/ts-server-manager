// Package ts talks to a TeamSpeak server over ServerQuery (SSH).
//
// Client is the long-lived, auto-reconnecting connection used by serve mode.
// Conn is a plain one-shot session for CLI commands. Both execute arbitrary
// commands via Exec and share the same encoding and error handling.
package ts

import (
	"errors"
	"fmt"
	"time"
)

// Config is the connection-time configuration for the long-lived client.
type Config struct {
	Host     string
	Port     string
	User     string
	Password string
	SID      int

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

// VersionInfo mirrors the ServerQuery `version` response.
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

// Well-known ServerQuery error IDs.
const (
	// ErrIDEmptyResult ("database empty result set") is what list commands
	// return when there is nothing to list. Exec turns it into zero rows.
	ErrIDEmptyResult = 1281
	// ErrIDInvalidChannel is returned for an unknown channel ID.
	ErrIDInvalidChannel = 768
	// ErrIDPermissionDenied is returned when the query account lacks a permission.
	ErrIDPermissionDenied = 2568
)

// QueryError is an error reply from the TeamSpeak server ("error id=N
// msg=..."). It means the command was rejected, not that the connection
// broke, so it never triggers a reconnect.
type QueryError struct {
	ID  int    `json:"id"`
	Msg string `json:"msg"`
	// ExtraMsg and FailedPermID are set by the server for some errors.
	ExtraMsg     string `json:"extra_msg,omitempty"`
	FailedPermID int    `json:"failed_permid,omitempty"`
	// Command is the name of the command that failed.
	Command string `json:"command,omitempty"`
}

func (e *QueryError) Error() string {
	s := fmt.Sprintf("ts query %s: %s (id=%d)", e.Command, e.Msg, e.ID)
	if e.ExtraMsg != "" {
		s += ": " + e.ExtraMsg
	}
	if e.FailedPermID != 0 {
		s += fmt.Sprintf(" [failed_permid=%d]", e.FailedPermID)
	}
	return s
}

// IsQueryError reports whether err is a QueryError, optionally with one of
// the given IDs.
func IsQueryError(err error, ids ...int) bool {
	var qe *QueryError
	if !errors.As(err, &qe) {
		return false
	}
	if len(ids) == 0 {
		return true
	}
	for _, id := range ids {
		if qe.ID == id {
			return true
		}
	}
	return false
}
