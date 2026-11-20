# 部署 Runbook（阿里云 2 核 2G · 单机 Docker Compose）

> 目标形态：一台 ECS e 实例，2C2G，Ubuntu 22.04，Docker Compose 起 MySQL + Redis + api。
> 前提：`deploy/docker-compose.yml` 与 `deploy/mysql/my.cnf`、`deploy/redis/redis.conf` 已随仓库发布。
> 这份文件按“照着敲就能上线”写，每一步都有验收命令。

---

## 0. 下单后必须先确认的三件事

| # | 事项 | 为什么 |
|---|---|---|
| 1 | **地域**（华东1/华北2/香港…） | 大陆节点 80/443 必须备案，否则被阻断；香港免备案但延迟 40–80ms |
| 2 | **公网带宽** | 页面写“无带宽”= 0 Mbps：公网访问不了，也 pull 不了 Docker Hub |
| 3 | **系统镜像** | 选 Ubuntu 22.04 LTS；Alibaba Cloud Linux 3 也行，但容器镜像源要换 |

**带宽怎么买**：平时 1 Mbps 固定（约 ¥23/月），演示前 1 小时临时升到 5 Mbps（按时计费）。
1 Mbps 跑 SSE 文本流够用（每秒几十 KB），但 5MB 合同上传要 40 秒，演示当天必须升。

**没备案怎么办**：不碰 80/443，直接用 **8443** 端口 + 公网 IP：`https://<IP>:8443`。
备案要 7–20 天，一个月周期内来不及。

---

## 1. 初始化机器（一次性，约 15 分钟）

```bash
# 1.1 开 2G swap —— 不是性能方案，是防 OOM Killer 猝死
sudo fallocate -l 2G /swapfile
sudo chmod 600 /swapfile
sudo mkswap /swapfile
sudo swapon /swapfile
echo '/swapfile none swap sw 0 0' | sudo tee -a /etc/fstab
sudo sysctl vm.swappiness=10
# 2G 机型的差异（本 runbook 默认按 4G 机型写）：
#   deploy/mysql/my.cnf 的 innodb_buffer_pool_size 改 256M，
#   compose 里 mysql 的 memory 限额从 1G 改 640M。

# 1.2 时区与时间同步
sudo timedatectl set-timezone Asia/Shanghai

# 1.3 装 Docker（阿里云内网镜像源，无公网带宽也能装）
curl -fsSL https://get.docker.com | sudo sh -s -- --mirror Aliyun
sudo systemctl enable --now docker

# 1.4 验收
free -m          # Swap 一行必须 >= 2047
docker version   # Server 段有版本号
```

**安全组**：只放行 `22`（改成非默认端口更好）和 `8443`。**不要**放行 3306 / 6379 ——
compose 本来就没把这些端口映射到宿主机，但安全组多开一个口就是多一个被扫的风险。

---

## 2. 镜像怎么上机器（没有公网带宽也能做）

镜像走**阿里云 ACR 个人版**（免费），ECS 从内网拉，不消耗公网带宽：

```bash
# 本地（Windows）构建 linux/amd64 镜像并推送
docker buildx build --platform linux/amd64 -f deploy/Dockerfile \
  -t registry.cn-hangzhou.aliyuncs.com/<命名空间>/laborlaw-api:v0.1.0 --push .

# ECS 上登录并拉取
docker login registry.cn-hangzhou.aliyuncs.com
docker pull registry.cn-hangzhou.aliyuncs.com/<命名空间>/laborlaw-api:v0.1.0
```

镜像 tag 用日期或 git short sha（如 `v0.1.0-350688e`），**至少保留最近 3 个**用于回滚。

---

## 3. 首次部署

```bash
# 本节所有 compose 命令都在仓库根执行，统一带 --env-file .env。
# 为什么必须带：deploy/ 下没有 .env，Compose V2 只从 compose 文件所在目录找 .env，
# 找不到就不会读仓库根那份，于是报 "required variable APP_ENV is missing a value"。
# 注意 --env-file 的路径是按**当前工作目录**解析的（不是 compose 文件所在目录）：
# 在仓库根写 .env；先 cd deploy 再执行的话才写 ../.env，写反了会报 couldn't find env file。
export APP_ENV=production   # 首次上云；.env 里也有 APP_ENV，但 shell 变量优先

# 3.1 拉仓库（只为了 compose / my.cnf / redis.conf，不需要源码）
git clone https://github.com/0623som7/Labor-Law-AI-Advisory-System.git
cd Labor-Law-AI-Advisory-System

# 3.2 生产 .env（不要用 .env.example 直接改完就上）
cp .env.example .env
vim .env
#   APP_ENV=production          —— 会强制 json 日志、禁止 AUTH_DEV_USER、禁止 AUTO_MIGRATE
#   MYSQL_DSN=root:<强密码>@tcp(mysql:3306)/laborlaw?...
#   JWT_SECRET=<openssl rand -hex 32>
#   LLM_API_KEY=<B 提供>
#   HTTP_ADDR=:8080
#   !! AUTH_DEV_USER=          —— 必须留空。production 下 config.Validate 会直接拒绝它（这是设计：
#                                它的存在意味着"所有请求都当成同一个用户"，线上等于没有鉴权）
#   !! FILE_PDFTOTEXT_PATH=pdftotext —— 容器里 apk 已装 poppler-utils，走 PATH 即可；
#                                填 Windows 路径（C:\...）会让解析进程 exec 失败，上传接口 500
chmod 600 .env

> `APP_ENV` 在 compose 里是必填项：没设（`docker compose ... config` 会直接报错）
> 起不来，避免部署时忘改 `APP_ENV` 就按 dev 跑起来。

> `AUTH_DEV_USER` 与 `AUTO_MIGRATE` 在 compose 里被强制写成 `""` / `"false"`，
> 覆盖 `.env` 里的值。原因：`config.Validate` 只在 `production` 下拦 DevUser，
> 按 `staging` 部署时 `.env` 里那个 `dev-user-0001` 会让所有未登录请求共享同一份数据；
> `AUTO_MIGRATE=true` 则让服务每次启动都自己改表结构。

> 国内机器上 `docker compose build` 可能卡在 `go mod download`：
> `proxy.golang.org` / `storage.googleapis.com` 的 TLS 握手超时。
> 用 `GOPROXY=https://goproxy.cn,direct docker compose -f deploy/docker-compose.yml --env-file .env build` 覆盖，
> 默认值仍是官方源，CI 与海外机器不用改。

# 3.3 首次建库要手动跑一次迁移（生产禁了 AUTO_MIGRATE）
docker compose -f deploy/docker-compose.yml --env-file .env up -d mysql redis
docker compose -f deploy/docker-compose.yml --env-file .env run --rm api -migrate

# 3.4 起服务
docker compose -f deploy/docker-compose.yml --env-file .env up -d api

# 3.4.1 导入法条种子（空库时检索恒 0 命中，模型只能答"没有检索到法条"，
#       前端一张卡片都不会有——这一步不做，前面全白搭）
docker compose -f deploy/docker-compose.yml --env-file .env --profile seed run --rm seed
# 期望输出：
#   共 517 条法条 → /app/seed/statutes.json
#   法条表重建完成：0 → 517 行
#   逐字复核通过：[LCL-38 LCL-82 LCL-87 LL-44]
# 必需在 3.3 的迁移之后跑（statutes 表由迁移建）。它执行的是 ReplaceAll
# （先 DELETE 再批量 INSERT，结果幂等），所以用 profile 隔离、不随 up 自动启动。
#
# ★ 重建 statutes 之后必须做两件事，否则库里"有法条、没向量"或"向量比法条多"：
#   1) 重跑 3.4.3 的 embeddingseed —— 它按缺失补齐，不跑新条号就永远没有向量，
#      只活在关键词那一路，语义与重排里等于不存在；
#   2) restart api —— 语义索引是启动时一次性读进内存的（LoadSemanticIndex），
#      不重启就是"库里 517、内存里 365"，日志里的 vectors= 会停在旧数字。
#   另外 embeddingseed 是幂等 upsert，默认**不删孤儿向量**：语料缩回去之后
#   embeddings 里会残留已不存在条号的向量，而 LoadSemanticIndex 是从 embeddings 表
#   读进内存的——孤儿向量照样参与检索，只是回表时拿不到条文。所以 3.4.3 带上 -prune：
#     docker compose ... run --rm --entrypoint /app/embeddingseed api -prune
#   它会打印「清理孤儿向量：N 条」。真要手工兜底：
#     DELETE e FROM embeddings e LEFT JOIN statutes s ON s.id = e.owner_id
#     WHERE e.owner_type = 'statute' AND s.id IS NULL;

# 3.4.2 确认 MySQL 的配置真的被读到了（改过 my.cnf 就必须重跑这一步）
#      deploy/mysql/my.cnf 由 deploy/mysql/Dockerfile COPY 进镜像并 chmod 644，
#      不再是 bind mount —— 挂载方式在 Windows 上落成 0777，MySQL 会整份忽略它。
docker compose -f deploy/docker-compose.yml --env-file .env exec -T mysql ls -l /etc/mysql/conf.d/my.cnf
#   期望 -rw-r--r--（或 -r--r--r--）；看到 rwxrwxrwx 说明又被挂载覆盖了
docker compose -f deploy/docker-compose.yml --env-file .env exec -T mysql \
  sh -c 'mysqld --defaults-extra-file=/etc/mysql/conf.d/my.cnf --print-defaults'
#   期望含 --innodb_buffer_pool_size=512M、--max_connections=50、--performance_schema=OFF，
#   并且没有 "World-writable config file ... is ignored" 这句 Warning
docker compose -f deploy/docker-compose.yml --env-file .env exec -T mysql mysql -uroot -p --default-character-set=utf8mb4 -N -e \
  "select @@innodb_buffer_pool_size, @@max_connections, @@performance_schema, @@ngram_token_size, @@global.time_zone, @@innodb_flush_log_at_trx_commit"
#   期望 536870912  50  0  2  +00:00  2

# 3.4.3 灌法条向量（可选；RETRIEVAL_SEMANTIC=on 之前必须做）
#      不灌向量而把开关打开，每次问答都会多一次公网往返再降级回关键词，
#      日志里只剩一串"语义检索降级"，检索质量一点没变。
#      先停 api 是为了腾内存：2G 机器上 mysql(1G) + api(512M) + redis(192M)
#      已接近上限，再起一个容器会触发 OOM killer 挑最胖的杀。
docker compose -f deploy/docker-compose.yml --env-file .env stop api
docker compose -f deploy/docker-compose.yml --env-file .env run --rm --entrypoint /app/embeddingseed api -prune
#   --entrypoint 不能省：镜像的 ENTRYPOINT 是 /app/api，不覆盖会把 embeddingseed 当成参数传给它
#   -prune 是 2026-11-16 加的：写完全部向量后顺手删掉 statutes 里已不存在的孤儿向量
#   期望输出：
#     完成：517 条，维度 1024，52 批，耗时约 17s
#     写入 embeddings：517 行（owner_type=statute，model=text-embedding-v3）
#     清理孤儿向量：0 条
docker compose -f deploy/docker-compose.yml --env-file .env up -d api
docker compose -f deploy/docker-compose.yml --env-file .env exec -T mysql sh -c \
  'mysql -uroot -p"$MYSQL_ROOT_PASSWORD" --default-character-set=utf8mb4 -N -e "select count(*) from laborlaw.embeddings"'
#   期望 517。0 就说明灌失败了：看上一次命令的输出是"向量库为空"还是 API key / 网络报错
#   换过 LLM_EMBEDDING_MODEL 之后要重灌：索引按 model 分开读，旧向量对不上新模型

# 3.5 自签证书（8443 的 TLS 由 nginx 终止，证书必须先在宿主机生成）
#     没有证书时 nginx 会直接退出（ssl_certificate 指向不存在的文件），
#     于是 8443 连不上——这不是"还没配好"，是容器起不来。
mkdir -p deploy/nginx/certs
openssl req -x509 -newkey rsa:2048 -nodes -days 365 \
  -subj "/CN=<公网IP>" \
  -keyout deploy/nginx/certs/server.key \
  -out deploy/nginx/certs/server.crt
chmod 600 deploy/nginx/certs/server.key

# 3.6 上传前端构建产物
#      前端 dist 不进仓库（web/.gitignore），必须在有 Node 的机器上 build 后传上去。
#      compose 把 web/dist 只读挂载到 /usr/share/nginx/html；
#      目录不存在时 Docker 会自动建一个空目录，页面返回 403 而不报错，所以传完必须实测根路径。
#      —— 在开发机执行 ——
cd web && pnpm install --frozen-lockfile && pnpm build && cd ..
scp -r web/dist ubuntu@<公网IP>:/opt/laborlaw/web/
#      —— 在服务器执行 ——
ls web/dist/index.html web/dist/assets/*.js     # 两个都在才算传成功
# 3.6.1 每次上线核对前端产物（dist 不进仓库，传漏了没有任何报错，页面会一直是旧的）
curl -sk https://127.0.0.1:8443/ | grep -o 'assets/index-[a-zA-Z0-9]*\.js'
ls web/dist/assets/*.js
#   两边 hash 必须一致；不一致说明这次没传 dist

# 3.6.2 只换前端也要检查迁移（2026-11-17 的事故，别再来一次）
#   1) 只改前端时 --build api 可以省，但只要合并进来过新的
#      internal/store/migrations/*.up.sql，就必须单独跑一次迁移——新二进制认新列，
#      旧库没有那一列，写库会静默失败（进程不崩、接口照回 200，数据全丢）：
docker compose -f deploy/docker-compose.yml --env-file .env exec -T api /app/api -migrate
#      期望 {"msg":"迁移完成","applied":N}；applied=0 且下面第 2 步仍查不到列，
#      说明容器镜像早于那个迁移文件，要先 up -d --build api 再跑一次。
#   2) 验证方式是查该列在不在，不要只看 -migrate 的输出（applied=0 也可能是"已经跑过"）：
docker exec deploy-mysql-1 sh -c 'mysql -uroot -p"$MYSQL_ROOT_PASSWORD" -N -e "select count(*) from information_schema.columns where table_schema=0x6c61626f726c6177 and table_name=0x6d65737361676573 and column_name=0x6174746163686d656e7473;"'
#      期望 1（0 = 没跑）。十六进制是为了绕开 sh -c 里再套单引号的转义地狱，
#      换别的表/列就把最后两段 0x... 换成对应字符串的十六进制。
#   3) 症状对照：日志里出现 persist chat turn failed / history load degraded，
#      且原因带 Error 1054 (42S22): Unknown column 'xxx' in 'field list'，
#      一律先怀疑漏跑迁移，不要去改 SQL 或手工 ALTER TABLE。
#      2026-11-17 那次：线上 messages 缺 attachments 列，messages 停在 11-16 14:37，
#      而会话数照涨（resolveSession 先建会话再写消息），一夜多了 12 条空会话。
#   4) 重建法条库之后（2026-11-20 补）
#
#      statutes 与 embeddings 是两张表，而进程内的语义索引是**启动时快照**，没有热加载。
#      所以 statuteseed -import + embeddingseed 之后必须重启 api：
#
#      docker compose --env-file ../.env restart api
#
#      并核对启动日志，两个数必须相等：
#
#      docker compose --env-file ../.env logs api --since 5m | grep "语义检索已启用"
#      # 期望 vectors=596
#
#      不等就别继续部署——语义层会静默失明，日志里连 Warn 都不会有。
#      2026-11-19 那次：先起 api、后灌库，进程停在 vectors=517（九部法时代），
#      库里已是 596 条，79 条新法条（保障农民工工资支付条例 64 + 最低工资规定 15）
#      对语义层完全失聪，而 /readyz 照回 ok。

# 3.6.3 挂载目录与静默失败（2026-11-20 的事故）
#   1) 经过：为清服务器脏工作区执行了 rm -rf deploy/nginx/snippets，git merge 把三个
#      snippet 重建回来时目录 inode 变了，而 nginx 容器更早创建、握的仍是旧目录 →
#      容器内 /etc/nginx/snippets 是空的，default.conf 里三处 include 全部打不开 →
#      nginx -t 一直失败 → 任何 reload 都会 emerg 退出。线上从 13:0x 到 15:24 处于
#      "能跑但不能 reload"：进程拿内存里的旧配置继续服务，所有返回码都正常，直到
#      下一次必须 reload 时才暴露。
#      处置（只重建 nginx，不动 api / mysql / redis）：
#      cd deploy && export APP_ENV=production
#      docker compose --env-file ../.env up -d --force-recreate --no-deps nginx
#      docker exec deploy-nginx-1 ls /etc/nginx/snippets/     # 三个 conf 必须在
#      docker exec deploy-nginx-1 nginx -t                    # 必须 syntax is ok
#      注意：不带 --force-recreate 的 up -d nginx 判不出差异，容器根本不会被重建。
#   2) 规矩：任何被 bind mount 的目录都不许 rm -rf 整个目录，清理只能 rm -rf dir/* 或
#      原地覆盖文件。已踩过两次：删 web/dist 目录后 nginx 变 403（compose 绑的是 inode）；
#      删 deploy/nginx/snippets 目录后 nginx -t 直接失败（就是本节这次）。
#   3) 改完证书、改完 nginx include、或动过任何挂载目录之后，必须走闭环，不能只看
#      reload 的返回码。做法是造一次真陈旧再跑守卫：
#      sudo md5sum deploy/nginx/certs/{fullchain.pem,privkey.pem}    # 记录基线
#      sudo cp -L deploy/nginx/certs/server.crt deploy/nginx/certs/fullchain.pem
#      sudo cp -L deploy/nginx/certs/server.key deploy/nginx/certs/privkey.pem
#      sudo /usr/local/bin/laborlaw-check-cert.sh; echo "exit=$?"
#      sudo tail -4 /var/log/laborlaw-cert.log   # 必须两行：副本陈旧，开始替换 / 替换并 reload 成功
#      sudo cmp -s /etc/letsencrypt/live/laborlaw-ai.cn/fullchain.pem deploy/nginx/certs/fullchain.pem \
#        && echo 已恢复一致
#      curl -s -o /dev/null -w '%{http_code}\n' https://laborlaw-ai.cn/   # 必须 200
#      造陈旧必须用**配对**的一份证书（8443 那套 server.crt + server.key）：证书与私钥不配对时
#      nginx -t 会因 key mismatch 失败，那测的是回滚路径，不是替换路径。
#      报告里要贴 docker exec deploy-nginx-1 nginx -t 的原样输出——nginx 的失败是静默的，
#      它继续用内存里的旧配置服务，不回看这一步就只能等下次 reload 才知道。
#   4) 守卫脚本在 /usr/local/bin/laborlaw-check-cert.sh（755），由 /etc/cron.d/laborlaw-cert
#      每天 04:00 跑；备份落 /var/backups/laborlaw/。不许把 .bak 或临时脚本放进
#      /opt/laborlaw：会脏 git（下次 git pull 报 "would be overwritten"），也会被执行
#      上面那种目录清理时顺手删掉。

# 3.6.4 上传接口对无文字层文件回 422（2026-11-18 契约变更）
#   1) POST /api/v1/files 遇到没有文字层的扫描件，从 200（data 里给个空壳记录）
#      改成 422 {"code":42201,"fields":{"file_id":"..."}}。旧前端配新后端时
#      uploadFile() 会抛 ApiError，只弹一条错误、不会跳到阻断页 —— 所以
#      前端和后端要一起上，或者前端先上（新前端认 fields.file_id 就会跳）。
#   2) 上完后必须重启 api 容器，再复跑一次全量验收：
#      node web/e2e/.tmp/verify-all.mjs
#      预期 46 PASS / 1 FAIL / 2 SKIP：只剩 G3（4 页审查 6–9s，口径是
#      "首次 6–9s、sha256 命中缓存 <0.1s"）与 D3/D5（线上造不出 failed、凑不满一页）。
#   3) 历史上"page_count>0 但没有页面"的坏记录（2026-11-18 之前产生的扫描件）
#      不用手工清库：同一份文件再传一次就会按"没解析过"补跑，拿到的仍是 422。

# 3.7 起 TLS 层 + 前端站点（api 的端口已改成 127.0.0.1:8080，公网只看得到 8443）
docker compose -f deploy/docker-compose.yml --env-file .env up -d nginx
# 配置改动（bind mount 进来的 default.conf）up -d 不会重读：容器没变就 Running 跳过。
# 改过 conf 必须重启，否则线上静默跑旧配置 —— 例如新加的 /metrics 404 不会生效。
docker compose -f deploy/docker-compose.yml --env-file .env restart nginx
curl -sk -o /dev/null -w 'root=%{http_code}\n' https://127.0.0.1:8443/          # 必须 200 且是 HTML
curl -sk -o /dev/null -w 'api=%{http_code}\n'  https://127.0.0.1:8443/healthz   # 必须 200 且是 JSON

# 3.8 存储目录属主（Linux 上必做，Windows 本机不用）
#      compose 用 ../storage:/app/storage 把宿主目录盖在镜像的 /app/storage 上，
#      Dockerfile 里那句 chown app:app /app 对 bind mount 不生效。
#      宿主目录默认属主是 root，容器内进程是 uid 10001，写不进去 → 上传接口 500
#      （日志里是 open /app/storage/...: permission denied）。
sudo mkdir -p storage
sudo chown -R 10001:10001 storage
sudo chmod 750 storage
```

> 这一步不能省。Windows 的 bind mount 忽略 uid，
> 所以本机怎么试都正常，第一次上 Linux ECS 就会在上传那一刻炸。

> `deploy/nginx/certs/` 在 `.gitignore` 里：证书与私钥绝不入库。
> 443 用 Let's Encrypt、8443 用自签现在分成两个 server 块，证书路径写在
> `deploy/nginx/default.conf` 里，换证书不用改配置——详见 §3.10。

> `AUTO_MIGRATE` 在 `production` 下会被 config 拒绝，这是故意的：schema 变更必须人工确认。
> 迁移走 `api -migrate`：跑完即退出，不需要装 mysql 客户端，也不会在线上悄悄改表。
> 输出 `迁移完成 applied=N` 即成功；重复执行为 `applied=0`，是幂等的。

# 3.9 用 bundle 传输（服务器 git fetch 报 HTTP2 framing 时的兜底）

服务器上 `git fetch origin` 有时会直接失败：

```
error: RPC failed; curl 16 Error in the HTTP2 framing layer
fatal: expected flush after ref listing
```

这不是网络断了，是 HTTP/2 帧层被中间设备掐掉。兜底做法是把提交打包带到机器上：

```bash
# 本机
git bundle create /tmp/x.bundle main
scp /tmp/x.bundle ubuntu@<主机>:/tmp/

# 服务器
cd /opt/laborlaw
git bundle verify /tmp/x.bundle
git fetch /tmp/x.bundle main:incoming
git update-ref refs/remotes/origin/main incoming   # 这行不能省，否则 remote ref 停在旧位置
git reset --hard incoming
git branch -D incoming                             # 指向已合并的提交，留着只会被误 checkout
```

> 症状：`git branch -vv` 显示 `main … [origin/main: ahead 30]`，而 `git rev-parse --short refs/remotes/origin/main` 指向一个旧提交；
> 后果：下一次 `git pull --ff-only` 或 `git reset --hard origin/main` 会把线上退回旧版本——工作区内容是对的，错的是指针。
> 只补指针：`git update-ref refs/remotes/origin/main <当前 HEAD>`，别 reset、别 checkout。

> `git fetch /tmp/x.bundle main:incoming` 只更新 `incoming` 分支，**不会**动 `refs/remotes/origin/main`。
> 这正是上面那行 `update-ref` 存在的原因：漏了它，`reset --hard origin/main` 会在下一次部署时把线上退回旧版本。

# 3.10 证书（443 公信 + 8443 自签）

443 是域名入口，用 Let's Encrypt；8443 是 IP 直连兜底，用自签。两套并存不是懒，
是证书选择规则决定的：**IP 直连不带 SNI，nginx 只能按端口给默认证书**，
一个 server 块里放两张证书也只会用第一张，所以必须两个块、两对证书。
（历史原因：2026-11-20 之前这两个端口共用一对自签证书，域名访问浏览器会拦一次。）

| 端口 | 证书 | 文件 | 谁在用 |
|---|---|---|---|
| 443 | Let's Encrypt（公信） | `deploy/nginx/certs/fullchain.pem` + `privkey.pem` | 域名 `laborlaw-ai.cn`、`www.laborlaw-ai.cn` |
| 8443 | 自签（LaborLaw Local CA） | `deploy/nginx/certs/server.crt` + `server.key` | IP 直连 `https://<IP>:8443` |

**四个自签文件（`ca.crt` / `ca.key` / `server.crt` / `server.key`）不许删、不许覆盖。**
8443 是答辩兜底入口，删了域名一挂就彻底进不去。

## 3.10.1 443 的证书怎么来的

```bash
# 80 段必须有 acme 豁免（deploy/nginx/default.conf 里 location ^~ /.well-known/acme-challenge/）
# 否则 HTTP-01 校验会被 301 吞掉，certbot 报 "Invalid response"
sudo apt-get install -y certbot
sudo certbot certonly --webroot -w /opt/laborlaw/deploy/acme   -d laborlaw-ai.cn -d www.laborlaw-ai.cn --agree-tos --no-eff-email

# 签出来的在 /etc/letsencrypt/live/laborlaw-ai.cn/，复制进 compose 能看到的目录
sudo cp /etc/letsencrypt/live/laborlaw-ai.cn/fullchain.pem /opt/laborlaw/deploy/nginx/certs/
sudo cp /etc/letsencrypt/live/laborlaw-ai.cn/privkey.pem   /opt/laborlaw/deploy/nginx/certs/
sudo chmod 644 /opt/laborlaw/deploy/nginx/certs/fullchain.pem
sudo chmod 600 /opt/laborlaw/deploy/nginx/certs/privkey.pem
docker exec deploy-nginx-1 nginx -t && docker exec deploy-nginx-1 nginx -s reload
```

验证（`-servername` 不能省，否则拿到的是默认证书）：

```bash
openssl s_client -connect laborlaw-ai.cn:443 -servername laborlaw-ai.cn </dev/null 2>/dev/null   | openssl x509 -noout -issuer -subject -dates
# 期望 issuer 含 Let's Encrypt、subject CN=laborlaw-ai.cn
curl -sI https://laborlaw-ai.cn/ | head -1     # 200，不带 -k
```

## 3.10.2 续期

certbot 自带 `certbot.timer`（每天两次），但**它只更新 `/etc/letsencrypt` 里的文件**，
nginx 用的是 `deploy/nginx/certs/` 里的副本，两者不会自动同步。所以挂了 deploy hook：

```bash
sudo ls /etc/letsencrypt/renewal-hooks/deploy/laborlaw-reload.sh   # 复制副本 + nginx -t + reload
sudo certbot renew --dry-run                                       # 只验续期流程
sudo sh -x /etc/letsencrypt/renewal-hooks/deploy/laborlaw-reload.sh # 验 hook 本身（dry-run 不跑 hook）
```

> certbot 1.21 的 `--dry-run` **不会执行 deploy hook**（新版本才有 `--run-deploy-hooks`），
> 所以 hook 要单独手工跑一次确认。别把"dry-run 通过"当成"续期后 nginx 会自动换证书"。

证书过期后浏览器会直接拦，症状是全站打不开、`curl` 报 `certificate has expired`。
手工续期：`sudo certbot renew`（hook 会自动复制并 reload）。

## 3.10.3 回退到"全自签"

```bash
cd /opt/laborlaw/deploy
sudo cp nginx/default.conf /tmp/default.conf.$(date +%F_%H%M)     # 先备份
# 把 443 那个块的两行证书换回 server.crt / server.key，或直接从 git 取旧版本：
sudo git -C /opt/laborlaw checkout -- deploy/nginx/default.conf
export APP_ENV=production && docker compose --env-file ../.env up -d nginx
docker exec deploy-nginx-1 nginx -t && docker exec deploy-nginx-1 nginx -s reload
```

回退后域名访问会重新出现"您的连接不是私密连接"，点"继续访问"仍可用；
8443 全程不受影响（它一直用自签）。

---

## 4. 验收（每一项都要亲眼看到）

```bash
curl -s localhost:8080/healthz                      # {"status":"ok"}
curl -s localhost:8080/readyz                       # checks 里 mysql/redis 都是 up
docker stats --no-stream                            # 三个容器 RSS，api 应 < 400MB
free -m                                             # available 必须 > 300MB
docker compose -f deploy/docker-compose.yml --env-file .env logs --tail=50 api   # 无 ERROR、有 trace_id

# 存储目录可写（§3.7 没做时这条必挂：permission denied）
curl -k -H "Authorization: Bearer <token>" -F "file=@sample.pdf" \
  https://<公网IP>:8443/api/v1/files                # code=0 且带回 file id
```

浏览器侧（8443，经过 nginx 的 TLS 层）：

```bash
curl -k https://<公网IP>:8443/healthz                 # {"status":"ok"}
curl -k https://<公网IP>:8443/readyz                  # mysql/redis 都是 up
```

> 8080 只绑在宿主机 loopback 上（compose 里写的是 `127.0.0.1:8080:8080`）：
> 在机器上 `curl localhost:8080` 能通，从公网打 8080 必须是不通的。
> 这条要亲测——测试时就遇到过"镜像里还是旧配置"的情况。

SSE 必须逐帧出来，不能最后一次性吐完：

```bash
# 先登录拿 token（中文请求体写 UTF-8 文件再 --data-binary，别用 -d '中文'）
curl -k -N -H 'Authorization: Bearer <token>' -H 'Content-Type: application/json' \
  --data-binary @req.json https://<公网IP>:8443/api/v1/chat/completions
# 期望：先看到 id: / event: meta，随后 event: delta 一行行刷出来；
# 若停顿 30 秒后整屏一起出现，说明 nginx 的 proxy_buffering 没关。
```

指标只在本机抓，公网必须 404：

```bash
# api 发布在 127.0.0.1:8080，只从服务器本机抓
curl -s http://127.0.0.1:8080/metrics
# 期望 7 行（每行前有 # HELP / # TYPE）：uptime、sse_subscribers、sse_streams、
# sse_dropped_total、goroutines、llm_spent_yuan、llm_budget_yuan；
# 没接模型时没有最后两行。
# sse_subscribers 是此刻真正挂着的连接数；sse_streams 是 Hub 跟踪的流数，
# 含 TTL 内已无订阅者的空闲流，会停在下一次 attach 之前。

# 公网入口必须 404——返回 200 HTML 说明 nginx 的 location = /metrics 没生效，
# 监控脚本会把 HTML 当指标读。
curl -k -o /dev/null -w '%{http_code}\n' https://<公网IP>:8443/metrics   # 404
```

---

## 5. 备份、恢复与回滚

### 5.1 备份（每次改动数据前，答辩前必做一次）

命令一律在 `deploy/` 目录下执行，用容器内的 mysql 客户端——宿主机不用装客户端，
密码从容器环境变量里取，不落命令历史：

```bash
docker compose --env-file ../.env exec -T mysql sh -c \
  'mysqldump -uroot -p"$MYSQL_ROOT_PASSWORD" --default-character-set=utf8mb4 laborlaw' \
  > ../backup_$(date +%F_%H%M).sql
```

三件事必须做：

1. **拉回本地留一份**。服务器只有一块云盘，盘坏了备份和库一起没：
   `scp ubuntu@<IP>:/opt/laborlaw/backup_*.sql ./`
2. **导出后复核行数**。不要看到"命令没报错"就当成功——空文件、只导了结构、导到一半断流，
   退出码都可能是 0：

   ```bash
   ls -lh ../backup_*.sql
   grep -c 'INSERT INTO' ../backup_*.sql
   ```

   判据：文件应是几 MB 而不是几 KB；`INSERT INTO` 语句数应是十几条——mysqldump 默认把每张表
   合并成一条 extended insert，所以**不要**用 `grep -c 'INSERT INTO `statutes`'` 去数行数，
   那个数只会是 1。真正的行数判据由 5.2 的演练核，这里只做粗筛。
3. **`--default-character-set=utf8mb4` 不能省**。省掉之后中文法名在客户端侧变成 `????`，
   导出文件看着"成功"，恢复回去就是一片问号。

`scripts/backup.sh` 是上面这段的封装（打文件大小与前三行自检，失败非 0 退出），
但它**不替你 scp**：传输是人的决定。

### 5.2 恢复演练（每学期至少一次，答辩前必做一次）

先给判据，再给命令。判据四条，任何一条不过就是演练失败：

| 判据 | 期望 |
|---|---|
| `statutes` 行数 | 与 `deploy/seed/statutes.json` 的条数一致（当前 517） |
| `embeddings` 行数（`owner_type='statute'`） | 同一条数 |
| `LCL-38` 正文 | 与 `deploy/seed/statutes.json` 逐字一致 |
| `users` / `reviews` / `findings` 行数 | 与源库一致 |

演练导进**临时库**，不碰生产库：

```bash
# 1) 建临时库
docker compose --env-file ../.env exec -T mysql sh -c \
  'mysql -uroot -p"$MYSQL_ROOT_PASSWORD" --default-character-set=utf8mb4 \
     -e "CREATE DATABASE IF NOT EXISTS laborlaw_restore"'

# 2) 把 dump 灌进去
docker compose --env-file ../.env exec -T mysql sh -c \
  'mysql -uroot -p"$MYSQL_ROOT_PASSWORD" --default-character-set=utf8mb4 laborlaw_restore' \
  < ../backup_2026-11-16_2130.sql

# 3) 四条判据逐条核（SQL 走 stdin，避免多层引号打架）
docker compose --env-file ../.env exec -T mysql sh -c \
  'mysql -uroot -p"$MYSQL_ROOT_PASSWORD" --default-character-set=utf8mb4 -N -r' <<'SQL'
select count(*) from laborlaw_restore.statutes;
select count(*) from laborlaw_restore.embeddings where owner_type = 'statute';
select left(text, 60) from laborlaw_restore.statutes where id = 'LCL-38';
select (select count(*) from laborlaw_restore.users),
       (select count(*) from laborlaw_restore.reviews),
       (select count(*) from laborlaw_restore.findings);
SQL

# 4) 演练完删临时库（除非要留着查）
docker compose --env-file ../.env exec -T mysql sh -c \
  'mysql -uroot -p"$MYSQL_ROOT_PASSWORD" --default-character-set=utf8mb4 \
     -e "DROP DATABASE laborlaw_restore"'
```

第 3 条判据的"逐字一致"是拿 `deploy/seed/statutes.json` 里 `LCL-38` 的 `text` 字段比，
不是比条号、不是比前 20 字。`-r`（raw）不能省：批处理模式下 mysql 会把正文里的换行输出成
字面的反斜杠加 n，拿它跟种子文件比一定"不一致"（实测 798 字节 vs 790 字节，差的就是换行）。
`scripts/restore-drill.sh` 把上面四步串成一条命令，逐条打 `期望 / 实际 / 通过?`，
任何一条不过退出码 1。

**演练失败时不许改判据去凑通过**：判据是演练的目的，不是演练的障碍。

演练中途失败时临时库会残留半截数据。重跑前先 `DROP DATABASE IF EXISTS laborlaw_restore`；
不要手工删表——mysqldump 每张表自带 `DROP TABLE IF EXISTS`，重跑本身是幂等的，
手工介入只会让状态更难判断。

### 5.3 回滚（镜像 / 数据库）

```bash
# 镜像回滚：compose 里 image 改成上一个 tag，然后
docker compose -f deploy/docker-compose.yml --env-file .env up -d --force-recreate api

# 数据库回滚：先按 5.1 快照，再手工执行 down 脚本
docker compose --env-file ../.env exec -T mysql sh -c \
  'mysqldump -uroot -p"$MYSQL_ROOT_PASSWORD" --default-character-set=utf8mb4 laborlaw' \
  > ../backup_$(date +%F_%H%M).sql
```

**迁移没有自动 down**：`internal/store/migrations/*.down.sql` 要人工执行，
且执行前必须 mysqldump。这条写在这里是为了防止有人以为 `up` 能撤销。

## 6. 已知取舍（答辩被问到要能答）

| 取舍 | 代价 | 为什么接受 |
|---|---|---|
| `innodb_flush_log_at_trx_commit=2` | 断电可能丢最后 1 秒事务 | 2G 机器上换 IO 与内存；本校项目可接受 |
| 单机无高可用 | 机器挂了服务就挂 | 预算 5000、2 人团队，99% 可用性已写进验收口径 |
| 不装 Grafana/Loki | 只能看 `/metrics` + docker logs | 内存装不下全家桶 |
| MySQL/Redis 与应用同机 | 互相抢内存 | 独立 RDS 一年几千，预算不够 |

---

## 7. 演示当天 Checklist（提前 1 天做一遍）

- [ ] 带宽临时升到 5 Mbps
- [ ] 安全组 8443 对校园网出口放行（在企业/校园网实测一次，有些会拦非标端口）
- [ ] `free -m` available > 300MB，`docker stats` 无异常增长
- [ ] 用真实账号走一遍：登录 → 咨询出字 → 上传合同 → 审查出结果 → 点标注高亮
- [ ] LLM 日预算没被打满（`usage_records` 当日 sum）
- [ ] 演练一次回滚（切到上一个镜像 tag 再切回来）

---

## 8. 排查清单（症状 → 原因 → 处理）

| 症状 | 原因 | 处理 |
|---|---|---|
| 改了 `my.cnf`，`select @@...` 还是默认值（pool 128M、`max_connections` 151、`performance_schema` 1） | MySQL 静默忽略 world-writable 的配置文件，只在启动日志打一条 `World-writable config file '/etc/mysql/conf.d/my.cnf' is ignored` | 确认容器内 `ls -l /etc/mysql/conf.d/my.cnf` 是 644/444。bind mount 在 Windows 上必定 0777、compose `configs` 在 standalone 模式也不改权限，所以本项目改用 `deploy/mysql/Dockerfile` 把文件 COPY 进镜像并 chmod 644；改完必须按 3.4.2 用 `--print-defaults` 验证一次 |
| 上传接口 500，日志 `open /app/storage/...: permission denied` | bind mount 的宿主目录属主是 root，容器内是 uid 10001 | 按 3.7 做 `chown -R 10001:10001 storage` |
| `docker compose exec api /app/api -migrate` 报 `exec: "C:/.../app/api": stat ...` | Git Bash 把 `/app/api` 当成路径改写成 Windows 路径 | 命令前加 `MSYS_NO_PATHCONV=1` |
| 检索恒 0 命中，模型只答"没有检索到法条" | 空库，没导种子 | 按 3.4.1 跑 `--profile seed run --rm seed` |
| 会话列表里刚建的会话排在最后 | `default-time-zone` 生效前后写入的时间混着 CST 与 UTC（8 小时差） | 统一时区口径后清库重灌，或把 `my.cnf` 的 `default-time-zone` 改成 `'+08:00'` 再重跑一次迁移 |

