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
  ENV_UDP_IPV6_ONLY="${ENV_UDP_IPV6_ONLY:-${UDP_IPV6_ONLY:-false}}"
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

  printf "14. UDP_IPV6_ONLY (TUIC/HY2 仅IPv6监听 true/false，默认双栈) [%s]: " "${ENV_UDP_IPV6_ONLY:-false}"
  read -r input </dev/tty
  ENV_UDP_IPV6_ONLY="${input:-${ENV_UDP_IPV6_ONLY:-false}}"

  # DEBUG 日志开关：默认开启，方便出问题时排查
  printf "15. DEBUG (是否记录运行日志，出问题好排查) [%s]: " "${ENV_DEBUG:-true}"
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
UDP_IPV6_ONLY=${ENV_UDP_IPV6_ONLY:-false}
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

urlencode() {
  # 最小 URL 编码，够节点链接参数用
  if need_cmd python3; then
    python3 -c 'import urllib.parse,sys; print(urllib.parse.quote(sys.argv[1], safe=""))' "$1"
  elif need_cmd python; then
    python -c 'import urllib.parse,sys; print(urllib.parse.quote(sys.argv[1], safe=""))' "$1" 2>/dev/null \
      || python -c 'import urllib,sys; print(urllib.quote(sys.argv[1], safe=""))' "$1"
  else
    # 无 python 时原样输出（UUID/密码通常无需编码）
    printf '%s' "$1"
  fi
}

fetch_public_ip() {
  if need_cmd curl; then
    curl -fsS --connect-timeout 5 https://api-ipv4.ip.sb/ip 2>/dev/null | tr -d '[:space:]'
  elif need_cmd wget; then
    wget -qO- --timeout=5 https://api-ipv4.ip.sb/ip 2>/dev/null | tr -d '[:space:]'
  fi
}

fetch_public_ipv6() {
  local ip=""
  local u
  for u in \
    "https://api-ipv6.ip.sb/ip" \
    "https://api6.ipify.org" \
    "https://v6.ident.me"
  do
    if need_cmd curl; then
      ip="$(curl -fsS --connect-timeout 5 "$u" 2>/dev/null | tr -d '[:space:]')"
    elif need_cmd wget; then
      ip="$(wget -qO- --timeout=5 "$u" 2>/dev/null | tr -d '[:space:]')"
    fi
    # 粗判 IPv6
    if [ -n "$ip" ] && printf '%s' "$ip" | grep -q ':'; then
      printf '%s' "$ip"
      return 0
    fi
  done
  return 1
}

# URI host：IPv6 包 []，域名/IPv4 原样
format_uri_host() {
  local h="$1"
  case "$h" in
    \[*\]) printf '%s' "$h" ;;
    *:*)   printf '[%s]' "$h" ;;
    *)     printf '%s' "$h" ;;
  esac
}

is_truthy() {
  case "$(printf '%s' "${1:-}" | tr '[:upper:]' '[:lower:]')" in
    1|true|yes|on) return 0 ;;
    *) return 1 ;;
  esac
}

# 重启服务（装完改 .env 后用）
restart_app() {
  if [ "$(id -u)" != "0" ]; then
    warn "非 root，尝试 pkill 后 nohup 拉起..."
    pkill -9 -f "${APP_BIN}" >/dev/null 2>&1 || true
    nohup "${WRAPPER}" >/dev/null 2>&1 &
    return
  fi
  case "$(detect_init)" in
    systemd) systemctl restart "${APP_NAME}" ;;
    openrc)  rc-service "${APP_NAME}" restart ;;
    *)
      pkill -9 -f "${APP_BIN}" >/dev/null 2>&1 || true
      nohup "${WRAPPER}" >/dev/null 2>&1 &
      ;;
  esac
}

# 菜单/命令行：切换 UDP_IPV6_ONLY 并写回 .env、重启
set_udp_ipv6_only() {
  local want="${1:-}"
  if [ ! -f "${APP_ENV}" ]; then
    err "未找到 ${APP_ENV}，请先安装。"
    return 1
  fi
  if [ -z "$want" ]; then
    # shellcheck disable=SC1090
    . "${APP_ENV}"
    printf "当前 UDP_IPV6_ONLY=%s\n" "${UDP_IPV6_ONLY:-false}"
    printf "输入 true(仅IPv6) 或 false(双栈) [当前 %s]: " "${UDP_IPV6_ONLY:-false}"
    read -r want </dev/tty
    want="${want:-${UDP_IPV6_ONLY:-false}}"
  fi
  case "$(printf '%s' "$want" | tr '[:upper:]' '[:lower:]')" in
    1|true|yes|on) want=true ;;
    0|false|no|off) want=false ;;
    *) err "无效值: $want（请用 true/false）"; return 1 ;;
  esac

  if grep -qE '^UDP_IPV6_ONLY=' "${APP_ENV}" 2>/dev/null; then
    # 兼容 sed -i 在 GNU/BusyBox
    if sed --version >/dev/null 2>&1; then
      sed -i "s/^UDP_IPV6_ONLY=.*/UDP_IPV6_ONLY=${want}/" "${APP_ENV}"
    else
      sed -i '' "s/^UDP_IPV6_ONLY=.*/UDP_IPV6_ONLY=${want}/" "${APP_ENV}" 2>/dev/null \
        || sed -i "s/^UDP_IPV6_ONLY=.*/UDP_IPV6_ONLY=${want}/" "${APP_ENV}"
    fi
  else
    printf '\nUDP_IPV6_ONLY=%s\n' "${want}" >> "${APP_ENV}"
  fi
  say "已写入 UDP_IPV6_ONLY=${want} → ${APP_ENV}"
  restart_app
  say "服务已重启。当前模式: $([ "$want" = true ] && echo 'IPv6 only' || echo '双栈')"
  sleep 1
  show_nodes || true
}

# 根据 .env 在终端打印各协议节点链接（方便只拷某一个）
show_nodes() {
  if [ ! -f "${APP_ENV}" ]; then
    err "未找到配置文件: ${APP_ENV}"
    err "请先安装，或确认路径正确。"
    return 1
  fi

  # shellcheck disable=SC1090
  set -a
  . "${APP_ENV}"
  set +a

  local uuid port sub_path ws_path name
  local tuic_port tuic_domain tuic_pass
  local hy2_port hy2_domain hy2_pass hy2_obfs
  local cf_domain public_ip public_ip6 host sni host_uri udp_mode
  local vless_url cf_vless_url tuic_url hy2_url sub_url
  local enc_uuid enc_pass enc_name

  uuid="${UUID:-}"
  if [ -z "${uuid}" ]; then
    err ".env 里没有 UUID，无法生成节点。"
    return 1
  fi

  port="${PORT:-${SERVER_PORT:-${APP_PORT}}}"
  sub_path="${SUB_PATH:-sub}"
  sub_path="${sub_path#/}"
  ws_path="${WSPATH:-}"
  if [ -z "${ws_path}" ]; then
    ws_path="${uuid:0:8}"
  fi
  ws_path="${ws_path#/}"
  name="${NAME:-Nexus}"

  tuic_port="${TUIC_PORT:-}"
  tuic_domain="${TUIC_DOMAIN:-}"
  tuic_pass="${TUIC_PASSWORD:-${uuid}}"

  hy2_port="${HY2_PORT:-}"
  hy2_domain="${HY2_DOMAIN:-${TUIC_DOMAIN:-}}"
  hy2_pass="${HY2_PASSWORD:-${uuid}}"
  hy2_obfs="${HY2_OBFS_PASSWORD:-}"

  cf_domain="${CF_DOMAIN:-}"
  if is_truthy "${UDP_IPV6_ONLY:-false}"; then
    udp_mode="IPv6 only"
  else
    udp_mode="双栈"
  fi

  public_ip="$(fetch_public_ip || true)"
  public_ip6="$(fetch_public_ipv6 || true)"
  if [ -z "${public_ip}" ]; then
    public_ip="YOUR_SERVER_IP"
    warn "无法自动获取公网 IPv4，直连 VLESS 请自行替换 YOUR_SERVER_IP。"
  fi
  if is_truthy "${UDP_IPV6_ONLY:-false}" && [ -z "${public_ip6}" ]; then
    warn "UDP_IPV6_ONLY=true 但未能获取公网 IPv6，TUIC/HY2 请填域名或手动替换。"
    public_ip6="YOUR_IPV6"
  fi

  printf "\n%b\n" "${GREEN}======== Nexus 节点链接 ========${NC}"
  info "配置: ${APP_ENV}"
  info "本机 Web 端口: ${port}  |  订阅路径: /${sub_path}  |  WSPATH: /${ws_path}"
  info "TUIC/HY2 监听模式: ${udp_mode}  (UDP_IPV6_ONLY=${UDP_IPV6_ONLY:-false})"
  echo

  # --- VLESS 直连（IP:PORT，无 TLS）---
  printf "%b\n" "${YELLOW}[1] VLESS-WS 直连（IPv4，不经 CF）${NC}"
  vless_url="vless://${uuid}@${public_ip}:${port}?encryption=none&security=none&type=ws&host=${public_ip}&path=%2F${ws_path}#${name}-VLESS"
  printf "%s\n\n" "${vless_url}"

  # --- VLESS via CF ---
  if [ -n "${cf_domain}" ]; then
    printf "%b\n" "${YELLOW}[2] VLESS-WS + CF 域名（推荐走隧道/CDN）${NC}"
    cf_vless_url="vless://${uuid}@${cf_domain}:443?encryption=none&security=tls&sni=${cf_domain}&fp=chrome&type=ws&host=${cf_domain}&path=%2F${ws_path}#${name}-CF-VLESS"
    printf "%s\n\n" "${cf_vless_url}"

    printf "%b\n" "${YELLOW}[订阅地址]（客户端一键导入）${NC}"
    sub_url="https://${cf_domain}/${sub_path}"
    printf "%s\n\n" "${sub_url}"
  else
    info "[2] 未配置 CF_DOMAIN，跳过 CF 节点 / HTTPS 订阅地址"
    if [ -n "${public_ip}" ] && [ "${public_ip}" != "YOUR_SERVER_IP" ]; then
      printf "%b\n" "${YELLOW}[本地订阅]（仅本机/IP 可访问时）${NC}"
      printf "http://%s:%s/%s\n\n" "${public_ip}" "${port}" "${sub_path}"
    fi
  fi

  # --- TUIC ---
  if [ -n "${tuic_port}" ] && [ "${tuic_port}" != "0" ]; then
    if [ -n "${tuic_domain}" ]; then
      host="${tuic_domain}"
    elif is_truthy "${UDP_IPV6_ONLY:-false}"; then
      host="${public_ip6}"
    else
      host="${public_ip}"
    fi
    sni="${host}"
    host_uri="$(format_uri_host "${host}")"
    enc_uuid="$(urlencode "${uuid}")"
    enc_pass="$(urlencode "${tuic_pass}")"
    enc_name="$(urlencode "${name}-TUIC")"
    printf "%b\n" "${YELLOW}[3] TUIC（UDP，模式: ${udp_mode}）${NC}"
    tuic_url="tuic://${enc_uuid}:${enc_pass}@${host_uri}:${tuic_port}?allow_insecure=1&alpn=h3&congestion_control=bbr&insecure=1&skip-cert-verify=true&sni=${sni}&udp_relay_mode=native&version=5#${enc_name}"
    printf "%s\n\n" "${tuic_url}"
  else
    info "[3] 未开启 TUIC（TUIC_PORT 为空）"
    echo
  fi

  # --- HY2 ---
  if [ -n "${hy2_port}" ] && [ "${hy2_port}" != "0" ]; then
    if [ -n "${hy2_domain}" ]; then
      host="${hy2_domain}"
    elif is_truthy "${UDP_IPV6_ONLY:-false}"; then
      host="${public_ip6}"
    else
      host="${public_ip}"
    fi
    sni="${host}"
    host_uri="$(format_uri_host "${host}")"
    enc_pass="$(urlencode "${hy2_pass}")"
    enc_name="$(urlencode "${name}-HY2")"
    printf "%b\n" "${YELLOW}[4] Hysteria2 / hy2（UDP，模式: ${udp_mode}）${NC}"
    hy2_url="hysteria2://${enc_pass}@${host_uri}:${hy2_port}?insecure=1&sni=${sni}&alpn=h3"
    if [ -n "${hy2_obfs}" ]; then
      hy2_url="${hy2_url}&obfs=salamander&obfs-password=$(urlencode "${hy2_obfs}")"
    fi
    hy2_url="${hy2_url}#${enc_name}"
    printf "%s\n\n" "${hy2_url}"
  else
    info "[4] 未开启 HY2（HY2_PORT 为空）"
    echo
  fi

  printf "%b\n" "${GREEN}================================${NC}"
  info "提示: 只想用某一个协议时，复制对应那一行即可。"
  info "切换监听: bash install.sh v6only on|off   或菜单选 4"
  info "再次查看: bash install.sh nodes   或菜单选 3"
  echo
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
  # 装完直接打节点，方便复制
  sleep 1
  show_nodes || true
}

show_menu() {
  printf "\n%b\n" "${GREEN} Nexus (VPS 实体常驻版) 一键管理脚本 [系统自适应] ${NC}"
  printf "  ${YELLOW}1.${NC} 安装 / 启动服务 (自动适配 systemd / OpenRC)\n"
  printf "  ${YELLOW}2.${NC} 完全卸载节点\n"
  printf "  ${YELLOW}3.${NC} 打印节点链接 (VLESS / CF / TUIC / HY2)\n"
  printf "  ${YELLOW}4.${NC} 切换 TUIC/HY2 监听 (双栈 / IPv6 only)\n"
  printf "  ${YELLOW}0.${NC} 退出脚本\n"
  printf "请输入数字 [0-4]: "; read -r choice </dev/tty
  case "${choice}" in
    1) run_install ;;
    2) uninstall_app ;;
    3) show_nodes ;;
    4) set_udp_ipv6_only ;;
    *) exit 0 ;;
  esac
}

if [ "${1:-}" = "uninstall" ]; then uninstall_app
elif [ "${1:-}" = "install" ]; then run_install
elif [ "${1:-}" = "nodes" ] || [ "${1:-}" = "show" ]; then show_nodes
elif [ "${1:-}" = "v6only" ] || [ "${1:-}" = "ipv6-only" ]; then
  # bash install.sh v6only on|off|true|false
  case "${2:-}" in
    on|ON|true|TRUE|1) set_udp_ipv6_only true ;;
    off|OFF|false|FALSE|0) set_udp_ipv6_only false ;;
    "") set_udp_ipv6_only ;;
    *) set_udp_ipv6_only "$2" ;;
  esac
else show_menu; fi
