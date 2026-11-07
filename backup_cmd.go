package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/floholz/ts-server-manager/internal/backup"
	"github.com/floholz/ts-server-manager/internal/ts"
	"github.com/floholz/ts-server-manager/internal/version"
)

func runBackup(args []string) int {
	fs := flag.NewFlagSet("backup", flag.ContinueOnError)
	out := fs.String("o", "", "write the backup to `FILE` instead of stdout")
	rate := fs.Float64("rate", ts.DefaultRate, "max ServerQuery commands per second (0 = unlimited)")
	fs.Usage = printUsage
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "backup: unexpected argument %q\n", fs.Arg(0))
		return 2
	}

	// Writing JSON to a terminal is never what the user wants, and inside
	// `docker compose exec` without -T stdout *is* a terminal: the
	// redirected file then also gets the progress output and CRLF line
	// endings. Refuse before connecting.
	if *out == "" && isTerminal(os.Stdout) {
		fmt.Fprintln(os.Stderr, "backup: stdout is a terminal; use -o FILE or redirect the output to a file.\n"+
			"  With docker compose, add -T: docker compose exec -T ts-server-manager /ts-server-manager backup > ts-backup.json")
		return 2
	}

	conn, err := dialFromEnv()
	if err != nil {
		fmt.Fprintln(os.Stderr, "backup:", err)
		return 1
	}
	defer conn.Close()

	start := time.Now()
	f, err := backup.Export(ts.NewPacer(conn, *rate), backup.ExportOptions{
		ToolVersion: version.Version,
		Progress:    func(msg string) { fmt.Fprintln(os.Stderr, msg) },
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "backup:", err)
		return 1
	}

	if *out == "" {
		w := bufio.NewWriter(os.Stdout)
		if err := backup.Write(w, f); err == nil {
			err = w.Flush()
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "backup: write:", err)
			return 1
		}
	} else if err := writeFileAtomic(*out, func(w io.Writer) error { return backup.Write(w, f) }); err != nil {
		fmt.Fprintln(os.Stderr, "backup:", err)
		return 1
	}

	printBackupSummary(f, *out, time.Since(start))
	return 0
}

func printBackupSummary(f *backup.File, out string, took time.Duration) {
	channels, locked := 0, 0
	_ = backup.Walk(f.Channels, func(ch, _ *backup.Channel) error {
		channels++
		if ch.HasPassword {
			locked++
		}
		return nil
	})
	dest := out
	if dest == "" {
		dest = "stdout"
	}
	fmt.Fprintf(os.Stderr, "backup of %q (TeamSpeak %s) written to %s in %s:\n",
		f.Source.ServerName, f.Source.TSVersion, dest, took.Round(time.Second))
	fmt.Fprintf(os.Stderr, "  %d server groups, %d channel groups, %d channels (%d password-protected)\n",
		len(f.ServerGroups), len(f.ChannelGroups), channels, locked)
	if locked > 0 {
		fmt.Fprintln(os.Stderr, "  note: channel passwords are stored as hashes and cannot be restored directly;"+
			" keep the passwords somewhere safe and supply them on restore.")
	}
}

// writeFileAtomic writes via a temp file in the same directory and renames
// it into place, so an interrupted backup never leaves a truncated file.
// The file is created 0600: it contains the full permission setup and
// channel password hashes.
func writeFileAtomic(path string, write func(io.Writer) error) (err error) {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tmp.Close()
			_ = os.Remove(tmp.Name())
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		return fmt.Errorf("chmod: %w", err)
	}
	w := bufio.NewWriter(tmp)
	if err := write(w); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	if err := w.Flush(); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("sync: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return errors.Join(fmt.Errorf("rename into place: %w", err), os.Remove(tmp.Name()))
	}
	return nil
}
