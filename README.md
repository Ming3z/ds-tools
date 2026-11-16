# ds-tools

将 [DeepSeek 的工具 API](https://api-docs.deepseek.com/zh-cn/guides/search/) 封装为 CLI，并内置供 Agent 使用的 [`SKILL.md`](internal/skill/SKILL.md)。

## 示例

```bash
# Search
ds-tools search "DeepSeek V4 发布"

# Fetch
ds-tools fetch https://example.com

# OCR
ds-tools ocr <file>

# ASR
ds-tools asr <file>

# TTS
ds-tools tts "你好，我是 DeepSeek。" -o hello.wav
```

- `--format` 支持 `text`、`json`、`jsonl`、`md`（`tts` 仅 `text` / `json` / `jsonl`）。
- `search` / `fetch` 终端默认 `text`、管道默认 `json`，`ocr` / `asr` 默认 `text`。`--fields` 可做字段投影。

## 快速开始

暂未发布预编译包。需要 Go（版本见 `go.mod`）与 bash；安装支持 macOS / Linux，Windows 仅构建。

```bash
./dev.sh all v0.1.0     # 构建并安装二进制 + Skill
ds-tools init           # 交互向导；非交互用 --api-key-stdin < key.txt
ds-tools doctor --probe # 校验 Key 与余额
```

**配置**

```bash
# API Key 优先级：--api-key > DEEPSEEK_API_KEY > 配置文件
# 配置文件默认：<用户配置目录>/ds-tools/config.json
# 可用 --config 或 DS_TOOLS_CONFIG 指定
ds-tools config show              # 生效值、来源与文件原值（Key 脱敏）
ds-tools config set timeout 60s   # 修改配置
ds-tools config unset timeout     # 移除配置项
```

**Agent 接入**

```bash
ds-tools skill install --all   # 安装到全部三个 Agent skills 目录
ds-tools skill status          # 检查安装状态，以及是否与当前版本一致
```

不提供 MCP：CLI + Skill 覆盖终端与能执行 shell 的 Agent。

**开发**

```bash
make build                  # 本机构建，不生成发布产物
make test                   # 本机测试
./dev.sh build v0.1.0       # 生成 5 平台产物到 dist/release/<version>/
./dev.sh install            # 安装最近一次构建
./dev.sh --help             # 完整用法
```

## 命令

| 命令 | 作用 |
|---|---|
| `search` | 搜索网页 |
| `fetch` | 抓取已知 URL，可批量 |
| `ocr` | 文档 / 图片转文字 |
| `asr` | 音频转文字 |
| `tts` | 文字转语音 |
| `init` | 写入 API Key |
| `config` | 查看 / 修改配置 |
| `doctor` | 配置自检；`--probe` 校验 Key 与余额 |
| `cache` | 查看 / 清空本地缓存 |
| `skill` | 输出 / 安装 Agent Skill |
| `version` | 显示版本 |

- `search` 到 `tts` 为 API 命令；计费以 [官方文档](https://api-docs.deepseek.com/zh-cn/guides/search/) 为准，`search` 的 `-n` 与 `--content` 不影响单价。
- 其余为配置与辅助命令；详细用法见 `ds-tools <command> -h`。

## 退出码

| 退出码 | 含义 |
|---|---|
| `0` | 成功（搜索无结果也是 0） |
| `1` | 服务端 / 请求错误 |
| `2` | Key 缺失或无效 |
| `3` | 网络错误 |
| `4` | 本地用法错误 |

- 408 / 429 / 5xx 默认重试 2 次；
- `--error-format json` 时，stderr 输出 JSON 错误。

## License

[MIT](LICENSE)
