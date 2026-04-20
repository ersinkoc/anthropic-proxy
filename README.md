# anthropic-proxy

`anthropic-proxy` exposes an Anthropic-compatible `/v1/messages` API to clients such as Claude Code, then forwards requests to any OpenAI-compatible backend.

It is designed for the practical case where your client always asks for Claude-style model names, but you want to force everything to a model you control from `.env`.

## What It Does

```text
Claude Code / Anthropic SDK
        |
        |  POST /v1/messages
        v
anthropic-proxy
        |
        |  POST /v1/chat/completions
        v
OpenAI-compatible upstream
```

Supported upstreams include:

- OpenAI
- NVIDIA NIM
- Ollama
- vLLM
- LM Studio
- Groq
- DeepSeek
- OpenRouter
- LiteLLM

## Main Behavior

By default, the proxy now treats `DEFAULT_MODEL` as the model to actually use upstream.

That means:

- Claude Code may request `claude-opus`, `claude-sonnet`, or anything else.
- The proxy will still send your `.env` model upstream when `FORCE_MODEL=1`.
- You can change the upstream model by editing `.env`.
- Most config changes hot reload automatically without restarting the process.

## Features

- Anthropic-compatible `POST /v1/messages`
- Anthropic-compatible `POST /v1/messages/count_tokens`
- `GET /healthz`
- Sync and streaming support
- Tool call conversion between Anthropic and OpenAI formats
- Image block to `image_url` conversion
- Optional client auth with `PROXY_CLIENT_KEY`
- Force-model mode via `.env`
- Hot reload for request-time config

## Build

```bash
CGO_ENABLED=0 go build -ldflags="-s -w" -o anthropic-proxy .
```

Or:

```bash
make build
```

Cross-compile examples:

```bash
GOOS=linux   GOARCH=amd64 go build -o dist/anthropic-proxy-linux-amd64 .
GOOS=darwin  GOARCH=arm64 go build -o dist/anthropic-proxy-darwin-arm64 .
GOOS=windows GOARCH=amd64 go build -o dist/anthropic-proxy-windows-amd64.exe .
```

## Quick Start

1. Create your local env file:

```bash
cp .env.example .env
```

2. Edit `.env`.

3. Start the proxy:

```bash
./anthropic-proxy
```

4. Point Claude Code to the proxy:

```bash
export ANTHROPIC_BASE_URL=http://localhost:8787
export ANTHROPIC_API_KEY=anything
claude
```

If you set `PROXY_CLIENT_KEY`, use that value instead of `anything`.

## Windows Setup

This is the simplest setup path if you want to use the prebuilt Windows binary from the GitHub release.

### 1. Download the binary

Download `anthropic-proxy-windows-amd64.exe` from the latest release and place it in a folder such as:

```text
C:\tools\anthropic-proxy\
```

Example layout:

```text
C:\tools\anthropic-proxy\
  anthropic-proxy-windows-amd64.exe
  .env
```

You can rename the binary if you want:

```text
anthropic-proxy-windows-amd64.exe -> anthropic-proxy.exe
```

### 2. Create `.env`

Open PowerShell in the same folder and create `.env` from the example:

```powershell
Copy-Item .env.example .env
```

If you downloaded only the binary, create `.env` manually. Minimal NVIDIA example:

```env
UPSTREAM_URL=https://integrate.api.nvidia.com/v1/chat/completions
UPSTREAM_API_KEY=nvapi-...
DEFAULT_MODEL=z-ai/glm-5.1
FORCE_MODEL=1
LISTEN_ADDR=:8787
REQUEST_TIMEOUT_SEC=600
DEBUG=1
```

### 3. Start the proxy

From PowerShell:

```powershell
.\anthropic-proxy.exe
```

If your file still has the release name:

```powershell
.\anthropic-proxy-windows-amd64.exe
```

You should see startup logs similar to:

```text
anthropic-proxy
  listen   : :8787
  upstream : https://integrate.api.nvidia.com/v1/chat/completions
  default  : z-ai/glm-5.1
  force    : true
```

### 4. Verify that it is running

In another PowerShell window:

```powershell
Invoke-WebRequest http://127.0.0.1:8787/healthz | Select-Object -ExpandProperty Content
```

Expected output:

```text
ok
```

You can also inspect the active config:

```powershell
Invoke-WebRequest http://127.0.0.1:8787/ | Select-Object -ExpandProperty Content
```

### 5. Point Claude Code to the proxy on Windows

For the current PowerShell session only:

```powershell
$env:ANTHROPIC_BASE_URL = "http://127.0.0.1:8787"
$env:ANTHROPIC_API_KEY = "anything"
claude
```

If you enabled proxy auth:

```powershell
$env:ANTHROPIC_BASE_URL = "http://127.0.0.1:8787"
$env:ANTHROPIC_API_KEY = "my-local-proxy-key"
claude
```

For persistent user-level environment variables in PowerShell:

```powershell
[Environment]::SetEnvironmentVariable("ANTHROPIC_BASE_URL", "http://127.0.0.1:8787", "User")
[Environment]::SetEnvironmentVariable("ANTHROPIC_API_KEY", "anything", "User")
```

Then open a new terminal before running:

```powershell
claude
```

### 6. Running it in the background on Windows

If you do not want to keep one PowerShell window open, you can start the proxy in a separate process:

```powershell
Start-Process -FilePath ".\anthropic-proxy.exe" -WorkingDirectory (Get-Location)
```

If you want log files:

```powershell
Start-Process -FilePath ".\anthropic-proxy.exe" `
  -WorkingDirectory (Get-Location) `
  -RedirectStandardOutput ".\proxy.stdout.log" `
  -RedirectStandardError ".\proxy.stderr.log"
```

### 7. Editing config without restart

Most config changes are hot reloaded from `.env`. For example, you can change:

```env
DEFAULT_MODEL=z-ai/glm-5.1
```

to:

```env
DEFAULT_MODEL=meta/llama-3.1-8b-instruct
```

Then hit:

```powershell
Invoke-WebRequest http://127.0.0.1:8787/ | Select-Object -ExpandProperty Content
```

and the new model should appear without restarting the proxy.

### Windows Troubleshooting

- If PowerShell says the file cannot be found, make sure you are in the correct folder and use `.\anthropic-proxy.exe`.
- If Windows SmartScreen warns about the binary, use `More info` and then `Run anyway` if you trust the release you downloaded.
- If port `8787` is already in use, change `LISTEN_ADDR` in `.env` to another port such as `:8788`, then restart the proxy.
- If Claude Code cannot connect, confirm `http://127.0.0.1:8787/healthz` returns `ok` first.
- If requests hang, test the upstream directly before blaming the proxy.
- If you changed `LISTEN_ADDR`, restart is required. That one setting is not hot reloaded.
- If you use Command Prompt instead of PowerShell, session variables are:

```cmd
set ANTHROPIC_BASE_URL=http://127.0.0.1:8787
set ANTHROPIC_API_KEY=anything
claude
```

## How Model Selection Works

### Default mode

When `FORCE_MODEL=1`, every incoming model is replaced with `DEFAULT_MODEL`.

Example:

Incoming request:

```json
{
  "model": "claude-sonnet-4",
  "max_tokens": 256,
  "messages": [
    { "role": "user", "content": "hello" }
  ]
}
```

Upstream request becomes:

```json
{
  "model": "z-ai/glm-5.1",
  "messages": [
    { "role": "user", "content": "hello" }
  ]
}
```

if `.env` contains:

```env
DEFAULT_MODEL=z-ai/glm-5.1
FORCE_MODEL=1
```

### Mapping mode

If you set `FORCE_MODEL=0`, the proxy uses:

1. exact match in `MODEL_MAP`
2. prefix match in `MODEL_MAP`
3. `DEFAULT_MODEL` as fallback

Example:

```env
FORCE_MODEL=0
DEFAULT_MODEL=meta/llama-3.1-8b-instruct
MODEL_MAP={"claude-opus":"meta/llama-3.1-70b-instruct","claude-sonnet":"meta/llama-3.1-8b-instruct"}
```

## Hot Reload

The proxy checks `.env` on requests and reloads it automatically when the file changes.

Hot-reloaded settings:

- `UPSTREAM_URL`
- `UPSTREAM_API_KEY`
- `DEFAULT_MODEL`
- `FORCE_MODEL`
- `MODEL_MAP`
- `PROXY_CLIENT_KEY`
- `REQUEST_TIMEOUT_SEC`
- `DEBUG`

Not hot-reloaded:

- `LISTEN_ADDR`

Changing `LISTEN_ADDR` still requires restarting the process because the server socket is already bound.

## Config Reference

| Variable | Required | Default | Hot Reload | Description |
|---|---|---|---|---|
| `UPSTREAM_URL` | no | `https://api.openai.com/v1/chat/completions` | yes | OpenAI-compatible chat completions endpoint |
| `UPSTREAM_API_KEY` | yes | none | yes | Bearer token sent upstream |
| `DEFAULT_MODEL` | yes when `FORCE_MODEL=1` | none | yes | Model to send upstream |
| `FORCE_MODEL` | no | `1` | yes | Force every incoming request to `DEFAULT_MODEL` |
| `MODEL_MAP` | no | `{}` | yes | Optional model mapping JSON |
| `PROXY_CLIENT_KEY` | no | unset | yes | Require clients to send this key |
| `REQUEST_TIMEOUT_SEC` | no | `600` | yes | Per-request upstream timeout |
| `DEBUG` | no | `0` | yes | Enable request logging |
| `LISTEN_ADDR` | no | `:8787` | no | HTTP bind address |

## Example `.env` Files

### NVIDIA NIM

```env
UPSTREAM_URL=https://integrate.api.nvidia.com/v1/chat/completions
UPSTREAM_API_KEY=nvapi-...
DEFAULT_MODEL=meta/llama-3.1-8b-instruct
FORCE_MODEL=1
LISTEN_ADDR=:8787
REQUEST_TIMEOUT_SEC=600
DEBUG=1
```

### NVIDIA with GLM

```env
UPSTREAM_URL=https://integrate.api.nvidia.com/v1/chat/completions
UPSTREAM_API_KEY=nvapi-...
DEFAULT_MODEL=z-ai/glm-5.1
FORCE_MODEL=1
LISTEN_ADDR=:8787
REQUEST_TIMEOUT_SEC=600
DEBUG=1
```

### NVIDIA with Minimax

```env
UPSTREAM_URL=https://integrate.api.nvidia.com/v1/chat/completions
UPSTREAM_API_KEY=nvapi-...
DEFAULT_MODEL=minimaxai/minimax-m2.7
FORCE_MODEL=1
LISTEN_ADDR=:8787
REQUEST_TIMEOUT_SEC=600
DEBUG=1
```

### Ollama

```env
UPSTREAM_URL=http://localhost:11434/v1/chat/completions
UPSTREAM_API_KEY=ollama
DEFAULT_MODEL=qwen3-coder:30b
FORCE_MODEL=1
LISTEN_ADDR=:8787
REQUEST_TIMEOUT_SEC=600
DEBUG=0
```

### LM Studio / vLLM

```env
UPSTREAM_URL=http://localhost:8000/v1/chat/completions
UPSTREAM_API_KEY=not-needed
DEFAULT_MODEL=Qwen/Qwen3-Coder-30B
FORCE_MODEL=1
LISTEN_ADDR=:8787
REQUEST_TIMEOUT_SEC=600
DEBUG=0
```

### Mapping mode example

```env
UPSTREAM_URL=https://integrate.api.nvidia.com/v1/chat/completions
UPSTREAM_API_KEY=nvapi-...
FORCE_MODEL=0
DEFAULT_MODEL=meta/llama-3.1-8b-instruct
MODEL_MAP={"claude-opus":"meta/llama-3.1-70b-instruct","claude-sonnet":"meta/llama-3.1-8b-instruct","claude-haiku":"nvidia/llama-3.1-nemotron-nano-8b-v1"}
LISTEN_ADDR=:8787
REQUEST_TIMEOUT_SEC=600
DEBUG=1
```

## Claude Code Setup

Claude Code only needs an Anthropic-compatible base URL and some API key value.

If proxy auth is disabled:

```bash
export ANTHROPIC_BASE_URL=http://localhost:8787
export ANTHROPIC_API_KEY=anything
claude
```

If proxy auth is enabled:

```env
PROXY_CLIENT_KEY=my-local-proxy-key
```

then:

```bash
export ANTHROPIC_BASE_URL=http://localhost:8787
export ANTHROPIC_API_KEY=my-local-proxy-key
claude
```

## API Examples

### Health check

```bash
curl http://127.0.0.1:8787/healthz
```

Expected:

```text
ok
```

### Introspection

```bash
curl http://127.0.0.1:8787/
```

Example response:

```json
{
  "service": "anthropic-proxy",
  "upstream": "https://integrate.api.nvidia.com/v1/chat/completions",
  "default_model": "z-ai/glm-5.1",
  "force_model": true,
  "models": {},
  "request_timeout_sec": 600
}
```

### Anthropic sync request

```bash
curl http://127.0.0.1:8787/v1/messages \
  -H "content-type: application/json" \
  -d '{
    "model": "claude-sonnet-4",
    "max_tokens": 64,
    "messages": [
      { "role": "user", "content": "Reply with exactly: proxy works" }
    ]
  }'
```

Example response:

```json
{
  "id": "msg_xxx",
  "type": "message",
  "role": "assistant",
  "model": "claude-sonnet-4",
  "content": [
    { "type": "text", "text": "proxy works" }
  ],
  "stop_reason": "end_turn",
  "stop_sequence": null,
  "usage": {
    "input_tokens": 41,
    "output_tokens": 3
  }
}
```

### Anthropic streaming request

```bash
curl http://127.0.0.1:8787/v1/messages \
  -H "content-type: application/json" \
  -d '{
    "model": "claude-opus-4",
    "stream": true,
    "max_tokens": 64,
    "messages": [
      { "role": "user", "content": "Say hello" }
    ]
  }'
```

### Count tokens

```bash
curl http://127.0.0.1:8787/v1/messages/count_tokens \
  -H "content-type: application/json" \
  -d '{"model":"claude-test","max_tokens":10,"messages":[]}'
```

## Notes About Upstream Models

- Some upstream models are slower than others.
- Some reasoning models may emit hidden or partial reasoning content differently.
- Some providers behave inconsistently in streaming mode.
- If a model hangs directly at the upstream, the proxy cannot fix that.

For that reason, when debugging:

1. test the upstream directly first
2. test the same model through the proxy
3. compare the behavior

## Known Limitations

- `count_tokens` is only a rough estimate
- `cache_control` blocks are dropped because OpenAI-compatible APIs usually have no equivalent
- `LISTEN_ADDR` changes require restart
- Streaming support depends on how faithfully the upstream implements OpenAI-style SSE

## License

MIT
