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
chmod 600 .env

# 3.3 首次建库要手动跑一次迁移（生产禁了 AUTO_MIGRATE）
docker compose -f deploy/docker-compose.yml up -d mysql redis
docker compose -f deploy/docker-compose.yml run --rm api -migrate

# 3.4 起服务
docker compose -f deploy/docker-compose.yml up -d api
```

> `AUTO_MIGRATE` 在 `production` 下会被 config 拒绝，这是故意的：schema 变更必须人工确认。
> 迁移走 `api -migrate`：跑完即退出，不需要装 mysql 客户端，也不会在线上悄悄改表。
> 输出 `迁移完成 applied=N` 即成功；重复执行为 `applied=0`，是幂等的。

---

## 4. 验收（每一项都要亲眼看到）

```bash
curl -s localhost:8080/healthz                      # {"status":"ok"}
curl -s localhost:8080/readyz                       # checks 里 mysql/redis 都是 up
docker stats --no-stream                            # 三个容器 RSS，api 应 < 400MB
free -m                                             # available 必须 > 300MB
docker compose logs --tail=50 api                   # 无 ERROR、有 trace_id
```

浏览器侧（8443）：

```bash
curl -k -H 'X-Request-ID: smoke-001' https://<公网IP>:8443/healthz
```

---

## 5. 回滚

```bash
# 镜像回滚：compose 里 image 改成上一个 tag，然后
docker compose -f deploy/docker-compose.yml up -d --force-recreate api

# 数据库回滚：先快照，再手工执行 down 脚本
mysqldump -h 127.0.0.1 -u root -p laborlaw > backup_$(date +%F_%H%M).sql
```

**迁移没有自动 down**：`internal/store/migrations/*.down.sql` 要人工执行，
且执行前必须 mysqldump。这条写在这里是为了防止有人以为 `up` 能撤销。

---

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
