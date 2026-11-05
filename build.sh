#!/usr/bin/env bash

set -euo pipefail

die() {
  printf 'build.sh: %s\n' "$*" >&2
  exit 1
}

usage() {
  cat <<'EOF'
用法：
  ./build.sh [vMAJOR.MINOR.PATCH]

示例：
  ./build.sh
  ./build.sh v0.1.0

校验源码（格式、vet、测试），交叉编译所有受支持平台的发布产物，
生成 SHA-256 校验和与 manifest.json。脚本会先清理 dist/，但不会
发布产物、创建 Git tag 或修改远端仓库。

不传版本时：优先使用当前 HEAD 上的 git tag；否则交互询问，默认 v0.1.0。
EOF
}

require_command() {
  command -v "$1" >/dev/null 2>&1 || die "缺少必要命令：$1"
}

sha256_of() {
  if command -v shasum >/dev/null 2>&1; then
    shasum -a 256 "$1" | awk '{print $1}'
  else
    sha256sum "$1" | awk '{print $1}'
  fi
}

case "${1:-}" in
  --help|-h)
    usage
    exit 0
    ;;
esac

if [[ $# -gt 1 ]]; then
  printf 'build.sh: 最多传入一个版本号，例如 v0.1.0\n\n' >&2
  usage >&2
  exit 2
fi

readonly script_directory="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
readonly repository_root="$script_directory"
readonly output_directory="$repository_root/dist"
readonly binary_name="ds-tools"

for command_name in go; do
  require_command "$command_name"
done
if ! command -v shasum >/dev/null 2>&1 && ! command -v sha256sum >/dev/null 2>&1; then
  die "生成校验和需要 shasum 或 sha256sum"
fi

cd "$repository_root"

release_tag="${1:-}"
if [[ -z "$release_tag" ]]; then
  if command -v git >/dev/null 2>&1 && git rev-parse --git-dir >/dev/null 2>&1; then
    release_tag="$(git describe --tags --exact-match 2>/dev/null || true)"
  fi
fi
if [[ -z "$release_tag" ]]; then
  if [[ -t 0 ]]; then
    read -r -p '输入发布版本号 [v0.1.0]: ' answer || true
    release_tag="${answer:-v0.1.0}"
  else
    die "非交互环境必须显式传入版本号，例如 ./build.sh v0.1.0"
  fi
fi
readonly release_tag
[[ "$release_tag" =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]] || \
  die "版本号必须使用稳定语义化格式 vMAJOR.MINOR.PATCH，例如 v0.1.0"

readonly release_directory="$output_directory/release/$release_tag"
readonly built_at="$(date -u +%Y-%m-%dT%H:%M:%SZ)"

commit="none"
if command -v git >/dev/null 2>&1 && git rev-parse --git-dir >/dev/null 2>&1; then
  commit="$(git rev-parse --short HEAD 2>/dev/null || echo none)"
  if [[ -n "$(git status --porcelain 2>/dev/null || true)" ]]; then
    printf '提示：工作区有未提交改动，产物标记为 %s（dirty）\n' "$commit"
    commit="$commit-dirty"
  fi
fi
readonly commit

[[ "$output_directory" == "$repository_root/dist" ]] || \
  die "拒绝清理非预期目录：$output_directory"
printf '==> 清理旧构建产物：%s\n' "$output_directory"
rm -rf -- "$output_directory"
mkdir -p "$release_directory"

printf '==> 格式化并校验源码\n'
gofmt -w .
unformatted="$(gofmt -l .)"
[[ -z "$unformatted" ]] || die "格式化失败：$unformatted"
go vet ./...
go test ./...

build_target() {
  local target_os="$1"
  local target_arch="$2"
  local output_name="$3"
  printf '==> 构建 %s/%s\n' "$target_os" "$target_arch"
  env CGO_ENABLED=0 GOOS="$target_os" GOARCH="$target_arch" \
    go build -trimpath \
    -ldflags "-s -w \
      -X dstools/internal/cli.Version=$release_tag \
      -X dstools/internal/cli.Commit=$commit \
      -X dstools/internal/cli.Date=$built_at" \
    -o "$release_directory/$output_name" ./cmd/"$binary_name"
}

build_target darwin amd64 "$binary_name-darwin-amd64"
build_target darwin arm64 "$binary_name-darwin-arm64"
build_target linux amd64 "$binary_name-linux-amd64"
build_target linux arm64 "$binary_name-linux-arm64"
build_target windows amd64 "$binary_name-windows-amd64.exe"
chmod 0755 "$release_directory"/"$binary_name"-darwin-* "$release_directory"/"$binary_name"-linux-*

printf '==> 生成 SHA-256 校验和与发布清单\n'
(
  cd "$release_directory"
  shasum -a 256 "$binary_name"-* > checksums.txt 2>/dev/null || sha256sum "$binary_name"-* > checksums.txt
)

{
  printf '{\n'
  printf '  "name": "%s",\n' "$binary_name"
  printf '  "version": "%s",\n' "$release_tag"
  printf '  "commit": "%s",\n' "$commit"
  printf '  "built_at": "%s",\n' "$built_at"
  printf '  "files": [\n'
  first_file=1
  for asset_path in "$release_directory"/"$binary_name"-*; do
    asset_name="$(basename "$asset_path")"
    asset_sha="$(sha256_of "$asset_path")"
    asset_size="$(wc -c < "$asset_path" | tr -d ' ')"
    if [[ $first_file -eq 0 ]]; then
      printf ',\n'
    fi
    printf '    {"name": "%s", "sha256": "%s", "size": %s}' \
      "$asset_name" "$asset_sha" "$asset_size"
    first_file=0
  done
  printf '\n  ]\n}\n'
} > "$release_directory/manifest.json"

host_os="$(go env GOOS)"
host_arch="$(go env GOARCH)"
host_binary=""
case "$host_os/$host_arch" in
  darwin/amd64) host_binary="$binary_name-darwin-amd64" ;;
  darwin/arm64) host_binary="$binary_name-darwin-arm64" ;;
  linux/amd64) host_binary="$binary_name-linux-amd64" ;;
  linux/arm64) host_binary="$binary_name-linux-arm64" ;;
  windows/amd64) host_binary="$binary_name-windows-amd64.exe" ;;
esac

if [[ -n "$host_binary" ]]; then
  printf '==> 验证本机产物版本\n'
  version_output="$("$release_directory/$host_binary" version --format json)"
  case "$version_output" in
    *"\"version\":\"$release_tag\""*) ;;
    *) die "构建产物返回了非预期版本：$version_output" ;;
  esac
fi

printf '\n本地构建完成，未发布任何文件。\n'
printf '版本：%s（commit %s）\n' "$release_tag" "$commit"
printf '产物目录：%s\n' "$release_directory"
printf '下一步：./install.sh %s\n' "$release_tag"
