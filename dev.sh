#!/usr/bin/env bash
set -euo pipefail

readonly SCRIPT_NAME="dev.sh"
readonly BINARY_NAME="ds-tools"
readonly DEFAULT_VERSION="v0.1.0"

die() {
    printf '%s: %s\n' "$SCRIPT_NAME" "$*" >&2
    exit 1
}

usage() {
    cat <<'EOF'
用法：
  ./dev.sh                    交互菜单
  ./dev.sh build [VERSION]    校验并构建 5 平台产物
  ./dev.sh install [VERSION]  安装最近一次构建
  ./dev.sh all [VERSION]      构建后安装
  ./dev.sh --help             显示本帮助

选项：
  --prefix DIR  安装目录
  --no-skill    跳过 Skill 安装

VERSION：
  MAJOR.MINOR.PATCH，可加 v 前缀。
  build 省略版本：HEAD tag → 已安装版本 → v0.1.0
  install 省略版本：最近一次构建 → 最新产物

产物写入 dist/release/<version>/（checksums.txt + manifest.json）。
安装目录：--prefix；否则 PATH 内首个可写目录；都没有则 ~/.local/bin。
不发布、不使用 sudo、不修改 PATH；Skill 默认安装，--no-skill 可跳过。
EOF
}

usage_error() {
    printf '%s: %s\n' "$SCRIPT_NAME" "$*" >&2
    usage >&2
    exit 2
}

normalize_version() {
    local raw="$1"
    [[ "$raw" =~ ^v?(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]] || \
        die "版本号必须是 MAJOR.MINOR.PATCH（可加 v 前缀），例如 0.1.0"
    printf 'v%s\n' "${raw#v}"
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

path_contains() {
    case ":$PATH:" in
        *":$1:"*) return 0 ;;
        *) return 1 ;;
    esac
}

read_build_property() {
    local file="$1"
    local key="$2"
    local name
    local value
    while IFS='=' read -r name value; do
        if [[ "$name" == "$key" ]]; then
            printf '%s\n' "${value%$'\r'}"
            return 0
        fi
    done < "$file"
    return 1
}

latest_build_version() {
    local recorded
    local latest
    if [[ -f "$BUILD_RECORD" ]] && recorded="$(read_build_property "$BUILD_RECORD" version)"; then
        printf '%s\n' "$recorded"
        return 0
    fi
    if [[ -d "$RELEASE_ROOT" ]]; then
        latest="$(find "$RELEASE_ROOT" -mindepth 1 -maxdepth 1 -type d -name 'v*' \
            -exec basename {} \; | sort -V | tail -n 1)"
        if [[ -n "$latest" ]]; then
            printf '%s\n' "$latest"
            return 0
        fi
    fi
    return 1
}

detect_installed_version() {
    local candidate output
    candidate="$(command -v "$BINARY_NAME" 2>/dev/null || true)"
    if [[ -z "$candidate" && -x "$HOME/.local/bin/$BINARY_NAME" ]]; then
        candidate="$HOME/.local/bin/$BINARY_NAME"
    fi
    [[ -n "$candidate" ]] || return 1
    output="$("$candidate" version --format json 2>/dev/null)" || return 1
    if [[ "$output" =~ \"version\":\"(v?[0-9]+\.[0-9]+\.[0-9]+)\" ]]; then
        printf 'v%s\n' "${BASH_REMATCH[1]#v}"
        return 0
    fi
    return 1
}

MENU_OPTIONS=(
    "构建（校验 + 5 平台交叉编译）"
    "安装最近一次构建"
    "构建后安装"
)
MENU_ACTIONS=(build install all)
MENU_SELECTED=0
MENU_INSTALLED=""
MENU_LAST_BUILD=""

draw_menu() {
    local index=0
    local option
    printf '\r\033[K  已安装：%s    最近构建：%s\n' "$MENU_INSTALLED" "$MENU_LAST_BUILD"
    printf '\r\033[K  ↑↓ 移动 • Enter 确认 • 1-3 直选 • q 退出\n'
    printf '\r\033[K\n'
    for option in "${MENU_OPTIONS[@]}"; do
        if (( index == MENU_SELECTED )); then
            printf '\r\033[K\033[1m› [%d] %s\033[0m\n' "$((index + 1))" "$option"
        else
            printf '\r\033[K  [%d] %s\n' "$((index + 1))" "$option"
        fi
        index=$((index + 1))
    done
}

move_menu() {
    local delta="$1"
    local count="${#MENU_OPTIONS[@]}"
    MENU_SELECTED=$(( (MENU_SELECTED + delta + count) % count ))
    printf '\033[%dA' "$((count + 3))"
    draw_menu
}

choose_action() {
    local key
    local sequence
    MENU_SELECTED=0
    MENU_INSTALLED="$(detect_installed_version 2>/dev/null || true)"
    MENU_LAST_BUILD="$(latest_build_version 2>/dev/null || true)"
    [[ -n "$MENU_INSTALLED" ]] || MENU_INSTALLED="无"
    [[ -n "$MENU_LAST_BUILD" ]] || MENU_LAST_BUILD="无"
    draw_menu
    trap 'printf "\n"; exit 130' INT
    while true; do
        if ! IFS= read -rsn1 key; then
            trap - INT
            printf '\n'
            return 2
        fi
        case "$key" in
            '') trap - INT; printf '\n'; return 0 ;;
            1|2|3) MENU_SELECTED=$((key - 1)); trap - INT; printf '\n'; return 0 ;;
            q|Q) trap - INT; printf '\n'; return 2 ;;
            $'\x04') trap - INT; printf '\n'; return 2 ;;
            k) move_menu -1 ;;
            j) move_menu 1 ;;
            $'\x1b')
                sequence=""
                IFS= read -rsn2 -t 1 sequence || true
                case "$sequence" in
                    '[A'|'OA') move_menu -1 ;;
                    '[B'|'OB') move_menu 1 ;;
                    '') trap - INT; printf '\n'; return 2 ;;
                esac
                ;;
        esac
    done
}

RESOLVED_VERSION=""

resolve_build_version() {
    local requested="$1"
    local default_version=""

    if [[ -n "$requested" ]]; then
        RESOLVED_VERSION="$(normalize_version "$requested")" || return $?
        return 0
    fi
    if command -v git >/dev/null 2>&1 && git rev-parse --git-dir >/dev/null 2>&1; then
        default_version="$(git describe --tags --exact-match 2>/dev/null || true)"
    fi
    if [[ -z "$default_version" ]] && default_version="$(detect_installed_version)"; then
        :
    fi
    [[ -n "$default_version" ]] || default_version="$DEFAULT_VERSION"

    if [[ -t 0 ]]; then
        if ! read -r -p "版本号 [$default_version]（回车沿用，q 取消）：" answer; then
            printf '\n已取消。\n' >&2
            return 2
        fi
        case "$answer" in
            q|Q) printf '已取消。\n' >&2; return 2 ;;
        esac
        [[ -z "$answer" ]] || default_version="$answer"
    fi
    RESOLVED_VERSION="$(normalize_version "$default_version")" || return $?
}

resolve_install_version() {
    local requested="$1"
    local fallback

    if [[ -n "$requested" ]]; then
        RESOLVED_VERSION="$(normalize_version "$requested")" || return $?
        return 0
    fi
    if fallback="$(latest_build_version)"; then
        RESOLVED_VERSION="$(normalize_version "$fallback")" || return $?
        return 0
    fi
    die "未找到构建产物，请先执行 ./$SCRIPT_NAME build [VERSION]"
}

verify_version() {
    local binary="$1"
    local expected="$2"
    local output
    output="$("$binary" version --format json)" || die "无法执行：$binary"
    case "$output" in
        *"\"version\":\"$expected\""*) ;;
        *) die "版本不符：期望 $expected，实际 $output" ;;
    esac
}

BUILT_AT=""
BUILD_COMMIT=""

build_target() {
    local target_os="$1"
    local target_arch="$2"
    local output_name="$3"
    printf '==> 构建 %s/%s\n' "$target_os" "$target_arch"
    env CGO_ENABLED=0 GOOS="$target_os" GOARCH="$target_arch" \
        go build -trimpath \
        -ldflags "-s -w \
          -X dstools/internal/cli.Version=$BUILD_VERSION \
          -X dstools/internal/cli.Commit=$BUILD_COMMIT \
          -X dstools/internal/cli.Date=$BUILT_AT" \
        -o "$RELEASE_DIRECTORY/$output_name" ./cmd/"$BINARY_NAME"
}

run_build() {
    local version="$1"
    local unformatted
    local host_binary

    [[ "$version" =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]] || \
        die "内部错误：无效版本号：${version:-<empty>}"
    require_command go
    if ! command -v shasum >/dev/null 2>&1 && ! command -v sha256sum >/dev/null 2>&1; then
        die "生成校验和需要 shasum 或 sha256sum"
    fi
    [[ "$DIST_ROOT" == "$REPOSITORY_ROOT/dist" ]] || \
        die "拒绝清理非预期目录：$DIST_ROOT"

    BUILD_VERSION="$version"
    BUILT_AT="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
    BUILD_COMMIT="none"
    if command -v git >/dev/null 2>&1 && git rev-parse --git-dir >/dev/null 2>&1; then
        BUILD_COMMIT="$(git rev-parse --short HEAD 2>/dev/null || echo none)"
        if [[ -n "$(git status --porcelain 2>/dev/null || true)" ]]; then
            BUILD_COMMIT="$BUILD_COMMIT-dirty"
            printf '注意：工作区有未提交改动，commit 记为 %s\n' "$BUILD_COMMIT"
        fi
    fi
    RELEASE_DIRECTORY="$RELEASE_ROOT/$version"

    printf '==> 格式化并校验（vet + test）\n'
    gofmt -w .
    unformatted="$(gofmt -l .)"
    [[ -z "$unformatted" ]] || die "格式化失败：$unformatted"
    go vet ./...
    go test ./...

    printf '==> 清理旧构建产物：%s\n' "$DIST_ROOT"
    rm -rf -- "$DIST_ROOT"
    mkdir -p "$RELEASE_DIRECTORY"

    build_target darwin amd64 "$BINARY_NAME-darwin-amd64"
    build_target darwin arm64 "$BINARY_NAME-darwin-arm64"
    build_target linux amd64 "$BINARY_NAME-linux-amd64"
    build_target linux arm64 "$BINARY_NAME-linux-arm64"
    build_target windows amd64 "$BINARY_NAME-windows-amd64.exe"
    chmod 0755 "$RELEASE_DIRECTORY"/"$BINARY_NAME"-darwin-* \
        "$RELEASE_DIRECTORY"/"$BINARY_NAME"-linux-*

    printf '==> 生成 SHA-256 校验和与发布清单\n'
    (
        cd "$RELEASE_DIRECTORY"
        shasum -a 256 "$BINARY_NAME"-* > checksums.txt 2>/dev/null || \
            sha256sum "$BINARY_NAME"-* > checksums.txt
    )

    {
        printf '{\n'
        printf '  "name": "%s",\n' "$BINARY_NAME"
        printf '  "version": "%s",\n' "$version"
        printf '  "commit": "%s",\n' "$BUILD_COMMIT"
        printf '  "built_at": "%s",\n' "$BUILT_AT"
        printf '  "files": [\n'
        first_file=1
        for asset_path in "$RELEASE_DIRECTORY"/"$BINARY_NAME"-*; do
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
    } > "$RELEASE_DIRECTORY/manifest.json"

    printf 'version=%s\ncommit=%s\nbuilt_at=%s\n' \
        "$version" "$BUILD_COMMIT" "$BUILT_AT" > "$BUILD_RECORD"

    host_binary=""
    case "$(go env GOOS)/$(go env GOARCH)" in
        darwin/amd64) host_binary="$BINARY_NAME-darwin-amd64" ;;
        darwin/arm64) host_binary="$BINARY_NAME-darwin-arm64" ;;
        linux/amd64) host_binary="$BINARY_NAME-linux-amd64" ;;
        linux/arm64) host_binary="$BINARY_NAME-linux-arm64" ;;
        windows/amd64) host_binary="$BINARY_NAME-windows-amd64.exe" ;;
    esac
    if [[ -n "$host_binary" ]]; then
        printf '==> 验证本机产物版本\n'
        verify_version "$RELEASE_DIRECTORY/$host_binary" "$version"
    fi

    printf '\n构建完成，未安装或发布。\n'
    printf '版本：%s（commit %s）\n' "$version" "$BUILD_COMMIT"
    printf '产物目录：%s\n' "$RELEASE_DIRECTORY"
    printf '下一步：./%s install %s\n' "$SCRIPT_NAME" "$version"
}

INSTALL_TEMP=""
INSTALL_BACKUP=""
INSTALL_PATH=""
INSTALL_HAD_PREVIOUS=0
INSTALL_ROLLBACK=0

install_cleanup() {
    if [[ "$INSTALL_ROLLBACK" -eq 1 ]]; then
        if [[ "$INSTALL_HAD_PREVIOUS" -eq 1 && -f "$INSTALL_BACKUP" ]]; then
            mv -f -- "$INSTALL_BACKUP" "$INSTALL_PATH"
        else
            rm -f -- "$INSTALL_PATH"
        fi
    fi
    [[ -z "$INSTALL_BACKUP" ]] || rm -f -- "$INSTALL_BACKUP"
    [[ -z "$INSTALL_TEMP" ]] || rm -f -- "$INSTALL_TEMP"
    return 0
}

run_install() {
    local version="$1"
    local prefix="$2"
    local release_directory="$RELEASE_ROOT/$version"
    local manifest_path="$release_directory/manifest.json"
    local checksums_path="$release_directory/checksums.txt"
    local host_asset
    local installed_skill=""
    local skill_output
    local candidate

    [[ "$version" =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]] || \
        die "内部错误：无效版本号：${version:-<empty>}"
    [[ -d "$release_directory" ]] || \
        die "未找到构建产物，请先执行 ./$SCRIPT_NAME build $version"
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
    host_asset="$BINARY_NAME-$host_os-$host_arch"
    [[ -f "$release_directory/$host_asset" ]] || \
        die "未找到本机产物 $host_asset（可用：$(ls -1 "$release_directory" | \
            grep -v -e checksums.txt -e manifest.json | tr '\n' ' ')）"

    printf '==> 校验产物完整性\n'
    (
        cd "$release_directory"
        if command -v shasum >/dev/null 2>&1; then
            shasum -a 256 -c checksums.txt
        else
            sha256sum --check checksums.txt
        fi
    )

    if [[ -z "$prefix" ]]; then
        for candidate in /opt/homebrew/bin /usr/local/bin "$HOME/.local/bin" "$HOME/bin"; do
            if [[ -d "$candidate" && -w "$candidate" ]] && path_contains "$candidate"; then
                prefix="$candidate"
                break
            fi
        done
    fi
    if [[ -z "$prefix" ]]; then
        for candidate in /opt/homebrew/bin /usr/local/bin "$HOME/.local/bin" "$HOME/bin"; do
            if [[ -d "$candidate" && -w "$candidate" ]]; then
                prefix="$candidate"
                break
            fi
        done
    fi
    [[ -n "$prefix" ]] || prefix="$HOME/.local/bin"

    mkdir -p "$prefix"
    prefix="$(cd "$prefix" && pwd -P)"
    INSTALL_PATH="$prefix/$BINARY_NAME"

    printf '==> 安装 %s 到 %s\n' "$version" "$INSTALL_PATH"
    INSTALL_TEMP="$(mktemp "$prefix/.$BINARY_NAME-install.XXXXXX")"
    INSTALL_BACKUP=""
    INSTALL_HAD_PREVIOUS=0
    INSTALL_ROLLBACK=0
    trap install_cleanup EXIT
    trap 'exit 130' INT
    trap 'exit 143' HUP TERM

    cp "$release_directory/$host_asset" "$INSTALL_TEMP"
    chmod 0755 "$INSTALL_TEMP"
    printf '==> 校验待安装二进制\n'
    verify_version "$INSTALL_TEMP" "$version"

    if [[ -e "$INSTALL_PATH" || -L "$INSTALL_PATH" ]]; then
        INSTALL_BACKUP="$INSTALL_TEMP.backup"
        cp -p -- "$INSTALL_PATH" "$INSTALL_BACKUP"
        INSTALL_HAD_PREVIOUS=1
    fi
    INSTALL_ROLLBACK=1
    mv -f -- "$INSTALL_TEMP" "$INSTALL_PATH"
    INSTALL_TEMP=""

    printf '==> 验证安装结果\n'
    verify_version "$INSTALL_PATH" "$version"
    INSTALL_ROLLBACK=0
    trap - EXIT INT HUP TERM
    install_cleanup
    INSTALL_BACKUP=""
    "$INSTALL_PATH" version

    if [[ "$INSTALL_SKILL" -eq 1 ]]; then
        printf '==> 安装 agent skill\n'
        if skill_output="$("$INSTALL_PATH" skill install --force --format text 2>&1)"; then
            installed_skill="$(printf '%s\n' "$skill_output" | sed -n 's/^installed //p')"
        else
            printf '提示：跳过 skill 安装（%s）\n' "$skill_output"
        fi
    fi

    printf '\n%s %s 安装成功。\n' "$BINARY_NAME" "$version"
    printf '可执行文件：%s\n' "$INSTALL_PATH"
    if [[ -n "$installed_skill" ]]; then
        printf '技能文件：%s\n' "$installed_skill"
    fi
    if ! path_contains "$prefix"; then
        printf '\n提示：%s 不在 PATH 中，请加入后重开终端，例如：\n' "$prefix"
        printf '  export PATH="%s:$PATH"\n' "$prefix"
    fi
    printf '下一步：%s init && %s doctor --probe\n' "$BINARY_NAME" "$BINARY_NAME"
}

ACTION=""
VERSION=""
PREFIX=""
PREFIX_SET=0
INSTALL_SKILL=1

while [[ $# -gt 0 ]]; do
    case "$1" in
        build|install|all)
            [[ -z "$ACTION" ]] || usage_error "只能指定一个操作"
            ACTION="$1"
            shift
            ;;
        --prefix)
            [[ $# -ge 2 && -n "$2" && "$2" != -* ]] || usage_error "--prefix 必须指定目录"
            PREFIX="$2"
            PREFIX_SET=1
            shift 2
            ;;
        --no-skill)
            INSTALL_SKILL=0
            shift
            ;;
        -h|--help)
            usage
            exit 0
            ;;
        -*)
            usage_error "未知参数：$1"
            ;;
        *)
            [[ -z "$VERSION" ]] || usage_error "只能指定一个版本号"
            VERSION="$1"
            shift
            ;;
    esac
done

SCRIPT_DIRECTORY="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPOSITORY_ROOT="$SCRIPT_DIRECTORY"
DIST_ROOT="$REPOSITORY_ROOT/dist"
RELEASE_ROOT="$DIST_ROOT/release"
BUILD_RECORD="$DIST_ROOT/build.properties"
RELEASE_DIRECTORY=""

cd "$REPOSITORY_ROOT"

if [[ -z "$ACTION" ]]; then
    if [[ ! -t 0 ]]; then
        usage_error "未指定操作：build、install 或 all"
    fi
    if [[ -n "$VERSION" || "$PREFIX_SET" -eq 1 || "$INSTALL_SKILL" -eq 0 ]]; then
        usage_error "交互菜单不支持额外参数"
    fi
    STATUS=0
    choose_action || STATUS=$?
    case "$STATUS" in
        0) ACTION="${MENU_ACTIONS[$MENU_SELECTED]}" ;;
        2) exit 0 ;;
        *) exit 2 ;;
    esac
fi

case "$ACTION" in
    build)
        [[ "$PREFIX_SET" -eq 0 ]] || usage_error "build 不接受 --prefix"
        [[ "$INSTALL_SKILL" -eq 1 ]] || usage_error "build 不接受 --no-skill"
        resolve_build_version "$VERSION" || {
            status=$?
            [[ "$status" -eq 2 ]] && exit 0
            exit "$status"
        }
        run_build "$RESOLVED_VERSION"
        ;;
    install)
        resolve_install_version "$VERSION" || exit $?
        run_install "$RESOLVED_VERSION" "$PREFIX"
        ;;
    all)
        resolve_build_version "$VERSION" || {
            status=$?
            [[ "$status" -eq 2 ]] && exit 0
            exit "$status"
        }
        run_build "$RESOLVED_VERSION"
        run_install "$RESOLVED_VERSION" "$PREFIX"
        ;;
esac
