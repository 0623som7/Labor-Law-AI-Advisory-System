#!/bin/sh
# 恢复演练：把一份 dump 灌进临时库 laborlaw_restore，跑四条判据，然后删掉临时库。
#
# 用法：./scripts/restore-drill.sh <dump文件> [-k]
#   -k  保留 laborlaw_restore（默认演练完就删）
#
# 判据（任何一条不过退出码 1）：
#   1. statutes 行数 = deploy/seed/statutes.json 的条数（当前 517）
#   2. embeddings 行数（owner_type='statute'）= 同一条数
#   3. LCL-38 正文与 deploy/seed/statutes.json 里的 text 字段逐字一致
#   4. users / reviews / findings 行数与源库一致
#
# 判据 1、2 的期望值不写死：都从种子文件数，加一部法之后不用改脚本。
#
# 演练只碰 laborlaw_restore，不碰生产库 laborlaw。
# 密码一律从容器环境变量取，脚本里不出现真实密码。
set -eu

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
ROOT_DIR=$(CDPATH= cd -- "$SCRIPT_DIR/.." && pwd)

usage() {
  echo "用法：$0 <dump文件> [-k]" >&2
  echo "  -k  保留临时库 laborlaw_restore" >&2
}

DUMP=""
KEEP=0
for arg in "$@"; do
  case "$arg" in
    -k) KEEP=1 ;;
    -h|--help) usage; exit 0 ;;
    -*) echo "未知参数：$arg" >&2; usage; exit 2 ;;
    *)
      if [ -z "$DUMP" ]; then DUMP=$arg; else echo "多余参数：$arg" >&2; usage; exit 2; fi
      ;;
  esac
done

if [ -z "$DUMP" ]; then usage; exit 2; fi
if [ ! -f "$DUMP" ]; then echo "找不到 dump 文件：$DUMP" >&2; exit 2; fi

# 下面会 cd 到 deploy/，先把 dump 路径转成绝对路径
DUMP=$(CDPATH= cd -- "$(dirname -- "$DUMP")" && pwd)/$(basename -- "$DUMP")

cd "$ROOT_DIR/deploy"

RESTORE_DB=laborlaw_restore
SRC_DB=laborlaw

# 一条命令干四件事：容器内取密码、强制 utf8mb4、-N 去掉表头与边框、-r 关掉转义。
# -r 不能省：批处理模式下 mysql 会把正文里的换行输出成字面的反斜杠加 n，
# 拿它跟种子文件逐字比一定不等（实测 798 字节 vs 790 字节，差的就是那几处换行）。
mysql_run() {
  docker compose --env-file ../.env exec -T mysql \
    sh -c 'mysql -uroot -p"$MYSQL_ROOT_PASSWORD" --default-character-set=utf8mb4 -N -r'
}

FAIL=0
report() { # 名称 期望 实际
  if [ "$2" = "$3" ]; then
    printf '  %-34s 期望 %-10s 实际 %-10s 通过\n' "$1" "$2" "$3"
  else
    printf '  %-34s 期望 %-10s 实际 %-10s 不过\n' "$1" "$2" "$3"
    FAIL=1
  fi
}

echo "== 1/4 建临时库 $RESTORE_DB"
printf 'CREATE DATABASE IF NOT EXISTS %s;\n' "$RESTORE_DB" | mysql_run

echo "== 2/4 导入 $(basename -- "$DUMP")"
docker compose --env-file ../.env exec -T mysql \
  sh -c "mysql -uroot -p\"\$MYSQL_ROOT_PASSWORD\" --default-character-set=utf8mb4 $RESTORE_DB" \
  < "$DUMP"

echo "== 3/4 四条判据"

# 判据 1、2：行数
# 期望值从种子文件数，不从脚本里写死
EXP_STATUTES=$(python3 -c 'import json,sys
print(len(json.load(open(sys.argv[1],encoding="utf-8"))))' "$ROOT_DIR/deploy/seed/statutes.json")

SRC_STATUTES=$(printf 'select count(*) from %s.statutes;\n' "$SRC_DB" | mysql_run | tr -d '\r')
REST_STATUTES=$(printf 'select count(*) from %s.statutes;\n' "$RESTORE_DB" | mysql_run | tr -d '\r')
report "statutes 行数" "$EXP_STATUTES" "$REST_STATUTES"

REST_VECTORS=$(printf "select count(*) from %s.embeddings where owner_type = 'statute';\n" "$RESTORE_DB" | mysql_run | tr -d '\r')
report "embeddings 行数（statute）" "$EXP_STATUTES" "$REST_VECTORS"

# 判据 3：LCL-38 正文逐字比对（期望值取自种子文件，不取自库）
EXP_LCL38=$(python3 -c 'import json,sys
d=json.load(open(sys.argv[1],encoding="utf-8"))
print(next(x["text"] for x in d if x["id"]=="LCL-38"))' "$ROOT_DIR/deploy/seed/statutes.json")
ACT_LCL38=$(printf 'select text from %s.statutes where id = "LCL-38";\n' "$RESTORE_DB" | mysql_run)

if [ "$EXP_LCL38" = "$ACT_LCL38" ]; then
  report "LCL-38 正文逐字一致" "一致" "一致"
else
  report "LCL-38 正文逐字一致" "一致" "不一致"
  echo "    期望前 40 字：$(printf '%s' "$EXP_LCL38" | head -c 120)"
  echo "    实际前 40 字：$(printf '%s' "$ACT_LCL38" | head -c 120)"
fi

# 判据 4：业务表行数与源库一致
for t in users reviews findings; do
  src=$(printf 'select count(*) from %s.%s;\n' "$SRC_DB" "$t" | mysql_run | tr -d '\r')
  rest=$(printf 'select count(*) from %s.%s;\n' "$RESTORE_DB" "$t" | mysql_run | tr -d '\r')
  report "$t 行数（对比源库）" "$src" "$rest"
done

echo "  （源库 statutes 行数：$SRC_STATUTES，只作参照，不作为判据）"

echo "== 4/4 清理"
if [ "$KEEP" -eq 1 ]; then
  echo "  保留 $RESTORE_DB（-k）"
else
  printf 'DROP DATABASE IF EXISTS %s;\n' "$RESTORE_DB" | mysql_run
  echo "  已删除 $RESTORE_DB"
fi

if [ "$FAIL" -ne 0 ]; then
  echo
  echo "演练失败：有判据不过。不要改判据去凑通过。" >&2
  exit 1
fi

echo
echo "演练通过：四条判据全部命中。"
