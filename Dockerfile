# arcli container image.
#
# The binary is built by GoReleaser (static, CGO_ENABLED=0, identical to
# the one in the release archives); this Dockerfile only packages it.
# GoReleaser's dockers_v2 stages the prebuilt binaries into the build
# context per platform (linux/amd64/arcli, linux/arm64/arcli), hence the
# COPY from ${TARGETPLATFORM}.
#
# Distroless static: CA bundle (HTTPS to Arc), tzdata, /etc/passwd with
# `nonroot` (uid 65532). HOME is not set in the image; the runtime takes
# /home/nonroot from /etc/passwd, so ~/.arcli/config.toml resolves and
# a `--user` other than 65532 gets HOME=/ and a clear write failure. No
# shell, no package manager.
#
# The base is pinned by digest (the tag is kept for readability) so the
# signed release describes exactly one image; bump the digest on purpose.
#
# Build locally:  goreleaser release --snapshot --clean --skip=publish,sign
#                 (.goreleaser.yaml lands with PR10b; until then there is
#                 no way to build this image from a plain checkout)
# Run:            docker run --rm -e ARC_ENDPOINT=... -e ARC_TOKEN=... \
#                   ghcr.io/basekick-labs/arcli ping
FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab

ARG TARGETPLATFORM
COPY ${TARGETPLATFORM}/arcli /usr/local/bin/arcli

ENTRYPOINT ["/usr/local/bin/arcli"]
