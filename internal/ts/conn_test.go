package ts

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// floodDialErr is what x/crypto/ssh returns when TS6's connection flood
// protection rejects the handshake.
func floodDialErr(seconds string) error {
	return errors.New("dial ts3: client: connect: ssh connecion: ssh client conn: ssh: handshake failed: " +
		"ssh: disconnect, reason 11: please wait " + seconds + " seconds")
}

func TestConnect_ShortFloodWaitIsRetriedOnce(t *testing.T) {
	tr := newFakeTransport()
	d := &fakeDialer{queue: []dialResult{{err: floodDialErr("1")}, {t: tr}}}
	var slept []time.Duration
	conn, err := connectRetry(context.Background(), Config{}, d, func(_ context.Context, w time.Duration) bool {
		slept = append(slept, w)
		return true
	})
	if err != nil {
		t.Fatalf("expected retry to succeed, got %v", err)
	}
	if conn == nil || d.calls.Load() != 2 || len(slept) != 1 || slept[0] != 2*time.Second {
		t.Fatalf("dials=%d slept=%v", d.calls.Load(), slept)
	}
}

func TestConnect_SecondFloodFailsWithAdvice(t *testing.T) {
	d := &fakeDialer{queue: []dialResult{{err: floodDialErr("1")}, {err: floodDialErr("1")}}}
	_, err := connectRetry(context.Background(), Config{}, d, func(context.Context, time.Duration) bool { return true })
	if !errors.Is(err, ErrFlooding) {
		t.Fatalf("expected ErrFlooding, got %v", err)
	}
}

func TestConnect_LongFloodWaitFailsImmediately(t *testing.T) {
	d := &fakeDialer{queue: []dialResult{{err: floodDialErr("600")}}}
	_, err := connectRetry(context.Background(), Config{}, d, func(context.Context, time.Duration) bool {
		t.Fatal("must not wait out a ban")
		return true
	})
	if !errors.Is(err, ErrFlooding) || d.calls.Load() != 1 {
		t.Fatalf("err=%v dials=%d", err, d.calls.Load())
	}
}

func TestConnect_OtherErrorsAreNotRetried(t *testing.T) {
	d := &fakeDialer{queue: []dialResult{{err: errors.New("ssh: handshake failed: unable to authenticate")}}}
	_, err := connectRetry(context.Background(), Config{}, d, func(context.Context, time.Duration) bool {
		t.Fatal("must not retry")
		return true
	})
	if err == nil || errors.Is(err, ErrFlooding) || d.calls.Load() != 1 {
		t.Fatalf("err=%v dials=%d", err, d.calls.Load())
	}
}

func TestConnect_HandshakeEOFGetsFloodHint(t *testing.T) {
	d := &fakeDialer{queue: []dialResult{{err: errors.New("dial ts3: client: connect: ssh connecion: ssh client conn: ssh: handshake failed: EOF")}}}
	_, err := connectRetry(context.Background(), Config{}, d, func(context.Context, time.Duration) bool { return true })
	if err == nil || !strings.Contains(err.Error(), "query_ip_allowlist.txt") || d.calls.Load() != 1 {
		t.Fatalf("err=%v dials=%d", err, d.calls.Load())
	}
}

func TestRawCommandIsSentVerbatim(t *testing.T) {
	c := RawCommand("  channelfind pattern=a\\sb -flags \n")
	if c.Name() != "channelfind" || c.String() != `channelfind pattern=a\sb -flags` {
		t.Fatalf("name=%q string=%q", c.Name(), c.String())
	}
}
