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

### Operate Arc

```bash
arcli db list
arcli measurement list --database metrics
arcli auth whoami
arcli compaction status
arcli retention list
arcli backup create --wait
arcli logs --level warn --since 6h
```

The command tree also covers API tokens, continuous queries, schedulers, predicate deletes, backups, cluster membership, and compaction. Run `arcli --help` or `arcli <command> --help` for the complete command reference.

## Output and automation

Commands support the formats appropriate to their API:

- `table` for interactive use
- `json` for scripts
- `csv` for tabular exports
- `arrow` for query streams

Destructive commands prompt before making changes; pass `--yes` in non-interactive scripts. New token secrets are written to stdout exactly once, so they can be captured without mixing them with status messages.

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
