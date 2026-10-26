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
curl -LO https://github.com/Basekick-Labs/arcli/releases/download/v26.09.4/arcli-26.9.4-1.x86_64.rpm
sudo rpm -i arcli-26.9.4-1.x86_64.rpm
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

Connection profiles live in `~/.arcli/config.toml` with file mode `0600`. For CI and one-off commands, use `ARC_CONNECTION`, `ARC_ENDPOINT`, and `ARC_TOKEN`, or pass `--connection` / `--endpoint` directly.

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
arcli license status
arcli license status -c prod -o json
arcli compaction status
arcli retention list
arcli backup create --wait
arcli logs --level warn --since 6h
```

`backup restore` is destructive and leaves metadata restoration to Arc's defaults: enabled on standalone nodes when present in the backup, skipped on cluster nodes. Use `--with-metadata` to request it explicitly (clusters refuse it), `--data-only` to explicitly skip metadata and config, or `--with-config` to also request config restoration. Stop writers and compaction before restoring. When the server reports staged metadata or a required restart, wait for the restore to complete before restarting.

The command tree also covers API tokens, continuous queries, schedulers, predicate deletes, backups, cluster membership, and compaction. Run `arcli --help` or `arcli <command> --help` for the complete command reference.

`license status` reads public `/health` without sending a token or checking authentication. It works on standalone and clustered deployments and supports table and JSON output. It reports the server's tier, status, source, and optional expiry, days remaining, and site-license state. Expiry is normalized to UTC RFC3339 with fractional seconds when provided.

Active, expired, invalid, and configured-but-unlicensed states retain the server's status. A missing or null license block is normal for OSS and is distinct from `unlicensed`: JSON returns `{"configured": false, "license": null}`. Present licenses use `{"configured": true, "license": {...}}`; optional fields are omitted when absent or null, while explicit zero days and `false` site-license values are retained. Unknown server status values are preserved. Successfully reading any of these states exits 0; configuration, request, decoding, and output failures exit nonzero. Installing licenses or configuring license keys/files belongs to the Arc server.

`db list`, `db show` and `measurement list` read Arc's database listing endpoints, which need a token carrying read permission. On a server that restricts reads per database, a token scoped to particular databases cannot list them all — Arc refuses rather than returning a filtered list, matching `SHOW DATABASES` — so `db list` reports that the token is scoped and asks you to name a database instead. Pass it to `arcli db show <name>` or `arcli measurement list --database <name>`.

## Output and automation

Commands support the formats appropriate to their API:

- `table` for interactive use
- `json` for scripts
- `csv` for tabular exports
- `arrow` for query streams

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
