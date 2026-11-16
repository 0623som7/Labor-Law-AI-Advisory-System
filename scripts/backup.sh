#!/bin/sh
# 导出 laborlaw 库到一份 SQL 文件，并做一次最低限度的自检。
#
# 用法：./scripts/backup.sh [输出目录]
#   输出目录默认是仓库根目录，文件名为 laborlaw_YYYY-MM-DD_HHMM.sql。
#
# 只负责导出，不 scp：传输是人的决定（服务器只有一块云盘，盘坏了备份和库一起没，
# 所以导出后一定要拉回本地留一份）。
#
# 依赖：服务器上有 docker + docker compose，且 .env 里配好了 MYSQL_ROOT_PASSWORD。
set -eu

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
ROOT_DIR=$(CDPATH= cd -- "$SCRIPT_DIR/.." && pwd)
OUT_DIR=${1:-$ROOT_DIR}
OUT_FILE="$OUT_DIR/laborlaw_$(date +%F_%H%M).sql"

mkdir -p "$OUT_DIR"

# compose 的 --env-file 是相对 deploy/ 的，所以先 cd 过去再执行
cd "$ROOT_DIR/deploy"

if ! docker compose --env-file ../.env exec -T mysql sh -c \
  'mysqldump -uroot -p"$MYSQL_ROOT_PASSWORD" --default-character-set=utf8mb4 laborlaw' \
  > "$OUT_FILE"; then
  echo "备份失败：mysqldump 退出码非 0，$OUT_FILE 不可用" >&2
  exit 1
fi

SIZE=$(wc -c < "$OUT_FILE" | tr -d ' ')

echo "备份文件：$OUT_FILE"
echo "大小：$SIZE 字节"
echo "前三行："
head -3 "$OUT_FILE"

# 空文件 / 只导了结构 / 导到一半断流，退出码都可能是 0，所以再看一眼大小
if [ "$SIZE" -lt 1024 ]; then
  echo "备份文件过小（$SIZE 字节），疑似失败" >&2
  exit 1
fi

echo
echo "下一步：拉回本地留一份 —— scp <user>@<IP>:$OUT_FILE ./"
