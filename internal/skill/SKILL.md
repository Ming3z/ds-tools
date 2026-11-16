---
name: ds-tools
description: Use DeepSeek's Search, Fetch, OCR, ASR and TTS APIs to find live web pages, turn them into Markdown, read documents/images, transcribe audio, and synthesize speech. Use when a task needs current information from the web, the full text of a known URL, text from a PDF/Office file/image, a transcript of a WAV/Ogg audio file, or spoken audio from text, and no native tool is available.
---

# ds-tools

A single static binary that wraps DeepSeek's own search engine and document/audio tools:

| Need | Command | Cost |
|---|---|---|
| Find pages for a query | `ds-tools search` | 1 request, **regardless of `-n`** |
| Find pages **and** read their full text | `ds-tools search --content` | 1 request |
| Read a URL you already have | `ds-tools fetch <url>` | 1 request per URL |
| Extract text from a document or image | `ds-tools ocr <file>` | ¥1.5 / 1000 pages |
| Transcribe an audio file | `ds-tools asr <file>` | ¥0.39 / hour (limited-time 50%) |
| Synthesize speech | `ds-tools tts <text> -o out.wav` | ¥1.5 / 10,000 billing chars (50%) |

Search/Fetch billing is per **successful, non-empty** request (¥5 / 1000). `count` and
`need_content` do not change the price, so fetching content through `search --content`
is free of extra charge — that is why `fetch` is for **known URLs only**, not "step 2
of a search". OCR/ASR/TTS bill by page, audio duration, and billing characters; see
their sections below.

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

`--format` defaults to `text` on a terminal and `json` when piped for search/fetch.
`ocr`/`asr` always default to `text` unless you pass `--format json|jsonl|md`; `tts`
audio is binary, and `--format` only controls the metadata line when `-o` is a file.
`md` renders a ready-to-read document with headings and source URLs.

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

## ocr: documents and images

```bash
ds-tools ocr scan.pdf                                   # text, all pages
ds-tools ocr report.docx --purpose rich_layout_recognition --format md -o report.md
ds-tools ocr scan.pdf --page 1-3 --format jsonl         # one page per line
cat scan.png | ds-tools ocr - --filename scan.png
```

- Inputs: PDF, DOC/DOCX, PPT/PPTX, PNG/JPEG/WebP/GIF/BMP/TIFF; ≤100 MiB, ≤1000 pages.
- `--purpose text_recognition` (default) extracts text from image-free Word/PPT and OCRs
  other files; `rich_layout_recognition` OCRs every page and keeps `<｜ref｜>`/`<｜box｜>`
  coordinates.
- Default output is `text`. `--format json` gives `id, object, created, purpose,
  page_count, pages[], usage` plus a concatenated `text`; `jsonl` is one page per line.
- `--page` is 1-based and client-side: `1-3,5`, `5-`, `-3`.
- Billing: ¥1.5 / 1000 successfully recognized pages; failures/timeouts are not billed.
  Results are cached by file content hash + purpose; `--no-cache` forces re-upload.

## asr: audio to text

```bash
ds-tools asr speech.wav                 # transcript
ds-tools asr speech.ogg --format json   # {request_id, text, duration_ms}
cat speech.wav | ds-tools asr - -o speech.txt
```

- Inputs: 16-bit PCM WAV or Ogg Opus, ≤8 MiB, ≤120 seconds. Language is detected
  automatically; there is no language flag.
- The CLI validates the WAV header/sample rate/duration or the Ogg Opus magic before
  uploading, so unsupported audio fails locally with exit 4 and no charge.
- Default output is `text`; `json`/`jsonl` support `--fields request_id,text,duration_ms`.
- Billing: ¥0.39 / hour (limited-time 50% off), per second, rounded up. Cached by file
  content hash.

## tts: text to speech

```bash
ds-tools tts "你好，我是 DeepSeek。" -o hello.wav
ds-tools tts - < article.md > speech.wav
ds-tools tts "..." --voice tide --audio-format pcm -o hello.pcm
```

- Audio is binary. With `-o file`, stdout carries one metadata line (`text` human-readable,
  `json` machine-readable). With no `-o`, audio goes to stdout only when stdout is a
  pipe; a terminal is refused with exit 4.
- `--voice stella` (default, female) or `tide` (male). `--audio-format wav` (default) or
  `pcm` (24 kHz, 16-bit signed, mono).
- Markdown is stripped before reading by default; pass `--keep-markdown` to keep it.
- Limits: 10,000 billing characters (Han = 2, other = 1), request body ≤64 KiB, ≤30
  minutes of audio per request.
- Billing: ¥1.5 / 10,000 billing chars (limited-time 50% off). TTS is not cached.

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
- OCR large documents with `--page` instead of loading all pages; use `rich_layout_recognition` only when you need coordinates, otherwise `text_recognition` is cheaper and cleaner.
- ASR accepts at most 120 seconds per request; split longer recordings externally before calling.
- Never print TTS audio to a terminal: use `-o file` or pipe stdout to a file/player. `--keep-markdown` only when the punctuation matters.
- OCR/ASR results are cached by file content hash; `--no-cache` forces a fresh request. TTS is never cached.
- Never bind a new API key inside a script; it already exists in the environment or config.
- `--dry-run` validates and prints the request without sending it — use it to check flags for free.
