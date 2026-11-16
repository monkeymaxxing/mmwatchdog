#!/bin/sh
# Gives the site app.example.com on mmwatchdog-dev a new certificate that expires
# in DAYS days, signed by the certificate authority of this test, and
# makes nginx serve it.
#   sh step_commands/issue_site_certificate.sh DAYS [--without-reload]
set -eu
days=$1
authority=/etc/web-service-test-certificate-authority
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
printf 'subjectAltName=DNS:app.example.com\nextendedKeyUsage=serverAuth\n' > "$work/extensions"
openssl req -new -newkey rsa:2048 -nodes -subj '/CN=app.example.com' \
  -keyout "$work/key.pem" -out "$work/request.pem" 2>/dev/null
openssl x509 -req -in "$work/request.pem" -CA "$authority/certificate.pem" -CAkey "$authority/key.pem" \
  -CAcreateserial -days "$days" -extfile "$work/extensions" -out "$work/certificate.pem" 2>/dev/null
install -m 0600 "$work/key.pem" /etc/nginx/web-service-site-key.pem
install -m 0644 "$work/certificate.pem" /etc/nginx/web-service-site-certificate.pem
if [ "${2:-}" != --without-reload ]; then
  systemctl reload nginx.service
fi
