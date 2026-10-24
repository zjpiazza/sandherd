#!/usr/bin/env python3
"""Build the release bundle and collect registry attestations for its pinned images."""
import argparse
import gzip
import hashlib
import io
import json
import os
import pathlib
import shutil
import subprocess
import tarfile

root = pathlib.Path(__file__).resolve().parents[1]
parser = argparse.ArgumentParser()
parser.add_argument('--output', default=str(root / 'dist' / 'release'))
parser.add_argument('--require-license', action='store_true')
args = parser.parse_args()
output = pathlib.Path(args.output).resolve()
output.mkdir(parents=True, exist_ok=True)
if any(output.iterdir()):
    raise SystemExit('Use an empty output directory to avoid including stale release files.')
lock = json.loads((root / 'release/images.lock.json').read_text())
version = lock['version']
if args.require_license and not (root / 'LICENSE').is_file():
    raise SystemExit('A project LICENSE must be selected before publishing a release.')

def run(command, **kwargs):
    return subprocess.check_output(command, cwd=root, **kwargs)

revision = run(['git', 'rev-parse', 'HEAD']).decode().strip()
epoch = int(run(['git', 'show', '-s', '--format=%ct', 'HEAD']))
subprocess.run(['make', 'build-linux', 'VERSION=' + version, 'COMMIT=' + revision], cwd=root, check=True)
for arch in ['amd64', 'arm64']:
    with (output / ('sandherd-' + version + '-linux-' + arch + '.tar.gz')).open('wb') as destination:
        with gzip.GzipFile(filename='', mode='wb', fileobj=destination, mtime=epoch) as compressed:
            with tarfile.open(fileobj=compressed, mode='w') as archive:
                for path in sorted((root / 'dist' / ('linux-' + arch)).iterdir()):
                    if not path.is_file():
                        continue
                    info = archive.gettarinfo(str(path), arcname=path.name)
                    info.uid = info.gid = 0
                    info.uname = info.gname = ''
                    info.mtime = epoch
                    with path.open('rb') as source:
                        archive.addfile(info, source)
run(['helm', 'package', 'charts/sandherd', '--destination', str(output)])
foundation = run(['go', 'run', 'sigs.k8s.io/kustomize/kustomize/v5@v5.8.1', 'build', 'deploy/agent-sandbox/release'])
(output / 'agent-sandbox-v0.5.3.yaml').write_bytes(foundation)
for source, name in [
    ('deploy/control-plane/agent-crd.yaml', 'agents.sandherd.dev.yaml'),
    ('scripts/bootstrap-release-credentials.sh', 'bootstrap-credentials.sh'),
    ('charts/sandherd/values.yaml', 'values.yaml'),
    ('release/images.lock.json', 'images.lock.json'),
    ('docs/installation.md', 'INSTALL.md'),
    ('CHANGELOG.md', 'CHANGELOG.md'),
]:
    shutil.copyfile(root / source, output / name)
if (root / 'LICENSE').is_file():
    shutil.copyfile(root / 'LICENSE', output / 'LICENSE')
for component in lock['images']:
    for kind, field in [('sbom', 'SBOM'), ('provenance', 'Provenance')]:
        data = run(['docker', 'buildx', 'imagetools', 'inspect', component['image'], '--format', '{{json .' + field + '}}'])
        parsed = json.loads(data)
        if not all(parsed.get(platform) for platform in component['architectures']):
            raise SystemExit('Missing ' + kind + ' for ' + component['component'])
        (output / (component['component'] + '.' + kind + '.json')).write_bytes(data)
metadata = {'version': version, 'sourceCommit': revision,
            'sourceDirty': bool(run(['git', 'status', '--porcelain']).strip()),
            'licensePresent': (root / 'LICENSE').is_file()}
(output / 'build.json').write_text(json.dumps(metadata, indent=2) + '\n')
with (output / 'SHA256SUMS').open('w') as checksums:
    for path in sorted(output.iterdir()):
        if path.is_file() and path.name != 'SHA256SUMS':
            checksums.write(hashlib.sha256(path.read_bytes()).hexdigest() + '  ' + path.name + '\n')
print('Release bundle written to ' + str(output))
