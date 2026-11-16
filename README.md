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

## 安装

macOS / Linux（需 Node.js ≥ 18）：

```bash
npm install -g @youmkdev/ds-tools
ds-tools skill install --all   # 为 Claude Code / Pi 等 Agent 安装 SKILL.md
ds-tools init                  # 交互式写入配置；脚本用 --api-key-stdin < key.txt
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
| `skill` | 安装 / 检查 Agent Skill |
| `version` | 显示版本 |

- `search` 到 `tts` 为 API 命令；计费以 [官方文档](https://api-docs.deepseek.com/zh-cn/guides/search/) 为准，`search` 的 `-n` 与 `--content` 不影响单价。
- 配置文件默认 `<用户配置目录>/ds-tools/config.json`，可用 `--config` 或 `DS_TOOLS_CONFIG` 指定；Key 优先级见 `ds-tools config --help`。
- Agent 接入：`ds-tools skill install --all` 安装到全部三个 skills 目录，`ds-tools skill status` 检查内容是否与当前版本一致。
- 不提供 MCP：CLI + Skill 覆盖终端与能执行 shell 的 Agent。
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

## 开发

```bash
make build                              # 本机构建，不生成发布产物
make test                               # 本机测试
./dev.sh build v1.0.0                   # 生成 5 平台产物到 dist/release/<version>/
./dev.sh install                        # 安装最近一次构建
./scripts/prepare-npm.sh --version v1.0.0  # 生成 dist/npm/ 包目录，可 npm pack 验证
./dev.sh --help                         # 完整用法
```

### 发布

npm 包名：顶层 `@youmkdev/ds-tools`，平台包 `ds-tools-<os>-<arch>`（`ds-tools` 与已有 `dstools` 过于相似，registry 拒绝）。

推 tag 即完成发布：`.github/workflows/release.yml` 构建 5 平台产物、创建 GitHub Release，并通过 npm Trusted Publishing（OIDC）发布 5 个包，无需 token、无需人工批准。

首次配置：5 个包各自 Settings → Trusted publishing → GitHub Actions（`Ming3z` / `ds-tools` / `release.yml`，勾选允许 `npm publish`）。配置后 2 天内需成功发布一次完成绑定。

npm 版本不可覆盖：发布失败需修好后升版本号重发；`dist/npm/` 与 `dist/release/` 均不入库。

## License

[MIT](LICENSE)
