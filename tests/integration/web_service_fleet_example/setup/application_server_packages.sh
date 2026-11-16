#!/bin/sh
# Installs every package that the application server (mmwatchdog-dev) runs. While
# apt installs, /usr/sbin/policy-rc.d keeps each new service stopped, so a
# service first starts with the settings that the later setup scripts write.
set -eu
export DEBIAN_FRONTEND=noninteractive
printf '#!/bin/sh\nexit 101\n' > /usr/sbin/policy-rc.d
chmod 0755 /usr/sbin/policy-rc.d
echo 'postfix postfix/main_mailer_type select Local only' | debconf-set-selections
echo 'postfix postfix/mailname string app.example.com' | debconf-set-selections
apt-get -o DPkg::Lock::Timeout=300 update -q
apt-get -o DPkg::Lock::Timeout=300 install -y -q --no-install-recommends \
  nginx mariadb-server mariadb-client redis-server memcached postfix dnsmasq \
  mdadm zfsutils-linux lame nftables openssl curl python3
rm -f /usr/sbin/policy-rc.d
