#!/usr/bin/env bash
# 包内启动器：把管理页指到同级的 web-dist，再运行网关。
# 数据仍按默认放在 ~/.config/llm-protocol-gateway（或 macOS 的 Application Support），
# 可用 GATEWAY_DB / GATEWAY_ADDR 覆盖。
set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
export GATEWAY_WEB_DIST="$HERE/web-dist"
exec "$HERE/gateway" "$@"
