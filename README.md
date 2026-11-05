# ds-tools

把 [DeepSeek Search / Fetch API](https://api-docs.deepseek.com/zh-cn/guides/search/) 封装成单个静态二进制，供 Agent 与脚本调用。CLI 提供能力，内嵌的 [`SKILL.md`](internal/skill/SKILL.md) 提供 Agent 使用契约。

```
init                   写入并校验 API Key
config  path|show|set|unset|clear-key
search  <query|->      POST /v1/tools/search
fetch   <url...|->     POST /v1/tools/fetch
doctor                 配置 / 鉴权 / 余额自检
cache   clear|path     本地响应缓存
skill   print|install  内嵌的 Agent Skill
version                [--json]
```

## 构建与安装

```bash
./build.sh                # 校验源码 + 交叉编译 5 平台 + checksums/manifest
./build.sh v0.1.0         # 指定版本

./install.sh              # 安装最新版本的二进制 + Agent Skill
./install.sh --prefix /tmp/ds --no-skill   # 隔离安装
```

`install.sh` 会校验 checksum、原子替换二进制、验证版本，默认装到 `PATH` 内第一个可写目录（`/opt/homebrew/bin`、`/usr/local/bin`、`~/.local/bin`、`~/bin`）。

开发快捷方式：`make build` / `make test`。

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
```

计费按「成功且有结果」的请求计次，与 `--count`、`--content` 无关，所以读正文优先用 `search --content`，`fetch` 只用于已知 URL。

### 输出格式

`--format text|json|jsonl|md`，默认终端 `text`、管道 `json`。
`--fields` 做字段投影，别名 `score`→`rerank_score`、`site`→`site_name`。
`md` 输出带标题、来源 URL 与最佳正文（`content` > `summary` > `snippet`）。

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

408/429/5xx 自动重试（默认 2 次，遵循 `Retry-After`）。加 `--error-format json` 时 stderr 为 `{"status","type","code","retryable","message"}`。

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
internal/cli/        命令、渲染、退出码映射、中文文案
internal/deepseek/   API 客户端（search/fetch/balance）、重试、错误类型
internal/config/     配置读写、Key 解析、脱敏
internal/cache/      磁盘 TTL 缓存
internal/skill/      内嵌 SKILL.md
```

限速：Search 100 QPS，Fetch 25 QPS（账号级、按目标域名），故 `--jobs` 默认 4。
