# Anthropic body proxy

A standalone Go service for debugging Anthropic Messages API requests and applying narrowly scoped body compatibility fixes. It uses only the Go standard library and preserves SSE streaming, cancellation, response headers and upstream error bodies.

## Run

```sh
go test -race ./...
go build -o anthropic-body-proxy .
./anthropic-body-proxy -listen 127.0.0.1:18083 -config config.example.json
```

Use `http://127.0.0.1:18083` as the Anthropic base URL. Supply your existing API key with each request. No key is embedded in the service or its configuration.

For Claude Code:

```sh
ANTHROPIC_BASE_URL=http://127.0.0.1:18083 claude --model claude-opus-5-5
```

The service converts `sk-ant-usr-` credentials to `x-api-key`, removes the two incompatible OAuth beta markers and clears Claude Code identity headers for that key family. Other credential families retain their original authentication headers. Other beta flags and query parameters are preserved.

## Body policies

The manual example leaves all body edits disabled. The macOS service installer uses the verified compatibility defaults in `config.macos.json` for a fresh installation. Configure independent switches in the policy JSON:

```json
{
  "strip_claude_attribution": false,
  "strip_claude_code_identity": false,
  "drop_metadata_user_id": false,
  "max_output_tokens": 0
}
```

- `strip_claude_attribution`: remove only the first system attribution block or first scalar line beginning `x-anthropic-billing-header:`. Later matching text and the remaining system instructions are preserved.
- `strip_claude_code_identity`: remove only the exact leading CLI boilerplate `You are Claude Code, Anthropic's official CLI for Claude.` or Agent SDK boilerplate `You are a Claude agent, built on Anthropic's Claude Agent SDK.`, optionally immediately after an attribution block. Longer instructions and later matching text are preserved. Both boilerplate policies apply only to `sk-ant-usr-` API keys, preserving normal OAuth identity.
- `drop_metadata_user_id`: remove only `metadata.user_id`, preserving other metadata.
- `max_output_tokens`: optional output cap; zero disables it. A cap that conflicts with an explicit thinking budget is refused.

Messages, tool definitions, thinking signatures, safeguards and permission controls are preserved. A no-op request retains its exact original body bytes. These switches are diagnostic controls; enabling one does not prove that a provider error has been repaired.

## Detailed debugging

`-debug-dir /private/directory` enables per-request JSON files with headers, before/after bodies, applied changes, response headers and error bodies. Known keys, credential headers, cookies, sensitive JSON fields and `sk-ant-*` text are redacted. Prompt and tool content remain available for debugging, so the directory is private (0700) and files are 0600. Existing shared directories are rejected rather than having their permissions changed.

`-log /private/requests.jsonl` writes metadata-only events. Detailed logs and runtime files are excluded from Git.

`-inspect-socket /private/directory/latest-request.sock` exposes the latest Messages request from memory through an owner-only Unix socket, for exact local replay. It is never exposed through HTTP and is not persisted to disk.

## Install as a separate macOS service

```sh
python3 scripts/install-macos.py
```

The installer creates `com.gwd.anthropic-body-proxy` under the current user's LaunchAgents and starts port 18083 independently of Nginx. It starts again when that user logs in and restarts after unexpected exits. Runtime configuration and logs live under `~/.local/state/anthropic-body-proxy/`. A fresh installation enables `strip_claude_attribution` and `strip_claude_code_identity`, leaves metadata unchanged, and applies no output-token cap. The existing policy file is preserved on reinstall.

On a new Mac with Homebrew installed, sign in to a GitHub account with access to this repository and run:

```sh
brew install go python gh
gh auth login
gh repo clone dickwu/anthropic-proxy ~/anthropic-proxy
cd ~/anthropic-proxy
python3 scripts/install-macos.py
curl -fsS http://127.0.0.1:18083/health
```

The health response is `{"status":"ok"}`. Installation enables detailed debug logs under `~/.local/state/anthropic-body-proxy/debug/` and metadata events in `requests.jsonl`. Configure each client to use `http://127.0.0.1:18083` and supply its API credential separately; the installer does not change client settings or store credentials.

To update an existing checkout, run `git pull --ff-only` followed by `python3 scripts/install-macos.py`.

After changing the policy, restart the service:

```sh
launchctl kickstart -k gui/$(id -u)/com.gwd.anthropic-body-proxy
```

## Validation

```sh
go test -race ./...
go vet ./...
python3 -m unittest discover -s scripts -p 'test_install_macos.py' -v
```

Tests cover body preservation and edits, credential redaction, private debug/inspector directories, upstream credential forwarding and immediate SSE delivery. Installer tests build the real binary inside a temporary user directory and verify fresh defaults, LaunchAgent configuration, private logs, and preservation of existing policies and client settings. They intercept launchctl calls to avoid changing the host's services.

## Verified Claude Code compatibility case

For a `sk-ant-usr-` key, exact curl replay of a complete Claude Code request returned HTTP 400 with a credit-balance error. Removing metadata or cache controls did not resolve it; either leading SDK identification block independently reproduced the error. Retaining the complete main system instructions and all other request fields while removing only the two leading SDK boilerplate blocks returned HTTP 200 and completed the stream.

For that observed case, enable `strip_claude_attribution` and `strip_claude_code_identity`. Leave `drop_metadata_user_id` false and `max_output_tokens` zero. This records observed compatibility behavior; the provider's internal billing implementation is not established by these tests.

Subagents use a different Agent SDK identity string. The same identity policy recognizes that exact string, with the same leading-position and credential restrictions. A same-key Fable comparison returned HTTP 400 with the SDK identity and HTTP 200 with a completed stream after removing only that identity.

Claude Code can separately delay delivery of a child-agent failure notification to its parent. See [subagent failure timing](docs/subagent-failure-timing.md) for measured API and notification timings. The proxy forwards upstream errors and does not retry requests.
