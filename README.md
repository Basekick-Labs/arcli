# arcli

CLI for [Arc](https://github.com/Basekick-Labs/arc) — operator-facing client for Arc time-series databases.

> **Status:** 26.09.1. Manages connection profiles, runs SQL queries, writes line protocol / MessagePack / JSON, administers databases, measurements, API tokens, retention policies and continuous queries, bulk-imports CSV / LP / Parquet / TLE files, deletes rows by predicate, takes and restores backups, checks connectivity with `ping`, reads server logs, and inspects clusters, compaction and schedulers.

## Why

Today operating Arc means hand-crafting `curl` calls: copying the bootstrap token from a stderr banner, building JSON query bodies, remembering header names like `x-arc-database`, and decoding `{"columns":[...],"data":[...]}` responses by eye. `arcli` replaces that with a familiar CLI workflow modeled on `influx`, `kubectl`, and `clickhouse-client`.

## Install

Every release ships archives for Linux / macOS / Windows (amd64 + arm64), deb / rpm / Arch packages, a Homebrew formula, and a multi-arch image on GHCR. Packages and the formula install shell completions and man pages too.

```bash
# Homebrew (macOS + Linux)
brew install basekick-labs/tap/arcli

# Debian / Ubuntu
curl -LO https://github.com/Basekick-Labs/arcli/releases/download/v26.09.1/arcli_26.9.1_amd64.deb   # or _arm64
sudo dpkg -i arcli_26.9.1_amd64.deb

# RHEL / Fedora / Rocky
sudo rpm -i https://github.com/Basekick-Labs/arcli/releases/download/v26.09.1/arcli-26.9.1-1.x86_64.rpm   # or .aarch64

# Arch
sudo pacman -U arcli-26.9.1-1-x86_64.pkg.tar.zst   # or -aarch64

# Docker (distroless, non-root; config via env or a mounted ~/.arcli)
docker run --rm -e ARC_ENDPOINT=http://arc:8000 -e ARC_TOKEN=... ghcr.io/basekick-labs/arcli ping

# Archive
tar xzf arcli_26.09.1_linux_amd64.tar.gz && sudo install arcli /usr/local/bin/
```

Image tags: `26.09.1` (immutable), `26.9`, `26`, `latest` (move on final releases only). Git tags are zero-padded like Arc (`v26.09.1`); archives, image tags and `arcli --version` carry `26.09.1` verbatim, while the deb / rpm / Arch packagers normalise their version field to `26.9.1`.

Verify a download: `checksums.txt` covers every archive, package and SBOM and is signed keylessly with cosign from the release workflow (the packages themselves carry no GPG signature; the container image is not signed yet):

```bash
sha256sum -c --ignore-missing checksums.txt
cosign verify-blob --bundle checksums.txt.sigstore.json \
  --certificate-identity-regexp '^https://github.com/Basekick-Labs/arcli/\.github/workflows/release\.yml@refs/tags/v' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com checksums.txt
```

Each archive also carries a SPDX SBOM (`*.sbom.json`).

From source (Go 1.25+): `go install github.com/basekick-labs/arcli/cmd/arcli@latest` builds the current `main` (see [Roadmap](#roadmap) for why tagged installs are not possible), or clone and `go build -o arcli ./cmd/arcli`.

## Quickstart

```bash
# 1. Create a connection profile from your Arc instance's bootstrap token
arcli config create \
  --name local \
  --endpoint http://localhost:8000 \
  --token <token-from-arc-stderr-banner>

# 2. Confirm it's active (first connection auto-activates)
arcli config current

# 3. List all profiles
arcli config list
```

## Connection management

`arcli` stores connection profiles in `~/.arcli/config.toml` (mode 0600). One profile is marked active; commands use it by default.

```bash
# Add multiple environments
arcli config create --name prod    --endpoint https://arc.prod.example.com --token PROD-TOK
arcli config create --name staging --endpoint https://arc.staging.example.com --token STAGING-TOK --default-database metrics

# Switch active
arcli config set-active staging

# Change a stored field (e.g. after rotating a token)
arcli config update prod --token NEW-TOKEN

# Override per-command (later PRs)
arcli --connection prod query "SELECT count(*) FROM cpu"

# Or via env vars (CI-friendly)
ARC_CONNECTION=prod arcli query "..."
ARC_ENDPOINT=https://... ARC_TOKEN=... arcli query "..."

# Show active (token redacted)
arcli config current

# Remove
arcli config delete staging --yes
```

`arcli config create` and `update` accept `--token-stdin` to read the token from a pipe or file instead of the command line (a terminal is refused, since the token would be echoed into the scrollback):

```bash
pass show arc/prod | arcli config create --name prod --endpoint https://arc.prod.example.com --token-stdin
```

### Precedence

1. `--connection NAME` flag
2. `--endpoint URL --token T` flags (full ad-hoc)
3. `ARC_CONNECTION` env var
4. `ARC_ENDPOINT` + `ARC_TOKEN` env vars (full ad-hoc)
5. Active connection in `~/.arcli/config.toml`

If none are set, commands fail with a clear "no active connection" error.

### Config file location

Honors `ARCLI_CONFIG` env var for test/CI overrides; otherwise `~/.arcli/config.toml`.

## Querying

```bash
# Pretty table (default)
arcli query "SELECT host, value FROM cpu ORDER BY value LIMIT 10"

# Override the database for one call
arcli query --database metrics "SELECT count(*) FROM cpu"

# Read SQL from a file
arcli query -f reports/p99.sql

# Pipe SQL from another command
echo "SELECT 1" | arcli query

# Machine-parseable output
arcli query "SELECT * FROM cpu" -o json | jq '.data[0]'
arcli query "SELECT * FROM cpu" -o csv > out.csv

# Arrow IPC stream — feed it to pyarrow / duckdb / polars
arcli query "SELECT * FROM cpu" -o arrow | duckdb -c "SELECT * FROM read_arrow('/dev/stdin')"

# Size a query before running it: the server runs SELECT COUNT(*) over it (a real scan)
arcli query --estimate "SELECT * FROM cpu WHERE time > now() - INTERVAL 30 DAY" --database metrics
```

The output formats:

- `-o table` (default) — pretty-printed bordered table; honors `--no-header` and `--limit N`
- `-o json` — the raw `{"columns":[...],"data":[...]}` response, jq-friendly
- `-o csv` — RFC 4180 with a header row by default
- `-o arrow` — binary Arrow IPC stream on stdout; server-side execution time goes to stderr

## Writing

```bash
# Stdin pipe (most common in CI / log forwarders)
echo "cpu,host=server-1 value=42.5 $(date +%s)000000000" | arcli write

# From a file
arcli write -f payload.lp --database metrics --precision ms

# Explicit precision (default is nanoseconds, matching the server)
echo "cpu v=1 1700000000" | arcli write --precision s
```

`--precision` accepts `ns`, `us`, `ms`, or `s` (anything else is rejected client-side before the request goes out). The body is streamed end-to-end — `cat huge.lp | arcli write` never buffers the whole payload in memory.

Two more input formats target Arc's MessagePack endpoint:

```bash
arcli write --format msgpack -f cpu.msgpack.zst --database metrics   # a prepared document, passed through as-is (gzip/zstd OK)
arcli write --format json -f cpu.json --database metrics             # same document as JSON, validated and encoded client-side
```

The JSON/msgpack document is Arc's columnar shape `{"m":"cpu","columns":{"time":[...],"host":[...],"usage":[...]}}`, its row shape `{"m":"cpu","t":...,"h":"srv1","fields":{...},"tags":{...}}`, or `{"batch":[...]}`. The row shape keeps tags as tag columns (so compaction can de-duplicate; `h` defaults to `unknown`); the columnar shape is fastest but carries no tag metadata. `--format json` rejects up front what the server would fail on later or drop silently (ragged columns, mixed types, integers beyond int64, a `time` column mixing units, nested values), and is capped at 64 MiB (the conversion holds several copies in memory); `--format msgpack` streams up to the server's 1 GiB limit. Timestamps carry their own unit (inferred from each value's magnitude; a columnar `time` column from its first value), so `--precision` is line-protocol only.

## Database & measurement admin

```bash
# List every database the active token can see
arcli db list

# Inspect one database (info + its measurements)
arcli db show production

# Create an empty database (server validates name: alphanumeric + `_-`,
# max 64 chars, "system" / "internal" / "_internal" are reserved)
arcli db create metrics

# Drop a database and ALL its files. Prompts for y/N; pass --yes to
# skip in scripts. The server requires delete.enabled=true in arc.toml
# AND an admin token — if either is missing the server's error message
# surfaces verbatim ("Set delete.enabled=true in arc.toml to enable.").
arcli db drop old_metrics
arcli db drop --yes ci_scratch          # no prompt

# List measurements inside a database (same data shown by `db show`,
# different default view)
arcli measurement list --database metrics
arcli measurement list -c prod --database logs -o json
```

`db list`, `db show`, and `measurement list` all support `-o table|json|csv` (no `-o arrow` — these endpoints return JSON, not Arrow IPC).

## Bulk import

`arcli import` ships four file-import flows backed by Arc's admin-only `/api/v1/import/*` endpoints. Upload bodies are streamed via `io.Pipe`, so even multi-GB files don't buffer in memory.

```bash
# CSV — file has no measurement, so --measurement is required
arcli import csv -f data.csv --database metrics --measurement cpu
arcli import csv -f data.csv --database metrics --measurement cpu \
    --time-column ts --time-format epoch_ms --delimiter ';' --skip-rows 1

# Line protocol — measurement comes from the LP lines themselves;
# --measurement here is an optional filter. Server auto-detects gzip.
arcli import lp -f telegraf.lp --database metrics
arcli import lp -f data.lp.gz --database metrics --precision ms
arcli import lp -f data.lp --database metrics --measurement cpu  # filter

# Parquet — preserves types end-to-end (faster + lossless vs CSV)
arcli import parquet -f data.parquet --database metrics --measurement cpu

# TLE (NORAD two-line element / satellite tracking)
arcli import tle -f starlink.tle --database satellites
arcli import tle -f starlink.tle --database satellites --measurement starlink
```

All four require an **admin** token (server-side `adminAuth`). Each prints either a pretty result block or `-o json` for scripting; server errors (auth, validation, "file is empty", quota) surface verbatim.

## Auth & tokens

`arcli ping` is the first thing to run after `config create`: it hits `GET /health` (no token sent) and then `GET /api/v1/auth/verify` with the token, and exits non-zero if either fails. `-o json` always prints the report so a script can see which half failed.

```bash
arcli ping
arcli ping -c prod -o json
arcli auth whoami                       # the token this connection uses
```

Everything under `auth token` needs an **admin** token. Tokens are addressed by numeric id or exact name.

```bash
arcli auth token list                   # -o table|json|csv; revoked tokens show ENABLED=false
arcli auth token show grafana
arcli auth token permissions grafana    # effective per-database permissions (RBAC-aware)

# Create: the secret is printed to stdout exactly once; id + reminder go to stderr
T=$(arcli auth token create --name ci --permission read,write --expires-in 30d)
arcli auth token create --name ops --permission admin -o json

arcli auth token update ci --description "nightly" --permission read
arcli auth token rotate ci --yes        # new secret on stdout; old secret dead immediately
arcli auth token revoke ci --yes        # permanent; the row stays listed, disabled
arcli auth token delete ci --yes        # revoke/delete of the last enabled admin token needs --force
```

Permissions are `read`, `write`, `delete`, `admin`. `--expires-in` takes a Go duration (`24h`) or a day count (`7d`); once set, an expiry can be moved but not removed (server limitation).

Rotating, revoking, or deleting the token arcli itself is using prints a loud warning. For rotation, `--save` writes the new secret into every profile in the config file that held the old one. It is refused up front, before anything is rotated, unless the connection came from a named profile, the server confirms the target is the token in use, and the config directory is writable. The secret is always printed before the config file is touched; the updated profile names go to stderr:

```bash
arcli auth token rotate admin --save
```

Interactive confirmations go to stderr and answering anything but `y`/`yes` exits 1. When stdin is not a terminal the prompt is refused outright, so scripts must pass `--yes`.

## Cluster & compaction

`arcli cluster` talks to Arc Enterprise clustering. On a standalone server `cluster status` reports that clustering is disabled (with the server's reason) and exits 0; every other cluster subcommand exits 1 so scripts never mistake "no cluster" for an empty cluster.

```bash
arcli cluster status                    # name, local node, raft leader, node table; -o json is the raw server body
arcli cluster nodes --state unhealthy   # -o table|json|csv; --role writer|reader|compactor|standalone
arcli cluster node show n2
arcli cluster node show --local         # the node this connection talks to, with its capabilities
arcli cluster health
arcli cluster node remove n2 --yes      # admin; must be sent to the raft leader (arcli names the leader's API address if not)
```

`arcli compaction` observes and triggers Arc's background Parquet compaction. `trigger` needs an admin token; the rest work with any token. When `compaction.enabled=false` on the server every subcommand reports that compaction is disabled.

```bash
arcli compaction status                 # manager counters + per-tier schedulers (next run in UTC)
arcli compaction stats                  # lifetime totals, tier settings, recent jobs
arcli compaction candidates --database metrics
arcli compaction history --limit 10     # the server keeps only the 10 most recent jobs; entries carry no timestamp
arcli compaction trigger --tier hourly --database metrics
arcli compaction trigger --wait         # poll until the cycle finishes (--wait-timeout, default 30m)
```

Trigger is asynchronous on the server: the reported cycle id is the one the server expects to assign, and it can be off by one or belong to a scheduled cycle that raced the trigger; `--wait` detects both and stops early instead of sleeping to the timeout. A tier that is disabled or not configured server-side is skipped silently by Arc, so arcli warns before sending. Arc's `/compaction/jobs` endpoint is a stub that always reports zero jobs and is deliberately not exposed.

## Retention policies & continuous queries

A retention policy deletes whole files whose newest row is older than `retention-days + buffer-days` in one database (optionally one measurement). Listing works with any token; changes and execution need admin.

```bash
arcli retention list                                     # -o table|json|csv
arcli retention create --name metrics-90d --database metrics --retention-days 90 --buffer-days 7
arcli retention update metrics-90d --buffer-days 14      # read-merge-write: Arc's PUT is a full replace
arcli retention execute metrics-90d --dry-run            # server reports what it would delete
arcli retention execute metrics-90d                      # dry-run preflight, then a prompt with the server's cutoff and counts
arcli retention executions metrics-90d --limit 20
arcli retention delete metrics-90d --yes
```

`execute` is synchronous on the server and defaults to `--timeout 30m`; if the client times out the server keeps going, so check `executions` afterwards.

A continuous query runs an aggregation over a time window and writes the result into a destination measurement. Every `cq` command needs an admin token. The SQL must contain `{start_time}` and `{end_time}` and read the source as `FROM <database>.<source>` (Arc rewrites only that form); arcli warns otherwise, validates the interval (a Go duration, at least 10s, which Arc itself does not check at create time), and reads `--query-file` by the process, bounded at 64 KiB.

```bash
arcli cq list --database metrics --active
arcli cq create --name cpu-1m --database metrics --source cpu --destination cpu_1m --interval 1m --tag-column host --query-file cpu_1m.sql
arcli cq show cpu-1m
arcli cq update cpu-1m --interval 5m                    # read-merge-write; --clear-tag-columns, --description ""
arcli cq execute cpu-1m --dry-run                        # shows the SQL the server would run
arcli cq execute cpu-1m --start 2026-09-01T00:00:00Z --end 2026-09-02T00:00:00Z   # backfill; warns if --end rewinds the watermark
arcli cq executions cpu-1m
arcli cq delete cpu-1m --yes
```

On Arc OSS neither the retention nor the CQ scheduler runs (Enterprise feature), so nothing fires automatically; `arcli scheduler status` shows both schedulers' state and the reason, and `create` prints a hint when the relevant scheduler is not running. Follow-ups: chunked `cq execute --backfill`, `scheduler` reload/trigger commands (Enterprise).

## Deleting rows & backups

`arcli delete` removes rows matching a SQL predicate by rewriting the affected Parquet files (a file is removed when every row matches). It needs an admin token and `delete.enabled=true` on the server; `arcli db drop` removes a whole database.

```bash
arcli delete --database metrics --measurement cpu --where "host = 'old-01'" --dry-run
arcli delete --database metrics --measurement cpu --where "time < '2025-01-01'"     # server dry-run, then a prompt with its counts
arcli delete --database metrics --measurement cpu --where "1=1" --yes               # every row of the measurement
```

The predicate is sent as `(<where>) IS TRUE`, so rows where it is NULL are kept and the preview matches the real run exactly (Arc's own rewrite would otherwise drop NULL rows it never counted). A predicate that does not evaluate is reported as an error instead of "0 rows". The call is synchronous with a 30-minute default `--timeout`. Composed `--before`/`--after` flags are a follow-up; put time bounds in the predicate for now.

`arcli backup` takes full backups of every database into the server's `backup.local_path` and restores them. Create and restore run in the background on the server and share one slot.

```bash
arcli backup create --wait                 # prints the id; --wait polls to completion (default 2h)
arcli backup list                          # newest first; only `show` can flag an incomplete backup
arcli backup show backup-20260907-201105-a0f5e600
arcli backup status                        # running or most recent operation
arcli backup restore backup-20260907-201105-a0f5e600 --data-only --wait
arcli backup delete backup-20260907-201105-a0f5e600 --yes
```

Restore overwrites existing files at the same paths and is meant for a quiescent server (stop writers and compaction first; in cluster mode restored files are not registered in the cluster manifest). Metadata and config restores are staged and applied at the next server start; arcli says so after every restore that includes them.

## Server logs & counters

```bash
arcli logs --level warn --since 6h --limit 200    # admin; newest first; --level is a minimum
arcli import stats                                # process-wide import counters
```

Arc keeps the last 10 000 log entries in memory per process, so behind a load balancer each call may reach a different node. On ctrl-C (or SIGTERM) arcli cancels the in-flight request and exits 130 (143 for SIGTERM) with a reminder that anything the server already accepted continues there.

## Shell completion

`arcli completion bash|zsh|fish|powershell` prints the script for your shell. Completion knows arcli: `-c <TAB>` lists the connections in your config file (endpoint shown, active one marked), `--output`, `--format`, `--level`, `--precision`, `--permission` and `--time-format` complete to their valid values, `-f` and `--query-file` complete file paths, and everything else stays quiet instead of listing the current directory. Connection names come from the config file only; nothing talks to a server during completion.

```bash
# bash (needs bash-completion 2; macOS users: brew install bash bash-completion@2)
arcli completion bash > /etc/bash_completion.d/arcli          # or ~/.local/share/bash-completion/completions/arcli

# zsh
arcli completion zsh > "${fpath[1]}/_arcli"                    # then: rm -f ~/.zcompdump; compinit

# fish
arcli completion fish > ~/.config/fish/completions/arcli.fish

# powershell
arcli completion powershell | Out-String | Invoke-Expression
```

The Homebrew formula and the deb / rpm / Arch packages install the completions (and man pages: `man arcli`, `man arcli-query`) for you.

## Privacy

arcli never contacts Basekick or any third party; every request goes to the Arc server you configured, and the only thing it stores is `~/.arcli/config.toml`.

Requests carry a `User-Agent` with the arcli version and OS/architecture and, once a config file exists, a random installation id (`installation_id` in the config file, minted by the first `config create`, not derived from your machine or account) in the `Arcli-Installation-Id` header. Arc's own opt-out telemetry may report that id together with its instance id to Basekick once a day (and at shutdown), so Basekick can count how many CLI installations talk to how many Arc servers. Because the id is the same for every server you use, it links the servers one installation talks to. Only requests the server authenticated are counted, so `ping` alone never registers anything. Opt out with `DO_NOT_TRACK=1` or `send_installation_id = false` in the config file; disabling telemetry on the Arc server also stops it. `arcli config current` shows the id and whether it is being sent; delete the key and the next command that writes the config file (`config create|update|set-active|delete`) mints a new one. With no config file at all (env-only use in a container) there is no id to send. Nothing else is stored or sent.

## TLS

For HTTPS endpoints, certificate verification is on by default. To skip verification (lab / self-signed certs only), use either:

- `--insecure` on a single command, or
- `insecure_tls = true` in the connection profile (set once via `arcli config create --insecure`)

When verification is skipped, a `WARNING:` line is printed to stderr. The flag is a no-op on `http://` endpoints and the warning is suppressed.

## Roadmap

This repo is being built in [phased PRs](https://github.com/Basekick-Labs/arcli/pulls):

- ~~**PR1** — scaffold, `config` subcommand tree, multi-connection store~~ ✅ shipped
- ~~**PR2** — `arcli query`, `arcli write`, output formats: table/json/csv/arrow~~ ✅ shipped
- ~~**PR3** — `arcli db {list,show,create,drop}`, `arcli measurement list`~~ ✅ shipped
- ~~**PR4** — `arcli import {csv,lp,parquet,tle}`~~ ✅ shipped
- ~~**PR5** — `arcli auth {whoami,token ...}`, `arcli ping`, `arcli config update`~~ ✅ shipped
- ~~**PR6** — `arcli cluster {status,nodes,node show,node remove,health}`, `arcli compaction {status,stats,candidates,history,trigger}`~~ ✅ shipped
- ~~**PR7** — `arcli retention {...}`, `arcli cq {...}` (full CRUD + execute + executions), `arcli scheduler status`~~ ✅ shipped
- ~~**PR8** — `arcli delete` (predicate delete), `arcli backup {create,list,show,status,delete,restore}`~~ ✅ shipped
- ~~**PR9** — `arcli write --format msgpack|json`, `arcli query --estimate`, `arcli logs`, `arcli import stats`, signal-aware root, `--token-stdin`~~ ✅ shipped
- ~~**PR10a** — shell completion, build metadata in `--version`, distroless image, man pages~~ ✅ shipped
- ~~**PR10b** — GoReleaser: archives, deb / rpm / Arch packages, Homebrew tap, multi-arch GHCR image, SBOMs, cosign; cut the first CalVer tag (`v26.09.1`)~~ ✅ shipped
- **Later** — Arc Enterprise surface (`queries`, `governance`, `rbac`, `audit`, `tiering`, `spoke`, `mqtt`), `debug` commands, interactive shell

Versioning is CalVer like Arc: `YY.0M.PATCH`, tagged `v26.09.1`; only the deb / rpm / Arch package versions are normalised to `26.9.1` by their packagers. arcli 26.x speaks to Arc 26.06+. One consequence: Go only accepts `v0`/`v1` tags for this module path, so `go install …@v26.09.1` is not possible; `go install github.com/basekick-labs/arcli/cmd/arcli@latest` builds the current `main` instead, and the packages, Homebrew and Docker are the release channels.

## Development

```bash
go test -race ./...
go vet ./...
gofmt -l .
go run ./internal/tools/gendocs .gen     # man pages + completion scripts (what the packages ship)
goreleaser release --snapshot --clean --skip=sign,sbom   # every artifact a tag would produce, under dist/
```

CI runs gofmt / vet / tests on every PR plus a GoReleaser snapshot (no Docker / signing / SBOM there). Releasing is `git tag -a v26.09.2 -m "arcli 26.09.2" && git push origin v26.09.2` on a commit reachable from `main`; the workflow refuses tags that are not, and refuses to start without the `HOMEBREW_TAP_TOKEN` secret. If `docs/releases/<tag>.md` exists it becomes the release notes header above the generated changelog. After the first tag, make the GHCR package public in the org package settings (new packages start private; the workflow warns if the image is not anonymously pullable) and confirm the documented `cosign verify-blob` line with a current cosign. `arcli --version` reports the module version, commit and commit date: injected by the release build, or read from Go's embedded build info for a plain `go build` / `go install` (`+dirty` when the tree had uncommitted changes).

The `Dockerfile` packages a prebuilt binary the way GoReleaser stages it (`linux/<arch>/arcli` in the build context, distroless static, non-root); it is not a from-source build, so build images with the snapshot command above.

## License

Apache-2.0. See [LICENSE](LICENSE).
