package ts

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"sync"
	"time"
)

// Execer runs ServerQuery commands. *Conn, *Client and *Pacer implement it.
type Execer interface {
	Exec(*Command) ([]Row, error)
}

// ErrIDFlooding is the "client is flooding" error. Its extra_msg says how
// long to wait ("please wait N seconds").
const ErrIDFlooding = 524

// ErrFlooding is returned by Pacer when the server keeps throttling us or
// asks for a long wait (which in practice means a ban).
var ErrFlooding = errors.New("ts: server query flood protection triggered")

// DefaultRate is the default pacing for bulk work, in commands per second.
// The stock TS6 limit is 10 commands per 3 s for clients outside the query
// allowlist, and session setup counts towards it, so 2/s leaves headroom.
const DefaultRate = 2.0

// maxFloodWait is the longest "please wait" the pacer will sit out. Longer
// waits are bans (600 s by default); waiting those out silently is worse
// than failing with advice.
const maxFloodWait = 60 * time.Second

var pleaseWaitRe = regexp.MustCompile(`please wait (\d+) seconds?`)

// Pacer rate-limits commands to stay under the server's query flood
// protection, and recovers from a short "client is flooding" throttle by
// waiting it out and retrying once. A throttled command was rejected, not
// executed, so retrying it is safe.
//
// Rate <= 0 disables pacing (for allowlisted clients); 524 handling stays on.
type Pacer struct {
	next     Execer
	interval time.Duration

	mu   sync.Mutex
	last time.Time // when the previous command was released

	now   func() time.Time
	sleep func(time.Duration)
}

// NewPacer wraps next, allowing at most rate commands per second.
func NewPacer(next Execer, rate float64) *Pacer {
	p := &Pacer{next: next, now: time.Now, sleep: time.Sleep}
	if rate > 0 {
		p.interval = time.Duration(float64(time.Second) / rate)
	}
	return p
}

// Exec waits for its turn, runs cmd, and handles flood throttling.
func (p *Pacer) Exec(cmd *Command) ([]Row, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.waitTurn()
	rows, err := p.next.Exec(cmd)
	wait, flooded := floodWait(err)
	if !flooded {
		return rows, err
	}
	if wait >= maxFloodWait {
		return nil, floodErr(cmd, err, wait)
	}

	// Sit out the throttle with a margin, then retry exactly once. Sending
	// anything while throttled escalates to a ban.
	p.sleep(wait + time.Second)
	p.last = p.now()
	p.waitTurn()
	rows, err = p.next.Exec(cmd)
	if wait, flooded := floodWait(err); flooded {
		return nil, floodErr(cmd, err, wait)
	}
	return rows, err
}

// waitTurn sleeps until interval has passed since the previous command.
func (p *Pacer) waitTurn() {
	if p.interval > 0 && !p.last.IsZero() {
		if d := p.interval - p.now().Sub(p.last); d > 0 {
			p.sleep(d)
		}
	}
	p.last = p.now()
}

// floodWait reports whether err is a flood error and how long the server
// asked us to wait (1 s if it didn't say).
func floodWait(err error) (time.Duration, bool) {
	var qe *QueryError
	if !errors.As(err, &qe) || qe.ID != ErrIDFlooding {
		return 0, false
	}
	wait := time.Second
	if m := pleaseWaitRe.FindStringSubmatch(qe.ExtraMsg); m != nil {
		if n, convErr := strconv.Atoi(m[1]); convErr == nil {
			wait = time.Duration(n) * time.Second
		}
	}
	return wait, true
}

func floodErr(cmd *Command, err error, wait time.Duration) error {
	return fmt.Errorf("%w on %q (server asks to wait %s): add this host's IP to the "+
		"TeamSpeak query allowlist (query_ip_allowlist.txt) or lower --rate: %v",
		ErrFlooding, cmd.Name(), wait, err)
}
