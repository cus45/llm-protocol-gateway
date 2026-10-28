#!/usr/bin/env bash
# 构建网关二进制与可分发压缩包。CI（.github/workflows/release.yml）与本机发版共用这一份。
#
#   scripts/release.sh build    版本 [平台...]   # 仅构建
#   scripts/release.sh package  版本 [平台...]   # 构建 + 打包到 dist/
#   scripts/release.sh desktop  版本 windows/amd64   # 只出 Windows 桌面 App（其他平台由 CI 原生构建）
#
# 平台名沿用 Go 的 GOOS/GOARCH：linux/amd64、darwin/arm64、windows/amd64……
# 默认构建脚本里列出的全部平台。
#
# 每个包内结构一致，解压后可直接运行：
#   gateway(.exe)     无界面网关（Docker 外的裸机 / NAS / Windows 用）
#   web-dist/         管理页静态资源
#   RUN.txt           解压后怎么启动、数据与端口在哪
#   VERSION           目标平台、版本、源码修订号
#
# 版本号会被同时写入后端（BuildVersion）与前端（__APP_VERSION__）；
# 控制台靠二者比对来提示“后端已升级”，缺一不可，脚本会校验前端产物里确有该版本号。
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

DEFAULT_TARGETS=(
  linux/amd64 linux/arm64 linux/arm/v7
  darwin/amd64 darwin/arm64
  windows/amd64 windows/arm64
)

usage() {
  sed -n '2,12p' "$0" | sed 's/^# \{0,1\}//' >&2
  exit 2
}

MODE="${1:-}"
shift || true

if [[ ! -f .github/workflows/release.yml ]]; then
  echo "release.sh 必须在仓库根目录执行（找不到 .github/workflows/release.yml，当前 $PWD）" >&2
  exit 1
fi

if [[ "$MODE" != "build" && "$MODE" != "package" && "$MODE" != "desktop" ]]; then
  usage
fi

VERSION="${1:-}"
shift || true
if [[ -z "$VERSION" || ! "$VERSION" =~ ^v?[0-9][0-9A-Za-z.+-]*$ ]]; then
  echo "版本号缺失或不合法：'${VERSION}'（应为 v1.2.3 或 v0.1.0-rc1）" >&2
  exit 2
fi
VERSION="${VERSION#v}"

TARGETS=("$@")
if [[ ${#TARGETS[@]} -eq 0 ]]; then
  TARGETS=("${DEFAULT_TARGETS[@]}")
fi

DIST="$ROOT/dist"
CACHE="$ROOT/.cache/release"
mkdir -p "$CACHE"

commit="$(git rev-parse --short HEAD 2>/dev/null || echo unknown)"
build_date="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
ldflags="-s -w -X github.com/luca/llm-protocol-gateway/internal/gateway.BuildVersion=$VERSION"

log() { printf '==> %s\n' "$*" >&2; }

# -overlay 的 JSON 里路径要转义反斜杠（Windows 路径），否则解析不了。
json_path() {
  local p="$1"
  printf '%s' "${p//\\/\\\\}"
}

# ---------------------------------------------------------------- 平台拆分
# linux/arm/v7 → GOOS=linux GOARCH=arm GOARM=7
split_target() {
  local target="$1"
  local rest arch
  GOOS="${target%%/*}"
  rest="${target#*/}"
  GOARCH="${rest%%/*}"
  GOARM=""
  if [[ "$rest" == */* ]]; then
    local variant="${rest#*/}"
    case "$variant" in
      v6 | v7) GOARM="${variant#v}" ;;
      *)
        echo "不支持的 ARM 变体：$target" >&2
        return 1
        ;;
    esac
  fi
  TARGET="$target"
}

artifact_name() {
  printf 'llm-protocol-gateway_%s_%s_%s' "$VERSION" "$GOOS" "$GOARCH${GOARM:+v$GOARM}"
}

# ---------------------------------------------------------------- 前端
build_web() {
  if [[ "${SKIP_WEB:-0}" == "1" && -f web/dist/index.html ]]; then
    log "复用已构建的 web/dist（SKIP_WEB=1）"
  else
    log "构建管理页（版本 $VERSION）"
    (
      cd web
      if [[ ! -d node_modules ]]; then
        npm ci --no-audit --no-fund
      fi
      GATEWAY_APP_VERSION="$VERSION" npm run build
    )
  fi
  if ! grep -rqF -- "$VERSION" web/dist/assets/*.js; then
    echo "web/dist 的 JS 里找不到版本号 $VERSION：前端会被当成开发构建，控制台永远不提示升级" >&2
    exit 1
  fi
  # 桌面 App 内置的只是一个跳板页（desktop/frontend/dist），壳启动后会导航到本地网关。
  node desktop/frontend/sync-dist.mjs >/dev/null
}

# ---------------------------------------------------------------- 单个平台
build_target() {
  local target="$1"
  split_target "$target"

  local ext=""
  [[ "$GOOS" == "windows" ]] && ext=".exe"

  local work="$CACHE/$GOOS-$GOARCH${GOARM:+v$GOARM}"
  rm -rf "$work"
  mkdir -p "$work"

  local env=(
    CGO_ENABLED=0
    GOOS="$GOOS"
    GOARCH="$GOARCH"
    GOWORK=off
  )
  [[ -n "$GOARM" ]] && env+=(GOARM="$GOARM")

  log "编译 $target"
  env "${env[@]}" go build -trimpath -buildvcs=true \
    -ldflags "$ldflags" -o "$work/gateway$ext" ./cmd/gateway

  # ------------------------------------------------------------ 包内容
  cp -R web/dist "$work/web-dist"
  # 只留一份 RUN.txt：解压后先看它就能跑起来。
  cp "docs/release/RUN-$GOOS.md" "$work/RUN.txt"
  # 管理页的查找顺序里没有「可执行文件同级的 web-dist」，包内自带启动器显式指过去，
  # 用户就不必自己设 GATEWAY_WEB_DIST。
  if [[ "$GOOS" == "windows" ]]; then
    cp scripts/release/start-windows.cmd "$work/start.cmd"
  else
    cp scripts/release/start-unix.sh "$work/start.sh"
    chmod +x "$work/start.sh"
  fi

  {
    echo "TARGET=$TARGET"
    echo "VERSION=$VERSION"
    echo "GOARM=${GOARM:-}"
    echo "SOURCE_COMMIT=$commit"
    echo "BUILD_DATE=$build_date"
  } >"$work/VERSION"

  # 产物自检：用 -buildvcs 内嵌的修订号与本次构建对应，前端也必须带着版本号。
  # 从 git archive 导出（没有 .git）时 commit 为 unknown，-buildvcs 也会静默跳过，此时只查前端。
  if [[ "$commit" != "unknown" ]]; then
    local info
    info="$(env "${env[@]}" go version -m "$work/gateway$ext")"
    grep -qF "vcs.revision=$(git rev-parse HEAD)" <<<"$info" \
      || { echo "产物无法对应到 commit $(git rev-parse HEAD)" >&2; return 1; }
  fi

  if [[ "$MODE" == "package" ]]; then
    local name
    name="$(artifact_name)"
    local out
    if [[ "$GOOS" == "windows" ]]; then
      out="$DIST/$name.zip"
      rm -f "$out"
      (cd "$work" && node "$ROOT/scripts/release/zip-dir.mjs" "$out" .)
    else
      out="$DIST/$name.tar.gz"
      tar -czf "$out" -C "$work" .
    fi
    log "打包 $(basename "$out")"
  fi
}

# ---------------------------------------------------------------- 主流程
if [[ "$MODE" == "package" ]]; then
  mkdir -p "$DIST"
fi

build_web

build_desktop() {
  local target="$1"
  split_target "$target"
  # Wails 桌面壳通过 cgo 链接平台窗口框架：Windows 的 WebView2 不需要 cgo，
  # 可以在任何机器上交叉编译；Linux 需要 GTK/WebKitGTK，macOS 需要签名工具链，
  # 两者都由 release.yml 在对应 runner 上原生构建，不走这里。
  if [[ "$GOOS" != "windows" ]]; then
    echo "build_desktop 只支持在非 Windows 主机交叉编译 windows/*；$target 请在对应平台构建" >&2
    return 1
  fi

  local out="$DIST/llm-protocol-gateway-desktop-windows-$GOARCH.exe"
  local version4="${VERSION%%-*}"
  version4="${version4//[^0-9.]/.}"
  [[ "$version4" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || version4="0.1.0"
  version4="$version4.0"

  log "构建桌面 App $target（图标 / 版本信息 / DPI 清单）"
  (
    # 桌面壳是独立 module（desktop/go.mod）。这台机器可能处在 desktop/go.work 里，
    # 本地替换会把 CI 上的依赖解析弄错，这里先关闭 workspace。
    unset GOWORK
    cd desktop
    node frontend/sync-dist.mjs
    local rsrc
    rsrc="$(mktemp -d)"
    # go-winres 只接受 ≤256px 的图标源，仓库里保存的是缩好的那张。
    cp build/windows/icon.png build/windows/winres.json "$rsrc/"
    GOFLAGS=-mod=mod go run github.com/tc-hib/go-winres@v0.3.3 make \
      --in "$rsrc/winres.json" --arch "$GOARCH" --out "$rsrc/rsrc" \
      --product-version "$version4" --file-version "$version4" >/dev/null
    # go 只认包目录里的 *.syso，而发版不该往工作区写文件：
    # -overlay 把生成的资源挂到 desktop/ 下一个「本来不存在」的文件名上，包就多了这个源。
    printf '{"Replace":{"overlay_winres_%s.syso":"%s"}}\n' "$GOARCH" "$(json_path "$rsrc/rsrc_windows_$GOARCH.syso")" \
      >"$rsrc/overlay.json"
    CGO_ENABLED=0 GOOS="$GOOS" GOARCH="$GOARCH" GOFLAGS=-mod=mod \
      go build -trimpath -buildvcs=true -ldflags "-s -w -H windowsgui" \
      -overlay "$rsrc/overlay.json" -o "$out" .
    rm -rf "$rsrc"
  )
  log "打包 $(basename "$out")"
}

for target in "${TARGETS[@]}"; do
  if [[ "$MODE" == "desktop" ]]; then
    build_desktop "$target"
  else
    build_target "$target"
  fi
done

if [[ "$MODE" == "package" ]]; then
  (cd "$DIST" && sha256sum ./*.tar.gz ./*.zip >SHA256SUMS 2>/dev/null || shasum -a 256 ./*.tar.gz ./*.zip >SHA256SUMS)
  log "完成：$DIST/SHA256SUMS"
  sed 's/^/    /' "$DIST/SHA256SUMS" >&2
fi
