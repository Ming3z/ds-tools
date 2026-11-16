---
name: ds-tools
description: DeepSeek 的 Search、Fetch、OCR、ASR、TTS CLI：需要联网检索、读取已知 URL 全文、识别 PDF/Office 或图片文字、转写 WAV/Ogg 音频、文字转语音，且本地无对应工具时使用。Use the DeepSeek Search/Fetch/OCR/ASR/TTS CLI for live web results, the full text of a known URL, text from PDF/Office files or images, WAV/Ogg transcripts, and text-to-speech, when no native tool covers it.
---

# ds-tools

一个静态二进制，封装 DeepSeek 自有的搜索引擎与文档/音频工具：

| 需求 | 命令 | 费用 |
|---|---|---|
| 为查询找到网页 | `ds-tools search` | 1 次请求，**与 `-n` 无关** |
| 找网页**并**读取全文 | `ds-tools search --content` | 1 次请求 |
| 读取已知 URL | `ds-tools fetch <url>` | 每个 URL 1 次请求 |
| 从文档或图片提取文字 | `ds-tools ocr <file>` | ¥1.5 / 1000 页 |
| 转写音频文件 | `ds-tools asr <file>` | ¥0.39 / 小时（限时 5 折） |
| 合成语音 | `ds-tools tts <text> -o out.wav` | ¥1.5 / 万计费字符（限时 5 折） |

Search/Fetch 按**成功且非空**的请求计费（¥5 / 1000 次）。`count` 与 `need_content`
不改变价格，所以通过 `search --content` 抓正文不额外收费——这就是 `fetch` 只用于
**已知 URL**、而非"搜索的第二步"的原因。OCR/ASR/TTS 分别按页数、音频时长和计费字符
计费，详见下文各节。

## 配置

```bash
ds-tools init                      # 交互输入 Key，校验后以 0600 写入
ds-tools init --api-key-stdin < k  # 非交互，Key 不进 shell 历史
ds-tools doctor --probe            # 配置 + 鉴权 + 连通性 + 余额
```

Key 解析顺序：`--api-key` 参数 > `DEEPSEEK_API_KEY` 环境变量 > `~/.config/ds-tools/config.json`。
永远不要打印 Key；`ds-tools config show` 已经显示掩码值。

## 读结果：始终做字段投影

原始搜索响应很大。只需要部分字段时，**总是传 `--fields`，优先用 `--jsonl`**——能让
工具输出更小、更易解析。

```bash
# 初筛：挑选候选
ds-tools search "某事 最新进展" -n 8 --jsonl --fields title,url,rerank_score,date,site_name

# 直接读最好的几页（1 次请求，不额外 fetch）
ds-tools search "某事 最新进展" -n 3 --content --min-score 1.5 --limit 8000 --format md

# 已知的确切 URL
ds-tools fetch https://example.com/post --raw
```

`--format` 在终端默认 `text`，search/fetch 走管道时默认 `json`。`ocr`/`asr` 不传
`--format json|jsonl|md` 时始终默认 `text`；`tts` 音频是二进制，`-o` 为文件时
`--format` 只控制元数据行。`md` 渲染成带标题和来源 URL 的现成可读文档。

## fields

规范字段：`title url snippet summary content site_name site_icon date rerank_score authority`
别名：`score`→`rerank_score`，`site`→`site_name`。

质量信号（用来过滤，不要猜）：
- `rerank_score` 0–3，**≥1.5 是好结果** → `--min-score 1.5`
- `authority` 0–4（−1 未知），**≥3 权威性很高** → `--min-authority 3`
- `date` 为 RFC 3339 UTC，可能为 `null`

## search 语法（原样传给 DeepSeek，可自由组合）

```
site:github.com            限定站点（可用 OR 组合：site:a OR site:b）
-site:example.com          排除站点
-广告 -"release notes"     排除某个词 / 某个短语
"exact phrase"             精确匹配
lang:zh,en                 限定语言
after:2025-01-01 before:2026   发布时间窗口
```

查询被关键词化时，用 `--ori-question "<用户的真实问题>"` 搜索，可改善意图匹配。
用 `--localization <ip>` 偏向某个地区。

## fetch：批量与长文档

```bash
ds-tools fetch -f urls.txt --jobs 4 -o pages/     # 每个 URL 一个 .md，并发 4
ds-tools fetch https://long.doc --offset 8000 --limit 8000   # 分页读取长文档
```

`--jobs` 应保持 ≤4：Fetch 按账号和按目标域名都限流 25 QPS。长页面必须用
`--offset/--limit` 分页，而不是一次性灌全量。多 URL 运行每行输出一个 JSON 对象；
某个 URL 失败不会中断其余 URL。

## ocr：文档与图片

```bash
ds-tools ocr scan.pdf                                   # 文本，全部分页
ds-tools ocr report.docx --purpose rich_layout_recognition --format md -o report.md
ds-tools ocr scan.pdf --page 1-3 --format jsonl         # 每行一页
cat scan.png | ds-tools ocr - --filename scan.png
```

- 输入：PDF、DOC/DOCX、PPT/PPTX、PNG/JPEG/WebP/GIF/BMP/TIFF；≤100 MiB，≤1000 页。
- `--purpose text_recognition`（默认）对无图的 Word/PPT 直接抽文字，对其他文件走 OCR；
  `rich_layout_recognition` 对每一页都做 OCR 并保留 `<｜ref｜>`/`<｜box｜>` 坐标。
- 默认输出 `text`。`--format json` 给出 `id, object, created, purpose, page_count,
  pages[], usage` 以及拼接后的 `text`；`jsonl` 每行一页。
- `--page` 从 1 开始，在客户端裁剪：`1-3,5`、`5-`、`-3`。
- 计费：¥1.5 / 1000 个成功识别的页；失败/超时不计费。结果按文件内容哈希 + purpose
  缓存；`--no-cache` 强制重新上传。

## asr：语音转文字

```bash
ds-tools asr speech.wav                 # 文字稿
ds-tools asr speech.ogg --format json   # {request_id, text, duration_ms}
cat speech.wav | ds-tools asr - -o speech.txt
```

- 输入：16-bit PCM WAV 或 Ogg Opus，≤8 MiB，≤120 秒。语言自动检测，没有语言参数。
- CLI 会在上传前校验 WAV 头/采样率/时长或 Ogg Opus 魔数，因此不支持的音频在本地就以
  退出码 4 失败，不产生费用。
- 默认输出 `text`；`json`/`jsonl` 支持 `--fields request_id,text,duration_ms`。
- 计费：¥0.39 / 小时（限时 5 折），按秒向上取整。按文件内容哈希缓存。

## tts：文字转语音

```bash
ds-tools tts "你好，我是 DeepSeek。" -o hello.wav
ds-tools tts - < article.md > speech.wav
ds-tools tts "..." --voice tide --audio-format pcm -o hello.pcm
```

- 音频是二进制。带 `-o file` 时 stdout 输出一行元数据（`text` 人类可读，`json` 机器
  可读）。不带 `-o` 时，仅当 stdout 是管道才把音频写到 stdout；终端会被拒绝并返回
  退出码 4。
- `--voice stella`（默认，女声）或 `tide`（男声）。`--audio-format wav`（默认）或
  `pcm`（24 kHz，16-bit 有符号，单声道）。
- 默认朗读前剥离 Markdown；传 `--keep-markdown` 可保留。
- 限制：10,000 计费字符（汉字 = 2，其他 = 1），请求体 ≤64 KiB，每次请求 ≤30 分钟音频。
- 计费：¥1.5 / 万计费字符（限时 5 折）。TTS 不缓存。

## 退出码（据此分支，绝不解析 stderr）

| 退出码 | 含义 | 你该怎么做 |
|---|---|---|
| 0 | 成功——**包括零搜索结果** | 继续；`results` 为空表示"没找到"，不是"坏了" |
| 1 | 服务端/请求错误（400/402/403/404/413/500/504） | **不要**重试；读消息（`402` = 余额不足，`403` = fetch 被策略拦截） |
| 2 | Key 缺失或无效（401） | 让用户运行 `ds-tools init` |
| 3 | 网络错误（DNS/连接/TLS/超时） | 稍后重试 |
| 4 | 本地用法错误（参数错误、`count` 越界、空查询） | 修正调用；没有产生费用 |

`--error-format json` 会在 stderr 给出 `{"status","type","code","retryable","message"}`。
408/429/5xx 内部已重试（2 次，遵守 `Retry-After`），所以可重试的失败只会在客户端
放弃后才暴露出来。

## 实用规则

- 搜索返回 `[]` 是有效答案——改写查询或去掉 `after:`/`site:` 过滤，而不是盲目重试。
- 只读需要的部分：`-n 3..8`，再用 `--limit` 限制正文。不要把 50 个整页塞进上下文。
- `--fill-missing` 会重新抓取 `content` 为空的结果，因此额外消耗请求；默认关闭。只有
  确实需要每个正文时才用。
- 大文档用 `--page` 分页 OCR，不要一次加载所有页；只有需要坐标时才用
  `rich_layout_recognition`，否则 `text_recognition` 更便宜、更干净。
- ASR 每次请求最多 120 秒；更长的录音先在外面切分再调用。
- 绝不把 TTS 音频打印到终端：用 `-o file` 或把 stdout 重定向到文件/播放器。只有标点
  重要时才用 `--keep-markdown`。
- OCR/ASR 结果按文件内容哈希缓存；`--no-cache` 强制重新请求。TTS 永不缓存。
- 不要在脚本里绑定新的 API Key；它已经在环境变量或配置里。
- `--dry-run` 校验并打印请求但不发送——用它免费检查参数。
- 参数语义以 `ds-tools <cmd> --help` 为准；本文件只写跨命令的契约（计费、退出码、默认格式、字段投影）。
