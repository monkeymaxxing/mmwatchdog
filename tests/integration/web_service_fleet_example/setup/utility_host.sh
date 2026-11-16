#!/bin/sh
# The utility host (mmwatchdog-peer), which checks the application server over the
# network: nftables for the scenarios that block traffic, and the
# certificate of the test's certificate authority, which the site on
# mmwatchdog-dev serves and which the utility host's checks trust for that site.
# The site's name, app.example.com, leads to mmwatchdog-dev through /etc/hosts
# (host_names in scenarios.json). mmwatchdog-dev's setup has run before this
# script.
set -eu
export DEBIAN_FRONTEND=noninteractive
apt-get -o DPkg::Lock::Timeout=300 update -q
apt-get -o DPkg::Lock::Timeout=300 install -y -q --no-install-recommends nftables curl
install -d -m 0755 /etc/mmwatchdog
curl --fail --silent --show-error http://app.example.com/certificate-authority.pem \
  -o /etc/mmwatchdog/web-service-test-certificate-authority.pem
curl --fail --silent --show-error --cacert /etc/mmwatchdog/web-service-test-certificate-authority.pem \
  https://app.example.com/ | grep -q 'Example web service'
