#!/bin/sh
# The storage of the application server (mmwatchdog-dev):
#   - 1 GiB of swap space in /swapfile, with vm.swappiness=10, so that the
#     machine swaps only when it must;
#   - /srv/uploads, a small filesystem for the users' uploads (tmpfs of
#     32 MiB and 1000 inodes), which a scenario fills;
#   - /srv/shared, which stands in for a network share that must be mounted
#     (tmpfs, in /etc/fstab, so that "mount /srv/shared" mounts it again);
#   - /dev/md0, a software RAID 1 mirror of two loop devices;
#   - datapool, a ZFS pool that mirrors two files, with the ZFS kernel
#     module that comes with Ubuntu's kernel.
set -eu
if ! swapon --show=NAME --noheadings | grep -qx /swapfile; then
  fallocate -l 1G /swapfile
  chmod 0600 /swapfile
  mkswap /swapfile >/dev/null
  swapon /swapfile
fi
sysctl -q vm.swappiness=10

install -d -m 0755 /srv/uploads /srv/shared
grep -q ' /srv/uploads ' /etc/fstab || echo 'tmpfs /srv/uploads tmpfs size=32m,nr_inodes=1000,mode=0755 0 0' >> /etc/fstab
grep -q ' /srv/shared ' /etc/fstab || echo 'tmpfs /srv/shared tmpfs size=16m,mode=0755 0 0' >> /etc/fstab
systemctl daemon-reload
mountpoint -q /srv/uploads || mount /srv/uploads
mountpoint -q /srv/shared || mount /srv/shared

install -d -m 0700 /var/lib/raid-mirror
if [ ! -e /dev/md0 ]; then
  devices=''
  for disk in disk-a disk-b; do
    truncate -s 64M "/var/lib/raid-mirror/$disk.img"
    devices="$devices $(losetup --find --show "/var/lib/raid-mirror/$disk.img")"
  done
  modprobe raid1
  # shellcheck disable=SC2086 # the two loop devices
  yes | mdadm --create /dev/md0 --run --level=1 --raid-devices=2 --metadata=1.2 --assume-clean $devices
  # The scenario fails and adds the second device of the mirror.
  echo "${devices##* }" > /var/lib/raid-mirror/second-device
fi
mdadm --detail /dev/md0 | grep -E 'State :|Active Devices'

install -d -m 0700 /var/lib/zfs-pool
if ! zpool list datapool >/dev/null 2>&1; then
  modprobe zfs
  echo 67108864 > /sys/module/zfs/parameters/zfs_arc_max
  for disk in disk-a disk-b; do
    truncate -s 128M "/var/lib/zfs-pool/$disk.img"
  done
  zpool create -o ashift=12 datapool mirror /var/lib/zfs-pool/disk-a.img /var/lib/zfs-pool/disk-b.img
fi
zpool status -x datapool
