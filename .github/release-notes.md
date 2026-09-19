Stop repeated acquisition once a valid 332 or 292 is cached. Both lengths pause background requests until the refresh window. Prefer 292 among already available values; do not make extra requests just to search for 292. A newer valid standby of either length can also postpone refresh.

Missing/invalid state still retries at the configured five-second spacing; business requests pass through when no valid state exists. Existing selections, credential proxies, cache files and UI controls remain compatible. No additional plugin parameters are required.

Only **Linux amd64** is published. This release was built and verified locally, without GitHub Actions builds.

CPA plugin source:
https://raw.githubusercontent.com/jinshenganyuci/cpa-codex-turn-state-manager/main/registry.json
