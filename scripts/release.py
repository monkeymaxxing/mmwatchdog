#!/usr/bin/env python3
"""Build versioned, cgo-free Linux release bundles in MMWatchdog-owned storage."""
import argparse
import gzip
import hashlib
import io
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import tarfile

REPO = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(REPO / 'tests/integration'))
import testenv


def build(version, allow_dirty=False, output_dir=None):
    if not re.fullmatch(r'[0-9]+\.[0-9]+\.[0-9]+(?:-[A-Za-z0-9.]+)?', version):
        raise ValueError('version must look like 0.1.0 or 0.1.0-rc.1')
    revision = subprocess.check_output(['git', 'rev-parse', 'HEAD'], cwd=REPO, text=True).strip()
    dirty = bool(subprocess.check_output(['git', 'status', '--porcelain'], cwd=REPO))
    if dirty and not allow_dirty:
        raise ValueError('release requires a clean checkout; use --allow-dirty for local verification')
    epoch = int(subprocess.check_output(['git', 'show', '-s', '--format=%ct', 'HEAD'], cwd=REPO))
    output = Path(output_dir).resolve() if output_dir else testenv.root() / 'releases' / version
    output.mkdir(parents=True, exist_ok=True)
    metadata = {'version': version, 'revision': revision, 'modified': dirty,
                'go': subprocess.check_output(['go', 'version'], cwd=REPO, text=True).strip(), 'artifacts': {}}
    shared = {relative: REPO / 'examples/deploy' / relative for relative in (
        'mmx_project_settings.yml',
        'inventories/hub_and_standalone.yml', 'inventories/hub_members.yml',
        'playbooks/hub_and_standalone.yml', 'playbooks/join_hub.yml')}
    # The configuration sets of the playbooks, and the directory of the
    # project's own MMWatchdog modules, which the playbooks copy to the nodes.
    for directory in ('files', 'mmwatchdog_external_modules'):
        for path in sorted((REPO / 'examples/deploy' / directory).rglob('*')):
            if path.is_file():
                shared[str(path.relative_to(REPO / 'examples/deploy'))] = path
    shared.update({
              'mmwatchdog.service': REPO / 'examples/mmwatchdog.service',
              'README.md': REPO / 'README.md',
              'go.mod': REPO / 'go.mod'})
    # The source with the dependency copies in third_party/, and the official
    # extra modules in official_extra_modules/, which the playbooks install on
    # every node. An example module that is an executable keeps its
    # executable bit below.
    source_files = subprocess.check_output(
        ['git', 'ls-files', '--cached', '--others', '--exclude-standard', '-z', '--',
         'docs', 'examples', 'cmd', 'internal', 'scripts', 'tests', 'official_extra_modules',
         'third_party', 'LICENSE*', 'COPYING*', 'NOTICE*', 'official_extra_modules_test_seeds.go'], cwd=REPO, text=True)
    for relative in sorted(set(source_files.split('\0')) - {''}):
        path = REPO / relative
        if path.is_file():
            shared[relative] = path
    # The official extra modules of a release carry the version of the
    # release, and mmwatchdog refuses to run with official extra modules of another
    # version. The file in the repository says "0.1.0-dev", the version of a build
    # that is no release.
    shipped_with_mmwatchdog_version = output / 'shipped_with_mmwatchdog_version'
    shipped_with_mmwatchdog_version.write_text(version + '\n')
    shared['official_extra_modules/shipped_with_mmwatchdog_version'] = shipped_with_mmwatchdog_version
    goroot = Path(subprocess.check_output(['go', 'env', 'GOROOT'], cwd=REPO, text=True).strip())
    shared['licenses/Go-LICENSE'] = goroot / 'LICENSE'
    modules = subprocess.check_output(['go', 'list', '-m', '-json', 'all'], cwd=REPO, text=True)
    decoder = json.JSONDecoder()
    while modules.strip():
        module, offset = decoder.raw_decode(modules.lstrip())
        modules = modules.lstrip()[offset:]
        if module.get('Main') or not module.get('Dir'): continue
        for path in sorted(Path(module['Dir']).iterdir()):
            if path.is_file() and path.name.upper().startswith(('LICENSE', 'LICENCE', 'COPYING', 'NOTICE')):
                shared['licenses/' + module['Path'] + '@' + module['Version'] + '/' + path.name] = path
    for arch in ['arm64', 'amd64']:
        binary = output / ('mmwatchdog-linux-' + arch)
        env = dict(os.environ, CGO_ENABLED='0', GOOS='linux', GOARCH=arch)
        env.pop(testenv.LOCK_FD, None)
        subprocess.run(['go', 'build', '-trimpath',
                        '-ldflags', f'-s -w -X main.version={version} -X main.revision={revision}',
                        '-o', str(binary), './cmd/mmwatchdog'], cwd=REPO, env=env, check=True)
        name = f'mmwatchdog-{version}-linux-{arch}'
        files = dict(shared, mmwatchdog=binary)
        manifest = {'version': version, 'revision': revision, 'modified': dirty,
                    'os': 'linux', 'arch': arch,
                    'sha256': {n: hashlib.sha256(p.read_bytes()).hexdigest() for n, p in sorted(files.items())}}
        archive = output / (name + '.tar.gz')
        temporary = archive.with_suffix('.tmp')
        with temporary.open('wb') as stream, gzip.GzipFile(fileobj=stream, mode='wb', filename='', mtime=epoch) as compressed, tarfile.open(fileobj=compressed, mode='w') as tar:
            for relative, path in sorted(files.items()):
                info = tar.gettarinfo(str(path), arcname=name + '/' + relative)
                info.uid = info.gid = 0
                info.uname = info.gname = 'root'
                info.mtime = epoch
                info.mode = 0o755 if relative == 'mmwatchdog' or os.access(path, os.X_OK) else 0o644
                with path.open('rb') as source:
                    tar.addfile(info, source)
            data = (json.dumps(manifest, indent=2) + '\n').encode()
            info = tarfile.TarInfo(name + '/manifest.json')
            info.size, info.mtime, info.mode = len(data), epoch, 0o644
            tar.addfile(info, io.BytesIO(data))
        temporary.replace(archive)
        metadata['artifacts'][archive.name] = hashlib.sha256(archive.read_bytes()).hexdigest()
        binary.unlink()
    shipped_with_mmwatchdog_version.unlink()
    (output / 'SHA256SUMS').write_text(''.join(f'{digest}  {name}\n' for name, digest in sorted(metadata['artifacts'].items())))
    (output / 'build.json').write_text(json.dumps(metadata, indent=2) + '\n')
    print(output, flush=True)
    return output


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('version')
    parser.add_argument('--allow-dirty', action='store_true')
    parser.add_argument('--output-dir', help='write the release files to this directory instead of the testing directory, as the GitHub release workflow does')
    args = parser.parse_args()
    with testenv.locked(testenv.root()):
        build(args.version, args.allow_dirty, args.output_dir)
