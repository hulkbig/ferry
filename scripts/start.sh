#!/usr/bin/env bash
# scripts/start.sh — Ferry (module: ferry) 本机启动脚本
#
# 用法:
#   scripts/start.sh                # 构建 + 启动网关(前台运行, Ctrl-C 优雅退出)
#   scripts/start.sh --config X     # SQLite 为空时的一次性旧配置导入
#   scripts/start.sh --data-dir X   # SQLite 数据目录（默认 ./data）
#   scripts/start.sh --check-only   # 只跑测试不启动(等价 scripts/test.sh)
#   scripts/start.sh --no-build     # 跳过构建,直接用现有二进制启动
#   scripts/start.sh --background   # 后台启动,日志写入 ./data/ferry.log,输出 PID 文件
#   scripts/start.sh stop           # 停止后台实例
#   scripts/start.sh status         # 查看后台实例状态
#
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

GREEN='\033[0;32m'; RED='\033[0;31m'; DIM='\033[2m'; OFF='\033[0m'
info() { printf "${DIM}  %s${OFF}\n" "$*"; }
ok()   { printf "${GREEN}✓ %s${OFF}\n" "$*"; }
fail() { printf "${RED}✗ %s${OFF}\n" "$*"; exit 1; }

BIN="$ROOT/bin/ferry"
CFG="config.example.yaml"
DATA_DIR="$ROOT/data"

command -v go >/dev/null || fail "未找到 go，请先安装 Go"

CONFIG_ONLY=0 NO_BUILD=0 BACKGROUND=0
while [[ $# -gt 0 ]]; do
  case "$1" in
    --config)    CFG="$2"; shift 2 ;;
    --config=*)  CFG="${1#*=}"; shift ;;
    --data-dir)  DATA_DIR="$2"; shift 2 ;;
    --data-dir=*) DATA_DIR="${1#*=}"; shift ;;
    --check-only) CONFIG_ONLY=1; shift ;;
    --no-build)  NO_BUILD=1; shift ;;
    --background|-d) BACKGROUND=1; shift ;;
    stop|status) ACTION="$1"; shift ;;
    *)           fail "未知参数: $1（使用 --config、--data-dir、--check-only、--no-build、--background 或 stop/status）" ;;
  esac
done

mkdir -p "$DATA_DIR"
DATA_DIR="$(cd "$DATA_DIR" && pwd)"
PID_FILE="$DATA_DIR/ferry.pid"
# Recognize a live legacy process before starting or stopping anything. A stale
# PID belonging to another program is never signalled or silently removed.
ACTIVE_PID="" ACTIVE_FILE=""
for candidate_pid_file in "$PID_FILE" "$DATA_DIR/cpa.pid"; do
  [[ -f "$candidate_pid_file" ]] || continue
  candidate_pid="$(cat "$candidate_pid_file")"
  [[ "$candidate_pid" =~ ^[1-9][0-9]*$ ]] || fail "无效 PID 文件: $candidate_pid_file"
  kill -0 "$candidate_pid" 2>/dev/null || continue
  process_command="$(ps -p "$candidate_pid" -o command=)"
  matched=0
  for executable in "$BIN" "$ROOT/bin/cpa" "$ROOT/bin/smartapi"; do
    case "$process_command" in
      "$executable --data-dir $DATA_DIR"|"$executable --data-dir $DATA_DIR --config "*) matched=1 ;;
    esac
  done
  [[ "$matched" == 1 ]] || fail "PID 文件指向无法确认的进程，未执行操作: $candidate_pid_file"
  [[ -z "$ACTIVE_PID" || "$ACTIVE_PID" == "$candidate_pid" ]] || fail "发现两个实例，请分别核对 PID 文件"
  ACTIVE_PID="$candidate_pid" ACTIVE_FILE="$candidate_pid_file"
done
LOG_FILE="$DATA_DIR/ferry.log"

# ---------- 子命令: stop / status ----------
if [[ "${ACTION:-}" == "stop" || "${ACTION:-}" == "status" ]]; then
  [[ -n "$ACTIVE_PID" ]] || fail "没有已确认的后台实例: $DATA_DIR"
  PID="$ACTIVE_PID"; PID_FILE="$ACTIVE_FILE"
  if [[ "$ACTION" == "status" ]]; then
    ok "Ferry 运行中: pid=$PID  控制面=$DATA_DIR/control.db"
    exit 0
  fi
  info "发送 SIGTERM 给 $PID …"
  kill "$PID"
  for _ in $(seq 1 20); do
    kill -0 "$PID" 2>/dev/null || { rm -f "$PID_FILE"; ok "已停止"; exit 0; }
    sleep 0.5
  done
  fail "进程尚未退出，保留 PID 文件；请核对日志后处理"

fi

mkdir -p "$DATA_DIR"
RUN_ARGS=(--data-dir "$DATA_DIR")
# The database file may already exist after an interrupted first run. Let
# the read-only startup check inspect active listener configuration instead of guessing from
# file existence. An initialized database ignores the import path.
RUN_ARGS+=(--config "$CFG")

# ---------- 仅测试模式 ----------
if [[ "$CONFIG_ONLY" == "1" ]]; then
  exec "$ROOT/scripts/test.sh"
fi

[[ -z "$ACTIVE_PID" ]] || fail "已在运行: pid=$ACTIVE_PID（先使用同一 data-dir 执行 stop）"

# ---------- 构建 ----------
if [[ "$NO_BUILD" != "1" ]]; then
  printf '\n%b▶ 构建中…%b\n' "$GREEN" "$OFF"
  mkdir -p "$ROOT/bin"
  go build -o "$BIN" ./cmd/ferry || fail "go build 失败"
  ok "构建完成: bin/ferry"
else
  [[ -x "$BIN" ]] || fail "--no-build 但二进制不存在: $BIN"
  info "--no-build: 使用现有 $BIN"
fi

# ---------- 只读预检；数据库升级仅由正常服务启动执行 ----------
printf '\n%b▶ 读取监听配置 %s/control.db…%b\n' "$GREEN" "$DATA_DIR" "$OFF"
"$BIN" "${RUN_ARGS[@]}" --startup-check || fail "监听配置预检失败（见上方日志）"
ok "监听配置有效；启动服务并自动升级 SQLite"

# ---------- 启动 ----------
if [[ "$BACKGROUND" == "1" ]]; then
  if [[ -f "$PID_FILE" ]] && kill -0 "$(cat "$PID_FILE")" 2>/dev/null; then
    fail "已在后台运行: pid=$(cat "$PID_FILE")（scripts/start.sh stop 先停止）"
  fi
  nohup "$BIN" "${RUN_ARGS[@]}" >>"$LOG_FILE" 2>&1 &
  echo $! > "$PID_FILE"
  sleep 1
  PID="$(cat "$PID_FILE")"
  kill -0 "$PID" 2>/dev/null || { tail -5 "$LOG_FILE"; fail "启动失败（见 $LOG_FILE）"; }
  ok "Ferry 已后台启动: pid=${PID}"
  info "监听地址来自 SQLite active config；进入管理台可查看服务信息"
  info "日志: tail -f ${LOG_FILE}"
  info "停止: scripts/start.sh stop"
else
  printf '\n%b▶ 启动 Ferry…%b\n' "$GREEN" "$OFF"
  info "监听地址来自 SQLite active config；进入管理台可查看服务信息"
  info "Ctrl-C 优雅退出\n"
  exec "$BIN" "${RUN_ARGS[@]}"
fi
