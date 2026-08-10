# 构建阶段：纯 Go 编译（无 CGO），注入版本号
FROM golang:1.25-alpine AS build
WORKDIR /src

# 先拷贝依赖清单以利用缓存
COPY go.mod go.sum ./
RUN go mod download

COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/gateway ./cmd/server

# 运行阶段：alpine 最小镜像
FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata
WORKDIR /app
COPY --from=build /out/gateway .
COPY config.json .

# SQLite 数据目录（宿主机挂载卷）
ENV GATEWAY_DB_PATH=/data/gateway.db
VOLUME /data

EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
  CMD wget -q -O /dev/null http://127.0.0.1:8080/admin/login || exit 1

ENTRYPOINT ["./gateway"]
