# 常用命令入口。Windows 上没装 make 时，看每条命令下面的注释直接跑等价命令。
GOFLAGS := -buildvcs=false

.PHONY: help build run test test-race vet fmt fmt-check lint up down logs ps migrate clean

help: ## 列出所有目标
	@grep -E '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-12s\033[0m %s\n", $$1, $$2}'

build: ## 编译到 bin/api.exe
	GOFLAGS=$(GOFLAGS) go build -o bin/api.exe ./cmd/api

run: ## 本地起 api（读 .env）
	GOFLAGS=$(GOFLAGS) go run ./cmd/api

test: ## 跑全部单测
	GOFLAGS=$(GOFLAGS) go test ./...

test-race: ## 跑竞态检测（CI 用）
	GOFLAGS=$(GOFLAGS) go test -race ./...

vet: ## 静态检查
	GOFLAGS=$(GOFLAGS) go vet ./...

fmt: ## 格式化
	gofmt -w internal cmd

fmt-check: ## 检查格式（CI 用，有未格式化文件则失败）
	@out=$$(gofmt -l internal cmd); \
	if [ -n "$$out" ]; then echo "以下文件未格式化:"; echo "$$out"; exit 1; fi

up: ## 起 MySQL + Redis + api
	docker compose -f deploy/docker-compose.yml up -d --build

down: ## 停掉全部容器
	docker compose -f deploy/docker-compose.yml down

logs: ## 跟 api 日志
	docker compose -f deploy/docker-compose.yml logs -f api

ps: ## 看容器状态
	docker compose -f deploy/docker-compose.yml ps

clean: ## 清掉本地构建产物
	rm -rf bin
