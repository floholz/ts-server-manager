// Package config loads the manager's runtime configuration from environment variables.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	TSHost           string
	TSPort           string
	TSUser           string
	TSPassword       string
	TSSID            int
	HTTPAddr         string
	LogLevel         string
	DockerHubRepo    string
	UpdateCheckTTL   time.Duration
}

func Load(getenv func(string) string) (Config, error) {
	cfg := Config{
		TSHost:         getenv("TS_HOST"),
		TSPort:         envOr(getenv, "TS_PORT", "10022"),
		TSUser:         getenv("TS_USER"),
		TSPassword:     getenv("TS_PASSWORD"),
		HTTPAddr:       envOr(getenv, "HTTP_ADDR", ":9988"),
		LogLevel:       envOr(getenv, "LOG_LEVEL", "info"),
		DockerHubRepo:  envOr(getenv, "DOCKERHUB_REPO", "teamspeaksystems/teamspeak6-server"),
	}

	sidStr := envOr(getenv, "TS_SID", "1")
	sid, err := strconv.Atoi(sidStr)
	if err != nil {
		return cfg, fmt.Errorf("TS_SID must be an integer, got %q", sidStr)
	}
	cfg.TSSID = sid

	ttlStr := envOr(getenv, "UPDATE_CHECK_TTL", "15m")
	ttl, err := time.ParseDuration(ttlStr)
	if err != nil {
		return cfg, fmt.Errorf("UPDATE_CHECK_TTL must be a Go duration (e.g. 15m), got %q", ttlStr)
	}
	if ttl < 0 {
		return cfg, fmt.Errorf("UPDATE_CHECK_TTL must be non-negative, got %s", ttl)
	}
	cfg.UpdateCheckTTL = ttl

	var missing []string
	if cfg.TSHost == "" {
		missing = append(missing, "TS_HOST")
	}
	if cfg.TSUser == "" {
		missing = append(missing, "TS_USER")
	}
	if cfg.TSPassword == "" {
		missing = append(missing, "TS_PASSWORD")
	}
	if len(missing) > 0 {
		return cfg, fmt.Errorf("missing required env vars: %s", strings.Join(missing, ", "))
	}
	return cfg, nil
}

// LoadFromOS is a convenience wrapper that reads from os.Getenv.
func LoadFromOS() (Config, error) {
	return Load(os.Getenv)
}

func envOr(getenv func(string) string, key, fallback string) string {
	if v := getenv(key); v != "" {
		return v
	}
	return fallback
}
