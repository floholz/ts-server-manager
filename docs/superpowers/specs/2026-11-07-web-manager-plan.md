# Web Manager — Feature Plan

**Date:** 2026-11-07
**Status:** Broad plan, not yet a design. Each milestone gets its own spec before implementation, like the backup/restore one.

## Why

ts-server-manager started as a small sidecar (health, update check, backups). That part stays as it is: one small container for anyone who only wants health and backups.

The name promises more: actually *managing* a TeamSpeak server through a web interface, including things the TeamSpeak client doesn't offer and probably never will. That is a second program in this repository, shipped as its own image: the **web manager**.

## Products and versioning

| | Sidecar | Web manager |
|---|---|---|
| Purpose | Health, update check, scheduled backups, backup/restore CLI | Web UI with admin login, stats, broadcasts, automation |
| Footprint | One small binary, no database, no UI | One binary with an embedded database and web UI, one data volume |
| Image name | `ghcr.io/floholz/tsm-sidecar` (renamed from `ts-server-manager` in v0.3.0) | to be decided; likely takes over `ghcr.io/floholz/ts-server-manager` |
| Status | Feature-complete after scheduled backups (v0.3.0) | New |

- Both live in this repository and share `internal/ts` (ServerQuery connection, pacing, flood handling) and `internal/backup`.
- The web manager includes everything the sidecar does (health, update check, scheduled backups). Run one or the other, never both against the same server: two query clients would compete for the same per-IP flood budget.
- The project stays on **v0.x until the web manager is ready for v1**. While on v0, renaming the sidecar image is acceptable; the web manager then takes over as the main product of the repository.

## Architecture (direction, not final)

- **Repository layout:** `cmd/<sidecar>` and `cmd/<manager>` as two main packages; shared code stays in `internal/`. One release workflow builds both images.
- **Base: PocketBase** embedded as a Go library. It provides admin authentication, SQLite storage with migrations, a realtime API for the UI, an admin data browser, and serves the web UI's static files. Still a single container with a single volume.
- **One long-lived ServerQuery session** with event notifications (joins, leaves, moves, chat) plus a light polling loop for state that has no events (mute/away/idle). Built on the existing `ts.Client`; needs a notification channel added to the transport.
- **Query allowlist:** recommended, not required. Polling is about one command every few seconds, far below the 10-per-3-seconds limit; bursts (bulk moves, backups) go through the existing pacer.
- **Web UI:** a single-page app served by the manager. Framework still to be decided.

## Features, in priority order

### Priority 1 — User statistics

Track who is on the server, how long, and in which state.

- **Sessions:** join/leave per identity, with channel time (which channels, how long).
- **Presence states over time:** online, input muted, output muted ("deafened"), away, idle. Stored as state-change intervals, not raw samples, so storage stays small.
- **Views:** per user (time online per day/week, typical hours, favourite channels), server-wide (concurrent users over time, peak hours), leaderboards.
- **Identity:**
  - TeamSpeak identifies clients by a **unique identity** (a key pair), which exists without a myTeamSpeak account. It is stable until the person creates a new identity (new PC, reinstall without export).
  - With a linked **myTeamSpeak account**, TS6 also reports the account ID, which is stable across devices.
  - So stats are kept per identity, and identities can be grouped into a **person**: automatically when they share a myTeamSpeak ID, manually in the UI otherwise. Nicknames are recorded as history, never used as identity.
- **Privacy:** this is tracking people. Configurable retention (raw sessions vs. daily aggregates), no IP addresses stored by default, and a documented list of what is collected.
- **To verify on TS6 first:** which query notifications exist and fire (`notifycliententerview`, `notifyclientleftview`, `notifyclientmoved`), whether state changes (mute/away) produce any query event or must be polled via `clientlist -uid -away -voice -times`, and what TS6 exposes about myTeamSpeak IDs.

### Priority 1 — Broadcasts and announcements

- **Targets:** whole server, one or more channels, specific users or everyone in a server group. Delivery as a text message or a poke (popup).
- **One-off broadcasts** from the UI.
- **Scheduled and recurring announcements** (cron-style), e.g. "event tonight at 20:00", with an end date.
- **Event-triggered messages:** welcome message on join (first join vs. returning, per server group), message when someone enters a channel.
- **Templates** with variables such as `{nickname}`, `{online_count}`, `{next_event}`.
- **History:** every sent message is logged (who sent it, to whom, when).
- Groundwork for later features: "server restarts in 10 minutes" announcements before updates.

### Priority 2 — AFK tracker and auto-moving

- **AFK detection:** idle time over a threshold, optionally combined with muted/deafened or the away flag. Thresholds per server group (admins exempt, etc.).
- **Action:** move to the AFK channel and remember the original channel; move back on return (optional). Optional message explaining the move.
- **Auto-move rules:** per user or server group, "on join, move to channel X" (e.g. members land in their team channel). Also "never allowed in channel Y" style rules later.
- Uses the same presence tracking as the stats; no extra polling.

### Priority 3 — Server updates

- The update check already exists. Next steps:
  1. Show "update available" in the UI with the changelog link, take a backup on click.
  2. Optional, opt-in: perform the update. The manager pulls the new TeamSpeak image and recreates the container, after a scheduled announcement and a fresh backup.
- Performing updates needs control over the TeamSpeak container, i.e. the Docker socket, which is effectively root on the host. It must be opt-in, limited to the named container, and clearly documented. Alternative: a webhook to whatever orchestration runs the stack (to be decided with the infra rework).

### Later

- **Client group memberships in backups**, so a full restore also gives people their roles back. Same server: assign directly. New server: assign on first join (the manager sees joins anyway). Check TS6 for a direct way first (adding by identity, per-group privilege keys).
- Restoring backups from the UI, browsing and diffing scheduled backups.
- Channel icons and other files in backups.
- Chat commands (e.g. `!afk`, `!stats`) via text message events.
- Moderation helpers: ban/complaint overview, kick/ban from the UI with an audit log.

## Milestones

1. **Sidecar v0.3.0:** CI, scheduled backups, `/api/backup-status`, image rename.
2. **Repository split:** `cmd/` layout, second image in the release workflow, shared packages unchanged.
3. **TS6 event verification:** like Appendix A of the backup spec, for notifications, client list fields and identity/myTeamSpeak data.
4. **Manager foundation:** PocketBase, admin login, ServerQuery session with events, a dashboard showing who is online right now.
5. **User statistics** (priority 1).
6. **Broadcasts and announcements** (priority 1).
7. **AFK tracker and auto-moving** (priority 2).
8. **Server updates** (priority 3).
9. **v1.0** once milestones 4–8 are stable.

## Open decisions

- **Name** of the web manager image (the sidecar is `tsm-sidecar` since v0.3.0; the repository stays `ts-server-manager`).
- **Frontend framework** for the web UI.
- **Update mechanism:** Docker socket, orchestration webhook, or notify-only (depends on the infra rework).
- **Stats retention defaults** and what is shown to whom (admins only, or a public "who's online / leaderboard" page).
