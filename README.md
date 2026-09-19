# CPA Codex Turn State Manager

Native CPA plugin **0.2.3**. Acquire, privately retain and reuse Codex `X-Codex-Turn-State`, preferring **292** and using **332** as fallback. [中文安装说明](README_CN.md).

**Requests pass through when no valid cached state exists.** Background acquisition continues independently and successful states are used automatically. Installation requires no plugin-specific YAML or manually entered state values.

## Install through CPA

Requires native plugin ABI 1 / schema 4 and forwarding of plugin-injected state headers. Verified hosts: `jinshenganyuci/CLIProxyAPI` releases `v7.3.6-codex-identity.1` and `v7.3.8-codex-identity.1`. These hosts need no custom header mapping. Arbitrary stock or older builds are not guaranteed compatible.

1. Enable plugins in CPA's configuration UI and add this Plugin source registry URL:

   ```text
   https://raw.githubusercontent.com/jinshenganyuci/cpa-codex-turn-state-manager/main/registry.json
   ```

2. Open the plugin store, install and enable **回合状态管理**.
3. Open **回合状态**, check the desired credentials and models, then save. Default model options are `gpt-6-astra` and `gpt-5.6-sol`.

The CPA instance must already have working Codex OAuth credentials with their own proxy configured. A fresh installation selects nothing and does not probe until the selection is saved. `config.example.yaml` is optional advanced configuration, not an installation step.

Dashboard: `/v0/resource/plugins/codex-turn-state-manager/status`. It reuses same-origin saved CPA login; a separately entered key is remembered after successful verification. The management API still requires authentication. Browser storage uses reversible obfuscation, not encryption.

Alternatively, download a platform ZIP from [Releases](https://github.com/jinshenganyuci/cpa-codex-turn-state-manager/releases), place its native library in CPA's plugin directory and enable it through CPA. Linux, Windows and macOS packages cover amd64 and arm64. Linux requires glibc.

## Defaults and behavior

- Prefer accepted 292; use 332 immediately while continuing to look for 292. Later 332 cannot overwrite valid 292.
- No-cache business requests pass through without waiting for acquisition. The legacy `block_without_state` option defaults to false.
- Check acquisition every five seconds; ordinary retries are at least five seconds apart. Different credential/model pairs run independently, at most one attempt per pair. Manual duplicates return immediately without a pending queue.
- Each acquisition reads the selected credential's current proxy. No separate pool, automatic proxy switching or direct fallback. Missing or invalid credential proxies fail that acquisition.
- Acquire with medium reasoning. Continuous mode has no total/hourly cap and consumes account quota. Authentication/rate/quota errors retain backoff; slow attempts are not overlapped for the same pair.
- Cache only after terminal success, host completion, exact reported/executed model agreement, structural validation and timestamp checks.
- Prepare refresh approximately 55 minutes after issuance; use a value for at most 60 minutes from issuance. Keep active and standby slots, each preferring 292. Duplicates never renew age or provenance.
- Capture accepted new business-response states as active/standby, showing Business observation provenance. A newer valid 292 standby can reduce extra acquisition.
- Scope capture, acquisition and reuse to checked credentials, account identities and exact executed models. Deselecting cancels affected attempts and rejects late candidates without deleting archives.
- Show credential email/plan and separate requested, executed and reported models. Plan labels do not determine state priority. Model names come from response fields, never length or generated answers; absent evidence remains unknown.
- An explicit business model mismatch invalidates only the value actually used by that attempt and can promote verified standby. Streamed content already sent cannot be retracted.

Cancel stops current attempts; automatic acquisition may resume. Deselect and save to stop a pair. New credentials are not selected automatically. Stale multi-window selection writes return 409; failed persistence leaves the prior selection intact.

## Storage and upgrades

By default, private files live in `<CPA plugins.dir>/.codex-turn-state-manager/`, discovered from the host's startup configuration. With no custom directory, this is `plugins/` under the CPA working directory. Preserve a writable persistent mount for that directory. Existing explicit paths remain respected. Embedded hosts without a normal CPA config may supply an explicit `state_file`.

Atomic files retain active/standby state, selections, the newest 200 request observations and backoff. Unix files use 0600 and private directories 0700; Windows access is governed by the storage directory's inherited ACL. Complete accepted values also remain in private archives after expiry; they are not automatically reimported. Never publish these files. The UI exposes metadata only, not raw state/OAuth tokens.

**An existing explicit `block_without_state: true` is preserved during upgrade. Remove it or set it to false to adopt passthrough.** Other explicit settings and selection paths are preserved too. Cached values without captured model evidence must be reacquired under strict admission. Old proxy-pool fields are tolerated but discarded.

Modes: default `force` uses valid cache, `observe` only captures, `replace_only` replaces an incoming 312-length state when cache exists, and `dry_run` records without changing the request. UI mode changes last until reconfiguration/restart; credential/model selections persist. See optional [configuration](config.example.yaml).

The older manual `codex-turn-state` plugin is a separate project. Use one state-writing plugin per credential.

## Limits

The state is opaque. Structural validation does not verify its signature, meaning, service tier or model capability. Neither accepted lengths nor reported model names prove reasoning quality. One hour is a local retention policy. Cross-turn replay is experimental; established WebSocket handshake headers cannot change mid-connection. The host owns OAuth refresh. No automatic account-plan priority or business proxy switching is implemented.

Authenticated API: `/v0/management/codex-turn-state-manager/{status,probe,cancel,mode,selection}`. Static resource routes contain no credential data. HTTP/SSE final response evidence and raw WebSocket metadata are used where available; translated Chat Completions streams without original model evidence remain unknown.

## Build and verify

Go 1.26+, a C compiler and CGO are required.

```bash
go test -race ./...
go vet ./...
bash scripts/build.sh
CPA_TEST_BINARY=/path/to/CLIProxyAPI CPA_TEST_PLUGIN="$PWD/dist/codex-turn-state-manager.so" go test -v -count=1 ./integration
```

Normal tests use synthetic data and skip native integration unless paths are provided. Native integration launches a real CPA with a local mock upstream: lifecycle, SSE/WebSocket capture, credential isolation, parallel acquisition, refresh, strict model admission, hot upgrades, and plugin-store installation without plugin parameters. Synthetic state values are not valid upstream credentials.

Browser checks:

```bash
CPA_PLAYWRIGHT_MODULE=/path/to/playwright node scripts/test-selection-ui.cjs
CPA_PLAYWRIGHT_MODULE=/path/to/playwright node scripts/test-login-ui.cjs
```

CI runs tests and browser checks, builds six native platform archives plus source, verifies checksums and publishes a GitHub Release. Historical verification documents describe their named versions and may have different defaults.

`cmd/livecheck` and `cmd/hunt292` are opt-in development tools, not the automatic runtime or part of normal tests. They require separately supplied private credentials/proxies and never run merely from installation.

## Attribution

The original native ABI, parsing and transport implementation was adapted from Tonkic's MIT-licensed plugin; management concepts were informed by arden-aaai's plugin. See [THIRD_PARTY.md](THIRD_PARTY.md) and [LICENSE](LICENSE). This project is independently maintained.
