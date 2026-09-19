Install from CPA's plugin store using this source:

https://raw.githubusercontent.com/jinshenganyuci/cpa-codex-turn-state-manager/main/registry.json

Enable CPA plugins, add the source in CPA configuration, then install **回合状态管理** from the plugin store. Open **回合状态**, check the desired credentials and models, and save. No plugin YAML parameters or manual state values are required. Existing CPA OAuth credentials must have their own proxy configured.

- Prefer validated 292; use 332 while continuing to acquire 292.
- Pass business requests through when no valid state exists; acquisition continues independently.
- Require matching reported/executed models, completed responses and valid timestamps before caching.
- Capture new business states as active/standby, without renewing duplicate values.
- Acquire different credential/model pairs concurrently, only through each credential's proxy.
- Preserve selections, active/standby and recent history in the CPA plugin directory.
- Include the management UI and same-origin remembered CPA login.

Linux packages require glibc. Verified with jinshenganyuci/CLIProxyAPI v7.3.6-codex-identity.1 and v7.3.8-codex-identity.1; other hosts need the native plugin API and state-header forwarding. Token lengths and reported model names are not proof of model quality. This is separate from the older manual `codex-turn-state` plugin; use one state-writing plugin for a given credential.

Existing installations with an explicit `block_without_state: true` must set it to false or remove the option to adopt passthrough. Fresh store installations already pass through by default.
