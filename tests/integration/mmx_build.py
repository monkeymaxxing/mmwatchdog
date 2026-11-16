#!/usr/bin/env python3
"""Build the sibling MMX working tree into this test environment."""
import hashlib
import json
import os
from pathlib import Path
import subprocess
import testenv

SOURCE = Path(__file__).resolve().parents[3] / 'mmx'


def build():
    dest = testenv.root() / 'mmx-bin'
    manifest = dest / 'manifest.json'
    commit = subprocess.check_output(['git', '-C', str(SOURCE), 'rev-parse', 'HEAD'], text=True).strip()
    dirty = bool(subprocess.check_output(['git', '-C', str(SOURCE), 'status', '--porcelain'], text=True))
    dest.mkdir(parents=True, exist_ok=True)
    manifest.unlink(missing_ok=True)
    hashes = {}
    env = dict(os.environ, CGO_ENABLED='0')
    env.pop(testenv.LOCK_FD, None)
    for name, package, arch in [('mmx', './cmd/mmx', None), ('mmx-runner-linux-arm64', './cmd/mmx-runner', 'arm64'), ('mmx-runner-linux-amd64', './cmd/mmx-runner', 'amd64')]:
        build_env = dict(env)
        if arch:
            build_env.update(GOOS='linux', GOARCH=arch)
        # At the lowest priority, as MMWatchdog's own build: see scripts/build.sh.
        subprocess.run(['nice', '-n', '19', 'go', 'build', '-trimpath', '-o', str(dest / name), package], cwd=SOURCE, env=build_env, check=True)
        hashes[name] = hashlib.sha256((dest / name).read_bytes()).hexdigest()
    info = {'commit': commit, 'dirty': dirty, 'sha256': hashes}
    manifest.write_text(json.dumps(info, indent=2) + '\n')
    return info

if __name__ == '__main__':
    with testenv.locked(testenv.root()):
        print(json.dumps(build(), indent=2))
