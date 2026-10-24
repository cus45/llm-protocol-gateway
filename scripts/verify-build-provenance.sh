#!/usr/bin/env bash
#
# verify-build-provenance.sh — 校验 Go 二进制内嵌的 VCS 溯源信息
#
# 规则：线上运行的网关二进制必须能对应到一个完整的 commit ——
#   vcs.modified 必须是 false，vcs.revision 必须等于指定的 commit。
# 否则没人能证明服务器上跑的是哪份代码，也无法按 commit 回滚。
# （round35 P0-1：连续 14 轮部署的产物都是 vcs.modified=true、revision 落后于 HEAD。）
#
# 用法：
#   ./scripts/verify-build-provenance.sh <binary>            # 只检查 modified=false，并打印 revision
#   ./scripts/verify-build-provenance.sh <binary> <commit>   # 再校验 revision 与 commit 一致
#   ./scripts/verify-build-provenance.sh bin/gateway-linux HEAD   # commit 可写 HEAD / 分支 / 短 hash
#
# 环境变量：
#   EXPECT_REVISION  等价于第二个参数（commit 校验也可用它传）
#   GO               go 可执行文件路径，默认 go
#
# 退出码：0 通过；1 不通过；2 参数或工具缺失
#
# 交叉编译的 linux 二进制同样适用：go version -m 只读取文件内的 buildinfo，不需要能运行它。
# 服务器上若没有 go，可本地执行后或用任意装有 Go 的机器检查同一个文件。
#
set -euo pipefail

c_red=$'\e[31m'; c_green=$'\e[92m'; c_none=$'\e[0m'
die()  { printf '%s\n' "${c_red}✗ $*${c_none}" >&2; exit "${2:-1}"; }
ok()   { printf '%s\n' "${c_green}✓ $*${c_none}"; }

BIN="${1:-}"
if [[ -z "$BIN" || "$BIN" == "-h" || "$BIN" == "--help" ]]; then
  sed -n '2,26p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
  [[ -n "$BIN" ]] && exit 0 || exit 2
fi
[[ -f "$BIN" ]] || die "找不到二进制: $BIN" 2
GO="${GO:-go}"
command -v "$GO" >/dev/null || die "找不到 go 命令（用 GO=... 指定；至少需要 Go 1.18）" 2

# ── 解析 go version -m 的 build 行 ──
# 实际格式（以本机 go1.26 输出为准）：每行 "<TAB>build<TAB>key=value"，首行是 "<文件>: go1.26.1"。
# 注意 git-bash 的 bash 4.4：sed 的 \t、[[ =~ ]] 字符类、${x%%$'\t'*} 都不保留 TAB，
# 会静默解析失败。这里只用 [[ == *$'\t'* ]] 做匹配，用 slice_tab（IFS=$'\t' set --）切分。
slice_tab() { IFS=$'\t'; set -- $1; printf '%s\n' "$1"; }  # 取第一个 TAB 之前的部分
parse_build_info() {
  local go_line="" rev="" modified="" vcs="" goos="" goarch="" line kv key val
  while IFS= read -r line; do
    line="${line%$'\r'}"
    if [[ -z "$go_line" ]]; then go_line="$line"; continue; fi
    [[ "$line" == *$'\t'build$'\t'* ]] || continue
    line="${line#*$'\t'build$'\t'}"      # 去掉 "<TAB>build<TAB>" 前缀
    kv="$(slice_tab "$line")"            # 兼容 "key<TAB>value" 与 "key=value" 两种写法
    key="${kv%%=*}"; val="${kv#*=}"
    [[ "$kv" == *=* ]] || val=""
    case "$key" in
      vcs.revision) rev="$val" ;;
      vcs.modified) modified="$val" ;;
      vcs)          vcs="$val" ;;
      GOOS)         goos="$val" ;;
      GOARCH)       goarch="$val" ;;
    esac
  done < <(printf '%s\n' "$1")
  # 逐行输出 6 个字段，父 shell 用 mapfile 读回（全程不依赖 TAB 进入变量后被正确保留）
  printf '%s\n' "${go_line#*: }" "${vcs}" "${rev}" "${modified}" "${goos}" "${goarch}"
}
BUILD_INFO_RAW="$("$GO" version -m "$BIN" 2>&1)" || die "go version -m 读取失败（不是 Go 二进制？）: $BUILD_INFO_RAW" 2
FIELDS=()
mapfile -t FIELDS < <(parse_build_info "$BUILD_INFO_RAW")
GO_VER="${FIELDS[0]:-}"; GOVCS="${FIELDS[1]:-}"; REV="${FIELDS[2]:-}"
MODIFIED="${FIELDS[3]:-}"; BUILD_OS="${FIELDS[4]:-}"; BUILD_ARCH="${FIELDS[5]:-}"

echo "二进制 : $BIN"
echo "go     : ${GO_VER:-?}  ${BUILD_OS:-?}/${BUILD_ARCH:-?}"
echo "vcs    : ${GOVCS:-<缺失>}"
echo "revision: ${REV:-<缺失>}"
echo "modified: ${MODIFIED:-<缺失>}"

[[ -n "$GOVCS" ]] || die "二进制里没有 vcs 信息：构建时 VCS 标记被跳过（工作目录不在 git 仓库里、临时目录、或用 -buildvcs=false 构建），无法溯源"
[[ -n "$REV" ]]    || die "缺少 vcs.revision，无法溯源"
[[ "$MODIFIED" == "false" ]] || die "vcs.modified=$MODIFIED：二进制由带未提交改动的工作区构建，无法对应任何 commit（先把改动提交后重新构建）"
ok "vcs.modified=false"

# ── 可选：revision 必须等于期望的 commit ──
EXPECT="${2:-${EXPECT_REVISION:-}}"
if [[ -n "$EXPECT" ]]; then
  # 在仓库内时用 git 把分支 / HEAD / 短 hash 解析成完整 hash
  if command -v git >/dev/null && RESOLVED="$(git rev-parse --verify --quiet "${EXPECT}^{commit}" 2>/dev/null)"; then
    EXPECT="$RESOLVED"
  fi
  echo "expect : $EXPECT"
  REV_LC="${REV,,}"; EXPECT_LC="${EXPECT,,}"
  if [[ "$REV_LC" = "$EXPECT_LC" ]]; then
    ok "vcs.revision == $EXPECT"
  else
    # 期望值可能只是缩写 hash（如部署日志里记的短 hash）。用 case 做前缀匹配：
    # 写成 [[ "$a" == "$b"* ]] 在 git-bash 的 bash 4.4 上会误判。
    case "$REV_LC" in
      "$EXPECT_LC"*) ok "vcs.revision 以 $EXPECT 开头" ;;
      *) die "vcs.revision 与期望 commit 不一致：产物不是由 $EXPECT 构建的" ;;
    esac
  fi
fi

ok "溯源检查通过"
