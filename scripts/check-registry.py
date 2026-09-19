"""Validate the public CPA store entry against the library metadata."""
import json
import os
import re
from pathlib import Path
root = Path(__file__).resolve().parent.parent
version = re.search(r'pluginVersion\s*=\s*"([^"]+)"', (root / 'plugin.go').read_text()).group(1)
data = json.loads((root / 'registry.json').read_text())
assert data['schema_version'] == 1 and len(data['plugins']) == 1
plugin = data['plugins'][0]
assert plugin['id'] == 'codex-turn-state-manager'
assert plugin['repository'] == 'https://github.com/jinshenganyuci/cpa-codex-turn-state-manager'
assert plugin['install']['type'] == 'github-release' and plugin['version'] == version
if os.environ.get('GITHUB_REF_TYPE') == 'tag':
    assert os.environ['GITHUB_REF_NAME'] == 'v' + version
print('CPA registry and version verified')
