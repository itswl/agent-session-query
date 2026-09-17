# 本地 Agent 会话查询 API（只读；无 cgo，唯一外部依赖是纯 Go 的 SQLite 驱动）
#
# 两段构建：编译出静态二进制，运行镜像里只有它和探活小程序——没有 shell、没有包管理器。
# 国内构建可覆盖镜像源与模块代理，例如：
#   docker build \
#     --build-arg GO_IMAGE=swr.cn-north-4.myhuaweicloud.com/ddn-k8s/docker.io/library/golang:1.24-alpine \
#     --build-arg GOPROXY=https://goproxy.cn,direct \
#     -t agent-session-query .
ARG GO_IMAGE=golang:1.24-alpine
FROM ${GO_IMAGE} AS build

ARG GOPROXY=https://proxy.golang.org,direct
ENV GOPROXY=${GOPROXY}

WORKDIR /src
# 先只拷依赖清单拉模块，源码改动不会让这一层失效
COPY go.mod go.sum ./
RUN go mod download

COPY internal ./internal
COPY cmd ./cmd
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/agent-session-query ./cmd/agent-session-query \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/healthcheck ./cmd/healthcheck

FROM scratch

COPY --from=build /out/agent-session-query /agent-session-query
COPY --from=build /out/healthcheck /healthcheck

# 会话数据靠挂载进来，镜像里不预建目录（避免空目录被当成"数据源存在"）。
# 数据源路径由 $HOME 推导，这里固定成 /root，与下面的挂载示例一致。
ENV HOME=/root

EXPOSE 8080

# scratch 里没有 curl、没有 shell，所以探活用自带的静态小程序
HEALTHCHECK --interval=30s --timeout=10s --retries=3 CMD ["/healthcheck"]

# 用法：默认参数如下，运行时可覆盖或追加，例如
#   docker run -p 8080:8080 -v ~/.claude/projects:/root/.claude/projects:ro \
#     agent-session-query --mode claude --hook_token xxx
ENTRYPOINT ["/agent-session-query"]
CMD ["--host", "0.0.0.0", "--port", "8080"]
