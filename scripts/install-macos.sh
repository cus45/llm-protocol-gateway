#!/usr/bin/env bash
# 安装 macOS 桌面 App 到 /Applications。
#
#   bash install.sh           # 最新版
#   bash install.sh 0.2.0     # 指定版本（不带 v）
#
# 换仓库（fork）时设 LPG_REPO=owner/repo。
# 不做 Apple 公证，装完会去掉 quarantine 标记，双击即可打开。
set -euo pipefail

REPO="${LPG_REPO:-yangyongyongyong/llm-protocol-gateway}"
VER="${1:-}"

case "$(uname -m)" in
  arm64) ARCH=arm64 ;;
  x86_64) ARCH=amd64 ;;
  *) echo "不支持的架构：$(uname -m)" >&2; exit 1 ;;
esac

if [ -n "$VER" ]; then
  VER="${VER#v}"
  URL="https://github.com/$REPO/releases/download/v$VER/llm-protocol-gateway-desktop-darwin-$ARCH.zip"
  echo "安装 $REPO $VER（macOS $ARCH）…"
else
  URL="https://github.com/$REPO/releases/latest/download/llm-protocol-gateway-desktop-darwin-$ARCH.zip"
  echo "安装 $REPO 最新版（macOS $ARCH）…"
fi

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

curl -fL --progress-bar "$URL" -o "$TMP/app.zip"
ditto -x -k "$TMP/app.zip" "$TMP/out"

APP="$(find "$TMP/out" -maxdepth 3 -name 'LLM Protocol Gateway.app' -print -quit)"
[ -n "$APP" ] || { echo "压缩包里没有 LLM Protocol Gateway.app" >&2; exit 1; }

# 先退出正在运行的旧版本，否则替换后旧进程仍占着数据目录
osascript -e 'quit app "LLM Protocol Gateway"' >/dev/null 2>&1 || true

rm -rf "/Applications/LLM Protocol Gateway.app"
ditto "$APP" "/Applications/LLM Protocol Gateway.app"
xattr -dr com.apple.quarantine "/Applications/LLM Protocol Gateway.app" 2>/dev/null || true

echo
echo "已安装到 /Applications/LLM Protocol Gateway.app"
echo "数据保留在 ~/Library/Application Support/llm-protocol-gateway/"
echo "打开：open -a \"LLM Protocol Gateway\""
