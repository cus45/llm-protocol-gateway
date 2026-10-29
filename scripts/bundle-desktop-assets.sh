#!/usr/bin/env bash
# 把 web/dist 同步到 desktop/assets，供桌面壳用 -tags desktop_ui 内嵌进二进制。
# 桌面产物常常被单独拷走（一个 exe、一个 AppImage、Finder 里拖出来的 .app），
# 旁边不会再有 web-dist；缺少内嵌资源时窗口里只有「404 page not found」。
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
SRC="$ROOT/web/dist"
DST="$ROOT/desktop/assets"

if [[ ! -f "$SRC/index.html" ]]; then
  echo "缺少 $SRC/index.html：先构建 web（npm --prefix web run build）" >&2
  exit 1
fi

rm -rf "$DST"
mkdir -p "$DST"
cp -R "$SRC/." "$DST/"
[[ -f "$DST/index.html" ]] || { echo "复制后仍找不到 $DST/index.html" >&2; exit 1; }
echo "已同步 web/dist → desktop/assets（$(du -sh "$DST" | cut -f1)）"
