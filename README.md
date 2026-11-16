# ds-tools

把 [DeepSeek Search / Fetch](https://api-docs.deepseek.com/zh-cn/guides/search/) 与
[OCR](https://api-docs.deepseek.com/zh-cn/guides/ocr) /
[ASR](https://api-docs.deepseek.com/zh-cn/guides/asr) /
[TTS](https://api-docs.deepseek.com/zh-cn/guides/tts) API 封装成单个静态二进制，供 Agent 与脚本调用。CLI 提供能力，内嵌的 [`SKILL.md`](internal/skill/SKILL.md) 提供 Agent 使用契约。

```
fetch   <url...|->     POST /v1/tools/fetch
search  <query|->      POST /v1/tools/search
ocr     <file|->       POST /v1/tools/ocr
asr     <file|->       POST /v1/tools/asr
tts     <text|->       POST /v1/tools/tts

init                   写入并校验 API Key
config  path|show|set|unset|clear-key
doctor                 配置 / 鉴权 / 余额自检

cache   clear|path     本地响应缓存
skill   print|install  内嵌的 Agent Skill

help                   显示任何命令的帮助信息
version                [--format json]
```

## 构建与安装

当前未发布预编译包，以下为从源码构建的方式。需要 `go`（版本见 `go.mod`）和 bash；安装仅支持 macOS / Linux。

```bash
./dev.sh                          # 交互菜单：构建 / 安装 / 构建后安装
./dev.sh build v0.1.0             # 格式化 + vet/test，交叉编译 5 平台 + checksums/manifest
./dev.sh install                  # 安装最近一次构建的二进制 + Agent Skill
./dev.sh install --prefix /tmp/ds --no-skill   # 隔离安装
./dev.sh all v0.1.0               # 构建后立即安装
./dev.sh --help                   # 完整用法
```

`build` 产物写入 `dist/release/<version>/`；`install` 会校验 checksum、原子替换二进制、验证版本，默认装到 `PATH` 内第一个可写目录（`/opt/homebrew/bin`、`/usr/local/bin`、`~/.local/bin`、`~/bin`）。Windows 只支持构建，安装需手动复制产物。

开发快捷方式：`make build` / `make test`（仅本机，不生成发布产物）。

## 初始化

```bash
ds-tools init                        # 交互输入，不回显，写 0600
ds-tools init --api-key-stdin < key  # 非交互，不进 shell 历史
ds-tools doctor --probe              # 校验 Key 并查看余额
```

Key 优先级：`--api-key` > `DEEPSEEK_API_KEY` > 配置文件（`$DS_TOOLS_CONFIG` 或 `<用户配置目录>/ds-tools/config.json`）。

## 用法

```bash
# 先看候选（字段投影，省 token）
ds-tools search "DeepSeek V4 发布" -n 8 --jsonl --fields title,url,score,date

# 一次请求直接读正文
ds-tools search "某事 进展" -n 3 --content --min-score 1.5 --limit 8000 --format md

# 从 stdin 读搜索词
echo "rust async runtime" | ds-tools search - --jsonl --fields title,url

# 抓已知 URL
ds-tools fetch https://example.com/post --raw

# 批量抓取，每页一个 .md
ds-tools fetch -f urls.txt --jobs 4 -o pages/

# 长文分段读取
ds-tools fetch https://long.doc --offset 8000 --limit 8000

# OCR：扫描件 / 文档 -> 文本
ds-tools ocr scan.pdf
ds-tools ocr report.docx --purpose rich_layout_recognition --format md -o report.md
ds-tools ocr scan.pdf --page 1-3 --format jsonl

# ASR：音频 -> 文本
ds-tools asr speech.wav
ds-tools asr speech.ogg --format json
cat speech.wav | ds-tools asr - -o speech.txt

# TTS：文本 -> 音频
ds-tools tts "你好，我是 DeepSeek。" -o hello.wav
ds-tools tts - < article.md > speech.wav
ds-tools tts "..." --voice tide --audio-format pcm -o hello.pcm
```

Search/Fetch 按「成功且有结果」的请求计次，与 `--count`、`--content` 无关，所以读正文优先用 `search --content`，`fetch` 只用于已知 URL。OCR 按成功页数、ASR 按音频时长、TTS 按计费字符计费。

### 输出格式

`--format text|json|jsonl|md`。Search/Fetch 默认终端 `text`、管道 `json`；OCR/ASR 未显式指定时始终 `text`，需要结构化结果时显式 `--format json`；TTS 的 `--format` 只控制写入文件后的元信息，音频始终是二进制。
`--fields` 做字段投影；Search 支持别名 `score`→`rerank_score`、`site`→`site_name`。OCR 的 JSON 字段为 `id,object,created,purpose,page_count,pages,usage,text`，JSONL 字段为 `index,text,finish_reason`；ASR 字段为 `request_id,text,duration_ms`。
Search/Fetch 的 `md` 输出带标题、来源 URL 与最佳正文（`content` > `summary` > `snippet`）；OCR 的 `md` 每页带 `## 第 N 页`，ASR 的 `md` 带时长。

### OCR / ASR / TTS 限制

| 工具 | 输入限制 | 输出 | 默认超时 | 计费 |
|---|---|---|---|---|
| `ocr` | PDF/DOC/DOCX/PPT/PPTX/图片，≤100 MiB，≤1000 页 | 按页文本 | 210s | ¥1.5 / 1000 页 |
| `asr` | 16 位 PCM WAV 或 Ogg Opus，≤8 MiB，≤120 秒 | 转写文本 | 300s | ¥0.39 / 小时（限时 5 折） |
| `tts` | ≤10,000 计费字符（汉字算 2），请求体 ≤64 KiB | WAV / PCM 音频 | 600s | ¥1.5 / 10,000 字符（限时 5 折） |

OCR 的 `--purpose rich_layout_recognition` 会逐页识别并保留 `<｜ref｜>`/`<｜box｜>` 坐标标记；默认 `text_recognition` 对不含图片的 Word/PPT 直接提取文字。`--page` 是 1-based，支持 `1-3,5`、`5-`、`-3`。
ASR 会在本地校验 WAV 头（16 bit PCM、采样率、时长）或 Ogg Opus 魔数，避免无效请求。
TTS 音频可用 `-o file` 写文件，或在 stdout 是管道时直接输出：`ds-tools tts "..." > speech.wav`。终端下没有 `-o` 会拒绝执行。

### 搜索语法

原样透传给 DeepSeek，可自由组合：

| 操作符 | 示例 |
|---|---|
| `site:` / `-site:` | `DeepSeek site:github.com` · `-site:example.com` |
| 排除词 / 短语 | `-广告` · `-"release notes"` |
| 精确短语 | `"exact phrase"` |
| 语言 | `lang:zh,en` |
| 时间范围 | `after:2025-01-01` · `before:2026` |

质量信号：`rerank_score` 0–3（≥1.5 优质）、`authority` 0–4（≥3 极权威，-1 未知）。

## 退出码

| 码 | 含义 |
|---|---|
| 0 | 成功（**搜索无结果也是 0**） |
| 1 | 服务端/请求错误（400/402/403/404/413/500/504），不要重试 |
| 2 | Key 缺失或无效（401） |
| 3 | 网络错误（DNS/连接/TLS/超时） |
| 4 | 本地用法错误，未发出请求、未计费 |

408/429/5xx 自动重试（默认 2 次，遵循 `Retry-After`；TTS 只在收到音频前重试）。加 `--error-format json` 时 stderr 为 `{"status","type","code","retryable","message"}`。

## Agent 接入

```bash
ds-tools skill print                      # 完整使用契约
ds-tools skill install                    # -> ~/.claude/skills/ds-tools/SKILL.md
ds-tools skill install --dir ~/.pi/agent/skills
```

不提供 MCP：CLI + Skill 同时覆盖「人在终端」和「能执行 shell 的任意 Agent」。

## 全局选项

`--api-key` `--base-url` `--config` `--format` `--fields` `--timeout` `--retries` `--no-cache` `--dry-run` `-q/--quiet` `-v/--verbose` `--no-color` `--error-format`

环境变量：`DEEPSEEK_API_KEY`、`DEEPSEEK_BASE_URL`、`DS_TOOLS_CONFIG`、`DS_TOOLS_FORMAT`、`DS_TOOLS_SKILL_DIR`、`NO_COLOR`。

## 目录结构

```
cmd/ds-tools/        入口
internal/cli/        命令、渲染、退出码映射、中文文案（search/fetch/ocr/asr/tts）
internal/deepseek/   API 客户端（search/fetch/ocr/asr/tts/balance）、重试、错误类型
internal/config/     配置读写、Key 解析、脱敏
internal/cache/      磁盘 TTL 缓存
internal/skill/      内嵌 SKILL.md
```

本地缓存：Search/Fetch 按请求缓存；OCR 按文件内容 hash + `purpose` 缓存，ASR 按文件内容 hash 缓存；TTS 不缓存（产物是音频文件）。

限速：Search 100 QPS，Fetch 25 QPS（账号级、按目标域名），故 `--jobs` 默认 4；OCR 最多同时 15 个识别，ASR/TTS 最多同时 50 个请求。

## License

[MIT](LICENSE)
