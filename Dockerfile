# asq, built from source: for a change of your own, or behind a registry mirror. The
# image a release publishes is Dockerfile.release — the released binaries copied in, the
# same bytes as the archives, no compiler involved.
#
# Two stages: compile a static binary, and a runtime image that holds it, the root
# certificates and nothing else — no shell, no package manager.
#
# The build stage runs on the build machine's own platform and cross-compiles for the
# target, which Go does natively: a multi-arch build never runs a compiler under
# emulation. The base image and the module proxy can be swapped:
#   docker build --build-arg GO_IMAGE=<mirror>/golang:1.26-alpine \
#                --build-arg GOPROXY=https://goproxy.cn,direct -t asq .
ARG GO_IMAGE=golang:1.26-alpine
FROM --platform=$BUILDPLATFORM ${GO_IMAGE} AS build

# buildx fills these in per target platform. Under a plain docker build they are empty,
# and an empty GOOS / GOARCH means the host.
ARG TARGETOS TARGETARCH
ARG VERSION=dev
ARG GOPROXY=https://proxy.golang.org,direct
ENV GOPROXY=${GOPROXY}

WORKDIR /src
# The dependency manifests first, so a change to the source does not fetch them again
COPY go.mod go.sum ./
RUN go mod download

COPY cmd ./cmd
COPY internal ./internal
COPY web ./web
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w -X github.com/itswl/agent-session-query/internal/app.Version=${VERSION}" -o /out/asq ./cmd/asq

FROM scratch

COPY --from=build /out/asq /asq
# For the one thing here that calls out: `asq otel` to an https collector
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt

# The stores are mounted in, read-only, where each CLI keeps its own under $HOME; the
# image creates none of them in advance (an empty directory would look like a store).
#
# No USER on purpose: transcripts are 0600 on a Linux host, and root is the one uid that
# reads any owner's. To drop the privilege, run with --user $(id -u):$(id -g) — a uid
# fixed here would read nothing.
ENV HOME=/root \
    ASQ_HOST=0.0.0.0 \
    ASQ_PORT=7717 \
    ASQ_INDEX=/data/index.db \
    ASQ_STATE=/data

# The index is a cache of the mounted stores: a volume keeps it across restarts, so a
# restart reads only what changed instead of the whole history.
VOLUME ["/data"]
EXPOSE 7717

# scratch has no curl and no shell: the binary asks itself
HEALTHCHECK --interval=30s --timeout=10s --start-period=10s --retries=3 CMD ["/asq", "ping"]

# It listens on every interface of the container, so it refuses to start without a
# token: docker run -e ASQ_TOKEN=… -p 127.0.0.1:7717:7717 -v ~/.claude/projects:/root/.claude/projects:ro asq
ENTRYPOINT ["/asq"]
CMD ["serve"]
