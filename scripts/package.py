#!/usr/bin/env python3
"""Build packages from explicit allowlists, never from credential directories."""
import hashlib
import re
from pathlib import Path
import sys
import zipfile
root = Path(__file__).resolve().parent.parent
platform, arch, extension = sys.argv[1:]
dist = root / 'dist'
name = 'codex-turn-state-manager'
version = re.search(r'pluginVersion\s*=\s*"([^"]+)"', (root / 'plugin.go').read_text()).group(1)
binary = dist / f'{name}.{extension}'
header = dist / f'{name}.h'
if header.exists():
    header.unlink()
docs = ['README.md', 'README_CN.md', 'LICENSE', 'THIRD_PARTY.md', 'config.example.yaml', 'LIVE_VERIFICATION_CN.md', 'DUAL_STATE_VERIFICATION_CN.md', 'CONTINUOUS_VERIFICATION_CN.md', 'SELECTION_VERIFICATION_CN.md', 'GUARD_VERIFICATION_CN.md', 'LOGIN_VERIFICATION_CN.md', 'DISPLAY_VERIFICATION_CN.md', 'PRIORITY_VERIFICATION_CN.md', 'CREDENTIAL_PROXY_VERIFICATION_CN.md']
def package(path, files):
    with zipfile.ZipFile(path, 'w', compression=zipfile.ZIP_DEFLATED) as archive:
        for source, target in files:
            if source.exists():
                info = zipfile.ZipInfo(target, date_time=(2026, 9, 19, 0, 0, 0))
                info.compress_type = zipfile.ZIP_DEFLATED
                info.external_attr = 0o100644 << 16
                archive.writestr(info, source.read_bytes())
release = dist / f'{name}_{version}_{platform}_{arch}.zip'
package(release, [(binary, binary.name), (dist/'live-verification.json', 'live-verification.json'), (dist/'dual-state-verification.json', 'dual-state-verification.json'), (dist/'continuous-verification.json', 'continuous-verification.json'), (dist/'selection-verification.json', 'selection-verification.json'), (dist/'guard-verification.json', 'guard-verification.json'), (dist/'login-verification.json', 'login-verification.json'), (dist/'display-verification.json', 'display-verification.json'), (dist/'priority-verification.json', 'priority-verification.json'), (dist/'credential-proxy-verification.json', 'credential-proxy-verification.json')] + [(root / f, f) for f in docs])
source = dist / f'{name}_{version}_source.zip'
files = [(dist/'live-verification.json','live-verification.json'), (dist/'dual-state-verification.json','dual-state-verification.json'), (dist/'continuous-verification.json','continuous-verification.json'), (dist/'selection-verification.json','selection-verification.json'), (dist/'guard-verification.json','guard-verification.json'), (dist/'login-verification.json','login-verification.json'), (dist/'display-verification.json','display-verification.json'), (dist/'priority-verification.json','priority-verification.json'), (dist/'credential-proxy-verification.json','credential-proxy-verification.json')] + [(p, p.relative_to(root).as_posix()) for p in root.glob('*.go')]
files += [(root/f,f) for f in docs + ['go.mod','go.sum','.gitignore','registry.json']]
for folder in ['web','scripts','integration','cmd/livecheck','cmd/hunt292']:
    files += [(p,p.relative_to(root).as_posix()) for p in (root/folder).iterdir() if p.is_file()]
files += [(p,p.relative_to(root).as_posix()) for p in (root/'.github').rglob('*') if p.is_file()]
package(source, files)
for p in [release,source]:
    p.with_suffix('.zip.sha256').write_text(f'{hashlib.sha256(p.read_bytes()).hexdigest()}  {p.name}\n')
checksums = ''.join(f'{hashlib.sha256(p.read_bytes()).hexdigest()}  {p.name}\n' for p in [binary,release,source])
(dist/'checksums.txt').write_text(checksums)
print(checksums,end='')
