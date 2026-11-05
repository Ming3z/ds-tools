---
name: ds-tools
description: Use DeepSeek's Search and Fetch APIs to find live web pages and turn them into Markdown. Use when a task needs current information from the web (news, releases, docs, prices, changelogs, forum threads) or the full text of a known URL, and no native web tool is available.
---

# ds-tools

A single static binary that wraps DeepSeek's own search engine:

| Need | Command | Cost |
|---|---|---|
| Find pages for a query | `ds-tools search` | 1 request, **regardless of `-n`** |
| Find pages **and** read their full text | `ds-tools search --content` | 1 request |
| Read a URL you already have | `ds-tools fetch <url>` | 1 request per URL |

Billing is per **successful, non-empty** request (¥5 / 1000). `count` and `need_content`
do not change the price, so fetching content through `search --content` is free of
extra charge — that is why `fetch` is for **known URLs only**, not "step 2 of a search".

## Setup

```bash
ds-tools init                      # prompts for the key, verifies it, writes 0600
ds-tools init --api-key-stdin < k  # non-interactive, key never enters shell history
ds-tools doctor --probe            # config + auth + reachability + balance
```

Key resolution order: `--api-key` flag > `DEEPSEEK_API_KEY` env > `~/.config/ds-tools/config.json`.
Never print the key; `ds-tools config show` already shows a masked value.

## Reading results: always project fields

A raw search response is large. **Always pass `--fields` and prefer `--jsonl`** when you
only need a subset — it keeps tool output small and parseable.

```bash
# triage: pick candidates
ds-tools search "某事 最新进展" -n 8 --jsonl --fields title,url,rerank_score,date,site_name

# read the best pages directly (1 request, no extra fetch)
ds-tools search "某事 最新进展" -n 3 --content --min-score 1.5 --limit 8000 --format md

# exact URL you already know
ds-tools fetch https://example.com/post --raw
```

`--format` defaults to `text` on a terminal and `json` when piped (`jsonl` is usually
better for you than `json`). `md` renders a ready-to-read document with headings and
source URLs.

## fields

Canonical: `title url snippet summary content site_name site_icon date rerank_score authority`
Aliases: `score`→`rerank_score`, `site`→`site_name`.

Quality signals (use them to filter, do not guess):
- `rerank_score` 0–3, **≥1.5 is a good result** → `--min-score 1.5`
- `authority` 0–4 (−1 unknown), **≥3 is highly authoritative** → `--min-authority 3`
- `date` is RFC 3339 UTC, may be `null`

## search syntax (passed through to DeepSeek, freely combinable)

```
site:github.com            restrict to a site (combine with OR: site:a OR site:b)
-site:example.com          exclude a site
-广告 -"release notes"     exclude a word / a phrase
"exact phrase"             exact match
lang:zh,en                 restrict languages
after:2025-01-01 before:2026   publication window
```

Search with `--ori-question "<the user's real question>"` when the query is
keyword-ified; it improves intent matching. Use `--localization <ip>` to favour a region.

## fetch: batches and long documents

```bash
ds-tools fetch -f urls.txt --jobs 4 -o pages/     # one .md per URL, 4 in flight
ds-tools fetch https://long.doc --offset 8000 --limit 8000   # page through a long doc
```

`--jobs` should stay ≤4: Fetch is rate limited to 25 QPS per account and per target
domain. Long pages must be paged with `--offset/--limit` instead of dumped whole.
Multi-URL runs emit one JSON object per line; a failed URL does not abort the rest.

## exit codes (branch on these, never parse stderr)

| Code | Meaning | What you should do |
|---|---|---|
| 0 | success — **including zero search results** | continue; empty `results` means "nothing found", not "broken" |
| 1 | server/request error (400/402/403/404/413/500/504) | do **not** retry; read the message (`402` = out of balance, `403` = fetch blocked by policy) |
| 2 | missing or invalid key (401) | tell the user to run `ds-tools init` |
| 3 | network error (DNS/connect/TLS/timeout) | retry later |
| 4 | local usage error (bad flag, `count` out of range, empty query) | fix the invocation; nothing was billed |

`--error-format json` gives `{"status","type","code","retryable","message"}` on stderr.
408/429/5xx are already retried internally (2 attempts, `Retry-After` honoured),
so a retryable failure surfaces only after the client gave up.

## Practical rules

- A search returning `[]` is a valid answer — rephrase the query or drop `after:`/`site:` filters instead of retrying blindly.
- Read only what you need: `-n 3..8`, then `--limit` the body. Do not load 50 full pages into context.
- `--fill-missing` re-fetches results whose `content` came back empty and therefore costs extra requests; it is off by default. Use it only when you truly need every body.
- Never bind a new API key inside a script; it already exists in the environment or config.
- `--dry-run` validates and prints the request without sending it — use it to check flags for free.
