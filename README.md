# nanocode-go

Go port of [nanocode](https://github.com/1rgs/nanocode) by [@1rgs](https://github.com/1rgs) — a minimal Claude Code alternative.

Single file, zero dependencies (stdlib only), ~330 lines.

Ported from nanocode commit [`b009d3d`](https://github.com/1rgs/nanocode/commit/b009d3dbedf14795a5c10804a5455386563f4b5b).

## Features

- Full agentic loop with tool use
- Tools: `read`, `write`, `edit`, `glob`, `grep`, `bash`
- Conversation history
- Colored terminal output
- OpenRouter and Anthropic API support

## Usage

```bash
go build -o nanocode .

export OPENROUTER_API_KEY="your-key"
./nanocode

# Custom model:
export MODEL="openai/gpt-4o"
./nanocode
```

Also loads env vars from `~/.env` automatically.

## Commands

- `/c` — Clear conversation
- `/q` or `exit` — Quit

## License

MIT
