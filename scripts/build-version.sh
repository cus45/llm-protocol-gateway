#!/usr/bin/env bash
# 生成并打印本次发布的版本号。
#
# 版本号同时注入两个地方，二者必须一致，否则控制台无法判断“后端已升级”：
#   1. 后端二进制   go build -ldflags "-X .../internal/gateway.BuildVersion=<版本>"
#   2. 前端资源     GATEWAY_APP_VERSION=<版本> npm run build  → __APP_VERSION__
#
# 格式：YYYYMMDD.HHMMSS（UTC）。等长、可直接按字符串比较，不会因为版本号
# 位数变化而比错。可用 GATEWAY_APP_VERSION 覆盖（手工发版 / 回滚到旧版本）。
#
# 用法：
#   ./scripts/build-version.sh            # 生成并打印版本号
#   ./scripts/build-version.sh --check A  # 判断 A 是否与当前工作区的版本一致
set -euo pipefail

if [[ "${1:-}" == "--check" ]]; then
  want="${2:-}"
  have="$(date -u +%Y%m%d.%H%M%S)"
  if [[ -n "${GATEWAY_APP_VERSION:-}" ]]; then
    have="$GATEWAY_APP_VERSION"
  fi
  [[ "$want" == "$have" ]] && exit 0
  printf '版本不一致：构建时为 %s，当前为 %s\n' "${want:-<空>}" "$have" >&2
  exit 1
fi

if [[ -n "${GATEWAY_APP_VERSION:-}" ]]; then
  printf '%s\n' "$GATEWAY_APP_VERSION"
  exit 0
fi
date -u +%Y%m%d.%H%M%S
