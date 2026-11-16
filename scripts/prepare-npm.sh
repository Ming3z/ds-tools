#!/usr/bin/env bash
# Build the npm package trees (one top-level launcher + one package per
# platform) from an existing dist/release/<version> artifact directory.
# Publishing is done by .github/workflows/release.yml.
set -euo pipefail

SCRIPT_NAME="$(basename "$0")"
REPOSITORY_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

VERSION=""
ARTIFACTS=""
OUT=""

usage() {
    cat <<'EOF'
用法：scripts/prepare-npm.sh --version vX.Y.Z [--artifacts <dir>] [--out <dir>]

从发布产物生成 npm 本地包目录：

  dist/npm/ds-tools               顶层包（Node shim + optionalDependencies）
  dist/npm/ds-tools-darwin-arm64  平台包（内含静态二进制）
  dist/npm/ds-tools-darwin-x64
  dist/npm/ds-tools-linux-arm64
  dist/npm/ds-tools-linux-x64

默认 --artifacts dist/release/<version>，--out dist/npm。
Windows 产物只进入 GitHub Release，不进入 npm（运行时尚未验证）。
EOF
}

fail() {
    printf '%s: %s\n' "$SCRIPT_NAME" "$*" >&2
    exit 1
}

sha256_of() {
    if command -v sha256sum >/dev/null 2>&1; then
        sha256sum "$1" | cut -d' ' -f1
    else
        shasum -a 256 "$1" | cut -d' ' -f1
    fi
}

while [[ $# -gt 0 ]]; do
    case "$1" in
        --version)
            [[ $# -ge 2 ]] || fail "--version 需要值"
            VERSION="$2"
            shift 2
            ;;
        --artifacts)
            [[ $# -ge 2 ]] || fail "--artifacts 需要值"
            ARTIFACTS="$2"
            shift 2
            ;;
        --out)
            [[ $# -ge 2 ]] || fail "--out 需要值"
            OUT="$2"
            shift 2
            ;;
        -h|--help)
            usage
            exit 0
            ;;
        *)
            fail "未知参数：$1（--help 查看用法）"
            ;;
    esac
done

[[ -n "$VERSION" ]] || { usage >&2; fail "缺少 --version"; }
[[ "$VERSION" =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]] || \
    fail "版本号格式错误：$VERSION（示例 v1.0.0）"
SEMVER="${VERSION#v}"

[[ -n "$ARTIFACTS" ]] || ARTIFACTS="$REPOSITORY_ROOT/dist/release/$VERSION"
[[ -n "$OUT" ]] || OUT="$REPOSITORY_ROOT/dist/npm"

[[ -d "$ARTIFACTS" ]] || fail "产物目录不存在：$ARTIFACTS（先运行 ./dev.sh build $VERSION）"
[[ -f "$ARTIFACTS/manifest.json" ]] || fail "缺少发布清单：$ARTIFACTS/manifest.json"
[[ -f "$ARTIFACTS/checksums.txt" ]] || fail "缺少校验和：$ARTIFACTS/checksums.txt"

manifest_version="$(sed -n 's/.*"version": "\([^"]*\)".*/\1/p' "$ARTIFACTS/manifest.json" | head -1)"
[[ "$manifest_version" == "$VERSION" ]] || \
    fail "清单版本 $manifest_version 与 $VERSION 不一致"

mkdir -p "$OUT"
OUT="$(cd "$OUT" && pwd -P)"
case "$OUT/" in
    "$REPOSITORY_ROOT"/*) ;;
    *) fail "--out 必须位于仓库内：$OUT" ;;
esac
rm -rf -- "$OUT"
mkdir -p "$OUT"

printf '==> 生成顶层包 @youmkdev/ds-tools@%s\n' "$SEMVER"
mkdir -p "$OUT/ds-tools/bin"
sed "s/__VERSION__/$SEMVER/g" \
    "$REPOSITORY_ROOT/npm/ds-tools/package.json.tmpl" > "$OUT/ds-tools/package.json"
cp "$REPOSITORY_ROOT/npm/ds-tools/bin/ds-tools.js" "$OUT/ds-tools/bin/ds-tools.js"
cp "$REPOSITORY_ROOT/README.md" "$REPOSITORY_ROOT/LICENSE" "$OUT/ds-tools/"

stage_platform() {
    local goos="$1" goarch="$2" npmarch="$3"
    local name="ds-tools-$goos-$npmarch"
    local asset="ds-tools-$goos-$goarch"
    local directory="$OUT/$name"
    local expected actual

    expected="$(awk -v name="$asset" '$2 == name {print $1}' "$ARTIFACTS/checksums.txt" | head -1)"
    [[ -n "$expected" ]] || fail "checksums.txt 缺少 $asset"
    actual="$(sha256_of "$ARTIFACTS/$asset")"
    [[ "$actual" == "$expected" ]] || fail "$asset 校验失败：期望 $expected，实际 $actual"

    printf '==> 生成平台包 %s@%s（%s）\n' "$name" "$SEMVER" "$(du -h "$ARTIFACTS/$asset" | cut -f1)"
    mkdir -p "$directory/bin"
    sed -e "s/__NAME__/$name/g" \
        -e "s/__VERSION__/$SEMVER/g" \
        -e "s/__OS__/$goos/g" \
        -e "s/__CPU__/$npmarch/g" \
        "$REPOSITORY_ROOT/npm/platform/package.json.tmpl" > "$directory/package.json"
    cp "$ARTIFACTS/$asset" "$directory/bin/ds-tools"
    chmod 0755 "$directory/bin/ds-tools"
    cp "$REPOSITORY_ROOT/LICENSE" "$directory/LICENSE"
}

stage_platform darwin amd64 x64
stage_platform darwin arm64 arm64
stage_platform linux amd64 x64
stage_platform linux arm64 arm64

host_platform=""
case "$(uname -s)/$(uname -m)" in
    Darwin/arm64) host_platform="darwin-arm64" ;;
    Darwin/x86_64) host_platform="darwin-x64" ;;
    Linux/x86_64) host_platform="linux-x64" ;;
    Linux/aarch64|Linux/arm64) host_platform="linux-arm64" ;;
esac
if [[ -n "$host_platform" ]]; then
    printf '==> 验证本机平台包可执行\n'
    host_version="$("$OUT/ds-tools-$host_platform/bin/ds-tools" version --format json)" || \
        fail "无法执行本机二进制"
    case "$host_version" in
        *"\"version\":\"$VERSION\""*) ;;
        *) fail "本机二进制版本不符：$host_version" ;;
    esac
fi

printf '\n生成完成，未发布。\n'
printf '目录：%s\n' "$OUT"
printf '本地验证：\n'
printf '  npm pack %s\n' "$OUT"/*/
printf '发布：推送 tag（.github/workflows/release.yml 自动构建并发布）\n'
