FROM swr.cn-north-4.myhuaweicloud.com/ddn-k8s/docker.io/python:3.10-slim

WORKDIR /app

# 复制应用代码
COPY openclaw_session_query_api.py .

# 创建数据目录（用于自动检测）
RUN mkdir -p /root/.openclaw/agents/default/sessions \
    && mkdir -p /root/.hermes/sessions

# 暴露端口
EXPOSE 8080

# 启动命令 - 支持自动检测模式和环境变量
# MODE: auto(默认) / all / openclaw / hermes
# HOOK_TOKEN: 认证令牌
CMD ["/bin/sh", "-c", "\
  CMD_ARGS=\"--host 0.0.0.0 --port 8080\"; \
  if [ -n \"$MODE\" ]; then CMD_ARGS=\"$CMD_ARGS --mode $MODE\"; fi; \
  if [ -n \"$HOOK_TOKEN\" ]; then CMD_ARGS=\"$CMD_ARGS --hook_token $HOOK_TOKEN\"; fi; \
  echo \"启动参数: $CMD_ARGS\"; \
  python openclaw_session_query_api.py $CMD_ARGS\
"]
