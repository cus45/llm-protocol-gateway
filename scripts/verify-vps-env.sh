#!/usr/bin/env bash
# verify-vps-env.sh — VPS 环境与 Claude 连通性自检（只读，不改动系统）
#
# 用途：在（美国西海岸）VPS 上核对环境一致性与 Claude 网络可达性：
#   系统时区/偏移、NTP 同步、Locale/Intl、字体（Emoji/中文）、DNS 解析出口、
#   出站 IPv4/IPv6 归属、Claude 各端点连通与延迟、本机网关健康（如已部署）。
#
# 用法：
#   bash scripts/verify-vps-env.sh
#   EXPECTED_TZ=America/Los_Angeles bash scripts/verify-vps-env.sh   # 覆盖预期时区
#
# 依赖：curl 必需；dig / fc-list / timedatectl / node 可选（缺失则跳过对应项）。
# 退出码：0 = 无失败项；1 = 存在失败项（详见输出）。
#
# 浏览器侧的检查（WebRTC / DNS 泄露 / 语言 / Intl / Emoji 渲染）无法由本脚本代劳，
# 仅在用浏览器访问 claude.ai 网页版时才相关，检查入口见脚本结尾提示。

set -u

EXPECTED_TZ="${EXPECTED_TZ:-America/Los_Angeles}"
GW_HEALTH_URL="${GW_HEALTH_URL:-http://127.0.0.1:18093/__health}"

PASS=0 WARN=0 FAIL=0
if [ -t 1 ]; then
  C_OK=$'\033[32m' C_W=$'\033[33m' C_BAD=$'\033[31m' C_DIM=$'\033[2m' C_B=$'\033[1m' C_0=$'\033[0m'
else
  C_OK='' C_W='' C_BAD='' C_DIM='' C_B='' C_0=''
fi

ok()   { printf '  %s✓%s %s\n' "$C_OK" "$C_0" "$*"; PASS=$((PASS+1)); }
warn() { printf '  %s⚠%s %s\n' "$C_W" "$C_0" "$*"; WARN=$((WARN+1)); }
bad()  { printf '  %s✗%s %s\n' "$C_BAD" "$C_0" "$*"; FAIL=$((FAIL+1)); }
info() { printf '    %s%s%s\n' "$C_DIM" "$*" "$C_0"; }
section() { printf '\n%s== %s ==%s\n' "$C_B" "$*" "$C_0"; }
have() { command -v "$1" >/dev/null 2>&1; }
jsonval() { printf '%s' "$1" | grep -o "\"$2\": *\"[^\"]*\"" | head -n1 | cut -d'"' -f4; }

get_tz() {
  if have timedatectl; then
    timedatectl show -p Timezone --value 2>/dev/null && return
    timedatectl 2>/dev/null | sed -n 's/.*Time zone: \([^ ]*\).*/\1/p'
  fi
}
get_ntp() {
  if have timedatectl; then
    timedatectl show -p NTPSynchronized --value 2>/dev/null && return
    timedatectl 2>/dev/null | grep -q 'synchronized: yes' && echo yes || echo no
  fi
}

# ---------------------------------------------------------------- 1. 系统信息
section "1. 系统信息"
info "主机名: $(hostname 2>/dev/null || echo '?')  |  内核: $(uname -sr)"
if [ -r /etc/os-release ]; then
  # shellcheck disable=SC1091
  . /etc/os-release
  info "发行版: ${PRETTY_NAME:-未知}"
fi

# ------------------------------------------------- 2. 系统时间 / 时区 / 偏移
section "2. 系统时间与时区（应与 VPS 实际所在地一致）"
info "当前时间: $(date '+%Y-%m-%d %H:%M:%S %Z %z')"
TZ_NOW="$(get_tz || true)"
if [ -n "${TZ_NOW:-}" ]; then
  if [ "$TZ_NOW" = "$EXPECTED_TZ" ]; then
    ok "系统时区 = $EXPECTED_TZ（与美国西海岸一致）"
  else
    warn "系统时区 = $TZ_NOW，预期 $EXPECTED_TZ — 修复: sudo timedatectl set-timezone $EXPECTED_TZ"
  fi
else
  info "无法读取系统时区（无 timedatectl？）"
fi
NTP_NOW="$(get_ntp || true)"
case "${NTP_NOW:-}" in
  yes) ok "NTP 时钟同步正常" ;;
  no)  bad "NTP 未同步 — 时钟偏移会导致 TLS 握手失败。修复: sudo apt install -y systemd-timesyncd && sudo timedatectl set-ntp true" ;;
  *)   info "NTP 同步状态未知" ;;
esac

# ------------------------------------------------------------- 3. Locale/Intl
section "3. Locale / Intl（美区一致性）"
LANG_NOW="$(locale 2>/dev/null | sed -n 's/^LANG=//p')"
info "LANG=${LANG_NOW:-未设置}"
case "${LANG_NOW:-}" in
  en_US.UTF-8|en_US.utf8|C.UTF-8|C.utf8) ok "LANG 为 ${LANG_NOW}（美式英语/中性，无区域信号）" ;;
  zh_*|*zh_CN*|*zh_TW*) bad "LANG 为 ${LANG_NOW} — 中文区域与美区 IP 不一致。修复: sudo locale-gen en_US.UTF-8 && sudo update-locale LANG=en_US.UTF-8" ;;
  '') warn "LANG 未设置（默认 C/POSIX，无区域信号，但建议设为 en_US.UTF-8）" ;;
  *)  info "LANG 为 ${LANG_NOW}（非中文区域，通常无碍）" ;;
esac
if have locale; then
  if locale -a 2>/dev/null | grep -qiE '^en_US\.utf-?8$'; then
    ok "en_US.UTF-8 locale 已生成"
  else
    warn "en_US.UTF-8 locale 未生成 — 修复: sudo locale-gen en_US.UTF-8 && sudo update-locale LANG=en_US.UTF-8"
  fi
fi
if have node; then
  info "Node Intl: $(node -e 'const o=Intl.DateTimeFormat().resolvedOptions();console.log(o.locale+" / "+o.timeZone)' 2>/dev/null || echo 读取失败)"
fi

# ------------------------------------------------------------------ 4. 字体
section "4. 字体（Emoji 渲染 / 中文）"
if have fc-list; then
  if fc-list 2>/dev/null | grep -qi 'emoji'; then
    ok "Emoji 字体已安装（Linux 环境即 Noto Color Emoji 渲染风格）"
  else
    warn "未安装 Emoji 字体 — 浏览器/界面中 Emoji 显示为方块（残缺环境特征）。修复: sudo apt install -y fonts-noto-color-emoji"
  fi
  ZH_FONTS=$(fc-list :lang=zh 2>/dev/null | wc -l | tr -d ' ')
  if [ "${ZH_FONTS:-0}" -gt 0 ]; then
    info "中文字体: ${ZH_FONTS} 个（存在，属常见 Linux 发行版配置）"
  else
    info "中文字体: 未安装（页面中文走回退字体；如需在服务器浏览器看中文: sudo apt install -y fonts-noto-cjk）"
  fi
else
  info "无 fc-list（fontconfig 未装），跳过字体检查"
fi

# --------------------------------------------------------------- 5. DNS 解析
section "5. DNS 解析路径与区域"
RESOLVERS=""
if have resolvectl; then
  RESOLVERS=$(resolvectl status 2>/dev/null | awk '/Current DNS Server|DNS Servers/{for(i=3;i<=NF;i++) printf "%s ", $i}')
fi
if [ -z "$RESOLVERS" ] && [ -r /etc/resolv.conf ]; then
  RESOLVERS=$(awk '/^nameserver/{printf "%s ", $2}' /etc/resolv.conf)
fi
info "本机 DNS: ${RESOLVERS:-未知}"
if have dig; then
  DNS_EGRESS=$(dig +short +time=3 +tries=1 whoami.akamai.net 2>/dev/null | head -n1)
  if [ -n "${DNS_EGRESS:-}" ]; then
    info "递归解析器出口 IP: $DNS_EGRESS"
    DNS_GEO=$(curl -m 8 -s "http://ip-api.com/json/${DNS_EGRESS}?fields=country,regionName,city,isp" 2>/dev/null || true)
    if [ -n "$DNS_GEO" ]; then
      info "解析器归属: $DNS_GEO"
      case "$DNS_GEO" in
        *'"country":"United States"'*) ok "DNS 出口在美国" ;;
        *) warn "DNS 出口不在美国或无法确认（以上方归属为准）" ;;
      esac
    fi
  else
    warn "whoami.akamai.net 无返回（网络受限？）"
  fi
else
  info "未装 dig（sudo apt install -y dnsutils 后可查 DNS 出口位置）"
fi

# ------------------------------------------------------------ 6. 出站 IPv4
section "6. 出站 IPv4 与归属"
IPINFO="$(curl -m 10 -s https://ipinfo.io/json 2>/dev/null || true)"
if [ -z "$IPINFO" ]; then
  bad "无法获取出站 IPv4（ipinfo.io 不可达）"
else
  IP4=$(jsonval "$IPINFO" ip)
  CITY4=$(jsonval "$IPINFO" city)
  REGION4=$(jsonval "$IPINFO" region)
  COUNTRY4=$(jsonval "$IPINFO" country)
  ORG4=$(jsonval "$IPINFO" org)
  IPTZ4=$(jsonval "$IPINFO" timezone)
  info "IP: ${IP4:-?}  位置: ${CITY4:-?}, ${REGION4:-?}, ${COUNTRY4:-?}"
  info "ASN/组织: ${ORG4:-?}"
  info "IP 地理时区: ${IPTZ4:-?}"
  if [ "${COUNTRY4:-}" = "US" ]; then ok "出站 IP 归属美国"; else bad "出站 IP 归属 ${COUNTRY4:-未知}，非美国"; fi
  case "${REGION4:-}" in
    California|Oregon|Washington) ok "位于美国西海岸（$REGION4）" ;;
    '') ;;
    *) warn "位于 $REGION4（非西海岸；与所选机房不符时留意）" ;;
  esac
  if [ -n "${IPTZ4:-}" ] && [ -n "${TZ_NOW:-}" ]; then
    if [ "$TZ_NOW" = "$IPTZ4" ]; then
      ok "系统时区与 IP 地理时区一致（$IPTZ4）"
    else
      warn "系统时区($TZ_NOW) ≠ IP 地理时区($IPTZ4) — 建议统一为 $IPTZ4"
    fi
  fi
  if [ -n "${IP4:-}" ]; then
    FLAGS=$(curl -m 8 -s "http://ip-api.com/json/${IP4}?fields=status,proxy,hosting,mobile,isp" 2>/dev/null || true)
    [ -n "$FLAGS" ] && info "IP 类型标记: $FLAGS"
    case "${FLAGS:-}" in
      *'"proxy":true'*)   warn "该 IP 被公共库标记为代理出口（proxy=true）" ;;
      *'"hosting":true'*) info "机房/托管网段（hosting=true，VPS 属正常现象，但属弱信号）" ;;
    esac
  fi
fi

# ------------------------------------------------------------ 7. 出站 IPv6
section "7. 出站 IPv6（如启用）"
IPINFO6="$(curl -m 8 -s -6 https://ipinfo.io/json 2>/dev/null || true)"
if [ -z "$IPINFO6" ]; then
  info "无 IPv6 出站（或不可达）——不构成问题"
else
  IP6=$(jsonval "$IPINFO6" ip)
  COUNTRY6=$(jsonval "$IPINFO6" country)
  REGION6=$(jsonval "$IPINFO6" region)
  info "IPv6 出站: ${IP6:-?} (${REGION6:-?}, ${COUNTRY6:-?})"
  if [ "${COUNTRY6:-}" = "US" ]; then ok "IPv6 出口在美国"; else warn "IPv6 出口归属 ${COUNTRY6:-未知} — 与 IPv4 不一致，上游可能随机走 v6"; fi
fi

# ------------------------------------------------------- 8. Claude 连通性
section "8. Claude 端点连通性与延迟"
probe() { # $1=名称 $2=URL $3=预期状态码列表 $4=方法(默认GET)
  local name="$1" url="$2" want="$3" method="${4:-GET}" out code tconn ttotal
  if ! out=$(curl -m 15 -sS -X "$method" -o /dev/null -w '%{http_code} %{time_connect} %{time_total}' "$url" 2>/dev/null); then
    bad "$name 不可达（连接失败，检查出站 443 / DNS）"; return
  fi
  code=$(printf '%s' "$out" | awk '{print $1}')
  tconn=$(printf '%s' "$out" | awk '{print $2}')
  ttotal=$(printf '%s' "$out" | awk '{print $3}')
  case " $want " in
    *" $code "*) ok "$name 可达（HTTP $code，连接 ${tconn}s / 总 ${ttotal}s）" ;;
    *) warn "$name 返回 HTTP $code（预期: $want）— 可能被风控挑战或路径受限" ;;
  esac
  if awk -v t="$ttotal" 'BEGIN{exit !(t>1.5)}'; then
    warn "$name 延迟偏高（${ttotal}s）——确认没有绕路代理"
  fi
}
probe "api.anthropic.com" "https://api.anthropic.com/v1/messages" "400 401 403" "POST"
probe "claude.ai" "https://claude.ai/" "200 301 302"
probe "console.anthropic.com" "https://console.anthropic.com/" "200 301 302"

# --------------------------------------------------- 9. 本机网关（可选）
section "9. 本机网关健康（如已部署）"
if curl -m 3 -s "$GW_HEALTH_URL" >/dev/null 2>&1; then
  ok "网关运行中: $(curl -m 3 -s "$GW_HEALTH_URL" | head -c 200)"
else
  info "本机 $GW_HEALTH_URL 无响应（未部署/未启动可忽略）"
fi

# ------------------------------------------------------------------ 汇总
section "汇总"
printf '  通过 %d 项 ｜ 警告 %d 项 ｜ 失败 %d 项\n' "$PASS" "$WARN" "$FAIL"
cat <<'HINTS'

  常见修复（Debian/Ubuntu）:
    时区:    sudo timedatectl set-timezone America/Los_Angeles
    NTP:     sudo apt install -y systemd-timesyncd && sudo timedatectl set-ntp true
    Locale:  sudo apt install -y locales && sudo locale-gen en_US.UTF-8 && sudo update-locale LANG=en_US.UTF-8
    字体:    sudo apt install -y fonts-noto-color-emoji fonts-noto-cjk
    DNS 出口: 若不在美国，检查 /etc/resolv.conf 或 resolvectl 的上游 DNS

  浏览器侧（仅当用浏览器访问 claude.ai 网页版时相关；纯 API/客户端用法不受影响）:
    WebRTC 泄露:  https://browserleaks.com/webrtc     期望: 只见 VPS 公网 IP
    DNS 泄露:     https://browserleaks.com/dns        期望: 解析器均在美国
    时区/Intl:    https://browserleaks.com/javascript 期望: America/Los_Angeles + en-US
    Emoji 渲染:   缺字体则显示方块；安装 fonts-noto-color-emoji 即可
    claude.ai 返回 403 多为 Cloudflare 对机房 IP 的挑战，API 路径不受影响

HINTS

if [ "$FAIL" -eq 0 ]; then
  exit 0
else
  exit 1
fi
