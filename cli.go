package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/floholz/ts-server-manager/internal/config"
	"github.com/floholz/ts-server-manager/internal/ts"
)

func printUsage() {
	fmt.Fprint(os.Stderr, `Usage: ts-server-manager [command]

Commands:
  serve         Run the HTTP sidecar (default when no command is given)
  healthcheck   Probe the local HTTP server's /healthz (used by Docker HEALTHCHECK)
  query [CMD]   Run raw ServerQuery commands and print the result as JSON.
                With CMD: run it and print its rows as a JSON array.
                Without:  read one command per line from stdin and print one
                          JSON object per command ({"cmd","rows"} or {"cmd","error"}).
                Commands are sent verbatim, so values must use ServerQuery
                escaping (\s for space). Any command can be run, including
                ones that modify the server.

Connection settings come from the TS_* environment variables (see .env.example).
`)
}

// dialFromEnv opens a one-shot ServerQuery session using the TS_* env vars.
func dialFromEnv() (*ts.Conn, error) {
	cfg, err := config.LoadFromOS()
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return ts.Connect(ctx, ts.Config{
		Host: cfg.TSHost, Port: cfg.TSPort,
		User: cfg.TSUser, Password: cfg.TSPassword,
		SID: cfg.TSSID,
	})
}

func runQuery(args []string) int {
	conn, err := dialFromEnv()
	if err != nil {
		fmt.Fprintln(os.Stderr, "query:", err)
		return 1
	}
	defer conn.Close()

	if len(args) > 0 {
		rows, err := conn.ExecRaw(strings.Join(args, " "))
		if err != nil {
			fmt.Fprintln(os.Stderr, "query:", err)
			return 1
		}
		return writeJSON(os.Stdout, rowsOrEmpty(rows), true)
	}
	return queryLines(conn, os.Stdin, os.Stdout)
}

// queryLines runs each non-empty, non-comment ('#') line of r and writes one
// JSON object per command to w. Query errors are reported per line and do
// not stop processing; a transport error does.
func queryLines(conn *ts.Conn, r io.Reader, w io.Writer) int {
	type result struct {
		Cmd   string         `json:"cmd"`
		Rows  []ts.Row       `json:"rows,omitempty"`
		Error *ts.QueryError `json:"error,omitempty"`
		Fatal string         `json:"fatal,omitempty"`
	}
	status := 0
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		res := result{Cmd: line}
		rows, err := conn.ExecRaw(line)
		var qe *ts.QueryError
		switch {
		case err == nil:
			res.Rows = rowsOrEmpty(rows)
		case errors.As(err, &qe):
			res.Error = qe
			status = 1
		default:
			res.Fatal = err.Error()
			writeJSON(w, res, false)
			return 1
		}
		if code := writeJSON(w, res, false); code != 0 {
			return code
		}
	}
	if err := sc.Err(); err != nil {
		fmt.Fprintln(os.Stderr, "query: read stdin:", err)
		return 1
	}
	return status
}

func rowsOrEmpty(rows []ts.Row) []ts.Row {
	if rows == nil {
		return []ts.Row{}
	}
	return rows
}

func writeJSON(w io.Writer, v any, indent bool) int {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	if indent {
		enc.SetIndent("", "  ")
	}
	if err := enc.Encode(v); err != nil {
		fmt.Fprintln(os.Stderr, "query: write output:", err)
		return 1
	}
	return 0
}
