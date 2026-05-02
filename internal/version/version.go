// Package version exposes the build-time version of the binary.
package version

// Version is overridden at build time via -ldflags="-X github.com/floholz/ts-server-manager/internal/version.Version=v0.1.0".
var Version = "dev"
