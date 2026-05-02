package ts

import (
	"context"
	"fmt"
	"time"

	ts3 "github.com/multiplay/go-ts3"
	"golang.org/x/crypto/ssh"
)

// transport is the minimal surface the Client needs from a connected
// ServerQuery session. Tests substitute a fake.
type transport interface {
	Version() (VersionInfo, error)
	Whoami() (whoami, error)
	ServerInfo() (rawServerInfo, error)
	Close() error
}

type whoami struct {
	ServerStatus    string
	ServerID        int
	ClientLoginName string
}

type rawServerInfo struct {
	Name          string
	UptimeSeconds int64
	ClientsOnline int
	ClientsMax    int
	Status        string
	Version       string
	Platform      string
}

// dialer abstracts the act of opening a connected transport. Tests
// substitute a fake.
type dialer interface {
	dial(ctx context.Context, cfg Config) (transport, error)
}

// sshDialer dials a real TeamSpeak ServerQuery over SSH and selects the
// configured virtual server.
type sshDialer struct{}

func (sshDialer) dial(ctx context.Context, cfg Config) (transport, error) {
	sshCfg := &ssh.ClientConfig{
		User:            cfg.User,
		Auth:            []ssh.AuthMethod{ssh.Password(cfg.Password)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         cfg.DialTimeout,
	}

	addr := cfg.Host + ":" + cfg.Port
	// The wrapper owns the keepalive (a real `version` ServerQuery from
	// runConnected). Set the library keepalive to a long interval so it
	// doesn't double up with whitespace pings on the same wire.
	c, err := ts3.NewClient(addr,
		ts3.SSH(sshCfg),
		ts3.Timeout(cfg.DialTimeout),
		ts3.KeepAlive(24*time.Hour),
	)
	if err != nil {
		return nil, fmt.Errorf("dial ts3: %w", err)
	}

	if err := c.Use(cfg.SID); err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("use sid=%d: %w", cfg.SID, err)
	}

	_ = ctx // ts3 library doesn't accept a Context yet; reserved for future use.
	return &ts3Transport{c: c}, nil
}

// ts3Transport adapts *ts3.Client to the transport interface.
type ts3Transport struct {
	c *ts3.Client
}

func (t *ts3Transport) Version() (VersionInfo, error) {
	v, err := t.c.Version()
	if err != nil {
		return VersionInfo{}, err
	}
	return VersionInfo{Version: v.Version, Platform: v.Platform, Build: v.Build}, nil
}

func (t *ts3Transport) Whoami() (whoami, error) {
	w, err := t.c.Whoami()
	if err != nil {
		return whoami{}, err
	}
	return whoami{
		ServerStatus:    w.ServerStatus,
		ServerID:        w.ServerID,
		ClientLoginName: w.ClientLoginName,
	}, nil
}

func (t *ts3Transport) ServerInfo() (rawServerInfo, error) {
	s, err := t.c.Server.Info()
	if err != nil {
		return rawServerInfo{}, err
	}
	return rawServerInfo{
		Name:          s.Name,
		UptimeSeconds: int64(s.Uptime),
		ClientsOnline: s.ClientsOnline,
		ClientsMax:    s.MaxClients,
		Status:        s.Status,
		Version:       s.Version,
		Platform:      s.Platform,
	}, nil
}

func (t *ts3Transport) Close() error {
	return t.c.Close()
}
