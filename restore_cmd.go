package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/floholz/ts-server-manager/internal/backup"
	"github.com/floholz/ts-server-manager/internal/ts"
	"golang.org/x/term"
)

func runRestore(args []string) int {
	fs := flag.NewFlagSet("restore", flag.ContinueOnError)
	dryRun := fs.Bool("dry-run", false, "show what would be done without changing anything")
	yes := fs.Bool("yes", false, "overwrite a non-fresh server without asking")
	rate := fs.Float64("rate", ts.DefaultRate, "max ServerQuery commands per second (0 = unlimited)")
	pwFile := fs.String("channel-passwords", "", "JSON `FILE` mapping channel path or ref to password")
	fs.Usage = printUsage

	// Allow flags before and after the backup file argument.
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			return 2
		}
		if fs.NArg() == 0 {
			break
		}
		positional = append(positional, fs.Arg(0))
		args = fs.Args()[1:]
	}
	if len(positional) != 1 {
		fmt.Fprintln(os.Stderr, "restore: expected exactly one backup file argument")
		return 2
	}

	f, err := readBackupFile(positional[0])
	if err != nil {
		fmt.Fprintln(os.Stderr, "restore:", err)
		return 1
	}
	passwords, err := readPasswords(*pwFile)
	if err != nil {
		fmt.Fprintln(os.Stderr, "restore:", err)
		return 1
	}

	conn, err := dialFromEnv()
	if err != nil {
		fmt.Fprintln(os.Stderr, "restore:", err)
		return 1
	}
	defer conn.Close()

	if *dryRun {
		fmt.Fprintln(os.Stderr, "DRY RUN: reading the target, nothing will be changed.")
	}
	start := time.Now()
	report, err := backup.Restore(ts.NewPacer(conn, *rate), f, backup.RestoreOptions{
		DryRun:           *dryRun,
		Passwords:        passwords,
		ConfirmOverwrite: func(s backup.TargetSummary) bool { return confirmOverwrite(s, *yes) },
		Progress: func(msg string) {
			if !*dryRun {
				fmt.Fprintln(os.Stderr, msg)
			}
		},
	})
	if report != nil {
		printRestoreReport(os.Stderr, report, time.Since(start))
	}
	switch {
	case errors.Is(err, backup.ErrNotConfirmed):
		fmt.Fprintln(os.Stderr, "restore aborted: nothing was changed.")
		return 2
	case err != nil:
		fmt.Fprintln(os.Stderr, "restore failed:", err)
		if report != nil && !report.DryRun && len(report.Actions) > 0 {
			fmt.Fprintln(os.Stderr, "the target is partially restored; fix the cause and run restore again (it can be re-run).")
		}
		return 1
	}
	return 0
}

func readBackupFile(path string) (*backup.File, error) {
	fh, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer fh.Close()
	return backup.Read(bufio.NewReader(fh))
}

func readPasswords(path string) (map[string]string, error) {
	if path == "" {
		return nil, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("channel passwords: %w", err)
	}
	var m map[string]string
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("channel passwords: %s must be a JSON object of \"channel path or ref\": \"password\": %w", path, err)
	}
	return m, nil
}

// confirmOverwrite explains what an overwrite does and asks the user to
// type the server name. Non-interactive sessions need -yes.
func confirmOverwrite(s backup.TargetSummary, yes bool) bool {
	w := os.Stderr
	fmt.Fprintf(w, "\nWARNING: %q (TeamSpeak %s) is not a fresh server. Restoring will overwrite it:\n", s.ServerName, s.TSVersion)
	fmt.Fprintf(w, "  - all %d existing channels are deleted and replaced by the backup's", s.ChannelsDeleted)
	if s.ClientsOnline > 0 {
		fmt.Fprintf(w, " (%d client(s) online will be moved to the default channel)", s.ClientsOnline)
	}
	fmt.Fprintln(w)
	if len(s.ServerGroupsDeleted) > 0 {
		fmt.Fprintf(w, "  - server groups not in the backup are deleted: %s\n", strings.Join(quoteAll(s.ServerGroupsDeleted), ", "))
	}
	if len(s.ChannelGroupsDeleted) > 0 {
		fmt.Fprintf(w, "  - channel groups not in the backup are deleted: %s\n", strings.Join(quoteAll(s.ChannelGroupsDeleted), ", "))
	}
	fmt.Fprintln(w, "  - groups that also exist in the backup are updated in place and keep their members")
	fmt.Fprintln(w, "  - server settings covered by the backup are overwritten")

	if yes {
		fmt.Fprintln(w, "-yes given: continuing.")
		return true
	}
	if !isTerminal(os.Stdin) {
		fmt.Fprintln(w, "stdin is not a terminal: re-run with -yes to confirm, or -dry-run to see the full plan.")
		return false
	}
	fmt.Fprintf(w, "Type the server name (%s) to continue, anything else aborts: ", s.ServerName)
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	return strings.TrimRight(line, "\r\n") == s.ServerName
}

// isTerminal reports whether f is an interactive terminal. (A char-device
// check is not enough: /dev/null is a char device too.)
func isTerminal(f *os.File) bool {
	return term.IsTerminal(int(f.Fd()))
}

func quoteAll(names []string) []string {
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = fmt.Sprintf("%q", n)
	}
	return out
}

func printRestoreReport(w io.Writer, r *backup.Report, took time.Duration) {
	if r.DryRun {
		state := "a fresh server"
		if !r.Target.Fresh {
			state = "NOT a fresh server: a real run will ask for confirmation and overwrite it"
		}
		fmt.Fprintf(w, "\nTarget %q (TeamSpeak %s) is %s.\n", r.Target.ServerName, r.Target.TSVersion, state)
		fmt.Fprintf(w, "Planned actions (%d):\n", len(r.Actions))
		for _, a := range r.Actions {
			fmt.Fprintln(w, "  "+a)
		}
	} else if len(r.Actions) > 0 {
		fmt.Fprintf(w, "\nRestore of %q finished %d actions in %s.\n", r.Target.ServerName, len(r.Actions), took.Round(time.Second))
	}

	if len(r.SkippedPerms) > 0 {
		fmt.Fprintf(w, "\nSkipped permissions (%d):\n", len(r.SkippedPerms))
		for _, s := range r.SkippedPerms {
			fmt.Fprintf(w, "  %s: %s (%s)\n", s.Target, s.Name, s.Reason)
		}
	}
	if len(r.SkippedProps) > 0 {
		fmt.Fprintf(w, "\nSkipped settings (%d):\n", len(r.SkippedProps))
		for _, s := range r.SkippedProps {
			fmt.Fprintf(w, "  %s: %s (%s)\n", s.Target, s.Name, s.Reason)
		}
	}
	if len(r.Passwords) > 0 {
		fmt.Fprintln(w, "\nPassword-protected channels:")
		for _, p := range r.Passwords {
			fmt.Fprintf(w, "  %s: %s", p.Channel, p.Outcome)
			if p.Password != "" {
				fmt.Fprintf(w, " -> %s", p.Password)
			}
			fmt.Fprintln(w)
		}
		if r.DryRun {
			fmt.Fprintln(w, "  (random passwords shown in a dry run are not the ones a real run will set)")
		}
	}
	if len(r.UnusedPasswords) > 0 {
		fmt.Fprintf(w, "\nWarning: these -channel-passwords entries match no channel in the backup: %s\n",
			strings.Join(quoteAll(r.UnusedPasswords), ", "))
	}
}
