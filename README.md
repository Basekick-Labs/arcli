# arcli

The command-line client for [Arc](https://github.com/Basekick-Labs/arc) time-series databases.

`arcli` gives operators one interface for querying, writing, importing, and administering Arc. It supports named connections, human-readable and machine-readable output, shell completion, and safe prompts for destructive operations.

## Install

The latest release is [v26.09.4](https://github.com/Basekick-Labs/arcli/releases/tag/v26.09.4).

Release tags and archives use `26.09.4`; Linux package filenames use the normalized version `26.9.4`.

### Homebrew

```bash
brew install basekick-labs/tap/arcli
```

### Debian / Ubuntu

```bash
curl -LO https://github.com/Basekick-Labs/arcli/releases/download/v26.09.4/arcli_26.9.4_amd64.deb
sudo dpkg -i arcli_26.9.4_amd64.deb
```

Use `_arm64.deb` on ARM64 systems.

### RHEL / Fedora / Rocky

```bash
sudo rpm --import https://raw.githubusercontent.com/Basekick-Labs/arcli/main/packaging/RPM-GPG-KEY-basekick
curl -LO https://github.com/Basekick-Labs/arcli/releases/download/v26.09.4/arcli-26.9.4-1.x86_64.rpm
sudo dnf install -y ./arcli-26.9.4-1.x86_64.rpm
```

Use `.aarch64.rpm` on ARM64 systems.

### Arch Linux

```bash
curl -LO https://github.com/Basekick-Labs/arcli/releases/download/v26.09.4/arcli-26.9.4-1-x86_64.pkg.tar.zst
sudo pacman -U arcli-26.9.4-1-x86_64.pkg.tar.zst
```

Use `-aarch64.pkg.tar.zst` on ARM64 systems.

### Docker

```bash
docker run --rm ghcr.io/basekick-labs/arcli:26.09.4 --version
```

Archives for Linux, macOS, and Windows are available on the [releases page](https://github.com/Basekick-Labs/arcli/releases). Release archives include shell completions and man pages; the Homebrew formula and Linux packages install them automatically.

To build the current `main` branch with Go 1.25 or later:

```bash
go install github.com/basekick-labs/arcli/cmd/arcli@latest
```

## Quickstart

Create a connection using the bootstrap token printed by Arc:

```bash
arcli config create \
  --name local \
  --endpoint http://localhost:8000 \
  --token <token>

arcli ping
arcli query --database metrics "SELECT * FROM cpu LIMIT 10"
```

Omit `--token` when authentication is disabled on the Arc server. The first connection becomes active automatically.

## Common workflows

### Manage connections

```bash
arcli config list
arcli config current
arcli config set-active production
arcli config update production --token-stdin
```

Connection profiles live in `~/.arcli/config.toml` with file mode `0600`. Profile names are case-sensitive and may contain dots. For CI and one-off commands, use `ARC_CONNECTION`, `ARC_ENDPOINT`, and `ARC_TOKEN`, or pass `--connection` / `--endpoint` directly.

### Query and write

```bash
# Table output by default; JSON, CSV, and Arrow are also available.
arcli query --database metrics "SELECT host, avg(value) FROM cpu GROUP BY host"
arcli query -f report.sql -o csv > report.csv

# Line protocol from stdin or a file.
echo "cpu,host=server-1 value=42.5" | arcli write --database metrics
arcli write --database metrics -f metrics.lp

# Validated JSON or MessagePack.
arcli write --database metrics --format json -f metrics.json
arcli write --database metrics --format msgpack -f metrics.msgpack
```

### Import data

```bash
arcli import csv -f data.csv --database metrics --measurement cpu
arcli import lp -f data.lp.gz --database metrics
arcli import parquet -f data.parquet --database metrics --measurement cpu
arcli import tle -f satellites.tle --database satellites
```

### Load a sample dataset

```bash
arcli sample list
arcli sample show citibike
arcli sample load citibike
```

`sample load` downloads and verifies a public dataset, creates the target database when needed, imports the files, and prints useful starter queries. Downloads resume when verified files are already present.

If a download fails, queued downloads stop and arcli reports the original failure without importing any files. Already verified files remain available for a later attempt.

### Operate Arc

```bash
arcli db list
arcli measurement list --database metrics
arcli auth whoami
arcli rbac organization list
arcli license status
arcli license status -c prod -o json
arcli cluster files --database metrics
arcli compaction status
arcli governance policy list
arcli governance policy list -o csv
arcli governance usage batch
arcli retention list
arcli mqtt health
arcli mqtt subscription list
arcli tiering status
arcli tiering file --tier cold --limit 100 --output csv
arcli tiering policy list
arcli backup create --wait
arcli logs --level warn --since 6h
arcli admin query active
arcli admin query history --limit 200
arcli admin query show <query-id>
arcli admin query cancel <query-id>
arcli scheduler cq reload
arcli scheduler retention trigger
arcli audit log --since 2026-01-01T02:30:00.123456789+02:30 --limit 100
arcli audit log -o csv
arcli audit stats --since 2026-01-01T02:30:00.123456789+02:30
```

Query governance commands require an Enterprise licence with the feature enabled, and an admin token when authentication is enabled. Policies and counters are per-node and are not replicated: configure each node separately. Behind a load balancer, aggregate quotas can be N times the configured limit across N nodes.

Policy limits accept `0` for unlimited. On create, omitted limits are also unlimited and override `governance.default_*` values; pass at least one limit flag or explicitly use `arcli governance policy create batch --unlimited`. Updates preserve fields whose flags are omitted, but use read-then-write without version checks, so concurrent updates can lose changes. Use numeric token IDs to show or delete policies left behind by deleted tokens. Usage marks a policy with ID 0 as server defaults, untracked quota counters as `n/a`, and unrestricted rate limits as `unlimited`. JSON keeps the raw server counters and policy ID. Governance timestamps use UTC RFC3339 by default in table, CSV and text output; `--time-output rfc3339nano` shows fractional seconds. JSON retains available timestamp precision. Unavailable routes preserve the server HTTP error; a 404 alone does not identify configuration or license causes.

Query management requires an admin token when authentication is enabled and an Arc Enterprise license with the `query_management` feature enabled. Arc registers these routes only when `query_management.enabled` is true (default false) and the license includes the feature; a missing route reports the server's HTTP 404 error followed by a hint to check `query_management.enabled`, the license, the server log, and the Arc version. Cancellation prompts for confirmation; pass `--yes` for scripted use.

Query management is per node: target the node that runs the query. Requests are not forwarded, and history is lost on node restart. History is bounded by `query_management.history_size` (100 by default), even with a larger `--limit`. Active lists are sorted oldest first. List tables show SQL on one line (line breaks and tabs become spaces) shortened to 120 characters; `show` keeps the full SQL, and CSV/JSON keep the SQL as Arc returns it (Arc cuts active-query SQL to 200 bytes; history and `show` carry it whole). Tables and CSV include `forwarded_to`; Arc 26.09.4 does not report it, so it stays empty there. The cancellation prompt names the target endpoint.

Query-management table and CSV timestamps are UTC RFC3339 by default; `--time-output rfc3339nano` shows fractional seconds. JSON preserves the original response; CSV retains original SQL text, including line breaks.

MQTT commands inspect subsystem health and aggregate statistics, then list,
show, create, update, delete, and control individual subscriptions. List output
supports table, JSON, and CSV; object output supports table and JSON. Supply
create and update configuration as JSON with `--config-file` or `--config-stdin`;
all MQTT endpoints require an admin token. Keep files containing broker
credentials private (`chmod 600`); arcli does not accept broker credentials as
command-line flags and omits usernames and broker URL userinfo from subscription
output. Delete, stop, pause, and restart prompt for confirmation; pass `--yes`
for non-interactive use.

Subscriptions are per node and are not replicated. Pause is effectively stop;
a stopped subscription with `auto_start: true` starts again when Arc restarts.
Stop a running subscription before updating (409 otherwise). In update input,
`"password": ""` clears the password, `"topic_mapping": {}` clears mappings,
and `"topic_mapping": null` leaves mappings unchanged. Arc rejects `"topics": []`
(at least one topic is required).
Mapping rules can be strings or objects (measurement/tags, supported by Arc 27.01
and main). Input accepts writable fields only: `show -o json` includes read-only
fields such as `id` and cannot be piped directly into update.
Broker userinfo and sensitive query parameters (any name containing `token`,
`pass`, `pwd`, `secret`, `key`, `sig`, `auth`, `cred`, `jwt` or `session`) are
redacted and count as credentials for file permissions. When Arc cannot parse a
credential-bearing broker URL, arcli withholds the parse detail, which can quote
pieces of the password. On Windows use `--config-stdin`
for credentials because Unix permission bits cannot verify Windows ACLs.
Health exits nonzero when degraded/unhealthy (a disabled MQTT subsystem, which
Arc reports with HTTP 200, exits 0); create exits nonzero if the returned
subscription status is `error`, after displaying the created subscription.

`backup restore` is destructive and leaves metadata restoration to Arc's defaults: enabled on standalone nodes when present in the backup, skipped on cluster nodes. Use `--with-metadata` to request it explicitly (clusters refuse it), `--data-only` to explicitly skip metadata and config, or `--with-config` to also request config restoration. Stop writers and compaction before restoring. When the server reports staged metadata or a required restart, wait for the restore to complete before restarting.

Use `arcli backup restore --mode merge|replace` with Arc 27.01.1+ (currently unreleased) to choose how cluster data is restored. If omitted, Arc uses merge for full backups and may use replace for scoped backups on a cluster. `backup list` reports cluster-manifest gaps, while `backup show` includes samples and distinguishes missing data from files excluded or reconciled during the backup.

Cluster restore modes and restored-file manifest registration require Arc 27.01.1+ (currently unreleased), which also pauses compaction cluster-wide. On older clusters (26.06–26.09), stop compaction yourself and restored files are not registered; standalone servers also require stopped compaction. Older servers ignore `--mode` and merge, so an unechoed `--mode merge` is accepted; when Arc does not echo `--mode replace`, arcli warns and exits nonzero. The accepted restore may still run as a merge, so inspect `backup status` and verify data before retrying. A backup with known data gaps is refused for replace before the prompt. `backup list` subtracts reconciled skips and appends `skipped_reconciled` to CSV.

RBAC table and CSV timestamps are UTC RFC3339 by default; `--time-output rfc3339nano` shows fractional seconds. JSON retains available timestamp precision.

Tiering commands require an administrator token and an Enterprise licence with tiering enabled. File listings are limited to 100 entries by default; Arc 26.09 and earlier ignore --tier with --database, so arcli rejects that combination for compatibility. There is no offset or cursor. `--limit 0` returns an empty page. In `tiering file -o json`, `count` is the number of files returned (at most `--limit`), not the total number of indexed files. Policy deletion and every form of `tiering migrate`, including `--dry-run`, prompt for confirmation unless `--yes` is passed; without `--yes` they refuse a non-interactive stdin. Arc 26.09 and earlier ignore `tiering migrate --dry-run`, `--database` and `--measurement` and run a real, full, unfiltered migration cycle over every eligible file, so the prompt for those forms says so. arcli cannot detect the server version beforehand; when the response shows that a full cycle ran instead, it prints a warning on stderr and exits non-zero.

Tiering status, files, statistics and scans are per node. Run migrations against the primary writer; reader conflicts retain the server's retry guidance. A timeout or interrupt stops the CLI waiting but migration or scanning continues server-side; inspect status before retrying. Failed scans print available partial results and still exit nonzero.

`tiering policy set` is a full replacement and requires at least one policy flag. Omitted `--hot-only` resets to false (allowing cold migration); omitted `--hot-max-age-days` resets to the global default. Pass both flags to preserve an existing setting while changing the other. `policy show` reads only a custom policy; use `policy effective` to see inherited defaults.

`cluster files` returns one bounded manifest page at a time (1,000 files by default; `--limit` accepts 1–10,000). Use `--cursor` with the previous page's `next_cursor` to continue. The command requires an Arc server that supports pagination and errors if `next_cursor` is absent. JSON includes the page and its continuation cursor; table output prints the cursor below the rows, while CSV keeps stdout machine-readable and prints the page count and cursor to stderr.

Cluster file table and CSV timestamps are UTC RFC3339 by default; `--time-output rfc3339nano` shows fractional seconds. Use `--no-header` to suppress column headers in table or CSV output. CSV writes sizes in bytes and string fields verbatim (no control-character cleaning, unlike the table); JSON preserves the original response, including pagination.

The command tree also covers API tokens, continuous queries, schedulers, predicate deletes, backups, cluster membership, compaction, and tiered storage. Run `arcli --help` or `arcli <command> --help` for the complete command reference.

`scheduler cq reload` refreshes all active continuous queries in the scheduler; use `cq execute` to run one query immediately. `scheduler retention trigger` runs every active retention policy and may permanently delete old data, so it prompts for confirmation; pass `--yes` for scripted use. Use `retention execute <id|name>` to run one selected policy. Scheduler errors preserve Arc's status and message; the codes depend on the Arc version. Arc 26.09 and earlier answer 503 `... is not running - enterprise license required` whenever the scheduler is not running, and 500 when a node other than the primary writer is asked to trigger retention, a run is already in progress, or a policy fails. On those versions, if the license lapses after startup, the trigger answers 200 without running anything, so a 200 does not prove a run happened. Newer Arc answers 403 when the license is missing, lacks the feature or was revoked; 503 when the scheduler is disabled or failed to start; 409 for the role or already-running cases; and 500 when a policy fails. Arc runs every policy even when one fails, so an error does not mean nothing was deleted. The retention trigger runs every policy before Arc answers: if arcli stops waiting, the run may still be going server-side (`retention executions <id|name>` shows each policy's result). Each node keeps its own CQ jobs and only the primary writer runs them, so reload the primary writer (and any node that may take over).

`audit log` and `audit stats` query durable Enterprise audit records. They require an Enterprise license with audit logging enabled, and an administrator token when Arc authentication is enabled. Each node keeps its own audit records, so on a cluster a command shows only the records of the node it reaches. Logs support event type, actor, database, time, and pagination filters in table, JSON, or CSV output; stats defaults to the last 24 hours. This is separate from `arcli logs`, which reads the server's in-memory application log buffer. Table and CSV timestamps, including the displayed `audit stats` since value, use UTC RFC3339 by default; `--time-output rfc3339nano` shows fractional seconds. JSON preserves the original server response. Unavailable routes retain the HTTP error without inferring `audit_log.enabled=false`.

`license status` reads public `/health` without sending a token or checking authentication. It works on standalone and clustered deployments and supports table and JSON output. It reports the server's tier, status, source, and optional expiry, days remaining, and site-license state. Expiry is shown in UTC RFC3339; the table uses whole seconds unless `--time-output rfc3339nano` is given, and JSON keeps the fractional seconds.

Active, expired, invalid, and configured-but-unlicensed states retain the server's status. A missing or null license block is normal for OSS and is distinct from `unlicensed`: JSON returns `{"configured": false, "license": null}`. Present licenses use `{"configured": true, "license": {...}}`; optional fields are omitted when absent or null, while explicit zero days and `false` site-license values are retained. Unknown server status values are preserved. Successfully reading any of these states exits 0; configuration, request, decoding, and output failures exit nonzero. Installing licenses or configuring license keys/files belongs to the Arc server.

`db list`, `db show` and `measurement list` read Arc's database listing endpoints, which need a token carrying read permission. On a server that restricts reads per database, a token scoped to particular databases cannot list them all — Arc refuses rather than returning a filtered list, matching `SHOW DATABASES` — so `db list` reports that the token is scoped and asks you to name a database instead. Pass it to `arcli db show <name>` or `arcli measurement list --database <name>`.

MQTT CSV output preserves non-secret values, including whitespace and control characters, separately from terminal sanitization. Subscription credentials remain redacted in every output format. Table and text timestamps use UTC RFC3339 by default; `--time-output rfc3339nano` shows fractional seconds, and JSON retains fractional-second precision. Route-missing and server-reported MQTT-disabled 503 errors retain their HTTP status and message, with credentials scrubbed. Unknown subscription IDs return 404 on Arc main; on Arc 26.09.4, show returns 404, start may return 500, and stop may return 409.

`arcli rbac` manages Enterprise organizations, teams, roles, measurement permissions, and token team memberships. List commands support table, JSON, and CSV output; destructive operations prompt for confirmation and accept `--yes` for scripts. Token references accept a numeric id or an exact name. Arc registers the RBAC routes only when `auth.enabled` is true; a missing route reports the server's HTTP 404 error followed by a hint to check `auth.enabled`, the license, the server log, and the Arc version.

RBAC access safety:

- Measurement permissions restrict a role to the permitted measurement patterns. Deleting a role's last measurement permission restores its access to every measurement matching its database pattern; `rbac measurement delete` always warns about this, because Arc has no route that maps a permission id to its role.
- A token with no teams falls back to its coarse token permissions, which can widen access to every database. `rbac token team remove` warns when the team is the token's last one and the token has coarse permissions other than `admin` (an admin token bypasses RBAC either way). `rbac team delete` and `rbac organization delete` always warn, because Arc has no route that lists a team's tokens. Warnings are printed with `--yes` too.
- Arc refuses an organization or team delete with HTTP 409 when the cascade exceeds `cluster.rbac.max_cascade_descendants` (default 50000).
- An organization's `--enabled=false` does not revoke access (Arc 26.09.4 and current main); disable the team instead. Adding an admin token to a team does not restrict it.
- A role with `--permission admin` grants read, write and delete on the databases its pattern matches (every database for `*`); it does not grant cluster or token administration. `--permission` accepts repeated or comma-separated values (`--permission read,write`).

Tiering file and policy CSV output preserves original values with lowercase snake_case headers. Table and CSV timestamps use UTC RFC3339 by default; `--time-output rfc3339nano` shows fractional seconds. Unset timestamps are empty in CSV. JSON retains the original response, including null arrays, except that backend credentials in diagnostics are redacted. A missing route preserves the HTTP status and server message and does not imply that tiering is disabled.

## Output and automation

Commands support the formats appropriate to their API:

- `table` for interactive use
- `json` for scripts
- `csv` for tabular exports
- `arrow` for query streams

Administrative commands that format timestamps (retention, continuous queries, backups, cluster, logs, schedulers, tokens, compaction, license) print them in table, CSV and text as UTC RFC3339 with whole seconds, for example `2026-09-07T19:00:00Z`. Pass `--time-output rfc3339nano` to keep the sub-second precision the server reports, for example `2026-09-07T19:00:00.123456789Z`. Trailing zeros are trimmed, so these values vary in width and do not sort lexically; use the default when a script sorts or compares them as strings. The flag does not affect `-o json`, which always carries the server's full precision, and it does not apply to timestamps arcli passes through as the server sent them (`db`, `import`, `sample show`, `arcli query` results). Values are case-insensitive; an invalid value fails before any request is sent.

When Arc reports a truncated query result, arcli writes the received data to stdout, reports the failure on stderr, and exits with status 1. Check the exit status before treating an export as complete. JSON output preserves `truncated` and `truncation_reason`; Arrow output stays raw IPC, with completeness signals read from HTTP trailers after the stream ends. A truncated Arrow payload may not parse.

Governance row caps produce a notice on stderr and keep exit status 0: the result is complete up to the server's policy limit. JSON preserves `rows_capped` and `row_cap`. Client-side `--limit` only limits table/CSV display and does not suppress server truncation errors. Older servers without completeness metadata remain supported; missing signals do not guarantee completeness.

Destructive commands prompt before making changes. Declining confirmation or using non-terminal stdin without `--yes` exits 1 without making changes; pass `--yes` in non-interactive scripts. This includes `db drop` and `config delete` starting in 26.09.2. New token secrets are written to stdout exactly once, so they can be captured without mixing them with status messages.

## Privacy

Standard arcli commands connect only to the Arc endpoints you configure.

Requests identify the arcli version and OS/architecture. Once a config file exists, they also include a random installation ID stored in that file. Arc may include this ID in its opt-out telemetry so Basekick can count CLI installations across Arc servers. Disable the ID with either:

```bash
DO_NOT_TRACK=1 arcli <command>
```

or:

```toml
send_installation_id = false
```

The `arcli sample` commands fetch public datasets from `samples.basekick.net`. These requests include the same client information, but do not include your Arc token, endpoint, queries, or database contents.

## Security

- TLS certificates are verified by default for HTTPS endpoints.
- `--token-stdin` keeps tokens out of shell history.
- `--insecure` is available for development servers with self-signed certificates and prints a warning when used with HTTPS.
- Download checksums, signatures, and SPDX SBOMs are published with each release.

## Compatibility and versioning

arcli follows Arc's CalVer format: `YY.0M.PATCH`. arcli 26.x supports Arc 26.06 and later.

Because Go module major versions only support `v0` and `v1` without a versioned module path, CalVer release tags cannot be passed to `go install`. Use Homebrew, a package, an archive, or Docker for a pinned release; `go install ...@latest` builds the current `main` branch.

## Development

```bash
go test -race ./...
go vet ./...
gofmt -l .
```

## License

[Apache-2.0](LICENSE)
