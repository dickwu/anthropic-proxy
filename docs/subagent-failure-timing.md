# Subagent credit errors and delayed notifications

Two separate behaviors were observed on October 8, 2026.

## Request compatibility

Child agents send the exact standalone system text:

```text
You are a Claude agent, built on Anthropic's Claude Agent SDK.
```

The original identity policy recognized only the main CLI's identity text. It removed the child request's leading billing-attribution block but left the SDK identity, and the request still returned HTTP 400 with a credit-balance error.

A controlled same-key, same-model comparison through the local proxy reproduced HTTP 400 in 0.327 seconds with the SDK identity. Removing only that fixed identity returned HTTP 200 with a completed stream in 2.613 seconds. The key remained usable; the error alone did not establish exhausted credit.

The existing `strip_claude_code_identity` policy now also recognizes the exact SDK identity at the first system block or line, or immediately after a leading attribution block. It retains longer text, later matches, all actual instructions and other request fields. This applies only to `sk-ant-usr-` API-key requests. Signed thinking blocks and OAuth identity are preserved.

After deploying the fix on port 18083, replaying Child A's complete captured `claude-sonnet-5-5` request returned HTTP 200 and completed the SSE stream in 3.785 seconds. The request retained its original 128000 output-token limit, main system instructions and every other request field. The debug record confirmed only the leading attribution and SDK identity blocks were removed.

## Parent notification timing

Two real cases in installed Claude Code 2.1.295 showed delayed parent-visible notifications:

| Case | Child error recorded (UTC) | Parent notification received (UTC) | Delay |
| --- | --- | --- | --- |
| Child A | 20:27:16.397 | 20:35:15.885 | 7m 59.488s |
| Child B | 20:41:04.991 | 20:47:40.586 | 6m 35.595s |

The matching initial proxy requests completed in 0.585 and 0.415 seconds. The parent continued listing the children as `idle` before receiving their failure notifications. An idle listing therefore did not show whether the child's last request succeeded.

Across 86 inspected credit-error responses that afternoon, the proxy's median time to return upstream error headers was 0.330 seconds, with a maximum of 2.248 seconds. These HTTP requests do not account for the multi-minute notification gap. Captured SDK retry counters were zero; [the official Python SDK's default retry policy](https://platform.claude.com/docs/en/cli-sdks-libraries/sdks/python#retries) does not include HTTP 400.

The official Agent/ListAgents notification path belongs to the installed Claude Code executable. The local OMC hooks track starts and stops but do not own that notification transport. The proxy compatibility fix addresses the observed SDK request failure; it does not change parent notification scheduling in Claude Code.

## Inspect errors immediately

The service writes the upstream status as soon as it reads the error response. Inspect metadata events without waiting for the parent's agent notification:

```sh
tail -F ~/.local/state/anthropic-body-proxy/requests.jsonl
```

Filter events with `phase: "upstream_headers"` and `status >= 400`. Each event includes a request ID, model, applied body changes and the redacted error. Its detailed request record is in the sibling `debug/` directory. Runtime logs and user transcripts are excluded from this repository.
