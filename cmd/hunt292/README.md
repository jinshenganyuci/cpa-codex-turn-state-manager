# Bounded 292 acquisition test

This opt-in driver leaves the plugin library unchanged. It sends at most 100 sequential requests for `gpt-6-astra`, alternating `high` and `medium`, using two explicitly authorized proxies. The four-request schedule is proxy 1 high, proxy 1 medium, proxy 2 high, proxy 2 medium.

Run only inside a Docker container with `--network none`. The private mount must contain `credential.json`, `proxy-1.json`, `proxy-2.json`, and fixed-destination Unix socket relays `hunt-proxy-1.sock` and `hunt-proxy-2.sock`. Proxy profiles have the form `{"url":"socks5://..."}` and must never be committed. Mount the existing plugin library read-only at `/artifact/codex-turn-state-manager.so`.

The test runs two isolated CPA processes in observation mode. CPA retries, plugin probes and streaming bootstrap retries are disabled. The driver uses the existing access token; it does not initiate OAuth refresh. Each attempt is saved before transmission and included in the 100-request cap, including failures or interrupted attempts. The journal resides in `/private/hunt-20260918-high-medium/report.json`; an interrupted run resumes against this same cap.

The driver stops immediately after finding a successful, structurally valid, timestamp-valid 292-character archive, preserving the full value privately. It also stops on HTTP 401/403/429, mismatched returned reasoning effort, five consecutive failures or the cap. No additional inference request tests the acquired value. Structural checks do not verify the opaque token's HMAC or model quality.

Only short status metadata is printed. A completed response must report `status: completed`; the report records both requested and returned model/reasoning, observed state length and plugin completion status. Source/runtime credentials and full state values are excluded from the public report.

```bash
go test -race ./cmd/hunt292
go vet ./cmd/hunt292
go build -o /secure/private/hunt292 ./cmd/hunt292
```
