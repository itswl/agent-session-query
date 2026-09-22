# Local agent session query API (read-only; no cgo, the only external dependency is a
# pure-Go SQLite driver)
#
# Two-stage build: compile a static binary, and the runtime image holds nothing but it and
# the liveness probe — no shell, no package manager.
#
# The build stage always runs on the build machine's own platform and cross-compiles for
# the target: Go does that natively, so a multi-arch build (release.yml publishes
# linux/amd64 + linux/arm64) never runs a compiler under QEMU emulation, which is several
# times slower. release.yml pushes the result to GitHub Container Registry on every tag.
# The base image and module proxy can be overridden, for example:
#   docker build \
#     --build-arg GO_IMAGE=swr.cn-north-4.myhuaweicloud.com/ddn-k8s/docker.io/library/golang:1.24-alpine \
#     --build-arg GOPROXY=https://goproxy.cn,direct \
#     -t agent-session-query .
ARG GO_IMAGE=golang:1.24-alpine
FROM --platform=$BUILDPLATFORM ${GO_IMAGE} AS build

# buildx fills these in per target platform. Under a plain docker build they are empty,
# and an empty GOOS / GOARCH means the host — so a single-platform build needs no flags.
ARG TARGETOS TARGETARCH

ARG VERSION=dev
ARG GOPROXY=https://proxy.golang.org,direct
ENV GOPROXY=${GOPROXY}

WORKDIR /src
# Copy only the dependency manifests first so source changes do not invalidate this layer
COPY go.mod go.sum ./
RUN go mod download

COPY internal ./internal
COPY cmd ./cmd
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath -ldflags="-s -w -X github.com/itswl/agent-session-query/internal/app.buildVersion=${VERSION}" -o /out/agent-session-query ./cmd/agent-session-query \
 && CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath -ldflags="-s -w" -o /out/healthcheck ./cmd/healthcheck

FROM scratch

COPY --from=build /out/agent-session-query /agent-session-query
COPY --from=build /out/healthcheck /healthcheck

# Session data is mounted in; the image creates no directories in advance (an empty
# directory would otherwise look like an existing data source).
# Source paths derive from $HOME, pinned to /root here to match the mount examples below.
ENV HOME=/root

EXPOSE 8080

# scratch has no curl and no shell, so the liveness probe is the bundled static binary
HEALTHCHECK --interval=30s --timeout=10s --retries=3 CMD ["/healthcheck"]

# Usage: the defaults are below and can be overridden or extended at runtime, e.g.
#   docker run -p 8080:8080 -v ~/.claude/projects:/root/.claude/projects:ro \
#     agent-session-query --mode claude --hook_token xxx
ENTRYPOINT ["/agent-session-query"]
CMD ["--host", "0.0.0.0", "--port", "8080"]
