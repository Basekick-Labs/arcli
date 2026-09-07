# arcli

CLI for [Arc](https://github.com/Basekick-Labs/arc) — operator-facing client for Arc time-series databases.

> **Status:** v0.6.0-dev (PR6). Manages connection profiles, runs SQL queries, writes line protocol, administers databases, measurements, and API tokens, bulk-imports CSV / LP / Parquet / TLE files, checks connectivity with `ping`, and inspects clusters and compaction. `retention` / `cq` ship in follow-up PRs.

## Why

Today operating Arc means hand-crafting `curl` calls: copying the bootstrap token from a stderr banner, building JSON query bodies, remembering header names like `x-arc-database`, and decoding `{"columns":[...],"data":[...]}` responses by eye. `arcli` replaces that with a familiar CLI workflow modeled on `influx`, `kubectl`, and `clickhouse-client`.

## Install

Pre-built binaries + Docker images + Homebrew formula land in v1.0.

For now, build from source:

```bash
git clone https://github.com/Basekick-Labs/arcli
cd arcli
go build -o arcli ./cmd/arcli
./arcli --version
```

Requires Go 1.25+.

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
- **PR7** — `arcli retention {...}`, `arcli cq {...}` (full CRUD + execute + executions)
- **PR8** — `arcli delete` (predicate delete), `arcli backup {...}`, `arcli restore`
- **PR9** — `arcli write --format msgpack`, `arcli import stats`, `arcli query --estimate`, `arcli logs`
- **PR10** — release workflow + Homebrew tap + multi-arch Docker + shell completion, cut v1.0.0
- **Post-1.0** — Arc Enterprise surface (`queries`, `governance`, `rbac`, `audit`, `tiering`, `spoke`, `mqtt`), `debug` commands, interactive shell

Target: arcli 1.x speaks to Arc 26.06+.

## Development

```bash
go test -race ./...
go vet ./...
gofmt -l .
```

CI runs all three on every PR.

## License

Apache-2.0. See [LICENSE](LICENSE).
