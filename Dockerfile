# syntax=docker/dockerfile:1
FROM golang:1.23-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY go.sum* ./
# 国内构建走 goproxy.cn；海外 CI 自动回落官方源。
RUN go env -w GOPROXY=https://goproxy.cn,direct && go mod download
COPY . .
# -trimpath 去掉构建机绝对路径（二进制可复现）；-s -w 去掉符号表与调试信息。
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath \
      -ldflags="-s -w -X github.com/rsfree/xhx2api/internal/version.Version=${VERSION}" \
      -o /out/xhx2api ./cmd/server

FROM alpine:3.20
# tzdata：签到/「今天」的判断按东八区，缺 tzdata 会静默回落 UTC；
# ca-certificates：上游 HTTPS；wget：healthcheck。
RUN apk add --no-cache ca-certificates tzdata wget \
 && adduser -D -u 10001 app \
 && mkdir -p /app/auths /app/data \
 && chown -R app:app /app
WORKDIR /app
COPY --from=build /out/xhx2api /app/xhx2api
# 镜像不带真实配置：放样例作为默认（生产由挂载卷的 /app/config.json 覆盖）。
# 首次启动若挂载点为空，程序会自己生成一份带随机 api_key 的配置。
COPY config.example.json /app/config.example.json
ENV TZ=Asia/Shanghai
USER app
EXPOSE 7867
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s \
  CMD wget -qO- http://127.0.0.1:7867/healthz >/dev/null || exit 1
ENTRYPOINT ["/app/xhx2api", "-config", "/app/config.json"]
