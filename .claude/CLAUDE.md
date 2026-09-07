# arcli - Claude Code Instructions

## Project Overview

`arcli` is the operator-facing CLI for [Arc](https://github.com/Basekick-Labs/arc) — a high-performance columnar analytical database. arcli is a **client only**: it talks to one or more Arc clusters over the existing HTTP API (`/api/v1/query`, `/api/v1/write`, `/api/v1/import/*`, etc.). No embedded query engine, no Arc server code is shipped here.

The model for connection management is the InfluxDB v2 CLI (`influx config create/list/set-active/...`) — multiple named connections, one marked active, overridable per-command via flags or env vars.

**Tech stack:** Go 1.25+, [cobra](https://github.com/spf13/cobra) (commands), [viper](https://github.com/spf13/viper) (config), [tablewriter](https://github.com/olekukonko/tablewriter) (table output), stdlib `net/http`. Apache-2.0 licensed (client-tool license — Arc server stays AGPL-3.0).

## Architecture

- **Config file:** `~/.arcli/config.toml` (mode 0600), honored override `ARCLI_CONFIG` env var (for tests + CI)
- **Connection precedence (highest first):**
  1. `--connection NAME` / `-c NAME` flag
  2. `--endpoint URL --token T` flags (full ad-hoc)
  3. `ARC_CONNECTION` env var
  4. `ARC_ENDPOINT` + `ARC_TOKEN` env vars (full ad-hoc)
  5. `active` connection in config file
  → No fallback past 5; commands error with a clear "no active connection" message
- **No state on disk besides the config file.** No history file, no cache. `arcli` never phones home: it only ever talks to the Arc endpoints the user configures. The one identity it carries is `installation_id` (random UUID minted by `config.Save()`, stored in the config file), sent as `Arcli-Installation-Id` so Arc's own opt-out telemetry can count CLI installations per instance (the same id goes to every server the user configures, and the README's Privacy section says so). Opt-outs: `send_installation_id = false`, `DO_NOT_TRACK=1`. Never extend this into the CLI reporting to Basekick directly.
- **Targeted server version:** arcli 1.x talks to Arc 26.06+ (pre-26.06 lacks Phase A cluster auth replication, so token admin would behave inconsistently across nodes).

## Build & Test

```bash
go build ./...
go test -race -count=1 ./...
gofmt -l .                  # must return empty
go vet ./...                # must return empty
```

**Before opening a PR** that touches `cmd/arcli/main.go`, command wiring, or anything the user types as a flag: actually run the built binary against a local `arc serve` (or — for `config`-only PRs — against a temp `ARCLI_CONFIG`) and exercise the changed code path end-to-end. Unit tests and reviewer agents have a blind spot for flag wiring, help-text drift, and config-resolution edge cases; the binary running for 30 seconds catches the loudest of them for free. This is the [integration-test-the-binary discipline](../README.md#development) — non-negotiable.

## Conventions

### Code Style
- One command per file in `internal/commands/` (`config.go`, `query.go`, `write.go`, ...)
- Cobra commands use `SilenceUsage: true` on root — usage text on a "missing flag" error is noise; real errors should be terse
- Errors at top level: `fmt.Fprintln(os.Stderr, err)` + `os.Exit(1)` from `main`, NOT inside command functions; commands return `error` and let cobra propagate
- Validate inputs at command-boundary (flag values, file paths read from `-f`); trust internal callers
- Atomic file writes for any persisted state (`os.CreateTemp` in target dir + `os.Rename`) — never leave half-written config on crash
- Plaintext token storage in 0600 file matches `~/.aws/credentials` posture — that's the bar; do not invent encryption
- `RedactToken()` in `internal/config/config.go` is the canonical token-display formatter; reuse it for every place a token surfaces in output

### Git & PRs
- Always create a branch from `main`.
- Branch naming: `feat/description`, `fix/description`
- Commit format: `feat(scope): description` or `fix(scope): description` — scopes mirror the command surface (`config`, `query`, `write`, `import`, `db`, `auth`, `cluster`, `output`, `release`)
- PR descriptions: Summary bullets + Test plan checklist
- Main branch: `main`
- **PR review:** After creating a PR, always request a review from `gemini-code-assist` by adding it as a reviewer (`gh pr edit --add-reviewer gemini-code-assist[bot]`). Wait for Gemini's review comments and address any findings before merging.

### Command Handler Pattern
```go
func newQueryCmd() *cobra.Command {
    var (
        connectionName string
        endpoint       string
        token          string
        database       string
        outputFormat   string
    )
    cmd := &cobra.Command{
        Use:   "query [SQL]",
        Short: "Run a SQL query against an Arc cluster",
        Args:  cobra.ExactArgs(1),
        RunE: func(cmd *cobra.Command, args []string) error {
            cfg, err := config.Load()
            if err != nil {
                return err
            }
            conn, _, err := cfg.Resolve(config.ResolveOptions{
                ConnectionName: connectionName,
                Endpoint:       endpoint,
                Token:          token,
            })
            if err != nil {
                return err
            }
            // ... use conn.Endpoint / conn.Token to call client
            return nil
        },
    }
    cmd.Flags().StringVarP(&connectionName, "connection", "c", "", "named connection (overrides active)")
    cmd.Flags().StringVar(&endpoint, "endpoint", "", "ad-hoc endpoint URL")
    cmd.Flags().StringVar(&token, "token", "", "ad-hoc bearer token")
    cmd.Flags().StringVar(&database, "database", "", "database (defaults to connection's default_database)")
    cmd.Flags().StringVarP(&outputFormat, "output", "o", "table", "table|json|csv|arrow")
    return cmd
}
```

## Planning & Review Process

### Plan Documentation
When exiting plan mode to begin implementation, save the implementation plan first as a markdown file in `docs/progress/` (gitignored) with date + phase in the filename, e.g. `2026-05-31-pr2-query-write.md`.

### Pre-Plan Validation
Before finalizing a plan, use another agent to validate your findings. If there is consensus, ask for authorization from the user to move forward.

### Post-Implementation Review

**Be honest about what internal review is.** It's a sanity pass on the first draft before Gemini sees it. It is NOT a substitute for Gemini. Past Arc data showed that sprawling reviewer prompts pattern-match on past findings while missing actual bugs (PR #444: 3 Gemini rounds + a follow-up PR; PR #445: critical nil-deref the 4-agent review missed). **Reviewers run on the diff, not on the running system** — bake that into expectations.

The fix is a two-step process. Both steps are mandatory; **the configuration matrix is not optional and must come first.**

#### Step 1: Configuration matrix (written by the implementer, not an agent)

Before invoking any reviewer, write down — in the conversation, not a file — a small table. For arcli, the relevant axis is **how the connection was resolved** plus **what's in the config file**:

| Configuration | Reaches new code? | Preconditions established? |
|---|---|---|
| No config file (first run) | ... | ... |
| Config file exists, no active connection | ... | ... |
| Config file with active connection only | ... | ... |
| `--connection NAME` flag (named exists) | ... | ... |
| `--connection NAME` flag (named missing) | ... | ... |
| `--endpoint + --token` flag override (no config file) | ... | ... |
| `ARC_CONNECTION` env (named exists) | ... | ... |
| `ARC_ENDPOINT + ARC_TOKEN` env (no config file) | ... | ... |
| Half-set: `--endpoint` only / `ARC_TOKEN` only | ... | ... |
| Output flag: `-o json` / `-o csv` / `-o arrow` / default | ... | ... |

For each cell:
- "Reaches new code?" — yes / no / partial. Trace from `main()` through cobra dispatch through the actual `if` blocks. Do not handwave; cite line numbers.
- "Preconditions established?" — for every pointer dereference / map lookup / file read the new code performs, what invariant guarantees the precondition? Cite the line that establishes it.

If a row is "yes, but I don't know if precondition X holds in this mode" — that row IS the bug. Find it and fix it before continuing.

**Concrete shapes to enumerate explicitly**, because they bit Arc and will bite us too:
- "user typed `--token` without `--endpoint`" (or vice versa) — the half-set ad-hoc case must error, not silently fall through to the active connection
- "config file missing vs. config file present but empty" — both are valid first-run states; `Load()` must distinguish IO errors from "file does not exist"
- "`Resolve()` returns an unnamed Connection (ad-hoc)" — code that prints `"using connection: %s"` must handle the `(flags)` / `(env)` sentinel display name, not assume a real name
- "output format is `arrow` but the server doesn't support arrow" — graceful fallback, not a panic on nil response body
- "`ARCLI_CONFIG` points at a path the user can't write" — `Save()` must error cleanly, not partially-write
- "Token contains characters that need escaping in shell history" — never log tokens; redact via `RedactToken` for any human-facing output
- "TLS verification: `--insecure` flag set vs. `insecure_tls = true` in config vs. neither" — three independent paths, must converge to one decision before client construction

#### Step 2: Single deep reviewer (one agent, not four)

Spawn **one** general-purpose agent. The prompt MUST include:

1. **The configuration matrix from Step 1**, verbatim. The reviewer is told to confirm each row by tracing the code, and to flag any row that's wrong.
2. **The diff to review** (paste or reference `git diff main..HEAD`).
3. **Five specific things to check, in order**:
   - **(a) Precondition trace** — for every new map lookup, pointer deref, type assertion, or file read, what guarantees safety under the configurations in the matrix? Flag any operation that doesn't have a cited establishment line. Highest-yield check; do it FIRST.
   - **(b) First-run smoke test** — does a brand-new user (no `~/.arcli/config.toml`, no env vars) get a useful error from every command, or does something panic / produce an empty success?
   - **(c) Failure modes from the matrix** — for each "yes" row, what does a partial failure look like? If `Resolve()` succeeds but the HTTP call fails, what does the user see? If `Save()` partially writes, what's left on disk?
   - **(d) Doc-vs-code drift** — README claims, help text on cobra commands (`Short`/`Long`), comments above exported helpers. Does every prose claim match the code that just shipped? Did flag names/aliases change without the README being updated?
   - **(e) Hot-path nits** — token leakage in error messages, `fmt.Sprintf` where `strings.Builder` matters in a streaming write path, missing `defer file.Close()`, missing `resp.Body.Close()`. Brief pass; cheap section.

4. **Output format constraint**: Blockers / High / Medium / Style. For each finding, cite file:line. **For Blockers and High, cite which row of the configuration matrix the finding falls into** — this proves the reviewer actually traced rather than pattern-matched.

5. **"Don't be deferential"** directive remains.

#### What NOT to do

- **Do not run four parallel agents** with overlapping prompts. Arc's past data: they find ~the same things and miss the same things. One deep agent with the matrix is cheaper and more effective.
- **Do not prompt the reviewer with a long list of "past findings"**. That trains shallow-broad pattern matching. The matrix forces deep-narrow tracing.
- **Do not skip the matrix** because "it's a small diff". The bugs we miss are exactly in small diffs touching flag wiring or config resolution.

#### When to use additional reviewers

- **Security-relevant changes (token handling, TLS, file I/O, shell-out, input that becomes a URL or filename)**: add a second agent focused on the [Security Checklist](#security-checklist) below. Its job is token-leak surfaces, file mode, TLS skip-verify wiring, and any place a user-supplied string could escape its lane.
- **Skip the additional reviewer** for changes that don't touch those domains. Most arcli PRs need just the single deep reviewer + the matrix.

#### Release hygiene

- Update the README's Roadmap section as each PR lands; flip the PR's bullet from pending to shipped.
- For a tagged release (v1.0.0 etc.), draft release notes at implementation time and update them after every post-review fix-up commit. Do not let release-note prose drift behind the code across review rounds.

### Review Loop Discipline

When the deep reviewer finds issues, address all of them in a single follow-up commit BEFORE asking Gemini for review. Don't ship intermediate commits to Gemini that the internal review already caught.

If Gemini flags items the internal review missed:

1. **First, update the configuration matrix to include the row Gemini found.** This is the durable artifact — next PR's matrix should learn from this one.
2. Then fix the finding.
3. Do NOT re-spawn the internal review agent on the same diff. The matrix update prevents the next miss; another review pass on the same code is wasted tokens.

**Honest expectations**: Gemini will catch things. The goal of internal review is not zero Gemini findings; it's zero **embarrassing** Gemini findings — token leaks in logs, first-run panics, doc-vs-code drift, missing `Body.Close()`. If Gemini's findings are all genuinely subtle (HTTP/2 vs HTTP/1.1 connection-reuse behavior under cobra-level retries, etc.), internal review did its job.

### Internal Review Is Non-Negotiable On EVERY PR

Even small, test-only, or single-file diffs go through the matrix + deep reviewer before commit → push → Gemini. Past Arc data (PR #354 review on a 1-file perf refactor caught 2 real findings; PR #465 skip was caught by the user) shows the variance: skipping looks safe until it isn't.

## Security Checklist

When adding or modifying any command, verify ALL of the following:

1. **Tokens never appear in stdout/stderr unredacted** — use `config.RedactToken()` for every human-facing print. Verbose mode (`-v`) may print HTTP headers; redact the `Authorization` value there too. **The one sanctioned exception:** `auth token create` and `auth token rotate` print the freshly minted secret to stdout, because delivering it is their purpose and the server never shows it again. They print nothing else on stdout in table mode (so `$(...)` capture is clean) and put the "store it securely" reminder on stderr. Every other command handles `TokenInfo`, which carries no secret.
2. **Tokens never appear in error messages** — wrap underlying HTTP/network errors so the bearer header doesn't leak through a `%v` of an `http.Request`.
3. **Config file mode is 0600** — verified by `internal/config/config.go#Save()`; if you add a new persisted file (history, cache), it gets the same treatment.
4. **Config directory mode is 0700** — `os.MkdirAll(dir, 0o700)` before any file create.
5. **TLS skip-verify is opt-in and loud** — `--insecure` flag (or `insecure_tls = true` in config) must log a `WARNING: TLS verification disabled` line to stderr before the request goes out. Never default-on.
6. **No `os/exec` to a user-supplied string** — arcli should not shell out at all in v1. If a future feature needs it, the arg must be in an `exec.Command` slice, never `sh -c "..."`.
7. **File paths from `-f` flags are read by the process, not interpolated into anything** — `os.Open(path)` is fine; constructing `cat $path | curl` is not (and we don't shell out anyway).
8. **HTTP responses are bounded** — if a future command reads a response body fully into memory (e.g. `arcli query` to JSON), document the size ceiling; for stream-y paths (`-o arrow`, `arcli import`), use `io.Copy` not `io.ReadAll`.

## Common Pitfalls

- **Cobra `RunE` vs `Run`**: always use `RunE` and return an `error`. `Run` swallows errors silently.
- **`SilenceUsage: true` on root** — without this, every flag error prints the full help text under it. Set on the root cobra.Command and child commands inherit it.
- **Viper config format inferred from extension** — `viper.WriteConfigAs` rejects a `.tmp` suffix on the temp file. Atomic-write temp paths must end in `.toml` (use `os.CreateTemp(dir, "config.*.toml")`).
- **`os.UserHomeDir()` returns an error on weird CI environments** — handle it, don't `panic`. Tests should set `ARCLI_CONFIG` via `t.Setenv` to avoid hitting `$HOME` at all.
- **`http.DefaultClient` has no timeout** — use a constructed `http.Client{Timeout: ...}` for every request. A hung connection should not freeze the CLI forever.
- **Don't close `resp.Body` before reading it; don't forget to close it after** — `defer resp.Body.Close()` immediately after the error check on `client.Do(req)`.
- **Cobra completion is free but not on by default** — `arcli completion bash|zsh|fish` is a generated command and worth enabling once the command tree stabilizes (PR8).
- **The user's terminal may not be a TTY** — do not detect TTY to switch output format; default `-o table` always and let the user pipe `-o json` when scripting. Surprising behaviour is worse than a flag.
- **Don't add telemetry to the CLI** — arcli reporting to Basekick directly would be a trust violation. The sanctioned mechanism is the installation-id header to the user's own Arc server (see Architecture); anything beyond that needs an explicit decision, an opt-in, and a doc rewrite.
- **Don't add `-y` / `--yes` to destructive ops with a default-yes prompt** — destructive ops (`db drop`, `config delete`) prompt `y/N` and require `--yes` to skip. Default is always no.

## Carry-overs from Arc work (user-level conventions)

These come from the user's auto-memory and apply across all Basekick repos, including arcli:

- **UTC always** — every timestamp/date/duration in code, logs, release notes, PR threads, commit messages.
- **Pause for confirmation before `git commit`** — even after reviews pass, surface staged changes and wait for user OK.
- **Split deployment artifacts from code PRs** — if a PR mixes `cmd/arcli/` changes with `.github/workflows/` or `Dockerfile` changes that need separate iteration, split it. Past Arc PR #464 burned 6 Gemini rounds where only 1 finding hit product code.
- **Verify before declining a Gemini finding** — trace the data flow end-to-end before pushing back. Confidently-wrong "declined" replies cost follow-up PRs.
- **Bench before *accepting* a Gemini perf suggestion** — symmetric. Don't take "make this parallel" or "switch to a different library" on vibes.
- **Drive the entire smoke harness yourself** — if a PR's verification requires `arc serve` + a sequence of `arcli` calls, run the whole thing rather than splitting "you run the server, I run the CLI" with the user.
