# 本地 Agent 会话查询 API（只读，仅标准库，无需 pip install）
#
# 基础镜像默认用官方 python:3.10-slim；国内构建可覆盖，例如：
#   docker build --build-arg PYTHON_IMAGE=swr.cn-north-4.myhuaweicloud.com/ddn-k8s/docker.io/python:3.10-slim -t agent-session-query .
ARG PYTHON_IMAGE=python:3.10-slim
FROM ${PYTHON_IMAGE}

WORKDIR /app

COPY session_query_api.py .

ENV PYTHONUNBUFFERED=1

EXPOSE 8080

# 健康检查：基础镜像里没有 curl，用 python 自己探活
HEALTHCHECK --interval=30s --timeout=10s --retries=3 \
  CMD python -c "import urllib.request,sys; sys.exit(0 if urllib.request.urlopen('http://127.0.0.1:8080/health', timeout=5).status == 200 else 1)"

# 会话数据靠挂载进来，镜像里不预建目录（避免空目录被当成"数据源存在"）
#
# 用法：默认参数如下，运行时可覆盖或追加，例如
#   docker run -p 8080:8080 -v ~/.claude/projects:/root/.claude/projects:ro agent-session-query --mode claude --hook_token xxx
ENTRYPOINT ["python", "session_query_api.py"]
CMD ["--host", "0.0.0.0", "--port", "8080"]
