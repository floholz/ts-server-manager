# Backup and Restore — Design

**Date:** 2026-11-07
**Status:** Scope agreed; TS6 behaviour verified against 6.0.0-beta13.1 (see Appendix A)
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
ts-server-manager backup  [-o FILE] [--rate N]                           # default: stdout
ts-server-manager restore FILE [--dry-run] [--yes] [--rate N] [--channel-passwords FILE]
ts-server-manager query   [CMD]                                          # raw ServerQuery, JSON out
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
      // Sort order, name display mode, icon and "save to db" are all
      // permissions on TS6 (i_group_sort_id, i_group_show_name_in_tree,
      // i_icon_id, b_group_is_permanent), so a group is fully described by
      // its name and its permission set.
      "perms": [
        { "permsid": "b_virtualserver_modify_name", "value": 1, "negated": false, "skip": false },
        { "permsid": "i_group_sort_id", "value": 10, "negated": false, "skip": false }
      ]
    }
  ],

  "channel_groups": [
    { "ref": "cg:Channel Admin", "name": "Channel Admin", "perms": [ ... ] }
  ],

  "channels": [
    {
      "ref": "c:1",
      "name": "Lobby",
      "properties": { "channel_topic": "...", "channel_codec": "4", "channel_flag_default": "1", ... },
      "has_password": false,
      // Present only when has_password is true. Deterministic server-side
      // hash; cannot be written back, but lets restore verify a password the
      // user supplies (see "Channel passwords").
      "password_hash": "enDElJk5HEDQo7PmB469eMoCfSs=",
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
- **Properties: capture wide, restore narrow.** Backup records all properties the server returns for a channel and the server (lossless, future-proof for new TS6 properties). Restore only writes properties on a curated allowlist (Appendix A) and reports any it skipped. Read-only and volatile properties (IDs, UUIDs, file paths, counters, timestamps, uptime) are dropped at backup time via a denylist to keep files diffable.
- **The server allowlist is curated, not "whatever `serveredit` accepts".** TS6 accepts edits to `virtualserver_port`, `virtualserver_address`, `virtualserver_machine_id`, `virtualserver_status`, `virtualserver_password` and even traffic counters. Restoring those onto a different host could make it unreachable or corrupt it, so they are never written.
- Property values are stored as the raw ServerQuery strings (`"4"`, not `4`): lossless, and restore sends them back verbatim.
- `schema_version` is bumped on any incompatible change; restore refuses files with an unknown major schema version.

## Restore algorithm

Restore is two-phase: **plan**, then **apply**. The plan is a list of steps referencing refs (target IDs are not known until apply). `--dry-run` prints the plan and stops.

1. **Preflight**
   - Connect, `use sid`, read `version`, `serverinfo`, `servergrouplist`, `channelgrouplist`, `channellist`, `permissionlist`.
   - Validate the file (schema version, ref integrity, exactly one default channel).
   - Check every `permsid` in the file exists on the target. Unknown permissions are reported and skipped, not fatal (newer/older TS6 builds may add or rename permissions).
   - **New-server detection.** The target counts as new when it matches the fresh-server fingerprint in Appendix A: one channel, and only the stock regular server and channel groups. Otherwise:
     ```
     WARNING: target server "Example TS" (sid=1) is not a fresh server.
       14 channels, 6 server groups, 3 channel groups will be DELETED and replaced.
       Clients in deleted channels will be moved to the default channel.
     Type the server name to continue:
     ```
     Non-interactive sessions require `--yes`; without it, restore exits with code 2.
2. **Wipe** (only when overwriting): backup groups are first matched to existing regular groups (default pointers, then exact name). Matched groups are **kept and updated in place** in steps 4–5, so their members survive the overwrite (memberships are not part of the backup). Unmatched regular groups are deleted (`servergroupdel force=1`, `channelgroupdel force=1`), except the target's current default groups, which TS6 refuses to delete. All channels except the current default are deleted (top-level channels and the default's children, `channeldelete force=1` removes subtrees); the default channel is replaced in step 8.
3. **Server settings:** apply allowlisted `virtualserver_*` properties via `serveredit`.
4. **Server groups:** the backup's default server group maps onto the target's current default group; every other group maps onto an existing regular group with the same name, else is created (`servergroupadd name=…`). For each mapped group: `servergrouprename` if needed, then **replace** its permission set. New groups come with default permissions (`b_group_is_permanent`, `i_group_needed_*_power`), so "replace" means: `servergroupdelperm` everything not in the backup, then add/update the rest.
5. **Channel groups:** same as server groups, matching the two default pointers (default channel group, default channel admin group) first.
6. **Default group pointers:** set `virtualserver_default_server_group` / `_channel_group` / `_channel_admin_group` to the mapped IDs.
7. **Channels:** depth-first walk of the tree. `channelcreate` with `cpid=<mapped parent>`, `channel_order=<mapped previous sibling>` and the allowlisted properties, all permanent. Record ref → new CID. Then batched `channeladdperm`.
8. **Default channel:** `channeledit channel_flag_default=1` on the restored default channel, then `channeldelete force=1` the bootstrap channel of the fresh server (deleting it first fails with "invalid channel flags").

9. **Report:** counts of created/updated objects, skipped permissions, skipped properties, channels whose password could not be restored.

### Permission batches

Permissions are sent batched (`servergroupaddperm sgid=X permsid=a permvalue=1 permnegated=0 permskip=0|permsid=b …`), at most 100 items per command, to keep the command count low. `servergroupaddperm`/`channelgroupaddperm` require all four fields; `channeladdperm` takes only `permsid` and `permvalue`.

**Batches are not atomic.** When one item in a batch is invalid, TS6 sometimes applies the items before it ("invalid parameter", 1538) and sometimes none ("invalid permission ID", 2562). So on any error from a batch, the applier falls back to sending that batch's items one at a time and records each item that fails as a skipped permission in the report. Permission names unknown to the target (`permissionlist`) are filtered out before sending.

If any step fails, restore stops and reports where. It does not attempt a rollback: the target is by definition a new or explicitly overwritten server, so the recovery path is to fix the cause and run restore again (step 2 makes it rerunnable).

### Flood protection

TS6 has two separate limits for query clients whose IP is not in the query allowlist (by default only `127.0.0.1` and `::1` inside the TS container):

- **Connection rate.** Opening SSH sessions in quick succession first gets `please wait 1 seconds`, then the IP is silently refused (handshake EOF, nothing in `banlist` or the log) for several minutes. Backup and restore therefore always use **one session** for the whole run, and the CLI never retries a failed dial in a tight loop.
- **Command rate** within a session: default instance limit is 10 commands per 3 s, including session setup. Exceeding it returns `client is flooding` (524, `please wait 1 seconds`); continuing to send while throttled escalates to a 600 s ban.

Both backup and restore therefore go through a pacer in front of `ts.Conn`:

- **Fixed spacing**, default 2 commands/s with no burst (`-rate N`; `-rate 0` disables pacing for allowlisted setups). Session setup already uses part of the 10-per-3 s budget, so 2/s leaves headroom. A typical backup (~50 channels, ~15 groups, ~120 commands) takes about a minute; restore of the same a few minutes. Measured: 24 commands in 12 s from a non-allowlisted host with no throttling.
- **524 handling:** parse `please wait N seconds`. For N < 60, stop sending, sleep N + 1 s, drain the bucket, and retry the same command once. For N ≥ 60, or a second 524 in a row, abort with an error that tells the user to add the manager's IP to the query allowlist (`query_ip_allowlist.txt`) or lower `--rate`.
- **Batching:** permission commands carry up to 100 items each, so the command count is driven by channels and groups, not permissions.

The README documents the allowlist (recommended for anything beyond small servers) and `--rate`.

### Channel passwords

TS6 behaves like TS3: `channelinfo` returns the password **hash** (`channel_password`), and writing that value back hashes it a second time. A password cannot be restored from the backup.

The hash is deterministic **per server** but differs between servers (same plaintext, different hash on another instance; verified with two beta13.1 containers). So:

- The backup stores `has_password`, `password_hash`, and the source's `virtualserver_unique_identifier` (`source.server_uid`).
- `restore -channel-passwords FILE` accepts a JSON object mapping channel path (`"Lobby/Gaming"`) or ref (`"c:5"`) to password. Supplied passwords are always set.
  - Restoring onto the **same** server (matching `server_uid`): the result is verified against `password_hash`; a mismatch is reported as a likely typo (the supplied password is kept).
  - Restoring onto **another** server: the report says the password was set but cannot be verified. It never claims a correct password is wrong.
- Channels with a password but no supplied one get a **random password** (shown in the report) rather than being left open: silently turning a locked channel into an open one would be a security regression.
- Password entries that match no channel are reported, to catch typos in paths.

The hash is unsalted within a server, so weak channel passwords in a backup file are open to offline guessing; the README tells users to treat backup files as sensitive.

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

### Changes to `internal/ts` (done)

- The transport is a single raw `Exec(line)`. Commands are built with `ts.Command` (escaping, `|`-batched parameter groups, `-options`) and responses parsed into `ts.Row` maps, so any command works without a per-command struct. go-ts3 is only used for the SSH session.
- **Query errors vs transport errors.** Error replies from the server become `*ts.QueryError` and leave the connection up; "database empty result set" (1281) becomes zero rows. I/O errors and timeouts drop the connection.
- `ts.Conn` is a one-shot session for CLI commands; `ts.Client` stays the long-lived, reconnecting connection for serve mode. Both serialize all commands.
- `ts-server-manager query` runs raw commands and prints JSON (used for Appendix A).

## Verification

Done against `teamspeaksystems/teamspeak6-server:6.0.0-beta13.1` on 2026-11-07. Results in Appendix A.

## Testing

| Layer | Approach |
|---|---|
| `internal/backup` model | Golden-file tests: JSON round-trip, stable ordering, schema validation errors. |
| Exporter | Scripted fake query returning recorded TS6 responses (captured during the spike) → expected model. |
| Planner | Table tests: fresh target, non-fresh target (wipe steps), default-group mapping, unknown permsids, password channels. |
| Applier | Fake query that assigns IDs; asserts command sequence, ref → ID mapping, batching, rate limiting (with a fake clock). |
| `internal/ts` | Query-error vs transport-error handling (query errors keep the connection). |
| End to end | `//go:build integration` round-trip against two TS containers (an unlicensed TS6 instance hosts only one virtual server): seed A, back up A, restore into fresh B, back up B, compare the two files after normalisation (drop `created_at`, `source`). Run manually and optionally in CI. The test binary connects from inside the Docker network or the containers' query allowlist must include it, otherwise connection flood protection kicks in. |

## Implementation order

1. ~~Serialize all ServerQuery calls~~; ~~Docker healthcheck on `/healthz`~~ (done 2026-11-07).
2. ~~`internal/ts`: raw `Exec`, query-error vs transport-error split~~ (done 2026-11-07).
3. ~~Verification against beta13.1~~ (done 2026-11-07, Appendix A).
4. ~~`internal/backup` model + exporter + `backup` subcommand~~ (done 2026-11-07; verified against beta13.1 from a non-allowlisted host, output deterministic).
5. ~~Restore for fresh targets, `restore -dry-run`~~ (done 2026-11-07). Dry run and real run share one code path: writes are recorded instead of sent, new IDs become placeholders.
6. ~~New-server detection, overwrite, confirmation prompt~~ (done 2026-11-07). Verified live: fresh restore into a second beta13.1 container and overwrite of a modified server both reproduce the backup exactly (apart from password hashes, see above).
7. Integration round-trip test; README section on backup/restore, query allowlist and rate.

## Appendix A: TS6 verification results (6.0.0-beta13.1, 2026-11-07)

Run with `ts-server-manager query` against `compose.teamspeak.yaml` (MariaDB backend, unlicensed).

### Instance and fresh-server fingerprint

- Unlicensed instance: **max 1 virtual server, 32 slots**.
- Query allowlist defaults to `127.0.0.1/32, ::1/128` (inside the container only).
- A fresh virtual server has:
  - **1 channel**: `Default Channel` (cid 1, default, permanent, topic "Default Channel has no topic").
  - **Server groups**: query groups `Guest Server Query`, `Admin Server Query` (type 2); templates `Server Admin`, `Normal`, `Guest` (type 0); regular `Server Admin`, `Normal`, `Guest` (type 1).
  - **Channel groups**: templates `Channel Admin`, `Operator`, `Voice`, `Guest` (type 0) and the same four as regular groups (type 1). Channel groups have a `type` on TS6.
  - Default pointers: `virtualserver_default_server_group` = regular `Guest`, `virtualserver_default_channel_group` = regular channel `Guest`, `virtualserver_default_channel_admin_group` = regular `Channel Admin`.
- Regular `Server Admin` starts with 424 permissions, regular `Channel Admin` with 33. `permissionlist` returns 510 named permissions.

### Commands

| Command | Works | Notes |
|---|---|---|
| `version`, `whoami`, `serverinfo` | yes | `serverinfo` has 109 properties. |
| `channellist -topic -flags -voice -limits -icon -secondsempty` | yes | |
| `channelinfo cid=` | yes | TS6 adds `channel_unique_identifier`, `channel_banner_gfx_url`, `channel_banner_mode`, `channel_security_salt`, `channel_storage_quota`. |
| `servergrouplist`, `channelgrouplist` | yes | Rows carry `sortid`, `namemode`, `iconid`, `savedb`, but these mirror permissions (see below). |
| `servergrouppermlist -permsid`, `channelgrouppermlist -permsid`, `channelpermlist -permsid` | yes | Unlike TS3, the group/channel ID is repeated on **every** row. |
| `channelcreate` with `cpid`, `channel_order` | yes | `channel_order` = CID of the sibling above; 0 = first. |
| `channeledit channel_flag_default=1` then `channeldelete cid=<old default> force=1` | yes | Deleting the default channel directly: `invalid channel flags` (775). |
| `servergroupadd name=…` | yes | Returns `sgid`. `type` defaults to regular. New groups get `b_group_is_permanent` and `i_group_needed_{modify,member_add,member_remove}_power` = 100 automatically. |
| `servergrouprename`, `servergroupdelperm` | yes | |
| `servergroupaddperm` | yes | **Requires** `permnegated` and `permskip`; without them: `parameter not found` (1539). `|`-batching works. `permnegated`/`permskip` round-trip correctly. |
| `channelgroupadd`, `channelgroupaddperm` | yes | Same automatic default permissions as server groups. |
| `channeladdperm` | yes | Takes `permsid` + `permvalue` only. `|`-batching works. |
| `servergroupdel` on a default group | refused | `access to default group is forbidden` (2564). |
| `serveredit` | yes | See allowlist below. |
| `banlist` (empty) | yes | Returns 1281 → zero rows. |

### Permission batches are not atomic

- Batch with an item that is not valid in that context (`b_channel_join_ignore_password` via `channeladdperm`): error `invalid parameter` (1538), **items before it were applied**.
- Batch with an unknown permission name: error `invalid permission ID` (2562), **nothing applied**.

### Group properties are permissions

`sortid` ↔ `i_group_sort_id`, `namemode` ↔ `i_group_show_name_in_tree`, `iconid` ↔ `i_icon_id`, `savedb` ↔ `b_group_is_permanent`. Restoring the permission set restores these; a group is fully described by name + permissions. Channel icons likewise come from the channel's `i_icon_id` permission (`channel_icon_id` is not writable). Icon IDs of stock groups (100–600) are built-in icons, so they survive a restore; uploaded custom icons do not (deferred).

### Channel passwords

- `channelinfo` returns `channel_password` as a 28-character base64 hash; `channel_flag_password=1`.
- The hash is deterministic on one server (the same plaintext gives the same hash) but **differs between servers**: `s3cret` hashed to `enDElJk5…` on one instance and `yQNENBWE…` on another. Not plain SHA-1.
- Writing the hash back as `channel_password` hashes it again (different value). **Passwords cannot be restored from the hash.**

### Writable channel properties (`channeledit` with current value)

Accepted: `channel_name`, `channel_name_phonetic`, `channel_topic`, `channel_description`, `channel_codec`, `channel_codec_quality`, `channel_codec_latency_factor`, `channel_codec_is_unencrypted`, `channel_maxclients`, `channel_maxfamilyclients`, `channel_flag_maxclients_unlimited`, `channel_flag_maxfamilyclients_unlimited`, `channel_flag_maxfamilyclients_inherited`, `channel_flag_permanent`, `channel_flag_semi_permanent`, `channel_needed_talk_power`, `channel_order`, `channel_storage_quota`, `channel_banner_gfx_url`, `channel_banner_mode`, `channel_flag_password`.

Rejected (read-only or set another way): `pid` (use `cpid`/`channelmove`), `channel_flag_default` (only settable to 1), `channel_icon_id` (permission), `channel_delete_delay` (temporary channels only), `channel_forced_silence`, `channel_security_salt`, `channel_unique_identifier`, `channel_filepath`, `seconds_empty`.

**Restore allowlist** = the accepted list minus `channel_order` (derived from the tree) and `channel_flag_password` (follows from setting a password).

### Server properties

`serveredit` accepted 82 of 109 properties, including dangerous and meaningless ones (`virtualserver_port`, `virtualserver_address`, `virtualserver_machine_id`, `virtualserver_status`, `virtualserver_autostart`, `virtualserver_password`, `virtualserver_sfu_endpoint`, and all `connection_*` traffic counters). Rejected: IDs, version/platform, uptime, created, online counts, totals, `virtualserver_flag_password`, `virtualserver_ask_for_privilegekey`.

**Restore allowlist** (curated):

- Identity & messages: `virtualserver_name`, `virtualserver_name_phonetic`, `virtualserver_nickname`, `virtualserver_welcomemessage`, `virtualserver_hostmessage`, `virtualserver_hostmessage_mode`
- Branding: `virtualserver_hostbanner_url`, `virtualserver_hostbanner_gfx_url`, `virtualserver_hostbanner_gfx_interval`, `virtualserver_hostbanner_mode`, `virtualserver_hostbutton_tooltip`, `virtualserver_hostbutton_url`, `virtualserver_hostbutton_gfx_url`, `virtualserver_icon_id`
- Behaviour: `virtualserver_codec_encryption_mode`, `virtualserver_channel_temp_delete_delay_default`, `virtualserver_priority_speaker_dimm_modificator`, `virtualserver_min_clients_in_channel_before_forced_silence`, `virtualserver_needed_identity_security_level`, `virtualserver_min_client_version`, `virtualserver_min_android_version`, `virtualserver_min_ios_version`, `virtualserver_mytsid_connect_only`, `virtualserver_weblist_enabled`, `virtualserver_reserved_slots`
- Anti-flood & complaints: `virtualserver_antiflood_points_tick_reduce`, `virtualserver_antiflood_points_needed_command_block`, `virtualserver_antiflood_points_needed_ip_block`, `virtualserver_antiflood_points_needed_plugin_block`, `virtualserver_complain_autoban_count`, `virtualserver_complain_autoban_time`, `virtualserver_complain_remove_time`
- Logging: `virtualserver_log_client`, `virtualserver_log_query`, `virtualserver_log_channel`, `virtualserver_log_permissions`, `virtualserver_log_server`, `virtualserver_log_filetransfer`
- Default group pointers: written in step 6 with mapped IDs, never copied verbatim.

Never written: port, address, machine ID, status, autostart, password, SFU endpoint, `virtualserver_maxclients` (depends on the target's license), bandwidth limits and quotas (instance-specific), `connection_*` counters.

### Flood protection (non-allowlisted client)

- **Connection rate:** opening ~5 SSH sessions within a few seconds → `ssh: disconnect, reason 11: please wait 1 seconds`, after which every handshake fails with EOF. Nothing in `banlist`, nothing in the server log.
  The IP stayed blocked for ~10 minutes (unblocked after 620 s of polling).
- **Instance limits** (`instanceinfo`, defaults): `serverinstance_serverquery_flood_commands=10` per `serverinstance_serverquery_flood_time=3` s, `serverinstance_serverquery_ban_time=600` s, `serverinstance_serverquery_max_connections_per_ip=5`. Session setup counts towards the budget (`use`, and apparently the login).
- **Command rate:** in a fresh session right after another connection, only 3 commands went through; command 4 and 5 got `client is flooding` (524) with `extra_msg: please wait 1 seconds`, command 6 got `please wait 600 seconds`. **Firing again while throttled escalates to the 10-minute ban.**

