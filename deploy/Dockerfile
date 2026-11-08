# 多阶段构建：编译在 golang 镜像里，运行在精简 alpine 里。
# 本机是 Windows，构建产物必须是 linux/amd64，所以 GOOS/GOARCH 显式指定。
FROM golang:1.22-alpine AS build

WORKDIR /src

# 先只拷依赖清单：依赖没变时这一层能命中缓存，改业务代码不会重下模块。
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# CGO_ENABLED=0：产出静态二进制，运行时不需要 libc，也不需要 gcc。
# -trimpath 去掉本机绝对路径，-s -w 去掉符号表（镜像小一半）。
#
# 两条命令必须在同一层：statuteseed 和 api 用同一份依赖缓存与同一份源码，
# 拆成两层等于把 go build 的模块解析再来一遍，构建时间翻倍。
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -ldflags="-s -w" -o /out/api ./cmd/api \
    && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -ldflags="-s -w" -o /out/statuteseed ./cmd/statuteseed

FROM alpine:3.20

# tzdata：日志时间要能按 Asia/Shanghai 显示；ca-certificates：调 LLM 网关要 TLS；
# poppler-utils：internal/file 调 pdftotext 解析 PDF，缺了它上传接口会直接 500。
RUN apk add --no-cache ca-certificates tzdata poppler-utils \
    && adduser -D -u 10001 app

WORKDIR /app
COPY --from=build /out/api /app/api

# 法条种子也进镜像：容器化之后没有别的途径把 205 条法条灌进库，
# 云上会是一张空表，检索空库时模型只能答"没有检索到法条"，citation 链路等于没有。
# 46KB 的 JSON + 两份原文 HTML，代价可以忽略。
COPY --from=build /out/statuteseed /app/statuteseed
COPY deploy/seed /app/seed

# 合同落盘目录必须存在且属于运行用户，否则上传接口在运行期才报权限错。
RUN mkdir -p /app/storage && chown -R app:app /app

USER app

# GOMEMLIMIT 比容器硬限低一截，让 GC 在 OOM Killer 之前先自己回收；
# 否则 2G 机器上会出现"进程被 kill 但日志里没有任何错误"。
ENV GOMEMLIMIT=350MiB \
    GOGC=100 \
    TZ=Asia/Shanghai

EXPOSE 8080

ENTRYPOINT ["/app/api"]
