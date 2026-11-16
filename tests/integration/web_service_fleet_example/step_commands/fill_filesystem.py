#!/usr/bin/env python3
"""Fills the filesystem at PATH until more than PERCENT percent of its space
or of its inodes are in use, with files named fill-*, or removes those
files again. The percent is the one of MMWatchdog's node_filesystem: used space
that ordinary accounts cannot fill, and used inodes.

  python3 step_commands/fill_filesystem.py PATH space|inodes PERCENT
  python3 step_commands/fill_filesystem.py PATH empty
"""
import os
from pathlib import Path
import sys


def used_percent(path, what):
    counts = os.statvfs(path)
    if what == 'space':
        return 100 * (1 - counts.f_bavail / counts.f_blocks)
    return 100 * (1 - counts.f_ffree / counts.f_files)


path, what = Path(sys.argv[1]), sys.argv[2]
if what == 'empty':
    for file in path.glob('fill-*'):
        file.unlink()
elif what == 'space':
    target = float(sys.argv[3])
    counts = os.statvfs(path)
    # The bytes to add, and one block more, so that the reading is above the target.
    missing = (target / 100 - used_percent(path, 'space') / 100) * counts.f_blocks * counts.f_frsize + counts.f_frsize
    if missing > 0:
        with open(path / f'fill-space-{target}', 'wb') as file:
            os.posix_fallocate(file.fileno(), 0, int(missing))
elif what == 'inodes':
    target = float(sys.argv[3])
    number = len(list(path.glob('fill-inode-*')))
    while used_percent(path, 'inodes') <= target:
        (path / f'fill-inode-{number}').touch()
        number += 1
else:
    sys.exit(f'unknown kind {what}: space, inodes, or empty')
print(f'{path}: space_used_percent is {used_percent(path, "space"):.1f}, inodes_used_percent is {used_percent(path, "inodes"):.1f}')
