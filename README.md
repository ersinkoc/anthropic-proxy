# anthropic-proxy

An Anthropic-compatible reverse proxy for Claude Code and other Anthropic SDK clients.
It talks to an OpenAI-compatible backend behind the scenes: OpenAI, vLLM, Ollama, LM Studio,
Groq, DeepSeek, Together, OpenRouter, LiteLLM, fastchat, and more.

**Single binary · stdlib-only · zero deps · 5.4 MB**

## Architecture

```text
Claude Code ──Anthropic API──▶ anthropic-proxy ──OpenAI API──▶ gpt-5 / qwen / llama / ...
            /v1/messages                      /v1/chat/completions
```

## Features

- `POST /v1/messages` — sync + streaming
- `POST /v1/messages/count_tokens` — estimated count (~4 chars/token)
- `GET /healthz`
- **Tool calling** — full Anthropic `tool_use` ↔ OpenAI `tool_calls` conversion, including streaming
- **Multi-modal** — image block → OpenAI `image_url` data URL
- **System prompt** — top-level string or block array → OpenAI `role:"system"`
- **tool_choice** — `auto` / `any`→`required` / `tool`→`function` / `none`
- **stop_sequences**, **temperature**, **top_p**, **max_tokens** pass through 1:1
- **Model mapping** — prefix-fallback mapping such as `claude-opus-4-7` → `gpt-5`
- **Stream state machine** — keeps text and tool_use blocks opening/closing correctly with a single-open-block invariant
- **Error passthrough** — upstream 4xx/5xx → Anthropic error envelope with the correct error type
- Optional client-side auth via `PROXY_CLIENT_KEY`

## Build

```bash
CGO_ENABLED=0 go build -ldflags="-s -w" -o anthropic-proxy .
```

Or:

```bash
make build
```

Cross-compile:

```bash
GOOS=linux   GOARCH=amd64 go build -o dist/anthropic-proxy-linux-amd64 .
GOOS=darwin  GOARCH=arm64 go build -o dist/anthropic-proxy-darwin-arm64 .
GOOS=windows GOARCH=amd64 go build -o dist/anthropic-proxy-windows-amd64.exe .
```

## Config (env)

The proxy auto-loads a local `.env` file on startup. Existing process environment variables still win, so you can override any value from the shell when needed.

| Var | Required | Default | Description |
|---|---|---|---|
| `UPSTREAM_URL` | — | `https://api.openai.com/v1/chat/completions` | OpenAI-compatible backend URL |
| `UPSTREAM_API_KEY` | ✓ | — | Upstream API key, injected as `Bearer` on every request |
| `MODEL_MAP` | ✓* | `{}` | JSON: `{"claude-opus-4-7":"gpt-5",...}` |
| `DEFAULT_MODEL` | ✓* | — | Fallback if a model is not found in the mapping |
| `LISTEN_ADDR` | — | `:8787` | Listen address |
| `PROXY_CLIENT_KEY` | — | (unset) | If set, incoming `x-api-key` must match |
| `REQUEST_TIMEOUT_SEC` | — | `600` | Upstream timeout |
| `DEBUG` | — | `0` | `1` enables per-request logging |

`*` At least one of these is required.

## .env Files

```bash
cp .env.example .env
```

Then edit `.env` with your provider settings. The repository keeps `.env.example` as a safe template and ignores `.env` via `.gitignore`.

## Run

```bash
UPSTREAM_URL=https://api.openai.com/v1/chat/completions \
UPSTREAM_API_KEY=sk-... \
MODEL_MAP='{
  "claude-opus-4-7":"gpt-5",
  "claude-sonnet-4-6":"gpt-5",
  "claude-haiku-4-5":"gpt-5-mini"
}' \
./anthropic-proxy
```

If `.env` is present, running the binary without extra shell exports is enough:

```bash
./anthropic-proxy
```

## With Claude Code

```bash
export ANTHROPIC_BASE_URL=http://localhost:8787
export ANTHROPIC_API_KEY=anything-works-unless-PROXY_CLIENT_KEY-set
claude
```

## Examples

**OpenAI:**
```text
UPSTREAM_URL=https://api.openai.com/v1/chat/completions
UPSTREAM_API_KEY=sk-proj-...
MODEL_MAP={"claude-opus-4-7":"gpt-5","claude-sonnet-4-6":"gpt-5","claude-haiku-4-5":"gpt-5-mini"}
```

**Ollama (local):**
```text
UPSTREAM_URL=http://localhost:11434/v1/chat/completions
UPSTREAM_API_KEY=ollama
DEFAULT_MODEL=qwen3-coder:30b
```

**vLLM / LM Studio:**
```text
UPSTREAM_URL=http://localhost:8000/v1/chat/completions
UPSTREAM_API_KEY=not-needed
DEFAULT_MODEL=Qwen/Qwen3-Coder-30B
```

**Groq:**
```text
UPSTREAM_URL=https://api.groq.com/openai/v1/chat/completions
UPSTREAM_API_KEY=gsk_...
MODEL_MAP={"claude-opus-4-7":"llama-3.3-70b-versatile"}
```

**DeepSeek:**
```text
UPSTREAM_URL=https://api.deepseek.com/v1/chat/completions
UPSTREAM_API_KEY=sk-...
MODEL_MAP={"claude-opus-4-7":"deepseek-chat","claude-sonnet-4-6":"deepseek-chat"}
```

**NVIDIA NIM:**
```text
UPSTREAM_URL=https://integrate.api.nvidia.com/v1/chat/completions
UPSTREAM_API_KEY=nvapi-...
DEFAULT_MODEL=minimaxai/minimax-m2.7
```

## Protocol Notes

### Request Flow (Anthropic → OpenAI)

| Anthropic | OpenAI |
|---|---|
| `system` (top-level) | first `messages` entry as `{role:"system"}` |
| `tool_result` block inside a user turn | separate `{role:"tool", tool_call_id}` message |
| `tool_use` block inside an assistant turn | `tool_calls[].function.arguments` as JSON **string** |
| `image` block (base64) | `image_url` data URL |
| `tool_choice: "any"` | `"required"` |

### Response Flow (OpenAI → Anthropic)

| OpenAI | Anthropic |
|---|---|
| `choices[0].message.content` | `content: [{type:"text"}]` |
| `choices[0].message.tool_calls[]` | `content: [{type:"tool_use", input: object}]` |
| `finish_reason:"stop"` | `stop_reason:"end_turn"` |
| `finish_reason:"length"` | `"max_tokens"` |
| `finish_reason:"tool_calls"` | `"tool_use"` |

### Stream Event Map

OpenAI `chat.completion.chunk` → Anthropic typed event machine:

```text
[first chunk]         → message_start
[content var, text]   → content_block_start(type=text) + content_block_delta(text_delta)
[tool_calls[i] var]   → content_block_start(type=tool_use) + content_block_delta(input_json_delta)
[block changes]       → content_block_stop(old) → content_block_start(new)
[finish_reason]       → content_block_stop + message_delta + message_stop
```

## Known Limitations

- `cache_control` blocks are silently dropped because OpenAI has no equivalent
- `count_tokens` is an estimate (`len(body)/4`); exact counting would require adding tiktoken or another tokenizer
- In streaming mode, `input_tokens` is `0` in the initial `message_start`; when usage arrives later from upstream, internal state is updated but `message_start` has already been sent. The final `message_delta` still reports the correct `output_tokens`.
- Prefill behavior, where the final input message is an assistant turn, may differ across OpenAI-compatible models

## License

MIT
