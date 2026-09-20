## Linux amd64 · built locally

- Hide unchecked credentials/models from the detail table and recent-request view. Keep all credentials in the selection controls.
- Add a probe-only proxy pool with add, edit, delete, enable/disable, connection test and cooldown reset buttons.
- Business API requests always keep their CPA credential proxy. Enabling the pool changes only independent state acquisition; an empty or cooling pool never falls back to the business proxy or direct access.
- Preserve concurrent acquisition across selected pairs. Fixed exits rotate and cool for 60 seconds after three failures; account limits retain their own backoff. Rotating exits do not cool merely because of state/model rejection.
- Test buttons send an unauthenticated HTTPS connectivity check, not a model request. Proxy passwords are never returned by status APIs; settings persist privately across reloads.
- A valid 292 or 332 pauses acquisition until refresh. Prefer existing 292; pass business requests through when no valid cache exists.

The pool defaults off on upgrade. Add proxies and enable it in the UI; no extra plugin YAML is required. Normal API credentials and their proxy settings are not edited.

CPA plugin source:
https://raw.githubusercontent.com/jinshenganyuci/cpa-codex-turn-state-manager/main/registry.json

This release is built locally and uploaded directly; no automatic GitHub build is triggered.
