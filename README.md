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

All body edits are disabled by default. Configure independent switches in the policy JSON:

```json
{
  "strip_claude_attribution": false,
  "drop_metadata_user_id": false,
  "max_output_tokens": 0
}
```

- `strip_claude_attribution`: remove only the first system attribution block or first scalar line beginning `x-anthropic-billing-header:`. Later matching text and the remaining system instructions are preserved.
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

The installer creates `com.gwd.anthropic-body-proxy` under the current user's LaunchAgents and starts port 18083 independently of Nginx. Runtime configuration and logs live under `~/.local/state/anthropic-body-proxy/`. The existing policy file is preserved on reinstall.

After changing the policy, restart the service:

```sh
launchctl kickstart -k gui/$(id -u)/com.gwd.anthropic-body-proxy
```

## Validation

```sh
go test -race ./...
go vet ./...
```

Tests cover body preservation and edits, credential redaction, private debug/inspector directories, upstream credential forwarding and immediate SSE delivery.
