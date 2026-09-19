"""Verify all six platform archives and the matching source before publication."""
import hashlib
import json
from pathlib import Path
import zipfile
root = Path(__file__).resolve().parent.parent
version = json.loads((root / 'registry.json').read_text())['plugins'][0]['version']
platforms = [('linux', 'amd64', 'so'), ('linux', 'arm64', 'so'), ('darwin', 'amd64', 'dylib'), ('darwin', 'arm64', 'dylib'), ('windows', 'amd64', 'dll'), ('windows', 'arm64', 'dll')]
archives = [(root / 'dist' / f'codex-turn-state-manager_{version}_{goos}_{arch}.zip', f'codex-turn-state-manager.{ext}') for goos, arch, ext in platforms]
archives.append((root / 'dist' / f'codex-turn-state-manager_{version}_source.zip', 'plugin.go'))
lines = []
for path, required in archives:
    digest = hashlib.sha256(path.read_bytes()).hexdigest()
    assert path.with_suffix('.zip.sha256').read_text().split()[0] == digest, path.name
    with zipfile.ZipFile(path) as archive:
        assert archive.testzip() is None and required in archive.namelist(), path.name
        if required != 'plugin.go':
            assert b'cliproxy_plugin_init' in archive.read(required), path.name
    lines.append(f'{digest}  {path.name}\n')
(root / 'dist' / 'checksums.txt').write_text(''.join(lines))
print('Seven release archives and six ABI exports verified')
