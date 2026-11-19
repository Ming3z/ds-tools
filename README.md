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

1. macOS / Linux（需 Node.js ≥ 18）：

```bash
npm install -g @youmkdev/ds-tools
```

2. 交互式写入配置并安装 SKILL.md

```bash
ds-tools init
```

- 没装 Node.js：从 [Releases](https://github.com/Ming3z/ds-tools/releases) 下载对应平台的静态二进制放入 PATH。
- Windows：暂未验证运行时，仅提供构建产物。
- 从源码构建：需要 Go（版本见 `go.mod`）与 bash，`./dev.sh all v1.0.0`。

完整命令与参数见 `ds-tools --help` 与 `ds-tools <command> --help`。

## 命令

| 命令 | 作用 |
|---|---|
| `search` | 搜索网页 |
| `fetch` | 抓取已知 URL，可批量 |
| `ocr` | 文档 / 图片转文字 |
| `asr` | 音频转文字 |
| `tts` | 文字转语音 |
| `init` | 写入配置（交互向导） |
| `config` | 查看 / 修改配置 |
| `doctor` | 配置自检；`--probe` 校验 Key 与余额 |
| `cache` | 查看 / 清空本地缓存 |
| `skill` | 安装 / 卸载 / 检查 Agent Skill |
| `version` | 显示版本 |

- `search` 到 `tts` 为 API 命令；计费以 [官方文档](https://api-docs.deepseek.com/zh-cn/guides/search/) 为准，`search` 的 `-n` 与 `--content` 不影响单价。
- `search` / `fetch` 的响应分别按完整请求体与规范化 URL 缓存 1 小时；`search --fill-missing` 的补抓与 `fetch` 共用缓存，重复运行同一条命令不再计费；`--no-cache` 跳过读取缓存、强制重取。
- 配置文件默认 `<用户配置目录>/ds-tools/config.json`，可用 `--config` 或 `DS_TOOLS_CONFIG` 指定；Key 优先级见 `ds-tools config --help`。
- Agent 接入：`ds-tools skill install --all` 安装到全部三个 skills 目录，`ds-tools skill status` 检查内容是否与当前版本一致，`ds-tools skill uninstall --all` 全部移除。
- 不提供 MCP：CLI + Skill 覆盖终端与能执行 shell 的 Agent。
- 其余为配置与辅助命令；详细用法见 `ds-tools <command> -h`。

## 退出码

| 退出码 | 含义 |
|---|---|
| `0` | 成功（搜索无结果也是 0） |
| `1` | 不可重试的请求 / 服务端错误：400/402/403/404/413；OCR 504 或识别超时 |
| `2` | Key 缺失或无效 |
| `3` | 临时错误，稍后可安全重试：网络失败，或内部重试耗尽后仍是 408/429/5xx，或 OCR 200 响应体中的临时错误 |
| `4` | 本地用法错误（未发出请求） |
| `130` / `143` | 被 SIGINT / SIGTERM 中断 |

- 408 / 429 / 5xx 默认内部重试 2 次（`--retries` 可调），退避带 ±20% 抖动；
- OCR 例外：429 仅在带 `Retry-After` 时内部重试（每次重试都会重新上传整个文件，服务端还占用并发槽），否则直接以 `3` 退出；504 与 200 响应体中的识别超时不重试（换更小的文件重来），退出码 `1`；200 响应体中的 `temporarily unavailable` / `internal server error` 会重试，耗尽后退出码 `3`；
- `ocr` / `asr` 文件已上传、等待结果时断连也不重试，直接返回 `1`——服务端可能仍在识别并计费，重试会重复计费；
- 批量 `fetch` 混有非法与合法 URL 时，只要有合法 URL 被请求（可能已计费）就返回 `1`，全部非法才返回 `4`；多个失败按 `2` > `1` > `3` > `4` 的严重度取最高，成功条目照常输出；
- TTS 是二进制音频：不给 `-o` 时只允许 stdout 是普通文件（`ds-tools tts "..." > speech.wav`），stdout 是管道或终端时以退出码 `4` 拒绝；确实要写管道时显式用 `-o -`（`ds-tools tts "..." -o - | player`）；
- 被 SIGINT / SIGTERM 中断时先清理临时文件再退出（130 / 143），不会留下半成品；
- `--error-format json` 时，stderr 输出 JSON 错误，`retryable` 为 `true` 当且仅当退出码为 `3`。

## 开发

```bash
make build                              # 本机构建，不生成发布产物
make test                               # 本机测试
./dev.sh build v1.0.0                   # 生成 5 平台产物到 dist/release/<version>/
./dev.sh install                        # 安装最近一次构建
./scripts/prepare-npm.sh --version v1.0.0  # 生成 dist/npm/ 包目录，可 npm pack 验证
./dev.sh --help                         # 完整用法
```

## License

[MIT](LICENSE)
