#!/usr/bin/env bash
# 把 Wails 产出的 Linux 可执行文件打成 AppImage 与 deb。
# 只在 Linux 上运行（release.yml 的 desktop-linux job）；产物没有签名，安装包内也没有
# cloudflared/bun，用户在设置页按提示自行安装，或用官方提供的 Docker 镜像。
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

VERSION="${VERSION:?VERSION 未设置（例如 GATEWAY_APP_VERSION 或 tag 去掉 v）}"
VERSION="${VERSION#v}"
ARCH="amd64"
APP_NAME="LLM Protocol Gateway"
BIN="$ROOT/desktop/build/bin/$APP_NAME"
[[ -x "$BIN" ]] || { echo "找不到 $BIN：先跑 wails build" >&2; exit 1; }

DIST="$ROOT/dist"
WORK="$ROOT/.cache/linux-package"
APPDIR="$WORK/AppDir"
rm -rf "$WORK"
mkdir -p "$DIST" "$APPDIR/usr/bin" "$APPDIR/usr/share/applications" \
  "$APPDIR/usr/share/icons/hicolor/scalable/apps"

cp "$BIN" "$APPDIR/usr/bin/llm-protocol-gateway"
chmod 0755 "$APPDIR/usr/bin/llm-protocol-gateway"

cp "$ROOT/web/public/logo.svg" "$APPDIR/usr/share/icons/hicolor/scalable/apps/llm-protocol-gateway.svg"

cat >"$APPDIR/llm-protocol-gateway.desktop" <<DESKTOP
[Desktop Entry]
Type=Application
Name=LLM Protocol Gateway
Name[zh_CN]=LLM 协议网关
Comment=Local LLM protocol gateway and console
Comment[zh_CN]=本地 LLM 协议网关与控制台
Exec=llm-protocol-gateway
Icon=llm-protocol-gateway
Terminal=false
Categories=Development;Utility;
StartupWMClass=LLM Protocol Gateway
DESKTOP
cp "$APPDIR/llm-protocol-gateway.desktop" "$APPDIR/usr/share/applications/"

# AppImage 规范要求的入口：根目录直接放 .desktop 与图标（上面已经写好，
# 不要再 cp 一次 —— 源和目标同一个文件，cp 会报 "same file" 并中止打包）。
ln -s usr/bin/llm-protocol-gateway "$APPDIR/AppRun"
cp "$APPDIR/usr/share/icons/hicolor/scalable/apps/llm-protocol-gateway.svg" "$APPDIR/"

desktop-file-validate "$APPDIR/llm-protocol-gateway.desktop"

# ---------------------------------------------------------------- AppImage
APPIMAGETOOL="$WORK/appimagetool"
curl -fsSL -o "$APPIMAGETOOL" \
  "https://github.com/AppImage/appimagetool/releases/download/continuous/appimagetool-x86_64.AppImage"
chmod +x "$APPIMAGETOOL"
# AppImage 的架构名是 x86_64，不是 Go 的 amd64；传错会被 appimagetool 拒掉。
case "$ARCH" in
  amd64|x86_64) APPIMAGE_ARCH=x86_64 ;;
  arm64|aarch64) APPIMAGE_ARCH=aarch64 ;;
  *) echo "unsupported AppImage arch: $ARCH" >&2; exit 1 ;;
esac
APPIMAGE_OUT="$DIST/llm-protocol-gateway-desktop-linux-$ARCH.AppImage"
# GitHub runner 上没有 FUSE：既传 --appimage-extract-and-run，也设环境变量兜底。
ARCH="$APPIMAGE_ARCH" APPIMAGE_EXTRACT_AND_RUN=1 \
  "$APPIMAGETOOL" --appimage-extract-and-run "$APPDIR" "$APPIMAGE_OUT"
chmod +x "$APPIMAGE_OUT"

# ---------------------------------------------------------------- deb
DEB_ROOT="$WORK/deb"
mkdir -p "$DEB_ROOT/DEBIAN"
cp -R "$APPDIR/usr" "$DEB_ROOT/"
INSTALLED_KB="$(du -sk "$DEB_ROOT/usr" | cut -f1)"

cat >"$DEB_ROOT/DEBIAN/control" <<CONTROL
Package: llm-protocol-gateway-desktop
Version: $VERSION
Architecture: $ARCH
Maintainer: Luca <noreply@lucadesign.uk>
Installed-Size: $INSTALLED_KB
Depends: libgtk-3-0, libwebkit2gtk-4.0-37 | libwebkit2gtk-4.1-0, ca-certificates
Section: utils
Priority: optional
Homepage: https://github.com/yangyongyongyong/llm-protocol-gateway
Description: LLM Protocol Gateway desktop app
 Local LLM protocol gateway with a console window: converts between
 OpenAI Chat, OpenAI Responses, Claude and Gemini protocols, manages
 API keys and subscription accounts, and tracks cost per request.
CONTROL

# 数据在 ~/.config/llm-protocol-gateway；升级卸载都不动它。
dpkg-deb --root-owner-group -Zxz --build "$DEB_ROOT" \
  "$DIST/llm-protocol-gateway-desktop-linux-$ARCH.deb"

# 安装一遍，确认依赖与文件布局正确
sudo dpkg -i "$DIST/llm-protocol-gateway-desktop-linux-$ARCH.deb" || {
  sudo apt-get -f install -y
  sudo dpkg -i "$DIST/llm-protocol-gateway-desktop-linux-$ARCH.deb"
}
test -x /usr/bin/llm-protocol-gateway
sudo dpkg -r llm-protocol-gateway-desktop

ls -lh "$DIST"/*.AppImage "$DIST"/*.deb
