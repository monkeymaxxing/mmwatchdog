#!/bin/sh
# The web server and the web application of the application server (mmwatchdog-dev):
#   - web-application.service: setup/web_application/web_application.py as
#     the account webapp on 127.0.0.1:8000, with Restart=no, so that a killed
#     program stays down until MMWatchdog restarts it;
#   - nginx: the default site answers 404 to every request, as a load
#     balancer's catch-all site does; the site app.example.com serves a
#     static front page and passes /health and /internal/ (with basic
#     authentication) to the application, over HTTP and over HTTPS;
#   - a certificate authority of this test, which signs the site's
#     certificate (step_commands/issue_site_certificate.sh). mmwatchdog-peer downloads the
#     authority's certificate from the site, to verify the site with it;
#   - report-generator.service, the application's report job, limited to
#     32 MiB of memory;
#   - the test tone that the audio encoder check encodes.
set -eu
here=$(pwd)
id webapp >/dev/null 2>&1 || useradd --system --home-dir /opt/web-application --shell /usr/sbin/nologin webapp
install -d -m 0755 /opt/web-application /opt/web-application/bin /opt/web-application/test-media
install -m 0755 setup/web_application/web_application.py /opt/web-application/web_application.py
for program in check-job-queue generate-report web-application-version; do
  install -m 0755 "setup/web_application/$program" "/opt/web-application/bin/$program"
done
echo 2.4.1 > /opt/web-application/VERSION
install -d -m 0755 -o webapp -g webapp /var/lib/web-application
echo 0 > /var/lib/web-application/stalled-workers
echo 1 > /var/lib/web-application/report-size-mebibytes
chown webapp:webapp /var/lib/web-application/stalled-workers /var/lib/web-application/report-size-mebibytes

# One second of a 440 Hz tone, for the check that encodes it.
python3 - <<'PYTHON'
import math, struct, wave
with wave.open('/opt/web-application/test-media/test-tone.wav', 'wb') as tone:
    tone.setnchannels(1)
    tone.setsampwidth(2)
    tone.setframerate(44100)
    tone.writeframes(b''.join(struct.pack('<h', int(12000 * math.sin(2 * math.pi * 440 * n / 44100))) for n in range(44100)))
PYTHON

cat > /etc/systemd/system/web-application.service <<'UNIT'
[Unit]
Description=The web application behind nginx, on 127.0.0.1:8000
After=network.target
[Service]
User=webapp
Group=webapp
ExecStart=/usr/bin/python3 /opt/web-application/web_application.py
Restart=no
[Install]
WantedBy=multi-user.target
UNIT
cat > /etc/systemd/system/report-generator.service <<'UNIT'
[Unit]
Description=The report job of the web application, which runs once and ends
[Service]
Type=oneshot
User=webapp
Group=webapp
ExecStart=/usr/bin/python3 /opt/web-application/bin/generate-report
MemoryMax=32M
MemorySwapMax=0
UNIT

# The certificate authority of this test and the site's certificate, valid for 90 days.
authority=/etc/web-service-test-certificate-authority
install -d -m 0700 "$authority"
if [ ! -f "$authority/certificate.pem" ]; then
  openssl req -x509 -newkey rsa:2048 -nodes -days 365 -subj '/CN=Web service fleet test certificate authority' \
    -keyout "$authority/key.pem" -out "$authority/certificate.pem" 2>/dev/null
fi
install -d -m 0755 /var/www/web-service
install -m 0644 "$authority/certificate.pem" /var/www/web-service/certificate-authority.pem
printf '<!doctype html>\n<title>Example web service</title>\n<h1>Example web service</h1>\n' > /var/www/web-service/index.html

# The login of the status page: nginx keeps its hash, MMWatchdog the user:password line.
status_page_password=$(openssl rand -hex 16)
printf 'status:%s\n' "$(openssl passwd -apr1 "$status_page_password")" > /etc/nginx/web-service-status-page.htpasswd
chown root:www-data /etc/nginx/web-service-status-page.htpasswd
chmod 0640 /etc/nginx/web-service-status-page.htpasswd
install -d -m 0700 /etc/mmwatchdog /etc/mmwatchdog/secrets
chmod 0755 /etc/mmwatchdog
printf 'status:%s\n' "$status_page_password" > /etc/mmwatchdog/secrets/web-application-status-page-login
chmod 0600 /etc/mmwatchdog/secrets/web-application-status-page-login

rm -f /etc/nginx/sites-enabled/default
cat > /etc/nginx/sites-enabled/web-service.conf <<'NGINX'
# The default site: every request for another host name gets 404, as on a
# load balancer. The check default_site_answers_not_found expects that 404.
server {
    listen 80 default_server;
    server_name _;
    return 404;
}

server {
    listen 80;
    listen 443 ssl;
    server_name app.example.com;
    ssl_certificate /etc/nginx/web-service-site-certificate.pem;
    ssl_certificate_key /etc/nginx/web-service-site-key.pem;
    root /var/www/web-service;
    index index.html;

    location = /health {
        proxy_pass http://127.0.0.1:8000;
        proxy_read_timeout 5s;
    }
    location /internal/ {
        auth_basic "web service status";
        auth_basic_user_file /etc/nginx/web-service-status-page.htpasswd;
        proxy_pass http://127.0.0.1:8000;
        proxy_read_timeout 5s;
    }
}
NGINX
sh "$here/step_commands/issue_site_certificate.sh" 90 --without-reload

systemctl daemon-reload
systemctl enable --now web-application.service
systemctl enable nginx.service
systemctl restart nginx.service
for attempt in $(seq 50); do
  curl -sf -H 'Host: app.example.com' http://127.0.0.1/health | grep -qx ready && break
  sleep 0.2
done
curl -sf -H 'Host: app.example.com' http://127.0.0.1/health | grep -qx ready
curl -sf --cacert "$authority/certificate.pem" --resolve app.example.com:443:127.0.0.1 https://app.example.com/ | grep -q 'Example web service'
