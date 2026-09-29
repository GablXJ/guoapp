# 构建上下文必须是仓库根目录：
#   docker build -f nas/Dockerfile -t guoapp-nas .
FROM golang:1.24-bookworm AS build
ARG BUILD_ALL_SOURCES=false
ARG TARGETOS
ARG TARGETARCH
ARG TARGETVARIANT
WORKDIR /src
COPY native/go.mod native/go.sum ./native/
COPY nas/go.mod nas/go.sum ./nas/
RUN cd nas && go mod download
COPY native/ ./native/
COPY nas/ ./nas/
RUN cd nas && CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} GOARM=${TARGETVARIANT#v} \
    go build -trimpath -ldflags="-s -w -X duanjuapp/native/core.buildAllSources=${BUILD_ALL_SOURCES}" -o /out/guoapp-nas .

FROM debian:bookworm-slim
LABEL org.opencontainers.image.title="guoapp-nas" \
      org.opencontainers.image.description="短剧 NAS 版：浏览、下载、合并、导出"
RUN apt-get update \
    && apt-get install -y --no-install-recommends ffmpeg ca-certificates tzdata curl \
    && rm -rf /var/lib/apt/lists/*
COPY --from=build /out/guoapp-nas /usr/local/bin/guoapp-nas
ENV NAS_ADDR=:8090 \
    NAS_DATA=/data \
    NAS_DOWNLOAD_DIR=/downloads \
    TZ=Asia/Shanghai
VOLUME ["/data", "/downloads"]
EXPOSE 8090
HEALTHCHECK --interval=60s --timeout=10s --start-period=30s --retries=3 \
  CMD curl -fsS http://127.0.0.1:8090/login >/dev/null || exit 1
ENTRYPOINT ["/usr/local/bin/guoapp-nas"]
