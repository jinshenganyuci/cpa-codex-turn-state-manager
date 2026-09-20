# Third-party attribution

The following files began from the MIT-licensed Tonkic/cpa-codex-turn-state-plugin at commit `ec9fa6695239ad12b12965b8f2dd6346f126c59f`: `main.go`, `plugin.go`, `fernet.go`, `probe.go`, `refresh.go`, `transport.go`, and the original transport test cases. They have been modified for this project. The original copyright and MIT terms are retained in `LICENSE`.

Source: https://github.com/Tonkic/cpa-codex-turn-state-plugin

Dashboard, task management, state retention and operational controls were informed by inspection of arden-aaai/cpa-plugin-codex-turn-state at commit `de5f21e52d73b8bcba78d8552283305ce951e7ee`. No source files were copied from that project.

Reference: https://github.com/arden-aaai/cpa-plugin-codex-turn-state

The dependency licenses are retained by their respective upstream packages: gopkg.in/yaml.v3 (MIT / Apache-2.0) and github.com/gorilla/websocket (BSD-2-Clause).

The probe-only proxy pool management, redacted endpoint summaries and separate business/probe routing were informed by FlashyyL/oai-adversarial-plugin at commit `4659221a6cd6ac9f46419682c08a40149fc40dea`. The pool implementation in this repository is independently written.

Reference: https://github.com/FlashyyL/oai-adversarial-plugin
