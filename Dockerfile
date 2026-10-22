# 基础镜像可用 build-arg 覆盖，便于国内网络走镜像站。
# compose 构建时由 .env / 环境变量 CA2A_GO_IMAGE、CA2A_RUNTIME_IMAGE、CA2A_APK_MIRROR 传入；
# 单独用 docker build 时：
#   docker build --build-arg GO_IMAGE=docker.m.daocloud.io/library/golang:1.24-alpine \
#                --build-arg RUNTIME_IMAGE=docker.m.daocloud.io/library/alpine:3.20 .
ARG GO_IMAGE=golang:1.24-alpine
ARG RUNTIME_IMAGE=alpine:3.20

FROM ${GO_IMAGE} AS build
WORKDIR /src
COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal
RUN go build -o /out/codearts2api ./cmd/server && \
    go build -o /out/codearts2api-login ./cmd/login && \
    go build -o /out/codearts2api-credit ./cmd/credit && \
    go build -o /out/codearts2api-apply ./cmd/apply

FROM ${RUNTIME_IMAGE}
# 可选：替换 apk 源（如 https://mirrors.aliyun.com），留空则用官方源
ARG APK_MIRROR=
RUN if [ -n "${APK_MIRROR}" ]; then \
        sed -i "s#https\?://dl-cdn.alpinelinux.org#${APK_MIRROR}#g" /etc/apk/repositories; \
    fi \
    && apk add --no-cache ca-certificates tzdata
WORKDIR /app
COPY --from=build /out/ /usr/local/bin/
COPY config.example.json ./config.example.json
VOLUME ["/app/auths", "/app/data"]
EXPOSE 7866
CMD ["codearts2api", "-config", "config.json"]
