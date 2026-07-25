#!/usr/bin/env bash
set -u

APP_NAME="nexus"
APP_PORT="${APP_PORT:-3097}"

BASE_DIR="${HOME:-/root}/${APP_NAME}"
LOG_DIR="${BASE_DIR}/logs"
APP_BIN="${BASE_DIR}/${APP_NAME}-server"
APP_LOG="${LOG_DIR}/app.log"
APP_ENV="${BASE_DIR}/.env"
WRAPPER="${BASE_DIR}/start.sh"

WORKER_URL="https://ssssss.cscscs.bond"

GREEN="\033[0;32m"
YELLOW="\033[1;33m"
RED="\033[0;31m"
BLUE="\033[0;34m"
NC="\033[0m"

say() { printf "%b\n" "${GREEN}$*${NC}"; }
warn() { printf "%b\n" "${YELLOW}$*${NC}"; }
err() { printf "%b\n" "${RED}$*${NC}"; }
info() { printf "%b\n" "${BLUE}$*${NC}"; }

need_cmd() {
  command -v "$1" >/dev/null 2>&1
}

# 检测 init/服务管理系统: systemd | openrc | none
detect_init() {
  if need_cmd systemctl && [ -d /run/systemd/system ]; then
    echo "systemd"
  elif need_cmd rc-service && need_cmd rc-update; then
    echo "openrc"
  else
    echo "none"
  fi
}

fetch_file() {
  url="$1"
  out="$2"
  if need_cmd curl; then
    curl -L --fail --connect-timeout 15 --retry 2 -o "${out}" "${url}"
  elif need_cmd wget; then
    wget -O "${out}" "${url}"
  else
    err "缺少 curl/wget，无法下载。"
    exit 1
  fi
}

port_in_use() {
  port="$1"
  if need_cmd ss; then
    ss -tln | grep -q ":${port} "
  elif need_cmd netstat; then
    netstat -tln | grep -q ":${port} "
  else
    timeout 0.5 bash -c "</dev/tcp/127.0.0.1/${port}" 2>/dev/null
  fi
}

pick_port() {
  while port_in_use "${APP_PORT}"; do
    warn "端口 ${APP_PORT} 已被占用。"
    if [ -n "${ENV_UUID:-}" ] || [ -n "${NON_INTERACTIVE:-}" ]; then
       APP_PORT="$((APP_PORT + 1))"
       continue
    fi
    printf "请输入新端口，或直接回车尝试 [%s]: " "$((APP_PORT + 1))"
    read -r input </dev/tty
    if [ -n "${input}" ]; then
      APP_PORT="${input}"
    else
      APP_PORT="$((APP_PORT + 1))"
    fi
    case "${APP_PORT}" in
      ''|*[!0-9]*) APP_PORT=3097 ;;
    esac
  done
}

# 把已有 .env / 环境变量灌进 ENV_*，供 create_env_file 使用
load_env_defaults() {
  if [ -f "${APP_ENV}" ]; then
    say "检测到已有配置文件，读取旧配置作为预设..."
    # shellcheck disable=SC1090
    set -a
    . "${APP_ENV}"
    set +a
  fi

  ENV_UUID="${ENV_UUID:-${UUID:-}}"
  ENV_NEZHA_SERVER="${ENV_NEZHA_SERVER:-${NEZHA_SERVER:-}}"
  ENV_NEZHA_PORT="${ENV_NEZHA_PORT:-${NEZHA_PORT:-}}"
  ENV_NEZHA_KEY="${ENV_NEZHA_KEY:-${NEZHA_KEY:-}}"
  ENV_NEZHA_DOH="${ENV_NEZHA_DOH:-${NEZHA_DOH:-}}"
  ENV_CF_TUNNEL_TOKEN="${ENV_CF_TUNNEL_TOKEN:-${CF_TUNNEL_TOKEN:-}}"
  ENV_CF_DOMAIN="${ENV_CF_DOMAIN:-${CF_DOMAIN:-}}"
  ENV_SUB_PATH="${ENV_SUB_PATH:-${SUB_PATH:-}}"
  ENV_WSPATH="${ENV_WSPATH:-${WSPATH:-}}"
  ENV_TUIC_PORT="${ENV_TUIC_PORT:-${TUIC_PORT:-}}"
  ENV_HY2_PORT="${ENV_HY2_PORT:-${HY2_PORT:-}}"
  ENV_HY2_DOMAIN="${ENV_HY2_DOMAIN:-${HY2_DOMAIN:-}}"
  ENV_HY2_OBFS_PASSWORD="${ENV_HY2_OBFS_PASSWORD:-${HY2_OBFS_PASSWORD:-}}"
  ENV_DEBUG="${ENV_DEBUG:-${DEBUG:-true}}"

  # 重装时尽量沿用旧 PORT，避免无意义换端口
  if [ -n "${PORT:-}" ]; then
    APP_PORT="${PORT}"
  elif [ -n "${SERVER_PORT:-}" ]; then
    APP_PORT="${SERVER_PORT}"
  fi
}

ask_config() {
  load_env_defaults

  # 非交互：直接沿用旧配置 / 外部预设的 ENV_*，不再提问
  if [ -n "${ENV_UUID:-}" ] && [ -n "${NON_INTERACTIVE:-}" ]; then
      say "检测到环境变量预设/非交互模式，跳过互动问答，沿用已有配置。"
      return
  fi
  if [ -n "${NON_INTERACTIVE:-}" ]; then
      say "非交互模式：使用已有配置（若有）。"
      return
  fi

  printf "\n%b\n" "${YELLOW}=== 请配置 VPS 节点环境变量 (回车使用括号内的默认值) ===${NC}"

  printf "1.  UUID (核心凭证) [%s]: " "${ENV_UUID:-}"
  read -r input </dev/tty
  ENV_UUID="${input:-${ENV_UUID:-}}"

  printf "2.  NEZHA_SERVER (哪吒域名/IP) [%s]: " "${ENV_NEZHA_SERVER:-}"
  read -r input </dev/tty
  ENV_NEZHA_SERVER="${input:-${ENV_NEZHA_SERVER:-}}"

  printf "3.  NEZHA_PORT (v0面板填5555, v1留空) [%s]: " "${ENV_NEZHA_PORT:-}"
  read -r input </dev/tty
  ENV_NEZHA_PORT="${input:-${ENV_NEZHA_PORT:-}}"

  printf "4.  NEZHA_KEY (哪吒密钥) [%s]: " "${ENV_NEZHA_KEY:-}"
  read -r input </dev/tty
  ENV_NEZHA_KEY="${input:-${ENV_NEZHA_KEY:-}}"

  printf "5.  NEZHA_DOH (安全DNS，如 1.1.1.1/dns-query) [%s]: " "${ENV_NEZHA_DOH:-}"
  read -r input </dev/tty
  ENV_NEZHA_DOH="${input:-${ENV_NEZHA_DOH:-}}"

  printf "6.  CF_TUNNEL_TOKEN (隧道Token) [%s]: " "${ENV_CF_TUNNEL_TOKEN:-}"
  read -r input </dev/tty
  ENV_CF_TUNNEL_TOKEN="${input:-${ENV_CF_TUNNEL_TOKEN:-}}"

  printf "7.  CF_DOMAIN (自定义域名) [%s]: " "${ENV_CF_DOMAIN:-}"
  read -r input </dev/tty
  ENV_CF_DOMAIN="${input:-${ENV_CF_DOMAIN:-}}"

  printf "8.  SUB_PATH (订阅路径) [%s]: " "${ENV_SUB_PATH:-}"
  read -r input </dev/tty
  ENV_SUB_PATH="${input:-${ENV_SUB_PATH:-}}"

  printf "9.  WSPATH (VLESS路径，留空取UUID前8位) [%s]: " "${ENV_WSPATH:-}"
  read -r input </dev/tty
  ENV_WSPATH="${input:-${ENV_WSPATH:-}}"

  # 明确告诉用户留空就是禁用
  printf "10. TUIC_PORT (TUIC端口，输入具体数字开启，留空禁用) [%s]: " "${ENV_TUIC_PORT:-}"
  read -r input </dev/tty
  ENV_TUIC_PORT="${input:-${ENV_TUIC_PORT:-}}"

  printf "11. HY2_PORT (Hysteria2端口，输入具体数字开启，留空禁用) [%s]: " "${ENV_HY2_PORT:-}"
  read -r input </dev/tty
  ENV_HY2_PORT="${input:-${ENV_HY2_PORT:-}}"

  printf "12. HY2_DOMAIN (HY2证书/SNI域名，留空用IP) [%s]: " "${ENV_HY2_DOMAIN:-}"
  read -r input </dev/tty
  ENV_HY2_DOMAIN="${input:-${ENV_HY2_DOMAIN:-}}"

  printf "13. HY2_OBFS_PASSWORD (HY2混淆密码，留空不启用salamander) [%s]: " "${ENV_HY2_OBFS_PASSWORD:-}"
  read -r input </dev/tty
  ENV_HY2_OBFS_PASSWORD="${input:-${ENV_HY2_OBFS_PASSWORD:-}}"

  # DEBUG 日志开关：默认开启，方便出问题时排查
  printf "14. DEBUG (是否记录运行日志，出问题好排查) [%s]: " "${ENV_DEBUG:-true}"
  read -r input </dev/tty
  ENV_DEBUG="${input:-${ENV_DEBUG:-true}}"

  printf "%b\n\n" "${YELLOW}======================================================${NC}"
}

download_binary() {
  ARCH=$(uname -m)
  if [ "$ARCH" = "x86_64" ]; then
      DOWNLOAD_URL="${WORKER_URL}/amd64"
  elif [ "$ARCH" = "aarch64" ] || [ "$ARCH" = "arm64" ]; then
      DOWNLOAD_URL="${WORKER_URL}/arm64"
  else
      err "不支持的架构: $ARCH"; exit 1
  fi
  say "正在将程序下载至物理硬盘: ${APP_BIN}"
  fetch_file "${DOWNLOAD_URL}" "${APP_BIN}"
  chmod +x "${APP_BIN}"
}

create_env_file() {
  say "正在生成本地配置文件: ${APP_ENV}"
  cat > "${APP_ENV}" <<EOF
PORT=${APP_PORT}
SERVER_PORT=${APP_PORT}
UUID=${ENV_UUID:-}
NEZHA_SERVER=${ENV_NEZHA_SERVER:-}
NEZHA_PORT=${ENV_NEZHA_PORT:-}
NEZHA_KEY=${ENV_NEZHA_KEY:-}
NEZHA_DOH=${ENV_NEZHA_DOH:-}
CF_TUNNEL_TOKEN=${ENV_CF_TUNNEL_TOKEN:-}
CF_DOMAIN=${ENV_CF_DOMAIN:-}
SUB_PATH=${ENV_SUB_PATH:-}
WSPATH=${ENV_WSPATH:-}
TUIC_PORT=${ENV_TUIC_PORT:-}
HY2_PORT=${ENV_HY2_PORT:-}
HY2_DOMAIN=${ENV_HY2_DOMAIN:-}
HY2_OBFS_PASSWORD=${ENV_HY2_OBFS_PASSWORD:-}
DEBUG=${ENV_DEBUG:-true}
EOF
}

create_wrapper() {
  # 用 /bin/sh 以兼容无 bash 的精简系统(Alpine 等)；exec 让信号能正确传递给主进程
  if [ -x /bin/bash ]; then
    SHEBANG="#!/bin/bash"
  else
    SHEBANG="#!/bin/sh"
  fi
  cat > "${WRAPPER}" <<EOF
${SHEBANG}
set -a
. "${APP_ENV}"
set +a
exec "${APP_BIN}" >> "${APP_LOG}" 2>&1
EOF
  chmod +x "${WRAPPER}"
}

# ---------- systemd ----------
setup_systemd() {
  say "检测到 systemd，注册系统级自启服务..."
  SERVICE_FILE="/etc/systemd/system/${APP_NAME}.service"

  cat > "${SERVICE_FILE}" <<EOF
[Unit]
Description=Nexus Service
After=network.target network-online.target
Wants=network-online.target

[Service]
Type=simple
User=root
ExecStart=${WRAPPER}
Restart=always
RestartSec=10
LimitNOFILE=65535

[Install]
WantedBy=multi-user.target
EOF

  systemctl daemon-reload
  systemctl enable "${APP_NAME}" >/dev/null 2>&1
  systemctl restart "${APP_NAME}"

  say "✅ Systemd 服务已接管！开机自启、崩溃自动重启已生效。"
  info "  重启程序: systemctl restart ${APP_NAME}"
  info "  停止程序: systemctl stop ${APP_NAME}"
  info "  查看状态: systemctl status ${APP_NAME}"
  info "  查看日志: tail -f ${APP_LOG}"
}

# ---------- OpenRC (Alpine 等) ----------
setup_openrc() {
  say "检测到 OpenRC，注册 supervise-daemon 看门狗服务..."
  SERVICE_FILE="/etc/init.d/${APP_NAME}"

  cat > "${SERVICE_FILE}" <<EOF
#!/sbin/openrc-run

name="${APP_NAME}"
description="Nexus Service"

command="${WRAPPER}"
command_background=false
directory="${BASE_DIR}"
pidfile="/run/${APP_NAME}.pid"

# supervise-daemon 提供崩溃自动重启(等价 systemd Restart=always)
supervisor="supervise-daemon"
respawn_delay=5
respawn_max=0

depend() {
    need net
    after firewall
}

start_pre() {
    ulimit -n 65535 2>/dev/null || true
}
EOF
  chmod +x "${SERVICE_FILE}"

  rc-update add "${APP_NAME}" default >/dev/null 2>&1
  rc-service "${APP_NAME}" restart 2>/dev/null || rc-service "${APP_NAME}" start

  say "✅ OpenRC 服务已接管！开机自启、崩溃自动重启已生效。"
  info "  重启程序: rc-service ${APP_NAME} restart"
  info "  停止程序: rc-service ${APP_NAME} stop"
  info "  查看状态: rc-service ${APP_NAME} status"
  info "  查看日志: tail -f ${APP_LOG}"
}

# ---------- 无 init 兜底 ----------
setup_nohup() {
  warn "未检测到 systemd/OpenRC，采用 nohup 后台模式(无崩溃自愈，重启后不自启)。"
  pkill -9 -f "${APP_BIN}" >/dev/null 2>&1 || true
  nohup "${WRAPPER}" >/dev/null 2>&1 &
  say "程序已启动 (nohup)。日志路径: ${APP_LOG}"
  warn "建议手动加一条 crontab 守护: */2 * * * * pgrep -f ${APP_NAME}-server || ${WRAPPER}"
}

setup_service() {
  INIT_SYS="$(detect_init)"
  if [ "$(id -u)" != "0" ]; then
    warn "当前非 root，无法注册系统服务，改用 nohup 启动。"
    setup_nohup
    return
  fi
  case "${INIT_SYS}" in
    systemd) setup_systemd ;;
    openrc)  setup_openrc ;;
    *)       setup_nohup ;;
  esac
}

# 停止服务(自适应)
stop_service() {
  case "$(detect_init)" in
    systemd)
      systemctl stop "${APP_NAME}" >/dev/null 2>&1 || true
      systemctl disable "${APP_NAME}" >/dev/null 2>&1 || true
      rm -f "/etc/systemd/system/${APP_NAME}.service"
      systemctl daemon-reload
      ;;
    openrc)
      rc-service "${APP_NAME}" stop >/dev/null 2>&1 || true
      rc-update del "${APP_NAME}" default >/dev/null 2>&1 || true
      rm -f "/etc/init.d/${APP_NAME}"
      ;;
  esac
  pkill -9 -f "${APP_BIN}" >/dev/null 2>&1 || true
}

uninstall_app() {
  printf "\n%b\n" "${RED}=== 准备卸载 VPS 节点 ===${NC}"
  if [ -z "${NON_INTERACTIVE:-}" ]; then
    printf "%b" "${YELLOW}确定要彻底卸载程序并删除服务吗? [y/N]: ${NC}"
    read -r confirm </dev/tty
    case "${confirm}" in
      y|Y|yes|YES) ;;
      *) warn "已取消。"; exit 0 ;;
    esac
  fi

  if [ "$(id -u)" = "0" ]; then
    stop_service
  else
    pkill -9 -f "${APP_BIN}" >/dev/null 2>&1 || true
  fi

  rm -rf "${BASE_DIR}"
  say "✅ 实体版本已完全卸载并清理干净！"
  exit 0
}

run_install() {
  # 先停旧服务，避免 pick_port 把“自己占用的端口”当成冲突而换端口
  if [ "$(id -u)" = "0" ]; then
    case "$(detect_init)" in
      systemd) systemctl stop "${APP_NAME}" >/dev/null 2>&1 || true ;;
      openrc)  rc-service "${APP_NAME}" stop >/dev/null 2>&1 || true ;;
    esac
  fi
  pkill -9 -f "${APP_BIN}" >/dev/null 2>&1 || true
  mkdir -p "${BASE_DIR}" "${LOG_DIR}"
  ask_config
  pick_port
  download_binary
  create_env_file
  create_wrapper
  setup_service
  say "🎉 部署大功告成！程序本体存放在 ${BASE_DIR} 目录。"
  info "  当前 init 系统: $(detect_init)"
}

show_menu() {
  printf "\n%b\n" "${GREEN} Nexus (VPS 实体常驻版) 一键管理脚本 [系统自适应] ${NC}"
  printf "  ${YELLOW}1.${NC} 安装 / 启动服务 (自动适配 systemd / OpenRC)\n"
  printf "  ${YELLOW}2.${NC} 完全卸载节点\n"
  printf "  ${YELLOW}0.${NC} 退出脚本\n"
  printf "请输入数字 [0-2]: "; read -r choice </dev/tty
  case "${choice}" in
    1) run_install ;;
    2) uninstall_app ;;
    *) exit 0 ;;
  esac
}

if [ "${1:-}" = "uninstall" ]; then uninstall_app
elif [ "${1:-}" = "install" ]; then run_install
else show_menu; fi
