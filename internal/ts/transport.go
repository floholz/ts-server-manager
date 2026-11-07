package ts

import (
	"context"
	"errors"
	"fmt"
	"time"

	ts3 "github.com/multiplay/go-ts3"
	"golang.org/x/crypto/ssh"
)

// transport is the minimal surface needed from a connected ServerQuery
// session: send one raw command line, get its raw data lines back. Tests
// substitute a fake.
//
// Implementations return a *QueryError for error replies from the server
// and any other error for transport failures (I/O, timeout, closed).
type transport interface {
	Exec(line string) ([]string, error)
	Close() error
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
	tr := &ts3Transport{c: c}
	use := NewCommand("use").Arg("sid", cfg.SID)
	if _, err := execRows(tr, use); err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("use sid=%d: %w", cfg.SID, err)
	}
	_ = ctx // ts3 library doesn't accept a Context yet; reserved for future use.
	return tr, nil
}

// ts3Transport adapts *ts3.Client to the transport interface.
type ts3Transport struct {
	c *ts3.Client
}

func (t *ts3Transport) Exec(line string) ([]string, error) {
	lines, err := t.c.Exec(line)
	if err != nil {
		var te *ts3.Error
		if errors.As(err, &te) {
			return nil, queryErrorFrom(te)
		}
		return nil, err
	}
	return lines, nil
}

func (t *ts3Transport) Close() error {
	return t.c.Close()
}

func queryErrorFrom(te *ts3.Error) *QueryError {
	qe := &QueryError{ID: te.ID, Msg: te.Msg}
	if v, ok := te.Details["extra_msg"]; ok {
		qe.ExtraMsg = fmt.Sprint(v)
	}
	if v, ok := te.Details["failed_permid"].(int); ok {
		qe.FailedPermID = v
	}
	return qe
}

// execRows runs cmd on tr and parses the response. An "empty result set"
// reply becomes zero rows. QueryErrors are annotated with the command name.
func execRows(tr transport, cmd *Command) ([]Row, error) {
	return execLine(tr, cmd.String(), cmd.Name())
}

func execLine(tr transport, line, name string) ([]Row, error) {
	lines, err := tr.Exec(line)
	if err != nil {
		var qe *QueryError
		if errors.As(err, &qe) {
			if qe.ID == ErrIDEmptyResult {
				return nil, nil
			}
			qe.Command = name
			return nil, qe
		}
		return nil, err
	}
	return ParseRows(lines), nil
}
