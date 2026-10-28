# syntax=docker/dockerfile:1

# 多阶段构建：编译产物是静态单二进制，运行镜像里没有 Go 工具链。
# 对应 Python 版 Dockerfile（python:3.12-slim + pip install .）。

# ---- 阶段一：编译 ----------------------------------------------------------
FROM golang:1.27 AS build
COPY . /src
WORKDIR /src
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/loomy2api ./cmd/loomy2api

# ---- 阶段二：运行 ----------------------------------------------------------
FROM alpine:3.21
RUN apk add --no-cache ca-certificates tzdata
COPY --from=build /out/loomy2api /usr/local/bin/loomy2api

# 运行期状态（accounts.json、config.json、logs/）都放在 /data，请挂一个卷进来：
#   docker run -v $PWD/data:/data -p 17890:17890 loomy2api
# 把 config.example.json / accounts.example.json 复制到 /data 后按需改名即可。
# 容器里必须监听 0.0.0.0（默认 127.0.0.1 只在容器内可见），与 Python 版一致。
ENV LOOMY_HOST=0.0.0.0 \
    LOOMY_PORT=17890 \
    LOOMY_ACCOUNTS_FILE=/data/accounts.json \
    LOOMY_LOG_DIR=/data/logs

WORKDIR /data
VOLUME ["/data"]
EXPOSE 17890

HEALTHCHECK --interval=60s --timeout=5s --start-period=10s --retries=3 \
    CMD wget -qO- http://127.0.0.1:17890/health >/dev/null || exit 1

ENTRYPOINT ["loomy2api"]
CMD ["serve"]
