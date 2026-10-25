# Dependency review for ops #5

This evidence branch keeps the six Dependabot PRs separate. It does not
publish a release, push container images, sign artifacts, or update the
Homebrew tap. All arcli documentation and PR explanations are in English;
the tracking issue, [ops #5](https://github.com/Basekick-Labs/ops/issues/5),
is maintained in Spanish.

## Baseline and order

The source baseline is `2b4b873` on `main`. The required order is
PRs #43 and #38, then #39, #22, and #20, followed by #37 separately.
GitHub's raw PR API confirmed all six were mergeable at the start.

| PR | Dependency | Submitted head |
| --- | --- | --- |
| #43 | download-syft 0.24.2 to 0.24.3 | `8fcefbf55ceecd5657d81fdc68dcda3a62d2e7ae` |
| #38 | setup-buildx-action 4.3.0 to 4.4.1 | `6d21b4d14ae6efab2d17ec839b4934d02ee99f85` |
| #39 | setup-qemu-action 3.7.0 to 4.4.0 | `faaea01d45597c83fef8b58116caeff6867489bc` |
| #22 | setup-go 6 to 7 | `dab2cbbafa3a625a457f097f24692c0b8116af8e` |
| #20 | goreleaser-action 6 to 7 | `f1afe82e09e974d897990838e345e4f65686ac1b` |
| #37 | tablewriter 1.1.4 to 1.1.5 | `ecb526ce73f92dde3fadb0df74f87754b4fa61f6` |

## Action compatibility review

- Syft's annotated v0.24.3 tag resolves to the proposed `66cbf4b` commit.
  The [release](https://github.com/anchore/sbom-action/releases/tag/v0.24.3)
  and downloader metadata were inspected. The initial smoke workflow
  exercised archive SBOM generation with the baseline GoReleaser action.
- Buildx 4.4.0 adds a BuildKit image pre-pull; 4.4.1 avoids that pre-pull
  for explicitly supplied endpoints. arcli uses the default endpoint.
  See [4.4.0](https://github.com/docker/setup-buildx-action/releases/tag/v4.4.0)
  and [4.4.1](https://github.com/docker/setup-buildx-action/releases/tag/v4.4.1).
- QEMU's [4.0.0 migration](https://github.com/docker/setup-qemu-action/releases/tag/v4.0.0)
  moves to Node 24 and ESM, requiring Actions runner 2.327.1 or later.
  The hosted-runner smoke checks build and execute both configured image
  platforms, once with baseline QEMU and once with candidate QEMU.
- setup-go's [7.0.0 release](https://github.com/actions/setup-go/releases/tag/v7.0.0)
  changes ESM/cache dependencies. Go selection and caching inputs remain
  unchanged. Both Linux and macOS are exercised.
- GoReleaser action [7.0.0](https://github.com/goreleaser/goreleaser-action/releases/tag/v7.0.0)
  moves to Node 24/ESM. The CLI remains independently constrained to
  `~> v2.18`. At review time the CI `v7` tag and release `v7.2.3` pin resolved
  to different commits, so both forms are exercised. The release-pin job
  installs the same cosign 2.5.2 as the release workflow to exercise the
  candidate downloader's signature-verification path.

## Verification boundaries

The ordinary Release dry run skips Docker, SBOMs, signing, and publishing.
The branch-only dependency smoke workflow supplements that check with
seven hosted jobs and read-only permissions. It does not replace the
required final Release dry run on the resulting `main` commit.

The action-dependency deep review found no scoped static issues. A focused
security review identified the missing cosign-path coverage, corrected before
the final smoke execution. No security findings remain in that review.

Local baseline checks passed: `go build ./...`,
`go test -race -count=1 ./...`, `go vet ./...`, and `gofmt -l .`.
The local toolchain is Go 1.27.2; hosted jobs select Go 1.25 as CI does.
Local tests used temporary caches and disposable loopback servers.

## Tablewriter comparison protocol

After resolving the five action PRs, build baseline and candidate binaries
from the same source, changing only the tablewriter version and checksums.
Run `db list`, `backup list`, and `compaction status` against identical,
disposable Arc-shaped HTTP responses with fixed locale and terminal
settings. Capture stdout, stderr, and exit status for populated results,
empty results, and server errors; compare all supported table/JSON/CSV
formats, both a named profile and an explicitly tokenless endpoint.

The comparison also checks bearer authentication, database-header scope,
identity opt-out, absence of config persistence, and help/version without
a config file. These fixtures are not a live Arc server and do not prove
server behavior.

Baseline and candidate probes found that `db list` table output forwards an ESC
byte in a database name. This predates the dependency updates; the
dependency review checks that the update introduces no display delta.
Remediation of that separate display issue is outside ops #5.

Independent review also reproduced `config list` exiting successfully
with stdout redirected to `/dev/full` on both versions. The renderer
does not reliably propagate sink errors. The compatible invariant is
the header/append/render API and wrapper return behavior, not a promise
that every downstream write failure reaches the caller.

## Final evidence

All six PRs were merged separately in the required order:

| PR | Merge commit |
| --- | --- |
| [#43](https://github.com/Basekick-Labs/arcli/pull/43) | `fab6003bb01400dbbce4e8da3578ff8d49c9e01c` |
| [#38](https://github.com/Basekick-Labs/arcli/pull/38) | `5f0b9950ec977a53bfec57e54d088e4b84c107a0` |
| [#39](https://github.com/Basekick-Labs/arcli/pull/39) | `996b66b1e08a6bac2c5e011340303be85fdbe927` |
| [#22](https://github.com/Basekick-Labs/arcli/pull/22) | `a5354893545d97372f860384c761324ba9124a6e` |
| [#20](https://github.com/Basekick-Labs/arcli/pull/20) | `98e6c641858ff7cde8017b39e021f0385dc8fbe8` |
| [#37](https://github.com/Basekick-Labs/arcli/pull/37) | `dafcd8212393ef7829a8e253b2e2924a78818e43` |

All seven jobs passed in the
[corrected smoke run](https://github.com/Basekick-Labs/arcli/actions/runs/40243356192).
Its logs confirm cosign verified the GoReleaser CLI checksum signature.
The six SBOMs, both Docker combinations, both setup-go platforms, and
both GoReleaser forms passed. No action major needed deferral.

The tablewriter candidate (`5b6fc52`, isolated cherry-pick on `98e6c64`)
passed the full local build/race/vet/format checks. All 72 real-binary
comparisons were identical; config-list and help/version checks passed
as well. Captures included normal and headerless table/CSV output, long
and Unicode strings, empty results, and HTTP 500 failures. The reviewer
independently checked the raw comparison artifacts and output tests.
Final main `dafcd8212393ef7829a8e253b2e2924a78818e43` has an identical
source tree to the tested candidate, verified by an empty Git diff and
independently confirmed by the reviewer.

## Existing CI failures observed during integration

Do not interpret the passing supplemental smoke checks as a claim that
every main CI run passed. Two intermediate macOS race jobs failed in
unchanged sample-download tests:

- [Run after #43](https://github.com/Basekick-Labs/arcli/actions/runs/40243498173):
  `TestFetchFailsFast` observed 14 of 24 requests. This occurred before
  setup-go or GoReleaser were upgraded.
- [Run after #20](https://github.com/Basekick-Labs/arcli/actions/runs/40243582203):
  `TestFetchAll403StopsEarly` observed 23 requests.

The Release dry run passed in both runs. The source currently cancels
downloads in the result collector after a failing worker has released
its semaphore slot, allowing queued requests to start first. These
failures predate the tablewriter update and are outside this dependency
cleanup. A bounded 30-repeat diagnostic of the two tests passed locally
on Linux; that does not disprove the macOS failures. A separate existing
`fix/sample-fetch-errors` branch contains cancellation work and was not
merged as part of ops #5.

The [final-main CI run](https://github.com/Basekick-Labs/arcli/actions/runs/40243880858)
passed all three jobs on `dafcd8212393ef7829a8e253b2e2924a78818e43`:
Release dry run, Linux tests, and macOS tests. This satisfies the issue's
required release gate after the last merge. The earlier failures remain
recorded above rather than being hidden by this later success.

An [additional branch-only run](https://github.com/Basekick-Labs/arcli/actions/runs/40244099896)
tested source commit `86e289781760b3f96127490767f4e521eb34694c`, which
includes final main plus the smoke workflow. It finished with six jobs
passing and the macOS job failing. The successful Syft job generated and
verified all six archive SBOMs using the merged setup-go and GoReleaser
pins, release cosign 2.5.2, and final application source. Static
incremental security review found no issues and confirmed unchanged
no-publish/no-sign restrictions. Both Docker jobs, Linux tests, and
both GoReleaser jobs also passed.

The macOS job in this extra run hit the same existing
`TestFetchFailsFast` failure (21 of 24 requests). It was not rerun to hide
the result. This does not change the successful required final-main CI
run recorded above; the separate sample cancellation defect remains.
