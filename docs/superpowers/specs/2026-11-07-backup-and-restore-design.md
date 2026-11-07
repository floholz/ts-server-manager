# Backup and Restore — Design

**Date:** 2026-11-07
**Status:** Draft — scope agreed, TS6 behaviour still to be verified (see "Verification spike")
**Scope:** Export a TeamSpeak virtual server's channel layout, groups and permissions to a portable backup file, and restore that file onto a new (or explicitly overwritten) server. CLI only.

## Goals

- `ts-server-manager backup` writes a self-contained, versioned JSON file describing the server's channels, server groups, channel groups and all their permissions.
- `ts-server-manager restore <file>` rebuilds that setup on a target server, producing the same channel tree, group set and permission values.
- **Server groups are a primary deliverable.** They act as the server's roles: named permission sets that admins assign to users. A restored server must have every regular server group with its exact name, sort order and permission values, so that assigning users to them afterwards is a quick, mechanical step.
- The backup file is portable across server versions and instances: no server-assigned IDs are relied on, permissions are stored by name.
- `restore --dry-run` prints exactly what would be done without changing anything.
- Restoring onto a server that is not new is possible, but only after a clear warning that the server will be fully overwritten.

## Non-goals (deferred)

- **Client group memberships** (who is in which server/channel group), per-client permissions, and channel-client permissions. These are keyed by client database IDs, which do not carry over to a new server. Revisit with UID-keyed memberships applied on first join, or privilege keys.
- **Icons and other files** (requires the ServerQuery file transfer protocol, which go-ts3 does not implement).
- **Merge / reconcile restore** into a live server (diff and patch). v1 is "new server or full overwrite".
- **HTTP endpoints** for backup/restore. These come with admin auth (planned with the PocketBase integration). A backup contains the full permission setup and must not be exposed unauthenticated.
- **Scheduled backups** to a volume. A natural follow-up once the CLI backup is solid.
- Bans, complaints, privilege keys/tokens, server logs.

## Why a custom file rather than a database dump

A DB dump is tied to one server version and one DB engine (SQLite/MariaDB/Postgres), carries internal IDs that conflict on a different instance, and is opaque. A ServerQuery-level export is engine-agnostic, survives TS6 beta upgrades, is human-readable and diffable (it can live in git), and restore goes through the server's own validation.

## CLI

```
ts-server-manager backup  [-o FILE]                    # default: stdout
ts-server-manager restore FILE [--dry-run] [--yes] [--rate N]
```

- Connection settings come from the same env vars as serve mode (`TS_HOST`, `TS_PORT`, `TS_USER`, `TS_PASSWORD`, `TS_SID`). Only the TS variables are required for these subcommands.
- Each subcommand opens its own ServerQuery session; it does not need (or talk to) a running serve process. Typical use: `docker compose exec ts-server-manager /ts-server-manager backup > backup.json`.
- The query account must be `serveradmin` (or equivalent instance-level rights): restore deletes and creates groups and channels.
- Exit codes: `0` success, `1` error, `2` restore aborted by the user / refused because the target is not new and `--yes` was not given in a non-interactive session.

## Backup file format

JSON, UTF-8, pretty-printed (stable key and array order so two backups of an unchanged server are byte-identical apart from `created_at`).

```jsonc
{
  "schema_version": 1,
  "created_at": "2026-11-07T12:00:00Z",
  "tool_version": "v1.4.0",
  "source": { "ts_version": "6.0.0-beta13.1", "platform": "Linux", "server_name": "Example TS" },

  "server": {
    // Writable virtualserver_* settings worth carrying over (allowlist, see below).
    "properties": { "virtualserver_name": "Example TS", "virtualserver_welcomemessage": "..." },
    // Default group pointers, expressed as refs into the arrays below.
    "default_server_group": "sg:Normal",
    "default_channel_group": "cg:Guest",
    "default_channel_admin_group": "cg:Channel Admin"
  },

  "server_groups": [
    {
      "ref": "sg:Server Admin",
      "name": "Server Admin",
      "properties": { "sortid": 0, "namemode": 0, "savedb": 1 },
      "perms": [
        { "permsid": "b_virtualserver_modify_name", "value": 1, "negated": false, "skip": false }
      ]
    }
  ],

  "channel_groups": [
    { "ref": "cg:Channel Admin", "name": "Channel Admin", "properties": { ... }, "perms": [ ... ] }
  ],

  "channels": [
    {
      "ref": "c:1",
      "name": "Lobby",
      "properties": { "channel_topic": "...", "channel_codec": 4, "channel_flag_default": 1, ... },
      "has_password": false,
      "perms": [ { "permsid": "i_channel_needed_join_power", "value": 50, "negated": false, "skip": false } ],
      "children": [ { "ref": "c:4", "name": "AFK", ... } ]
    }
  ]
}
```

Rules:

- **Refs, not IDs.** `ref` values are file-local identifiers (derived from the source ID or name at backup time) used only for cross-references within the file. Restore maps refs to the IDs the target server assigns.
- **Permissions by name (`permsid`).** Numeric permission IDs may change between server versions; names are stable. Backup uses the `-permsid` flag on the `*permlist` commands, restore uses `permsid=` on the `*addperm` commands.
- **Channels as a tree.** Children are nested and ordered as they appear on the server, so parent/child relationships and sibling order (`channel_order`) are implicit. No source CIDs need to be preserved.
- **Only regular groups.** Server groups of type `1` (regular) are exported. Template (`0`) and query (`2`) groups are instance-wide and left alone, which also guarantees restore never touches the query account's own permissions.
- **Properties: capture wide, restore narrow.** Backup records all properties the server returns for a channel/group/server (lossless, future-proof for new TS6 properties). Restore only writes properties on a known-writable allowlist and reports any it skipped. Read-only properties (IDs, file paths, counters, timestamps) are dropped at backup time via a small denylist to keep files diffable.
- `schema_version` is bumped on any incompatible change; restore refuses files with an unknown major schema version.

## Restore algorithm

Restore is two-phase: **plan**, then **apply**. The plan is a list of steps referencing refs (target IDs are not known until apply). `--dry-run` prints the plan and stops.

1. **Preflight**
   - Connect, `use sid`, read `version`, `serverinfo`, `servergrouplist`, `channelgrouplist`, `channellist`, `permissionlist`.
   - Validate the file (schema version, ref integrity, exactly one default channel).
   - Check every `permsid` in the file exists on the target. Unknown permissions are reported and skipped, not fatal (newer/older TS6 builds may add or rename permissions).
   - **New-server detection.** The target counts as new when it has only the stock groups (no extra regular server groups or channel groups beyond the defaults) and a single channel. Otherwise:
     ```
     WARNING: target server "Example TS" (sid=1) is not a fresh server.
       14 channels, 6 server groups, 3 channel groups will be DELETED and replaced.
       Clients in deleted channels will be moved to the default channel.
     Type the server name to continue:
     ```
     Non-interactive sessions require `--yes`; without it, restore exits with code 2.
2. **Wipe** (only when overwriting): delete all non-default channels (`channeldelete force=1`), and all regular server/channel groups that are not referenced as defaults (`servergroupdel force=1`, `channelgroupdel force=1`). Default groups are kept and reused below because the server refuses to delete them.
3. **Server settings:** apply allowlisted `virtualserver_*` properties via `serveredit`.
4. **Server groups:** the backup's default server group maps onto the target's current default group; every other group maps onto an existing group with the same name, else is created (`servergroupadd`). For each mapped group: rename/edit properties, then replace its permission set (remove permissions not in the backup, add/update the rest). Permissions are sent batched (`servergroupaddperm sgid=X permsid=a permvalue=1 permnegated=0 permskip=0|permsid=b ...`) to keep the command count low.
5. **Channel groups:** same as server groups, matching the two default pointers (default channel group, default channel admin group) first.
6. **Default group pointers:** set `virtualserver_default_server_group` / `_channel_group` / `_channel_admin_group` to the mapped IDs.
7. **Channels:** depth-first walk of the tree. `channelcreate` with `cpid=<mapped parent>` and `channel_order=<mapped previous sibling>`, all permanent. Record ref → new CID. Then batched `channeladdperm`.
8. **Default channel:** set `channel_flag_default=1` on the restored default channel, then delete the original bootstrap channel of the fresh server.
9. **Report:** counts of created/updated objects, skipped permissions, skipped properties, channels whose password could not be restored.

If any step fails, restore stops and reports where. It does not attempt a rollback: the target is by definition a new or explicitly overwritten server, so the recovery path is to fix the cause and run restore again (step 2 makes it rerunnable).

### Flood protection

TeamSpeak bans query clients that exceed the flood limit (TS3 default: 10 commands per 3 seconds) unless their IP is in the query allowlist. Restore paces itself with a token-bucket limiter (`--rate`, default below the TS3 limit) and batches permission commands. The README documents adding the manager's IP to the query allowlist and raising `--rate` for faster restores.

### Channel passwords

On TS3, `channelinfo` returns the password **hash**, and setting that value would hash it again. The backup therefore stores `has_password` only. Restored channels that had a password are created without one and listed in the report, so they can be set by hand. If the spike shows TS6 behaves differently, this is revisited.

## Architecture

```
main.go                 subcommand dispatch: serve | healthcheck | backup | restore
internal/ts             connection + transport (gains a raw Exec)
internal/backup
  model.go              file types, (de)serialisation, schema validation
  export.go             Exporter: reads the server through a Query interface → model
  restore.go            Planner (model + target snapshot → []Step), Applier (executes steps, maps refs → IDs)
  query.go              narrow Query interface the package depends on
cmd wiring in main.go   flags, confirm prompt, output
```

`internal/backup` depends on an interface, not on go-ts3, so export and restore are unit-testable with a scripted fake query.

### Changes to `internal/ts`

- The `transport` interface gains a raw `Exec(cmd string, args ...)` that returns parsed response rows. The existing typed methods become helpers on top of it. go-ts3's `ExecCmd` already supports arbitrary commands.
- **Query errors vs transport errors.** A ServerQuery error response (`*ts3.Error`, e.g. "invalid channelID", "permission denied") is returned to the caller and does **not** drop the connection. Only I/O/timeout errors trigger a reconnect. The v1 spec required this, but the current code treats every error as a transport failure; backup/restore make the distinction necessary.
- All commands already run serialized under one call mutex (fixed 2026-11-07 together with this spec).

## Verification spike (before implementation)

TS6 is a new server codebase. Run against `teamspeaksystems/teamspeak6-server:6.0.0-beta13.1` (`compose.teamspeak.yaml`) and record the results in an appendix to this spec:

- [ ] `channellist -topic -flags -voice -limits -icon`, `channelinfo cid=`, `channelcreate`, `channeledit`, `channeldelete force=1`
- [ ] `channelpermlist cid= -permsid`, `channeladdperm` with `permsid=` and `|`-batching, `channeldelperm`
- [ ] `servergrouplist`, `servergrouppermlist sgid= -permsid`, `servergroupadd`, `servergroupaddperm` (batched), `servergroupdelperm`, `servergroupdel force=1`, `servergrouprename`
- [ ] Same set for `channelgroup*`
- [ ] `permissionlist` (names present? any TS6-only permissions?)
- [ ] `serverinfo`: which `virtualserver_*` properties exist and which `serveredit` accepts
- [ ] What `channelinfo` returns for `channel_password`
- [ ] Flood limit behaviour for a non-allowlisted query client
- [ ] Group/channel layout of a fresh server (for new-server detection and default mapping)

## Testing

| Layer | Approach |
|---|---|
| `internal/backup` model | Golden-file tests: JSON round-trip, stable ordering, schema validation errors. |
| Exporter | Scripted fake query returning recorded TS6 responses (captured during the spike) → expected model. |
| Planner | Table tests: fresh target, non-fresh target (wipe steps), default-group mapping, unknown permsids, password channels. |
| Applier | Fake query that assigns IDs; asserts command sequence, ref → ID mapping, batching, rate limiting (with a fake clock). |
| `internal/ts` | Query-error vs transport-error handling (query errors keep the connection). |
| End to end | `//go:build integration` round-trip against two `compose.teamspeak.yaml` TS instances: seed A, back up A, restore into fresh B, back up B, compare the two files after normalisation (drop `created_at`, `source`). Run manually and optionally in CI. |

## Implementation order

1. ~~Serialize all ServerQuery calls~~; ~~Docker healthcheck on `/healthz`~~ (done 2026-11-07).
2. `internal/ts`: raw `Exec`, query-error vs transport-error split.
3. Verification spike against beta13.1; record the appendix; settle the open points below.
4. `internal/backup` model + exporter + `backup` subcommand. Usable on its own from day one.
5. Planner + applier for fresh targets, `restore --dry-run`.
6. New-server detection, wipe, confirmation prompt.
7. Integration round-trip test; README section on backup/restore, query allowlist and rate.

## Open questions (resolved by the spike)

- Does TS6 hash channel passwords in `channelinfo` like TS3?
- Can a TS6 instance host multiple virtual servers (affects whether the integration test needs one or two containers)?
- Exact writable property allowlists for channels, groups and the server.
- Fresh-server fingerprint for new-server detection.
