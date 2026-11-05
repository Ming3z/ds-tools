#!/usr/bin/env bash

set -euo pipefail

die() {
  printf 'install.sh: %s\n' "$*" >&2
  exit 1
}

usage() {
  cat <<'EOF'
用法：
  ./install.sh [--prefix <directory>] [--no-skill] [vMAJOR.MINOR.PATCH]

示例：
  ./install.sh
  ./install.sh v0.1.0
  ./install.sh --prefix /tmp/ds-tools-test v0.1.0
  ./install.sh --no-skill

安装 build.sh 生成的二进制并验证其版本。默认安装目录按顺序选取
/opt/homebrew/bin、/usr/local/bin、~/.local/bin、~/bin 中第一个在 PATH
内且可写的目录。安装后会把内置的 agent skill 写入自动探测到的
skills 目录（--no-skill 可跳过）。

不传版本时自动使用 dist/release/ 下唯一或最新的产物。
EOF
}

path_contains() {
  case ":$PATH:" in
    *":$1:"*) return 0 ;;
    *) return 1 ;;
  esac
}

sha256_of() {
  if command -v shasum >/dev/null 2>&1; then
    shasum -a 256 "$1" | awk '{print $1}'
  else
    sha256sum "$1" | awk '{print $1}'
  fi
}

install_prefix=""
release_tag=""
install_skill=1
while [[ $# -gt 0 ]]; do
  case "$1" in
    --prefix)
      [[ $# -ge 2 ]] || die "--prefix 必须指定目录"
      [[ -z "$install_prefix" ]] || die "--prefix 只能指定一次"
      install_prefix="$2"
      shift 2
      ;;
    --no-skill)
      install_skill=0
      shift
      ;;
    --help|-h)
      usage
      exit 0
      ;;
    -*)
      die "未知选项：$1"
      ;;
    *)
      [[ -z "$release_tag" ]] || die "只能指定一个版本号"
      release_tag="$1"
      shift
      ;;
  esac
done

readonly script_directory="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
readonly repository_root="$script_directory"
readonly release_root="$repository_root/dist/release"
readonly binary_name="ds-tools"

[[ -d "$release_root" ]] || die "未找到构建产物，请先执行 ./build.sh"

if [[ -z "$release_tag" ]]; then
  available=()
  while IFS= read -r candidate; do
    available+=("$candidate")
  done < <(ls -1 "$release_root" 2>/dev/null || true)
  [[ ${#available[@]} -gt 0 ]] || die "dist/release/ 为空，请先执行 ./build.sh"
  if [[ ${#available[@]} -eq 1 ]]; then
    release_tag="${available[0]}"
  else
    release_tag="$(printf '%s\n' "${available[@]}" | sort -V | tail -n 1)"
    printf '提示：存在多个产物，使用最新版本 %s（可用参数指定其他版本）\n' "$release_tag"
  fi
fi
readonly release_tag
[[ "$release_tag" =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]] || \
  die "版本号必须使用稳定语义化格式 vMAJOR.MINOR.PATCH，例如 v0.1.0"

readonly release_directory="$release_root/$release_tag"
readonly manifest_path="$release_directory/manifest.json"
readonly checksums_path="$release_directory/checksums.txt"

[[ -d "$release_directory" ]] || die "未找到构建产物，请先执行 ./build.sh $release_tag"
[[ -f "$manifest_path" ]] || die "未找到发布清单：$manifest_path"
[[ -f "$checksums_path" ]] || die "未找到校验和文件：$checksums_path"

case "$(uname -s)" in
  Darwin) host_os="darwin" ;;
  Linux) host_os="linux" ;;
  *) die "不支持的操作系统：$(uname -s)" ;;
esac
case "$(uname -m)" in
  arm64|aarch64) host_arch="arm64" ;;
  x86_64|amd64) host_arch="amd64" ;;
  *) die "不支持的 CPU 架构：$(uname -m)" ;;
esac
readonly host_asset="$binary_name-$host_os-$host_arch"
[[ -f "$release_directory/$host_asset" ]] || \
  die "未找到本机产物 $host_asset（可用：$(ls -1 "$release_directory" | grep -v -e checksums.txt -e manifest.json | tr '\n' ' ')）"

printf '==> 校验产物完整性\n'
(
  cd "$release_directory"
  if command -v shasum >/dev/null 2>&1; then
    shasum -a 256 -c checksums.txt
  else
    sha256sum --check checksums.txt
  fi
)

if [[ -z "$install_prefix" ]]; then
  for candidate in /opt/homebrew/bin /usr/local/bin "$HOME/.local/bin" "$HOME/bin"; do
    if [[ -d "$candidate" && -w "$candidate" ]] && path_contains "$candidate"; then
      install_prefix="$candidate"
      break
    fi
  done
fi
if [[ -z "$install_prefix" ]]; then
  for candidate in /opt/homebrew/bin /usr/local/bin "$HOME/.local/bin" "$HOME/bin"; do
    if [[ -d "$candidate" && -w "$candidate" ]]; then
      install_prefix="$candidate"
      break
    fi
  done
fi
[[ -n "$install_prefix" ]] || install_prefix="$HOME/.local/bin"

mkdir -p "$install_prefix"
install_prefix="$(cd "$install_prefix" && pwd)"
readonly install_path="$install_prefix/$binary_name"

printf '==> 安装 %s 到 %s\n' "$release_tag" "$install_path"
temporary_path="$(mktemp "${TMPDIR:-/tmp}/ds-tools-install.XXXXXX")"
cleanup() {
  rm -f -- "$temporary_path"
}
trap cleanup EXIT HUP INT TERM
cp "$release_directory/$host_asset" "$temporary_path"
chmod 0755 "$temporary_path"
mv -f "$temporary_path" "$install_path"

printf '==> 验证已安装的程序\n'
version_output="$("$install_path" version --format json)"
case "$version_output" in
  *"\"version\":\"$release_tag\""*) ;;
  *) die "已安装的程序返回了非预期版本：$version_output" ;;
esac
"$install_path" version

installed_skill=""
if [[ $install_skill -eq 1 ]]; then
  printf '==> 安装 agent skill\n'
  if skill_output="$("$install_path" skill install --force --format text 2>&1)"; then
    installed_skill="$(printf '%s\n' "$skill_output" | sed -n 's/^installed //p')"
  else
    printf '提示：跳过 skill 安装（%s）\n' "$skill_output"
  fi
fi

printf '\n%s %s 安装成功。\n' "$binary_name" "$release_tag"
printf '可执行文件：%s\n' "$install_path"
if [[ -n "$installed_skill" ]]; then
  printf '技能文件：%s\n' "$installed_skill"
fi
if ! path_contains "$install_prefix"; then
  printf '\n提示：%s 不在 PATH 中，请加入后重开终端，例如：\n' "$install_prefix"
  printf '  export PATH="%s:$PATH"\n' "$install_prefix"
fi
printf '下一步：%s init && %s doctor --probe\n' "$binary_name" "$binary_name"
