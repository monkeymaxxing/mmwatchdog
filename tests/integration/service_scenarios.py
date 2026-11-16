"""Checks of common services against the real servers on a Linux guest: the
guest's own OpenSSH server, Postfix, Dovecot, memcached, Redis, dnsmasq,
PostgreSQL with pgBackRest, MariaDB, and Docker as the distribution
installs them, a small server of the test's own for a text exchange over
TCP and UDP, and a SQLite database that a program of the test writes. The
official extra modules apache_status and zfs_pool run against Apache and
against a real ZFS pool, and mattermost against a real Mattermost server
(mattermost_scenarios.py).

Every target distribution runs the service scenarios below.

zfs_pool checks a real pool, and its real scrubs, only on Ubuntu, whose
kernel packages hold ZFS's kernel module. A test of MMWatchdog builds no kernel
module, so every other distribution runs the check on a node without ZFS.
The module only reads ZFS's state, so the code that it runs is the same
everywhere."""
import json
import re
import shlex
import urllib.parse
import mattermost_scenarios
import vm

FIXTURE = '/var/tmp/mmwatchdog-fixture'
FRAGMENT = '/etc/mmwatchdog/check_groups/service_scenarios.yml'
# The name under which the checks reach the mail servers of the guest. The
# certificate of the test is valid for this name, and /etc/hosts gives it
# the address of the guest itself.
MAIL_HOST = 'mail.mmwatchdog-test.internal'
CERTIFICATE, PRIVATE_KEY = '/etc/mmwatchdog-test/mail.pem', '/etc/mmwatchdog-test/mail.key'
# A file of settings that turns the encryption of Dovecot off, for the
# scenario of a mail server that has no switch to encryption.
DOVECOT_WITHOUT_ENCRYPTION = '/etc/dovecot/conf.d/99-mmwatchdog-test-without-encryption.conf'
# The packages of Debian and Ubuntu. Docker 20.10 of Debian 12 starts no
# container where the kernel has AppArmor on and the program apparmor_parser
# is missing. A default Debian system has it, because apt installs apparmor
# with Docker as a recommended package; the Debian images of Hetzner tell apt
# to install no recommended package, so the suite names it.
PACKAGES = 'postfix dovecot-imapd dovecot-pop3d memcached redis-server dnsmasq-base postgresql pgbackrest mariadb-server docker.io apparmor busybox-static apache2'
# What differs between the distributions' packages of the servers, by the
# guest's package manager (vm.package_manager): the packages, the units, the
# place where Dovecot looks for its certificate, how a server starts that
# the package leaves without data, and MariaDB's and PostgreSQL's files. Arch
# Linux and openSUSE Leap start no server that a package brings, and make no
# data for it; the scenarios do both.
STATUS_PAGE_OF_APACHE = ('<Location "/server-status">\n  SetHandler server-status\n  Require local\n</Location>\nExtendedStatus On\n')
DOVECOT_WITH_POP3_AND_ENCRYPTION = 'protocols {\n  imap = yes\n  pop3 = yes\n}\nssl = yes\n'
# The same in the form of Dovecot 2.3, which the Red Hat family has.
DOVECOT_2_3_WITH_POP3_AND_ENCRYPTION = 'protocols = imap pop3\nssl = yes\n'
LAYOUTS = {
    'apt': {
        'packages': PACKAGES,
        'dovecot_certificate_and_private_key': ('/etc/dovecot/private/dovecot.pem', '/etc/dovecot/private/dovecot.key'),
        'units': 'postfix dovecot memcached redis-server postgresql mariadb docker apache2',
        'apache_unit': 'apache2',
        'mariadb_settings_directory': '/etc/mysql/mariadb.conf.d',
        'set_aside_mariadb_client': 'dpkg-divert --quiet --local --rename --divert /usr/bin/mariadb.set-aside-by-the-mmwatchdog-test --add /usr/bin/mariadb',
        'put_back_mariadb_client': 'if [ -e /usr/bin/mariadb.set-aside-by-the-mmwatchdog-test ]; then dpkg-divert --quiet --local --rename --remove /usr/bin/mariadb; fi',
        'pgbackrest_settings': '/etc/pgbackrest.conf',
        'pgbackrest_supports_the_postgresql': True,
        'prepare_the_mysql_client': 'true',
        'mariadb_client_logs_in_to_mysql': True,
        'setup': 'true',
        'docker_scenarios': True,
        'dnsmasq': 'dnsmasq',
    },
    'pacman': {
        'docker_scenarios': True,
        'dnsmasq': 'dnsmasq',
        'packages': 'postfix dovecot memcached valkey dnsmasq postgresql pgbackrest mariadb docker busybox apache',
        'dovecot_certificate_and_private_key': ('/etc/dovecot/ssl-cert.pem', '/etc/dovecot/ssl-key.pem'),
        'units': 'postfix dovecot memcached valkey postgresql mariadb docker httpd',
        'apache_unit': 'httpd',
        'pgbackrest_settings': '/etc/pgbackrest/pgbackrest.conf',
        'pgbackrest_supports_the_postgresql': True,
        # MySQL's own client program, from the image of MySQL, links libncurses.so.6; Arch Linux has only the wide-character library.
        'prepare_the_mysql_client': 'ln -sf libncursesw.so.6 /usr/lib/libncurses.so.6',
        'mariadb_client_logs_in_to_mysql': True,
        'mariadb_settings_directory': '/etc/my.cnf.d',
        'set_aside_mariadb_client': 'mv /usr/bin/mariadb /usr/bin/mariadb.set-aside-by-the-mmwatchdog-test',
        'put_back_mariadb_client': 'if [ -e /usr/bin/mariadb.set-aside-by-the-mmwatchdog-test ]; then mv /usr/bin/mariadb.set-aside-by-the-mmwatchdog-test /usr/bin/mariadb; fi',
        # Arch Linux's postgresql.service has the run-time directory /run/postgresql and removes it when the
        # primary stops, with the socket of the replica in it: the replica must outlive the primary.
        'setup': 'mkdir -p /etc/systemd/system/postgresql.service.d; printf "[Service]\\nRuntimeDirectoryPreserve=yes\\n" > /etc/systemd/system/postgresql.service.d/mmwatchdog-test.conf; systemctl daemon-reload; '
                 'install -d -o postgres -g postgres -m 0750 /var/lib/pgbackrest; newaliases; mkdir -p /etc/dovecot/conf.d; printf %s \'' + DOVECOT_WITH_POP3_AND_ENCRYPTION + '\' > /etc/dovecot/conf.d/10-mmwatchdog-test.conf; '
                 'if [ ! -s /var/lib/postgres/data/PG_VERSION ]; then runuser -u postgres -- initdb -D /var/lib/postgres/data --auth-local=peer --auth-host=scram-sha-256 >/dev/null; fi; '
                 'grep -q /etc/mmwatchdog-test/postgresql /var/lib/postgres/data/postgresql.conf || echo "include_dir = \'/etc/mmwatchdog-test/postgresql\'" >> /var/lib/postgres/data/postgresql.conf; '
                 '[ -d /var/lib/mysql/mysql ] || mariadb-install-db --user=mysql --basedir=/usr --datadir=/var/lib/mysql >/dev/null; '
                 'grep -q server-status /etc/httpd/conf/httpd.conf || printf %s \'' + STATUS_PAGE_OF_APACHE + '\' >> /etc/httpd/conf/httpd.conf; '
                 'systemctl start postgresql mariadb docker httpd',
    },
    'zypper': {
        'docker_scenarios': True,
        'dnsmasq': 'dnsmasq',
        'packages': 'postfix dovecot memcached valkey dnsmasq postgresql-server pgbackrest mariadb mariadb-client docker busybox-static apache2',
        'dovecot_certificate_and_private_key': ('/etc/dovecot/ssl-cert.pem', '/etc/dovecot/ssl-key.pem'),
        'units': 'postfix dovecot memcached valkey@default postgresql mariadb docker apache2',
        'apache_unit': 'apache2',
        'pgbackrest_settings': '/etc/pgbackrest.conf',
        'pgbackrest_supports_the_postgresql': False,
        'prepare_the_mysql_client': 'true',
        'mariadb_client_logs_in_to_mysql': False,
        'mariadb_settings_directory': '/etc/my.cnf.d',
        'set_aside_mariadb_client': 'mv /usr/bin/mariadb /usr/bin/mariadb.set-aside-by-the-mmwatchdog-test',
        'put_back_mariadb_client': 'if [ -e /usr/bin/mariadb.set-aside-by-the-mmwatchdog-test ]; then mv /usr/bin/mariadb.set-aside-by-the-mmwatchdog-test /usr/bin/mariadb; fi',
        # Valkey runs as an instance of a template unit, from a copy of the package's example settings.
        'setup': 'install -m 0640 -o root -g valkey /etc/valkey/default.conf.example /etc/valkey/default.conf; '
                 'if [ ! -s /var/lib/pgsql/data/PG_VERSION ]; then runuser -u postgres -- initdb -D /var/lib/pgsql/data --auth-local=peer --auth-host=scram-sha-256 >/dev/null; fi; '
                 'grep -q /etc/mmwatchdog-test/postgresql /var/lib/pgsql/data/postgresql.conf || echo "include_dir = \'/etc/mmwatchdog-test/postgresql\'" >> /var/lib/pgsql/data/postgresql.conf; '
                 '[ -d /var/lib/mysql/mysql ] || mariadb-install-db --user=mysql --basedir=/usr --datadir=/var/lib/mysql >/dev/null; '
                 'a2enmod status >/dev/null; mkdir -p /etc/dovecot/conf.d; '
                 'grep -q "conf.d/" /etc/dovecot/dovecot.conf || printf %b \'ssl_server {\\n  cert_file = /etc/dovecot/ssl-cert.pem\\n  key_file = /etc/dovecot/ssl-key.pem\\n}\\n!include_try conf.d/*.conf\\n\' >> /etc/dovecot/dovecot.conf; '
                 'systemctl start postgresql mariadb docker valkey@default',
    },
    # Rocky Linux and AlmaLinux. Their repositories have no Docker and no pgBackRest (both
    # come from other projects' repositories), so the scenarios of Docker, MySQL, MongoDB, and
    # Mattermost, which run in Docker's containers, and the scenario of archiving through
    # pgBackRest, do not run. Redis is Valkey on AlmaLinux 10 (install() names the package).
    # SELinux is enforcing: Dovecot reads its certificate where the distribution's
    # configuration looks for one, and the test's dnsmasq runs from a copy in /usr/local/bin,
    # because SELinux lets the program of the distribution bind only the ports of DNS and DHCP
    # (a new file in /usr/local/bin gets the label of a program, and runs unconfined).
    'dnf': {
        'packages': 'postfix dovecot memcached dnsmasq postgresql-server mariadb-server httpd openssl',
        'docker_scenarios': False,
        'dnsmasq': '/usr/local/bin/mmwatchdog-test-dnsmasq',
        'dovecot_certificate_and_private_key': ('/etc/pki/dovecot/certs/dovecot.pem', '/etc/pki/dovecot/private/dovecot.pem'),
        'units': 'postfix dovecot memcached redis valkey postgresql mariadb httpd',
        'apache_unit': 'httpd',
        'pgbackrest_settings': '/etc/pgbackrest.conf',
        'pgbackrest_supports_the_postgresql': False,
        'prepare_the_mysql_client': 'true',
        'mariadb_client_logs_in_to_mysql': True,
        'mariadb_settings_directory': '/etc/my.cnf.d',
        'set_aside_mariadb_client': 'true',
        'put_back_mariadb_client': 'true',
        'setup': 'install -m 0755 /usr/sbin/dnsmasq /usr/local/bin/mmwatchdog-test-dnsmasq; '
                 'if [ ! -s /var/lib/pgsql/data/PG_VERSION ]; then postgresql-setup --initdb >/dev/null; fi; '
                 'grep -q /etc/mmwatchdog-test/postgresql /var/lib/pgsql/data/postgresql.conf || echo "include_dir = \'/etc/mmwatchdog-test/postgresql\'" >> /var/lib/pgsql/data/postgresql.conf; '
                 'printf "local all all peer\\nhost all all 127.0.0.1/32 scram-sha-256\\nhost all all ::1/128 scram-sha-256\\nlocal replication all peer\\n" > /var/lib/pgsql/data/pg_hba.conf; '
                 '[ -d /var/lib/mysql/mysql ] || mariadb-install-db --user=mysql --basedir=/usr --datadir=/var/lib/mysql >/dev/null; '
                 'grep -q server-status /etc/httpd/conf/httpd.conf || printf %s \'' + STATUS_PAGE_OF_APACHE + '\' >> /etc/httpd/conf/httpd.conf; '
                 'mkdir -p /etc/dovecot/conf.d; printf %s \'' + DOVECOT_2_3_WITH_POP3_AND_ENCRYPTION + '\' > /etc/dovecot/conf.d/10-mmwatchdog-test.conf; '
                 'systemctl start postgresql mariadb httpd',
    },
}


# Fedora's differences from Rocky Linux and AlmaLinux, which share the layout 'dnf': Dovecot is
# version 2.4 (the form of Debian 13's), and the repositories have Docker, as the package
# moby-engine with the client docker-cli, so the scenarios of Docker, MySQL, and MongoDB run.
# Fedora has no pgBackRest either. Redis is Valkey (install() names the package).
FEDORA_DIFFERENCES = {
    'packages': 'postfix dovecot memcached dnsmasq postgresql-server mariadb-server httpd openssl moby-engine docker-cli busybox',
    'docker_scenarios': True,
    # MySQL's own client program, from the image of MySQL, needs OpenSSL 3's libssl.so.3; Fedora 45 has OpenSSL 4,
    # and the package openssl3-libs keeps version 3 beside it.
    'prepare_the_mysql_client': 'dnf install -y -q openssl3-libs',
    # Fedora's MariaDB has the program mariadb, which the check prefers to mysql: it is set aside for the
    # part of the scenario that runs MySQL's own client program.
    'set_aside_mariadb_client': 'mv /usr/bin/mariadb /usr/bin/mariadb.set-aside-by-the-mmwatchdog-test',
    'put_back_mariadb_client': 'if [ -e /usr/bin/mariadb.set-aside-by-the-mmwatchdog-test ]; then mv /usr/bin/mariadb.set-aside-by-the-mmwatchdog-test /usr/bin/mariadb; fi',
    'units': 'postfix dovecot memcached redis valkey postgresql mariadb httpd docker',
    'setup': LAYOUTS['dnf']['setup'].replace(DOVECOT_2_3_WITH_POP3_AND_ENCRYPTION, DOVECOT_WITH_POP3_AND_ENCRYPTION).replace('systemctl start postgresql mariadb httpd', 'systemctl start postgresql mariadb httpd docker'),
}


def layout():
    if vm.package_manager() == 'dnf' and vm.distribution()[0] == 'fedora':
        return {**LAYOUTS['dnf'], **FEDORA_DIFFERENCES}
    return LAYOUTS[vm.package_manager()]


# The transient units of the scenarios, which vm.reset stops after an
# interruption.
TRANSIENT_UNITS = ['mmwatchdog-dns', 'mmwatchdog-text-server', 'mmwatchdog-writes-sqlite', 'mmwatchdog-memcached-misses', 'mmwatchdog-memcached-hits', 'mmwatchdog-holds-a-snapshot']
# The containers of the Docker scenario, and the image that the test makes
# from a program of the guest, so that it downloads none.
CONTAINERS = ['mmwatchdog-test-runs', 'mmwatchdog-test-health', 'mmwatchdog-test-ended']
IMAGE = 'mmwatchdog-test:latest'
DATABASE_PASSWORD = 'a database password of the test'
# The name under which the checks reach the database servers of the guest
# over an encrypted connection, and a second name. /etc/hosts gives both the
# address of the guest itself. A certificate authority of the test, which
# the guest does not trust by itself, signed the certificate of the
# database servers, and that certificate is valid for the first name only.
DATABASE_HOST, ANOTHER_NAME_OF_THE_DATABASE_HOST = 'database.mmwatchdog-test.internal', 'another-name.mmwatchdog-test.internal'
DATABASE_CERTIFICATES = '/etc/mmwatchdog-test/database'
DATABASE_AUTHORITY, DATABASE_CERTIFICATE, DATABASE_PRIVATE_KEY = (DATABASE_CERTIFICATES + '/' + name for name in ('certificate-authority.pem', 'server.pem', 'server-key.pem'))
# The files with which a scenario gives a database server the certificate,
# or takes its encryption away: for each server its settings and its copies
# of the certificate and of the private key. MariaDB's copies are in the
# directory of its settings: the AppArmor profile of MariaDB on Ubuntu 26.04
# lets the server read files in /etc only below /etc/mysql and /etc/mariadb.
# PostgreSQL has a second file of settings, with which it archives its
# write-ahead log through pgBackRest.
POSTGRESQL_SETTINGS = ' '.join('/etc/postgresql/*/main/' + name for name in ['conf.d/mmwatchdog-test.conf', 'conf.d/mmwatchdog-test-archiving.conf', 'mmwatchdog-test-server.pem', 'mmwatchdog-test-server-key.pem'])
# pgBackRest's settings, where the distribution's package puts them, and its
# repository, which the package makes. The scenario of archiving makes the
# stanza main in the repository, and removes it and the settings again.
PGBACKREST_REPOSITORY = '/var/lib/pgbackrest'


def pgbackrest_files(settings):
    return settings + ' ' + PGBACKREST_REPOSITORY + '/archive ' + PGBACKREST_REPOSITORY + '/backup'


def mariadb_files(settings_directory):
    return {'settings': settings_directory + '/99-mmwatchdog-test.cnf',
            'certificate': settings_directory + '/mmwatchdog-test-server.pem',
            'private_key': settings_directory + '/mmwatchdog-test-server-key.pem'}
# MySQL and MongoDB themselves. Debian has no package of either, so each
# runs as containers of its maker's own image, which the guest downloads.
# The checks run on the guest, with the client programs of the guest.
MYSQL_IMAGE, MONGODB_IMAGE = 'mysql:8.4', 'mongo:8.0'
# MongoDB 8.0 refuses to start on Linux 6.19 and newer, such as the kernel
# of Ubuntu 26.04 (MongoDB's issue SERVER-121912). On such a kernel the
# scenario takes MongoDB 8.2, which starts there.
MONGODB_IMAGE_FOR_LINUX_6_19_AND_NEWER = 'mongo:8.2'
DATABASE_NETWORK = 'mmwatchdog-test-databases'
# A MySQL source and a replica that follows it, each at a port of the guest.
MYSQL_SERVERS = {'mmwatchdog-test-mysql-source': 3307, 'mmwatchdog-test-mysql-replica': 3308}
# MongoDB: a server of its own, a server that asks for a login, and a
# replica set of two members and an arbiter.
MONGODB_SERVERS = {'mmwatchdog-test-mongodb-alone': 27017, 'mmwatchdog-test-mongodb-login': 27018, 'mmwatchdog-test-mongodb-1': 27021, 'mmwatchdog-test-mongodb-2': 27022, 'mmwatchdog-test-mongodb-arbiter': 27023}
MONGODB_PASSWORD = 'a password: with @ and /'
# Where the test puts the client programs that it takes out of the two
# images: MySQL's own client, and MongoDB's shell.
CLIENT_PROGRAMS = '/usr/local/lib/mmwatchdog-test'
# The ZFS pool of the test, on two files.
POOL = 'mmwatchdogtest'
# OpenZFS's setting that holds every scan of a pool where it is, so that a
# scrub stays in progress for as long as a scenario needs.
ZFS_SCAN_SUSPEND_PROGRESS = '/sys/module/zfs/parameters/zfs_scan_suspend_progress'
# What a scenario of InnoDB's history list length changes, 300 times, while
# a session holds a snapshot open: each change is a transaction whose old
# row version InnoDB keeps for that session.
HISTORY_TABLE = 'mmwatchdog_test.history'
CHANGES_THAT_GROW_THE_HISTORY = 300
SNAPSHOT_HELD = "START TRANSACTION WITH CONSISTENT SNAPSHOT; SELECT COUNT(*) FROM " + HISTORY_TABLE + "; SELECT SLEEP(3600)"
# A program that adds a row to a SQLite database 20 times a second and waits
# for a lock as Python's sqlite3 module does by default, for 5 seconds: it
# writes the file held when it ever got an error.
SQLITE_WRITER = '''import sqlite3, sys, time
path = sys.argv[1]
db = sqlite3.connect(path, isolation_level=None)
db.execute('PRAGMA journal_mode=WAL')
db.execute('CREATE TABLE IF NOT EXISTS jobs (id INTEGER PRIMARY KEY, name TEXT)')
while True:
    try:
        db.execute('INSERT INTO jobs (name) VALUES (?)', ('x' * 200,))
    except sqlite3.OperationalError as error:
        open(path + '.held', 'a').write(str(error) + '\\n')
    time.sleep(0.05)
'''
REDIS_PASSWORD = 'a password with spaces and $1'
# A server of the test's own that answers a line over TCP and a packet over
# UDP.
TEXT_SERVER = '''import socket, threading
tcp = socket.socket(); tcp.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1); tcp.bind(('127.0.0.1', 7000)); tcp.listen()
udp = socket.socket(socket.AF_INET, socket.SOCK_DGRAM); udp.bind(('127.0.0.1', 7001))
def packets():
    while True:
        text, sender = udp.recvfrom(1500)
        if text.strip() == b'STATUS':
            udp.sendto(b'READY 3 jobs', sender)
threading.Thread(target=packets, daemon=True).start()
while True:
    connection, _ = tcp.accept()
    with connection:
        connection.sendall(b'myapp 1.0\\r\\n')
        if connection.recv(100).strip() == b'STATUS':
            connection.sendall(b'READY 3 jobs\\r\\n')
'''
# A client of the guest's memcached server that asks it for one key again
# and again: with "misses", a key that the server does not hold, and with
# "hits", a key that the client stores first.
MEMCACHED_CLIENT = '''import socket, sys, time
connection = socket.create_connection(('127.0.0.1', 11211))
answers = connection.makefile('rb')
key = b'mmwatchdog-test-' + sys.argv[1].encode()
if sys.argv[1] == 'hits':
    connection.sendall(b'set ' + key + b' 0 0 5\\r\\nvalue\\r\\n')
    assert answers.readline() == b'STORED\\r\\n'
while True:
    connection.sendall(b'get ' + key + b'\\r\\n')
    while answers.readline() != b'END\\r\\n':
        pass
    time.sleep(0.05)
'''


def install():
    """Install the servers, and give the database servers and the two mail servers the certificates of the test."""
    facts = layout()
    if vm.package_manager() == 'apt':
        vm.console("echo 'postfix postfix/main_mailer_type select Local only' | debconf-set-selections; echo 'postfix postfix/mailname string " + MAIL_HOST + "' | debconf-set-selections; "
                   # Debian 13 has the program docker in a package of its own.
                   'export DEBIAN_FRONTEND=noninteractive; apt-get -o DPkg::Lock::Timeout=600 update -qq && apt-get -o DPkg::Lock::Timeout=600 install -y -qq ' + PACKAGES + " $(apt-cache policy docker-cli | grep -q 'Candidate: [0-9]' && echo docker-cli) >/dev/null", timeout=900)
    elif vm.package_manager() == 'dnf':
        # Redis, or Valkey in its place: one question finds which of the two the distribution
        # has, and one transaction installs everything, because each run of dnf reads the
        # package lists again.
        available = vm.console("dnf -q repoquery --queryformat '%{name} ' redis valkey", timeout=900).stdout.split()
        redis = next((name for name in ('redis', 'valkey') if name in available), None)
        assert redis, 'the package lists of the guest have neither redis nor valkey'
        vm.install_packages(facts['packages'] + ' ' + redis, timeout=1500)
        # The Red Hat family starts no server that a package brings.
        vm.console('systemctl daemon-reload; systemctl start memcached ' + redis_unit())
    else:
        vm.install_packages(facts['packages'], timeout=1500)
    vm.console('install -d -m 0755 /etc/mmwatchdog-test /etc/mmwatchdog-test/postgresql && chown postgres /etc/mmwatchdog-test/postgresql 2>/dev/null; ' + facts['setup'], timeout=600)
    # The database servers: a certificate authority of the test, and a
    # certificate that it signed for the first of the two names.
    vm.console('install -d -m 0755 ' + DATABASE_CERTIFICATES + ' && cd ' + DATABASE_CERTIFICATES + ' && '
               "openssl req -x509 -newkey rsa:2048 -nodes -days 2 -subj '/CN=Certificate authority of the MMWatchdog test' -keyout certificate-authority-key.pem -out " + DATABASE_AUTHORITY + ' >/dev/null 2>&1 && '
               'openssl req -newkey rsa:2048 -nodes -subj /CN=' + DATABASE_HOST + ' -keyout ' + DATABASE_PRIVATE_KEY + ' -out server.request >/dev/null 2>&1 && '
               'echo subjectAltName=DNS:' + DATABASE_HOST + ' > server.extensions && '
               'openssl x509 -req -in server.request -CA ' + DATABASE_AUTHORITY + ' -CAkey certificate-authority-key.pem -CAcreateserial -days 2 -extfile server.extensions -out ' + DATABASE_CERTIFICATE + ' >/dev/null 2>&1 && '
               'chmod 0644 ' + DATABASE_AUTHORITY + ' ' + DATABASE_CERTIFICATE + '; chmod 0600 certificate-authority-key.pem ' + DATABASE_PRIVATE_KEY)
    vm.console("sed -i '/ # mmwatchdog-database-test$/d' /etc/hosts; echo '127.0.0.1 " + DATABASE_HOST + ' ' + ANOTHER_NAME_OF_THE_DATABASE_HOST + " # mmwatchdog-database-test' >> /etc/hosts")
    vm.console('openssl req -x509 -newkey rsa:2048 -nodes -days 2 -subj /CN=' + MAIL_HOST + ' -addext subjectAltName=DNS:' + MAIL_HOST +
               ' -keyout ' + PRIVATE_KEY + ' -out ' + CERTIFICATE + ' >/dev/null 2>&1; chmod 0644 ' + CERTIFICATE + '; chmod 0600 ' + PRIVATE_KEY)
    vm.console("sed -i '/ # mmwatchdog-mail-test$/d' /etc/hosts; echo '127.0.0.1 " + MAIL_HOST + " # mmwatchdog-mail-test' >> /etc/hosts")
    # Postfix: this certificate, STARTTLS on port 25, and a port that is
    # encrypted from the first byte.
    vm.console("postconf -e 'inet_interfaces = loopback-only' 'smtpd_tls_cert_file = " + CERTIFICATE + "' 'smtpd_tls_key_file = " + PRIVATE_KEY + "' 'smtpd_tls_security_level = may'; "
               "postconf -M smtps/inet='smtps inet n - y - - smtpd -o smtpd_tls_wrappermode=yes'; systemctl restart postfix")
    # Dovecot: the same certificate in the place where the distribution's
    # configuration looks for one.
    # The distribution puts links to its own certificate there: the copy
    # takes the place of each link and leaves that certificate alone.
    dovecot_certificate, dovecot_private_key = facts['dovecot_certificate_and_private_key']
    vm.console('rm -f ' + dovecot_certificate + ' ' + dovecot_private_key + '; cp ' + CERTIFICATE + ' ' + dovecot_certificate + '; install -o root -g root -m 0600 ' + PRIVATE_KEY + ' ' + dovecot_private_key + '; systemctl restart dovecot')
    vm.console('systemctl restart memcached ' + redis_unit())


def redis_unit():
    """The unit of the Redis server, or of Valkey in its place, under the name that the distribution has for it."""
    return vm.console('for unit in redis-server redis valkey valkey@default; do if systemctl cat $unit.service >/dev/null 2>&1; then echo $unit; exit 0; fi; done; exit 1').stdout.strip()


def redis_program():
    """The command-line program of that server."""
    return vm.console('for program in redis-cli valkey-cli; do if command -v $program >/dev/null; then echo $program; exit 0; fi; done; exit 1').stdout.strip()


def has_ready_zfs():
    """Whether the guest's distribution serves ZFS ready to use: Ubuntu, whose kernel packages hold ZFS's kernel module. Debian builds the module on the node; a test of MMWatchdog builds no kernel module."""
    return vm.distribution()[0] == 'ubuntu'


def install_zfs():
    """Install ZFS's programs from Ubuntu's own package, and load the kernel module that came with Ubuntu's kernel. Without the package's recommendations, apt never builds a module with DKMS: a kernel without ZFS's module fails modprobe here."""
    vm.console('export DEBIAN_FRONTEND=noninteractive; apt-get -o DPkg::Lock::Timeout=600 install -y -qq --no-install-recommends zfsutils-linux >/dev/null && modprobe zfs', timeout=600)


def postgresql_without_clusters(data_directory):
    """PostgreSQL's files and commands on a distribution that has one server with a unit of its own, such as Arch Linux and openSUSE Leap. The server reads the settings files that the test writes into /etc/mmwatchdog-test/postgresql (the setup of the layout adds the include_dir line); the replica is a second data directory beside the first, on port 5433."""
    directory = '/etc/mmwatchdog-test/postgresql'
    replica = data_directory + '-replica'
    as_postgres = 'runuser -u postgres -- '
    return {
        'files': directory, 'settings': directory + '/mmwatchdog-test.conf', 'host_based_authentication': data_directory + '/pg_hba.conf',
        'archiving_settings': directory + '/mmwatchdog-test-archiving.conf', 'data_directory': data_directory,
        'stop': 'systemctl stop postgresql', 'start': 'systemctl start postgresql', 'restart': 'systemctl restart postgresql',
        'make_the_replica': 'rm -rf ' + replica + '; ' + as_postgres + 'pg_basebackup --pgdata ' + replica + ' --write-recovery-conf --wal-method stream; '
                            + as_postgres + 'pg_ctl --pgdata ' + replica + ' --options "-p 5433" --log ' + replica + '.log --wait start',
        'promote_the_replica': as_postgres + 'pg_ctl --pgdata ' + replica + ' --wait promote',
        'remove_the_replica': as_postgres + 'pg_ctl --pgdata ' + replica + ' --mode fast --wait stop 2>/dev/null; rm -rf ' + replica + ' ' + replica + '.log; true'}


def suite(cli, status, state, events, received_by_webhook, wait, mark, setup_step_between_scenarios_done):
    quick = {'check_every': '2s', 'time_limit_per_run': '1500ms', 'alert_after_failing_for': '0s', 'recovered_after_healthy_for': '0s'}
    groups = {}

    def reload():
        vm.write(FRAGMENT, json.dumps({'check_groups': groups}, indent=2))
        cli('reload', 0)

    def last(name):
        return state(name)['last_observation']

    def becomes(name, status, message=''):
        """Wait until the check has the status and a message that starts with the text."""
        try:
            return wait(lambda: last(name)['status'] == status and last(name)['message'].startswith(message) and last(name))
        except AssertionError:
            raise AssertionError((name, status, message, last(name))) from None

    def fresh(*names):
        """Wait until each of the checks has an observation that it made after this moment, so that what follows looks at no older one."""
        moment = vm.console("date --utc --date '+1 second' +%Y-%m-%dT%H:%M:%S").stdout.strip()
        wait(lambda: all(last(name)['at'] > moment for name in names), timeout=60)

    def refuses(name):
        """Wait until the check fails because nothing accepts its connection."""
        try:
            return wait(lambda: last(name)['status'] == 'failed' and 'connection refused' in last(name)['message'])
        except AssertionError:
            raise AssertionError((name, last(name))) from None

    install()
    setup_step_between_scenarios_done('the servers that the services suite checks are installed')
    try:
        # A text and its reply over TCP and UDP, with a server of the test's own.
        vm.write(FIXTURE + '/text_server.py', TEXT_SERVER)
        vm.console('systemd-run --quiet --unit mmwatchdog-text-server python3 ' + FIXTURE + '/text_server.py')
        groups['text'] = dict(quick, checks={
            'answers_ready_over_tcp': {'tcp': {'address': '127.0.0.1:7000', 'text_to_send': 'STATUS\r\n', 'reply_contains': 'READY', 'reply_matches_regex': 'READY \\d+ jobs'}, 'alert_severity': 'sos'},
            'greets': {'tcp': {'address': '127.0.0.1:7000', 'reply_contains': 'myapp 1.'}, 'alert_severity': 'warn'},
            'expects_what_never_comes': {'tcp': {'address': '127.0.0.1:7000', 'text_to_send': 'STATUS\r\n', 'reply_contains': 'IDLE'}, 'alert_severity': 'info'},
            'answers_ready_over_udp': {'udp': {'address': '127.0.0.1:7001', 'text_to_send': 'STATUS\n', 'reply_contains': 'READY'}, 'alert_severity': 'sos'},
            'gets_no_reply_over_udp': {'udp': {'address': '127.0.0.1:7001', 'text_to_send': 'OTHER\n'}, 'alert_severity': 'info'},
            'nobody_listens_over_udp': {'udp': {'address': '127.0.0.1:7002', 'text_to_send': 'STATUS\n'}, 'alert_severity': 'info'}})
        reload()
        for check in ['answers_ready_over_tcp', 'greets', 'answers_ready_over_udp']:
            becomes('text/' + check, 'ok', 'the reply holds what the check expects')
        never = becomes('text/expects_what_never_comes', 'failed', 'the reply is not one that contains "IDLE"')
        assert never['details'] == ['the server sent: myapp 1.0  READY 3 jobs'], never
        becomes('text/gets_no_reply_over_udp', 'failed', 'no reply arrived from 127.0.0.1:7001 within 1.5s (time_limit_per_run)')
        becomes('text/nobody_listens_over_udp', 'failed', 'no program listens on the UDP port of 127.0.0.1:7002')
        vm.console('systemctl stop mmwatchdog-text-server')
        refuses('text/answers_ready_over_tcp')
        becomes('text/answers_ready_over_udp', 'failed', 'no program listens on the UDP port of 127.0.0.1:7001')
        del groups['text']
        mark('a tcp check sends a text and reads until the reply keeps its rules, and a udp check does the same with one packet; a reply without the expected text is shown in the details, and a server that is gone fails both')

        # The SSH server of the guest, which the harness itself uses.
        groups['ssh'] = dict(quick, check_every='5s', checks={
            'ssh_server_answers': {'ssh': '127.0.0.1:22', 'alert_severity': 'sos'},
            'no_ssh_server': {'ssh': '127.0.0.1:2', 'alert_severity': 'info'}})
        groups['ssh']['checks']['mail_server_is_no_ssh_server'] = {'ssh': '127.0.0.1:25', 'alert_severity': 'info'}
        reload()
        greeted = becomes('ssh/ssh_server_answers', 'ok', 'the SSH server at 127.0.0.1:22 greeted with SSH-2.0-OpenSSH_')
        assert greeted['message'] == 'the SSH server at 127.0.0.1:22 greeted with ' + vm.console("ssh-keyscan -p 22 127.0.0.1 2>&1 | sed -n 's/^# 127.0.0.1:22 //p' | head -1").stdout.strip(), greeted
        refuses('ssh/no_ssh_server')
        # A busy guest's Postfix can greet after the run's read ends: the
        # check then fails with nothing to show. Wait for a run that saw it.
        becomes('ssh/mail_server_is_no_ssh_server', 'failed', 'the server at 127.0.0.1:25 sent no SSH greeting')
        other = wait(lambda: last('ssh/mail_server_is_no_ssh_server').get('details') and last('ssh/mail_server_is_no_ssh_server'))
        assert other['message'].startswith('the server at 127.0.0.1:25 sent no SSH greeting') and other['details'][0].startswith('the server sent: 220 '), other
        # The server's log shows an ordinary connection that ended before a
        # login, and no error. Debian and Ubuntu name the unit ssh, the others sshd.
        logged = vm.console("journalctl --unit ssh --unit sshd --since '-30s' --no-pager --output cat | grep -F '127.0.0.1'", check=False).stdout
        assert 'error' not in logged.lower() and 'invalid' not in logged.lower(), logged
        del groups['ssh']
        mark('an ssh check reads the greeting of the guest\'s own OpenSSH server, fails for a closed port and for a server of another protocol, and leaves no error in the SSH server\'s log')

        # The mail servers: Postfix for SMTP and Dovecot for IMAP and POP3,
        # each with every kind of connection security.
        def mail(port, security, trusted=True):
            server = {'address': MAIL_HOST + ':' + str(port), 'connection_security': security}
            if trusted and security != 'unencrypted':
                server['trusted_certificate_authority_file'] = CERTIFICATE
            return server
        groups['mail'] = dict(quick, checks={
            'smtp_unencrypted': {'smtp': mail(25, 'unencrypted'), 'alert_severity': 'sos'},
            'smtp_starttls': {'smtp': mail(25, 'starttls'), 'alert_severity': 'sos'},
            'smtp_implicit_tls': {'smtp': mail(465, 'implicit_tls'), 'alert_severity': 'sos'},
            'smtp_unknown_certificate': {'smtp': mail(25, 'starttls', trusted=False), 'alert_severity': 'info'},
            'imap_unencrypted': {'imap': mail(143, 'unencrypted'), 'alert_severity': 'sos'},
            'imap_starttls': {'imap': mail(143, 'starttls'), 'alert_severity': 'sos'},
            'imap_implicit_tls': {'imap': mail(993, 'implicit_tls'), 'alert_severity': 'sos'},
            'pop3_unencrypted': {'pop3': mail(110, 'unencrypted'), 'alert_severity': 'sos'},
            'pop3_starttls': {'pop3': mail(110, 'starttls'), 'alert_severity': 'sos'},
            'pop3_implicit_tls': {'pop3': mail(995, 'implicit_tls'), 'alert_severity': 'sos'},
            'imap_on_the_smtp_port': {'imap': mail(25, 'unencrypted'), 'alert_severity': 'info'}})
        reload()
        names = {'smtp': 'SMTP', 'imap': 'IMAP', 'pop3': 'POP3'}
        for check, inputs in groups['mail']['checks'].items():
            protocol = check.split('_')[0]
            if check in {'smtp_unknown_certificate', 'imap_on_the_smtp_port'}:
                continue
            server = inputs[protocol]
            valid = '' if server['connection_security'] == 'unencrypted' else ', and its certificate is valid'
            becomes('mail/' + check, 'ok', 'the ' + names[protocol] + ' server at ' + server['address'] + ' answers' + valid)
        unknown = becomes('mail/smtp_unknown_certificate', 'failed', 'the SMTP server at ' + MAIL_HOST + ':25: the switch to encryption: ')
        assert 'certificate' in unknown['message'], unknown
        becomes('mail/imap_on_the_smtp_port', 'failed', 'the IMAP server at ' + MAIL_HOST + ':25: its greeting: expected a line that starts with * OK, received 220 ')
        vm.console('systemctl stop postfix dovecot')
        for check in ['smtp_starttls', 'imap_implicit_tls', 'pop3_unencrypted']:
            refuses('mail/' + check)
        vm.console('systemctl start postfix dovecot')
        becomes('mail/smtp_starttls', 'ok')
        # The same servers without encryption: a check with starttls fails
        # and says that the server has no switch to it, and a check without
        # encryption goes on.
        vm.console("postconf -e 'smtpd_tls_security_level = none'; printf 'ssl = no\\n' > " + DOVECOT_WITHOUT_ENCRYPTION + '; systemctl restart postfix dovecot')
        becomes('mail/smtp_starttls', 'failed', 'the SMTP server at ' + MAIL_HOST + ':25 does not offer STARTTLS, which connection_security: starttls requires')
        becomes('mail/imap_starttls', 'failed', 'the IMAP server at ' + MAIL_HOST + ':143 refused to switch to encryption, which connection_security: starttls requires. Its answer to STARTTLS: ')
        becomes('mail/pop3_starttls', 'failed', 'the POP3 server at ' + MAIL_HOST + ':110 refused to switch to encryption, which connection_security: starttls requires. Its answer to STLS: -ERR ')
        fresh('mail/smtp_unencrypted', 'mail/imap_unencrypted', 'mail/pop3_unencrypted')
        for check in ['smtp_unencrypted', 'imap_unencrypted', 'pop3_unencrypted']:
            becomes('mail/' + check, 'ok')
        vm.console("postconf -e 'smtpd_tls_security_level = may'; rm -f " + DOVECOT_WITHOUT_ENCRYPTION + '; systemctl restart postfix dovecot')
        for check in ['smtp_starttls', 'imap_starttls', 'pop3_starttls']:
            becomes('mail/' + check, 'ok')
        del groups['mail']
        mark('smtp, imap, and pop3 checks talk to Postfix and Dovecot with each kind of connection security and verify the certificate through trusted_certificate_authority_file; an unknown certificate, a server of another protocol, and a stopped server fail the check with the step that failed; the same servers without encryption fail a check with starttls, which says that the server has no switch to encryption, and answer a check without encryption')

        # DNS: dnsmasq with records of the test's own.
        vm.console("systemd-run --quiet --unit mmwatchdog-dns " + layout()['dnsmasq'] + " --keep-in-foreground --user=root --port=5353 --listen-address=127.0.0.1 --bind-interfaces --no-resolv --no-hosts --local=/internal.example.com/ "
                   "--address=/app.internal.example.com/10.0.0.5 --host-record=two.internal.example.com,10.0.0.6 --host-record=two.internal.example.com,10.0.0.7 --mx-host=example.com,mail.example.com,10 "
                   "--txt-record=example.com,'v=spf1 mx -all' --cname=www.internal.example.com,app.internal.example.com --ptr-record=5.0.0.10.in-addr.arpa,app.internal.example.com "
                   "--srv-host=_sip._tcp.example.com,sip.example.com,5060")

        def dns(name, record_type, expected=None):
            asked = {'dns_server': '127.0.0.1:5353', 'record_name': name, 'record_type': record_type}
            if expected:
                asked['expected_record_values'] = expected
            return {'dns': asked, 'alert_severity': 'warn'}
        groups['dns'] = dict(quick, checks={
            'address': dns('app.internal.example.com', 'A', ['10.0.0.5']),
            'two_addresses_in_any_order': dns('two.internal.example.com', 'A', ['10.0.0.7', '10.0.0.6']),
            'another_address': dns('app.internal.example.com', 'A', ['10.0.0.99']),
            'one_address_of_two': dns('two.internal.example.com', 'A', ['10.0.0.6']),
            'mail_exchanger': dns('example.com', 'MX', ['MAIL.example.com.']),
            'text': dns('example.com', 'TXT'),
            'canonical_name': dns('www.internal.example.com', 'CNAME', ['app.internal.example.com']),
            'name_of_an_address': dns('10.0.0.5', 'PTR', ['app.internal.example.com']),
            'service': dns('_sip._tcp.example.com', 'SRV', ['sip.example.com:5060']),
            'no_such_name': dns('missing.internal.example.com', 'A'),
            'name_that_the_server_refuses': dns('www.elsewhere.example.org', 'A'),
            'no_server': {'dns': {'dns_server': '127.0.0.1:5354', 'record_name': 'app.internal.example.com', 'record_type': 'A'}, 'alert_severity': 'info'}})
        reload()
        at = ' at the DNS server 127.0.0.1:5353 is '
        becomes('dns/address', 'ok', 'the A record of app.internal.example.com' + at + '10.0.0.5')
        becomes('dns/two_addresses_in_any_order', 'ok', 'the A record of two.internal.example.com' + at + '10.0.0.6, 10.0.0.7')
        becomes('dns/another_address', 'failed', 'the A record of app.internal.example.com' + at + '10.0.0.5, expected 10.0.0.99')
        becomes('dns/one_address_of_two', 'failed', 'the A record of two.internal.example.com' + at + '10.0.0.6, 10.0.0.7, expected 10.0.0.6')
        becomes('dns/mail_exchanger', 'ok', 'the MX record of example.com' + at + 'mail.example.com')
        becomes('dns/text', 'ok', 'the TXT record of example.com' + at + 'v=spf1 mx -all')
        becomes('dns/canonical_name', 'ok', 'the CNAME record of www.internal.example.com' + at + 'app.internal.example.com')
        becomes('dns/name_of_an_address', 'ok', 'the PTR record of 10.0.0.5' + at + 'app.internal.example.com')
        becomes('dns/service', 'ok', 'the SRV record of _sip._tcp.example.com' + at + 'sip.example.com:5060')
        becomes('dns/no_such_name', 'failed', 'the DNS server 127.0.0.1:5353 has no A record of missing.internal.example.com')
        becomes('dns/name_that_the_server_refuses', 'failed', 'MMWatchdog got no A record of www.elsewhere.example.org at the DNS server 127.0.0.1:5353: the DNS server refused the query')
        assert last('dns/no_server')['status'] == 'failed', last('dns/no_server')
        vm.console('systemctl stop mmwatchdog-dns')
        wait(lambda: last('dns/address')['status'] == 'failed')
        del groups['dns']
        mark('a dns check asks dnsmasq for records of six kinds (A, MX, TXT, CNAME, PTR, and SRV), accepts the expected values in any order and any way of writing them, and fails for another value, for one value of two, for a name without a record, and for a server that is gone')

        # memcached and Redis as the distribution installs them.
        vm.write('/etc/mmwatchdog/secrets/redis', REDIS_PASSWORD + '\n', '0600')
        vm.write('/etc/mmwatchdog/secrets/redis-wrong', 'another password\n', '0600')
        groups['cache'] = dict(quick, checks={
            'memcached_answers_and_has_room': {'memcached': '127.0.0.1:11211', 'fail_when_measurement_above': {'cache_used_percent': 95}, 'alert_severity': 'warn'},
            'redis_answers': {'redis': {'connection_security': 'unencrypted', 'address': '127.0.0.1:6379'}, 'alert_severity': 'sos'},
            'redis_answers_after_a_login': {'redis': {'connection_security': 'unencrypted', 'address': '127.0.0.1:6379', 'login_password_file': '/etc/mmwatchdog/secrets/redis'}, 'alert_severity': 'sos'},
            'redis_with_another_password': {'redis': {'connection_security': 'unencrypted', 'address': '127.0.0.1:6379', 'login_password_file': '/etc/mmwatchdog/secrets/redis-wrong'}, 'alert_severity': 'info'},
            'memcached_is_no_redis': {'redis': {'connection_security': 'unencrypted', 'address': '127.0.0.1:11211'}, 'alert_severity': 'info'}})
        reload()
        memcached = becomes('cache/memcached_answers_and_has_room', 'ok', 'cache_used_percent is ')
        assert re.search(r' \(fails above 95\); the memcached server at 127\.0\.0\.1:11211 gave its statistics; hits, misses, and evictions over \d+\.\d{3}s$', memcached['message']), memcached
        assert memcached['measurements']['current_connections']['unit'] == 'connections' and memcached['measurements']['current_connections']['value'] >= 1, memcached
        assert 0 <= memcached['measurements']['cache_used_percent']['value'] < 5, memcached
        units = {'cached_items': 'items', 'cache_hits_per_second': 'hits per second', 'cache_misses_per_second': 'misses per second', 'cache_evictions_per_second': 'evictions per second'}
        assert all(memcached['measurements'][name]['unit'] == unit and memcached['measurements'][name]['value'] >= 0 for name, unit in units.items()), memcached
        # The hit ratio covers the time since the run before: 0 while every
        # get finds no item, and 100 while every get finds its item.
        vm.write(FIXTURE + '/memcached_client.py', MEMCACHED_CLIENT)

        def measured(name):
            return last('cache/memcached_answers_and_has_room')['measurements'].get(name, {}).get('value')
        vm.console('systemd-run --quiet --unit mmwatchdog-memcached-misses python3 ' + FIXTURE + '/memcached_client.py misses')
        wait(lambda: measured('cache_hit_percent') == 0 and measured('cache_misses_per_second') > 0)
        vm.console('systemctl stop mmwatchdog-memcached-misses; systemd-run --quiet --unit mmwatchdog-memcached-hits python3 ' + FIXTURE + '/memcached_client.py hits')
        wait(lambda: measured('cache_hit_percent') == 100 and measured('cache_hits_per_second') > 0 and measured('cache_misses_per_second') == 0 and measured('cached_items') >= 1)
        vm.console('systemctl stop mmwatchdog-memcached-hits')
        # Redis without a password: a login is refused, and PING needs none.
        becomes('cache/redis_answers', 'ok', 'the Redis server at 127.0.0.1:6379 answered PING')
        vm.console(redis_program() + ' CONFIG SET requirepass ' + shlex.quote(REDIS_PASSWORD))
        becomes('cache/redis_answers', 'failed', 'the Redis server at 127.0.0.1:6379 answered: NOAUTH Authentication required.')
        becomes('cache/redis_answers_after_a_login', 'ok', 'the Redis server at 127.0.0.1:6379 answered PING')
        wrong = becomes('cache/redis_with_another_password', 'failed', 'the Redis server at 127.0.0.1:6379 answered: WRONGPASS ')
        assert 'another password' not in json.dumps(wrong), wrong
        becomes('cache/memcached_is_no_redis', 'failed', 'the server at 127.0.0.1:11211 did not answer PING as a Redis server does')
        vm.console('systemctl stop memcached ' + redis_unit())
        refuses('cache/memcached_answers_and_has_room')
        refuses('cache/redis_answers_after_a_login')
        vm.console('systemctl start memcached ' + redis_unit())
        becomes('cache/memcached_answers_and_has_room', 'ok')
        del groups['cache']
        reload()
        mark('a memcached check gets the statistics of the real server and reports its connections, its items, how full its cache is, and its hits, misses, and evictions since the run before, with a hit ratio of 0 while every get finds no item and of 100 while every get finds its item; a redis check gets PONG from the real server, with a login when the server has a password, and fails with the server\'s own words without it and with another password')

        # The address that the guest gives the two names of the database
        # servers. /etc/hosts says 127.0.0.1; a guest with systemd-resolved
        # puts the loopback address of IP version 6 in front of it.
        address_of_the_names = vm.console('getent ahosts ' + DATABASE_HOST + " | awk 'NR == 1 {print $1}'").stdout.strip()

        def as_postgres(statement, port=5432):
            return vm.console('runuser -u postgres -- psql --no-psqlrc --quiet --tuples-only --no-align --port ' + str(port) + ' --command ' + shlex.quote(statement)).stdout.strip()

        # PostgreSQL as the distribution installs it: asked through the
        # local socket with nothing to set up, and through a login.
        # On Debian and Ubuntu, pg_ctlcluster and pg_createcluster manage the
        # guest's servers; elsewhere the replica's data directory is made with
        # pg_basebackup and started with pg_ctl.
        if vm.package_manager() == 'apt':
            version = vm.console('ls /usr/lib/postgresql | sort -n | tail -1').stdout.strip()
            main = '/etc/postgresql/' + version + '/main'
            postgresql = {
                'files': main, 'settings': main + '/conf.d/mmwatchdog-test.conf', 'host_based_authentication': main + '/pg_hba.conf',
                'archiving_settings': main + '/conf.d/mmwatchdog-test-archiving.conf',
                'data_directory': '/var/lib/postgresql/' + version + '/main',
                'stop': 'pg_ctlcluster ' + version + ' main stop', 'start': 'pg_ctlcluster ' + version + ' main start', 'restart': 'pg_ctlcluster ' + version + ' main restart',
                'make_the_replica': 'pg_createcluster ' + version + ' replica --port 5433 >/dev/null; rm -rf /var/lib/postgresql/' + version + '/replica; '
                                    'runuser -u postgres -- pg_basebackup --pgdata /var/lib/postgresql/' + version + '/replica --write-recovery-conf --wal-method stream; pg_ctlcluster ' + version + ' replica start',
                'promote_the_replica': 'pg_ctlcluster ' + version + ' replica promote',
                'remove_the_replica': 'pg_ctlcluster ' + version + ' replica stop 2>/dev/null; pg_dropcluster ' + version + ' replica 2>/dev/null; true'}
        else:
            postgresql = postgresql_without_clusters(vm.console('echo ~postgres').stdout.strip() + '/data')
        pgbackrest_settings = layout()['pgbackrest_settings']
        vm.console('systemctl start postgresql')
        vm.console(postgresql['remove_the_replica'])
        as_postgres("DROP ROLE IF EXISTS mmwatchdog_monitoring")
        # An older server would keep the password in a form that it does
        # not take over the network.
        as_postgres("SET password_encryption = 'scram-sha-256'; CREATE ROLE mmwatchdog_monitoring LOGIN PASSWORD " + "'" + DATABASE_PASSWORD + "'")
        vm.write('/etc/mmwatchdog/secrets/database', DATABASE_PASSWORD + '\n', '0600')
        vm.write('/etc/mmwatchdog/secrets/database-wrong', 'another password\n', '0600')
        login = {'database_server_port': 5432, 'database_server_host': '127.0.0.1', 'connection_security': 'unencrypted', 'login_password_file': '/etc/mmwatchdog/secrets/database'}
        lag = {'replication_lag_seconds': 5}

        def over_the_network(connection_security, host=DATABASE_HOST, authority=False, **more):
            """The inputs of a check that reaches a database server of the guest by a name, with one kind of connection security. With authority the check trusts the certificate authority of the test."""
            inputs = dict(login, database_server_host=host, connection_security=connection_security, **more)
            if authority:
                inputs['database_server_certificate_authority_file'] = DATABASE_AUTHORITY
            return inputs

        def checks_of_the_connection_security(account):
            """Four checks of one server: a certificate that is valid, one of an authority that the guest does not know, one for another name, and encryption without verification."""
            checks = {
                'certificate_is_valid': dict(over_the_network('verified_tls', authority=True), **account),
                'certificate_of_an_unknown_authority': dict(over_the_network('verified_tls'), **account),
                'certificate_for_another_name': dict(over_the_network('verified_tls', host=ANOTHER_NAME_OF_THE_DATABASE_HOST, authority=True), **account),
                'encrypted_without_verification': dict(over_the_network('verified_tls', dangerously_skip_server_certificate_check=True, host=ANOTHER_NAME_OF_THE_DATABASE_HOST), **account)}
            return checks

        # The server gets the certificate of the test, as copies among its
        # own files: it takes a private key only from a file of its own
        # account.
        postgresql_settings = postgresql['settings']
        with_the_certificate = "ssl = on\nssl_cert_file = '" + postgresql['files'] + "/mmwatchdog-test-server.pem'\nssl_key_file = '" + postgresql['files'] + "/mmwatchdog-test-server-key.pem'\n"
        vm.console('install -o postgres -g postgres -m 0600 ' + DATABASE_PRIVATE_KEY + ' ' + postgresql['files'] + '/mmwatchdog-test-server-key.pem; '
                   'install -o postgres -g postgres -m 0644 ' + DATABASE_CERTIFICATE + ' ' + postgresql['files'] + '/mmwatchdog-test-server.pem')
        vm.write(postgresql_settings, with_the_certificate)
        vm.console('systemctl restart postgresql')
        groups['postgresql'] = dict(quick, checks={
            'server_on_this_node': {'postgresql': {}, 'fail_when_measurement_above': {'connections_used_percent': 90}, 'alert_severity': 'sos'},
            'through_a_login': {'postgresql': dict(login, login_role='mmwatchdog_monitoring'), 'alert_severity': 'sos'},
            'with_another_password': {'postgresql': dict(login, login_role='mmwatchdog_monitoring', login_password_file='/etc/mmwatchdog/secrets/database-wrong'), 'alert_severity': 'info'},
            'primary_is_0_seconds_behind': {'postgresql': {}, 'fail_when_measurement_above': lag, 'alert_severity': 'info'},
            'transaction_ids_above_zero': {'postgresql': {}, 'fail_when_measurement_above': {'transaction_ids_used_before_wraparound_percent': 0}, 'alert_severity': 'info'},
            'transaction_ids_below_half_way': {'postgresql': {}, 'fail_when_measurement_above': {'transaction_ids_used_before_wraparound_percent': 50}, 'alert_severity': 'info'},
            **{name: {'postgresql': inputs, 'alert_severity': 'info'} for name, inputs in checks_of_the_connection_security({'login_role': 'mmwatchdog_monitoring'}).items()}})
        reload()
        primary = becomes('postgresql/server_on_this_node', 'ok', 'connections_used_percent is ')
        assert primary['message'].endswith(' (fails above 90); the PostgreSQL server answers; it is a primary') and 0 < primary['measurements']['connections_used_percent']['value'] < 50, primary
        becomes('postgresql/through_a_login', 'ok', 'the PostgreSQL server answers; it is a primary')
        # The share of the blocks read since the run before that the shared
        # buffers held. Each run's own connection reads blocks, so an idle
        # server has the measurement too, after the first run.
        for check in ['server_on_this_node', 'through_a_login']:
            hits = wait(lambda: 'cache_hit_percent' in last('postgresql/' + check)['measurements'] and last('postgresql/' + check))
            assert hits['measurements']['cache_hit_percent']['unit'] == 'percent' and 0 <= hits['measurements']['cache_hit_percent']['value'] <= 100, hits
            age = hits['measurements']['transaction_ids_used_before_wraparound_percent']
            assert age['unit'] == 'percent' and 0 <= age['value'] < 20, hits
        for check, expected_status, limit in [('transaction_ids_above_zero', 'failed', 0), ('transaction_ids_below_half_way', 'ok', 50)]:
            age_limited = becomes('postgresql/' + check, expected_status, 'transaction_ids_used_before_wraparound_percent is ')
            assert ' (fails above ' + str(limit) + ');' in age_limited['message'], age_limited
        refused = becomes('postgresql/with_another_password', 'failed', 'the PostgreSQL server did not answer the check: psql: error: ')
        assert 'password authentication failed for user "mmwatchdog_monitoring"' in refused['message'] and 'another password' not in json.dumps(refused), refused
        becomes('postgresql/primary_is_0_seconds_behind', 'ok', 'replication_lag_seconds is 0 (fails above 5); the PostgreSQL server answers; it is a primary')
        # A check of a server that it reaches over the network says how the
        # connection is protected. With verified_tls the certificate must be
        # valid for the name of the check, by the authorities that the check
        # trusts; with dangerously_skip_server_certificate_check any certificate does.
        def psql_refuses(check, host, *reasons):
            """Wait until the check fails with one of the reasons, in the words of psql. From PostgreSQL 14, psql names the server in front of its reason."""
            name = 'postgresql/' + check
            where = 'connection to server at "' + host + '"' + ('' if host == '127.0.0.1' else ' (' + address_of_the_names + ')') + ', port 5432 failed: '
            expected = {'the PostgreSQL server did not answer the check: psql: error: ' + server + reason for reason in reasons for server in ('', where)}
            try:
                return wait(lambda: last(name)['status'] == 'failed' and last(name)['message'] in expected and last(name))
            except AssertionError:
                raise AssertionError((name, sorted(expected), last(name))) from None

        becomes('postgresql/certificate_is_valid', 'ok', 'the PostgreSQL server answers; it is a primary')
        psql_refuses('certificate_of_an_unknown_authority', DATABASE_HOST, 'SSL error: certificate verify failed')
        psql_refuses('certificate_for_another_name', ANOTHER_NAME_OF_THE_DATABASE_HOST, 'server certificate for "' + DATABASE_HOST + '" does not match host name "' + ANOTHER_NAME_OF_THE_DATABASE_HOST + '"')
        becomes('postgresql/encrypted_without_verification', 'ok', 'the PostgreSQL server answers; it is a primary')
        # The client program reads the file of certificate authorities as
        # the account mmwatchdog. When that account cannot read it, MMWatchdog says so in
        # its own words: the check is unknown, and validation refuses the
        # file.
        cannot_read = 'the account mmwatchdog, which runs the client program of the database server, cannot read ' + DATABASE_AUTHORITY + '. A certificate is no secret: give every account the permission to read the file and to pass through the directories above it'
        vm.console('chmod 0600 ' + DATABASE_AUTHORITY)
        becomes('postgresql/certificate_is_valid', 'unknown', 'database_server_certificate_authority_file: ' + cannot_read)
        # Validation names the first check that states the file.
        refused = cli('validate', 1).stderr
        assert 'check_groups.postgresql.checks.certificate_for_another_name.postgresql.database_server_certificate_authority_file: ' + cannot_read in refused, refused
        vm.console('chmod 0644 ' + DATABASE_AUTHORITY)
        becomes('postgresql/certificate_is_valid', 'ok')
        cli('validate', 0)
        # A server without encryption fails the two checks that state
        # encryption, and the check that states none goes on.
        vm.write(postgresql_settings, 'ssl = off\n')
        vm.console('systemctl restart postgresql')
        psql_refuses('certificate_is_valid', DATABASE_HOST, 'server does not support SSL, but SSL was required')
        psql_refuses('encrypted_without_verification', ANOTHER_NAME_OF_THE_DATABASE_HOST, 'server does not support SSL, but SSL was required')
        fresh('postgresql/through_a_login')
        becomes('postgresql/through_a_login', 'ok')
        # A server that takes this account only over an encrypted
        # connection refuses the check that states none.
        vm.write(postgresql_settings, with_the_certificate)
        vm.console("sed -i '1i hostnossl all mmwatchdog_monitoring 127.0.0.1/32 reject # mmwatchdog-test' " + postgresql['host_based_authentication'] + '; systemctl restart postgresql')
        # An older server says "SSL off" for a connection without
        # encryption.
        rejects = 'FATAL:  pg_hba.conf rejects connection for host "127.0.0.1", user "mmwatchdog_monitoring", database "postgres", '
        psql_refuses('through_a_login', '127.0.0.1', rejects + 'no encryption', rejects + 'SSL off')
        fresh('postgresql/encrypted_without_verification')
        becomes('postgresql/encrypted_without_verification', 'ok')
        vm.console("sed -i '/ # mmwatchdog-test$/d' " + postgresql['host_based_authentication'] + '; systemctl reload postgresql')
        becomes('postgresql/through_a_login', 'ok')
        for check in checks_of_the_connection_security({}):
            del groups['postgresql']['checks'][check]
        reload()
        # Permission checks refuse validation/reload, but a mode change on
        # a running node must not stop checks or alerts. Restore the fixture
        # even if an assertion fails, before the suite reloads its old config.
        assert 'permissions mode' not in cli('validate', 0).stdout
        vm.console('chmod 0644 /etc/mmwatchdog/secrets/database')
        try:
            # A refused validation exits 1; a refused reload exits 4.
            for command, code in (('validate', 1), ('reload', 4)):
                refused = cli(command, code)
                assert '/etc/mmwatchdog/secrets/database has permissions mode 0644' in refused.stderr, refused.stderr
                assert 'dangerously_let_every_account_read_secret: true' in refused.stderr and 'chmod 600' in refused.stderr, refused.stderr
            fresh('postgresql/through_a_login')
            becomes('postgresql/through_a_login', 'ok')
        finally:
            vm.console('chown root:root /etc/mmwatchdog/secrets/database; chmod 0600 /etc/mmwatchdog/secrets/database')
        assert 'permissions mode' not in cli('validate', 0).stdout
        # A second server on port 5433 follows the first as its replica.
        vm.console(postgresql['make_the_replica'], timeout=600)
        groups['postgresql']['checks']['replica_is_less_than_5_seconds_behind'] = {'postgresql': {'database_server_port': 5433}, 'fail_when_measurement_above': lag, 'alert_severity': 'warn'}
        groups['postgresql']['checks']['streaming_replica_is_connected'] = {'postgresql': {'database_server_port': 5433}, 'alert_severity': 'warn'}
        reload()
        as_postgres('CREATE TABLE IF NOT EXISTS mmwatchdog_test (written timestamptz)')
        as_postgres('INSERT INTO mmwatchdog_test VALUES (now())')
        level = becomes('postgresql/replica_is_less_than_5_seconds_behind', 'ok', 'replication_lag_seconds is 0 (fails above 5); the PostgreSQL server answers; it is a replica, 0 seconds behind its primary')
        assert level['measurements']['replication_lag_seconds'] == {'value': 0, 'unit': 'seconds'}, level
        age = level['measurements']['transaction_ids_used_before_wraparound_percent']
        assert age['unit'] == 'percent' and 0 <= age['value'] < 20, level
        # The replica stops applying changes while the primary goes on.
        as_postgres('SELECT pg_wal_replay_pause()', port=5433)
        as_postgres('INSERT INTO mmwatchdog_test VALUES (now())')
        behind = wait(lambda: last('postgresql/replica_is_less_than_5_seconds_behind')['status'] == 'failed' and last('postgresql/replica_is_less_than_5_seconds_behind'), timeout=60)
        assert behind['measurements']['replication_lag_seconds']['value'] > 5 and ' seconds behind its primary' in behind['message'] and 'not connected' not in behind['message'], behind
        as_postgres('SELECT pg_wal_replay_resume()', port=5433)
        becomes('postgresql/replica_is_less_than_5_seconds_behind', 'ok', 'replication_lag_seconds is 0 (fails above 5)')
        # The primary goes away: the server on this node no longer answers,
        # and the replica falls behind from then on.
        vm.console(postgresql['stop'])
        # While the server goes down it still answers, with "the database
        # system is shutting down". Then its socket is gone.
        gone = wait(lambda: last('postgresql/server_on_this_node')['status'] == 'failed' and 'No such file or directory' in last('postgresql/server_on_this_node')['message'] and last('postgresql/server_on_this_node'), timeout=60)
        assert gone['message'].startswith('the PostgreSQL server did not answer the check: psql: error: '), gone
        alone = wait(lambda: last('postgresql/replica_is_less_than_5_seconds_behind')['status'] == 'failed' and last('postgresql/replica_is_less_than_5_seconds_behind'), timeout=60)
        assert alone['message'].endswith(' seconds behind its primary, and it is not connected to its primary now'), alone
        disconnected = becomes('postgresql/streaming_replica_is_connected', 'failed')
        assert disconnected['message'].endswith(', and it is not connected to its primary now'), disconnected
        vm.console(postgresql['start'])
        becomes('postgresql/server_on_this_node', 'ok')
        wait(lambda: last('postgresql/replica_is_less_than_5_seconds_behind')['status'] == 'ok', timeout=90)
        # The replica is made a primary, as in a failover. The same check,
        # with its limit on the lag, stays right: a primary is 0 seconds
        # behind.
        vm.console(postgresql['promote_the_replica'])
        becomes('postgresql/replica_is_less_than_5_seconds_behind', 'ok', 'replication_lag_seconds is 0 (fails above 5); the PostgreSQL server answers; it is a primary')
        vm.console(postgresql['remove_the_replica'])
        # openSUSE Leap 16.1 has pgBackRest 2.53.1, which supports PostgreSQL up to 17, and its
        # package postgresql-server installs PostgreSQL 18: pgBackRest stops with "unexpected
        # control version = 1800". The archiving scenario waits for a pgBackRest that supports 18.
        if layout()['pgbackrest_supports_the_postgresql']:
            # The server archives its write-ahead log through pgBackRest, as
            # pgBackRest's user guide sets it up, and pgBackRest takes a full
            # backup. pgBackRest runs as the account of the server, from a
            # directory that this account may enter, as the check runs it.
            vm.write(pgbackrest_settings, '[global]\nrepo1-path=' + PGBACKREST_REPOSITORY + '\nrepo1-retention-full=2\nstart-fast=y\n\n[main]\npg1-path=' + postgresql['data_directory'] + '\n', '0640')
            vm.write(postgresql['archiving_settings'], "archive_mode = on\narchive_command = 'pgbackrest --stanza=main archive-push %p'\narchive_timeout = 60\n")
            vm.console('chown postgres:postgres ' + pgbackrest_settings + ' && ' + postgresql['restart'])
            pgbackrest = 'runuser -u postgres -- pgbackrest --stanza=main '
            vm.console('cd / && ' + pgbackrest + 'stanza-create && ' + pgbackrest + 'check && ' + pgbackrest + 'backup --type=full', timeout=600)
            archive_lag, latest_backup = 'postgresql/archive_is_less_than_20_seconds_behind', 'postgresql/latest_backup_is_less_than_26_hours_old'
            groups['postgresql']['checks'].update({
                'archive_is_less_than_20_seconds_behind': {'postgresql': {}, 'fail_when_measurement_above': {'wal_archive_lag_seconds': 20}, 'alert_severity': 'warn'},
                'latest_backup_is_less_than_26_hours_old': {'pgbackrest': {'stanza': 'main'}, 'fail_when_measurement_above': {'latest_backup_age_hours': 26}, 'alert_severity': 'warn'},
                'stanza_that_does_not_exist': {'pgbackrest': {'stanza': 'nosuch'}, 'alert_severity': 'info'}})
            reload()
            # While pgBackRest archives, no segment waits long. A check with
            # login_role does not measure the wait.
            archived = becomes(archive_lag, 'ok', 'wal_archive_lag_seconds is ')
            assert archived['message'].endswith(' (fails above 20); the PostgreSQL server answers; it is a primary') and archived['measurements']['wal_archive_lag_seconds']['unit'] == 'seconds' and archived['measurements']['wal_archive_lag_seconds']['value'] < 20, archived
            fresh('postgresql/through_a_login')
            through_a_login = becomes('postgresql/through_a_login', 'ok', 'the PostgreSQL server answers; it is a primary')
            assert 'wal_archive_lag_seconds' not in through_a_login['measurements'], through_a_login
            backed_up = becomes(latest_backup, 'ok', 'latest_backup_age_hours is ')
            assert ' (fails above 26); the latest backup of stanza main finished ' in backed_up['message'] and backed_up['message'].endswith(', a full backup') and backed_up['measurements']['latest_backup_age_hours']['unit'] == 'hours' and 0 <= backed_up['measurements']['latest_backup_age_hours']['value'] < 1, backed_up
            no_stanza = 'pgBackRest reports a problem with stanza nosuch: repo1: missing stanza path'
            missing = becomes('postgresql/stanza_that_does_not_exist', 'failed', no_stanza)
            assert missing['message'] == no_stanza, missing
            # pgBackRest can no longer use its repository's archive. The segment
            # that the server finishes next waits, past the limit, and
            # pgBackRest says why in its own words.
            vm.console('chmod 000 ' + PGBACKREST_REPOSITORY + '/archive')
            as_postgres('INSERT INTO mmwatchdog_test VALUES (now())')
            as_postgres('SELECT pg_switch_wal()')
            waiting = wait(lambda: last(archive_lag)['status'] == 'failed' and last(archive_lag), timeout=60)
            assert waiting['message'].startswith('wal_archive_lag_seconds is ') and waiting['message'].endswith(' (fails above 20); the PostgreSQL server answers; it is a primary') and waiting['measurements']['wal_archive_lag_seconds']['value'] > 20, waiting
            becomes(latest_backup, 'failed', 'pgBackRest reports a problem with stanza main: repo1: [FileOpenError] unable to load info file')
            # Repaired, the server archives what waits. A new segment wakes its
            # archiver at once: after three failed attempts it waits up to a
            # minute before it tries again.
            vm.console('chmod 750 ' + PGBACKREST_REPOSITORY + '/archive')
            as_postgres('INSERT INTO mmwatchdog_test VALUES (now())')
            as_postgres('SELECT pg_switch_wal()')
            wait(lambda: last(archive_lag)['status'] == 'ok', timeout=120)
            becomes(latest_backup, 'ok', 'latest_backup_age_hours is ')
            # Without archive_mode the server archives nothing, so the wait has
            # no measurement, and the check with a limit on it is unknown.
            vm.console('rm -f ' + postgresql['archiving_settings'] + ' && ' + postgresql['restart'])
            archive_mode_off = 'archive_mode is off, so the PostgreSQL server archives no write-ahead log and wal_archive_lag_seconds cannot be measured'
            unmeasured = becomes(archive_lag, 'unknown', archive_mode_off)
            assert unmeasured['message'] == archive_mode_off, unmeasured
        del groups['postgresql']
        reload()
        vm.console('rm -rf ' + pgbackrest_files(pgbackrest_settings))
        mark('a postgresql check asks the real server through its local socket with nothing to set up, and through a login with a password that a wrong password file fails; over the network it keeps to its connection_security: with verified_tls it answers for a certificate of the certificate authority that the check trusts and fails for an unknown authority and for another name, with dangerously_skip_server_certificate_check it takes any certificate, both fail for a server without encryption, and a server that wants encryption refuses the check that states none; a file of certificate authorities that the account mmwatchdog cannot read makes the check unknown and fails validation, in MMWatchdog\'s words; a primary is 0 seconds behind; a real replica is 0 seconds behind while it follows, behind while its replay is paused, and falls behind without its primary, and its check with a limit on the lag stays healthy when the replica is made a primary; a stopped server fails the check; mmwatchdog validate and mmwatchdog reload refuse a publicly readable password, while the running check still answers; '
             'it measures the share of block reads that the shared buffers served, through the local socket and through a login'
             + ('; with archiving through pgBackRest, a check without login_role measures how long the write-ahead log waits to be archived: '
                'below its limit while pgBackRest archives, above it while pgBackRest cannot use its repository, and below it again once it can; with archive_mode off the check with that limit is unknown, and a check with login_role does not measure the wait; '
                'a pgbackrest check reports how many hours ago the latest full backup finished, and fails with pgBackRest\'s own reason for a stanza that does not exist and for a repository that it cannot read'
                if layout()['pgbackrest_supports_the_postgresql'] else ''))

        # MariaDB as the distribution installs it.
        mariadb = mariadb_files(layout()['mariadb_settings_directory'])

        def restart_mariadb(settings=''):
            """Start the MariaDB server again, with the settings of one scenario or with those of the distribution alone."""
            vm.write(mariadb['settings'], '[mysqld]\n' + settings)
            vm.console('systemctl restart mariadb')
        vm.console('install -o mysql -g mysql -m 0600 ' + DATABASE_PRIVATE_KEY + ' ' + mariadb['private_key'] + ' && '
                   'install -m 0644 ' + DATABASE_CERTIFICATE + ' ' + mariadb['certificate'])
        with_the_certificate = 'ssl_cert = ' + mariadb['certificate'] + '\nssl_key = ' + mariadb['private_key'] + '\n'
        restart_mariadb()
        # The account of the checks, for each way in which the server can
        # see a connection from the guest itself.
        for seen_as in ['127.0.0.1', '::1', 'localhost']:
            vm.console("mariadb -e \"CREATE OR REPLACE USER 'mmwatchdog_monitoring'@'" + seen_as + "' IDENTIFIED BY '" + DATABASE_PASSWORD + "'\"")
        login['database_server_port'] = 3306
        groups['mysql'] = dict(quick, checks={
            'server_on_this_node': {'mysql': {}, 'fail_when_measurement_above': {'connections_used_percent': 90}, 'alert_severity': 'sos'},
            'through_a_login': {'mysql': dict(login, login_user='mmwatchdog_monitoring'), 'alert_severity': 'sos'},
            'with_another_password': {'mysql': dict(login, login_user='mmwatchdog_monitoring', login_password_file='/etc/mmwatchdog/secrets/database-wrong'), 'alert_severity': 'info'},
            'is_no_replica': {'mysql': {}, 'fail_when_measurement_above': lag, 'alert_severity': 'info'},
            'may_not_ask_for_the_replica_status': {'mysql': dict(login, login_user='mmwatchdog_monitoring'), 'fail_when_measurement_above': lag, 'alert_severity': 'info'},
            'innodb_history_is_short': {'mysql': {}, 'fail_when_measurement_above': {'innodb_history_list_length': 100}, 'alert_severity': 'warn'},
            'innodb_history_through_a_login': {'mysql': dict(login, login_user='mmwatchdog_monitoring'), 'fail_when_measurement_above': {'innodb_history_list_length': 100}, 'alert_severity': 'info'}})
        reload()
        server = becomes('mysql/server_on_this_node', 'ok', 'connections_used_percent is ')
        assert server['message'].endswith(' (fails above 90); the MySQL server answers; it is no replica') and 0 < server['measurements']['connections_used_percent']['value'] < 50, server
        becomes('mysql/through_a_login', 'ok', 'the MySQL server answers')
        refused = becomes('mysql/with_another_password', 'failed', 'the MySQL server did not answer the check: ERROR 1045 ')
        assert "Access denied for user 'mmwatchdog_monitoring'" in refused['message'] and 'another password' not in json.dumps(refused), refused
        becomes('mysql/is_no_replica', 'ok', 'replication_lag_seconds is 0 (fails above 5); the MySQL server answers; it is no replica')
        becomes('mysql/may_not_ask_for_the_replica_status', 'unknown', 'the account of the check may not ask for the status of a replica: it needs the privilege REPLICATION CLIENT, or REPLICA MONITOR on MariaDB')
        # InnoDB's history list length: MariaDB gives it to every account,
        # also to mmwatchdog_monitoring, which has no privilege. mmwatchdog_monitoring may
        # not ask whether the server is a replica, so its message does not
        # say.
        for check, answers in [('innodb_history_is_short', 'the MySQL server answers; it is no replica'), ('innodb_history_through_a_login', 'the MySQL server answers')]:
            short = becomes('mysql/' + check, 'ok', 'innodb_history_list_length is ')
            assert short['message'].endswith(' (fails above 100); ' + answers) and short['measurements']['innodb_history_list_length']['unit'] == 'transactions', short
        # A session holds a snapshot open, and another commits 300 changes:
        # InnoDB keeps the old row version of each for that session. When
        # InnoDB purges them after the session ends is the server's own
        # business: MariaDB 10.5 had not purged them two minutes later on a
        # guest without other work.
        vm.console('mariadb -e "CREATE DATABASE IF NOT EXISTS mmwatchdog_test; CREATE OR REPLACE TABLE ' + HISTORY_TABLE + ' (id INT PRIMARY KEY, changes INT); INSERT INTO ' + HISTORY_TABLE + ' VALUES (1, 0)"; '
                   'systemd-run --quiet --unit mmwatchdog-holds-a-snapshot mariadb --execute ' + shlex.quote(SNAPSHOT_HELD))
        wait(lambda: vm.console("mariadb --batch --skip-column-names --execute \"SELECT COUNT(*) FROM information_schema.PROCESSLIST WHERE INFO = 'SELECT SLEEP(3600)'\"").stdout.strip() == '1')
        vm.console('for i in $(seq ' + str(CHANGES_THAT_GROW_THE_HISTORY) + '); do echo "UPDATE ' + HISTORY_TABLE + ' SET changes = changes + 1 WHERE id = 1;"; done | mariadb')
        grown = becomes('mysql/innodb_history_through_a_login', 'failed', 'innodb_history_list_length is ')
        # How many of the changes the length counts at once is the server's
        # business: under load InnoDB counted 147 to 295 of the 300. The
        # limit of 100 is what the check must hold to.
        assert grown['measurements']['innodb_history_list_length']['value'] > 100 and grown['message'].endswith(' (fails above 100); the MySQL server answers'), grown
        becomes('mysql/innodb_history_is_short', 'failed', 'innodb_history_list_length is ')
        session = vm.console("mariadb --batch --skip-column-names --execute \"SELECT ID FROM information_schema.PROCESSLIST WHERE INFO = 'SELECT SLEEP(3600)'\"").stdout.strip()
        vm.console('mariadb --execute "KILL ' + session + '"; ' + vm.each_unit('stop', ['mmwatchdog-holds-a-snapshot']) + '; ' + vm.each_unit('reset-failed', ['mmwatchdog-holds-a-snapshot']) + '; mariadb -e "DROP DATABASE mmwatchdog_test"')
        for check in ['innodb_history_is_short', 'innodb_history_through_a_login']:
            del groups['mysql']['checks'][check]
        reload()
        mark('a mysql check reads the InnoDB history list length of the real MariaDB server from its status variable, through the local socket and through a login without a privilege; it grows past a limit of 100 when another session commits 300 changes while one holds a snapshot open, which fails the check')
        # MariaDB lets a replica follow a source under a name, and shows
        # such a source only to a question for all of them. This one cannot
        # reach its source, at a port where nothing listens.
        vm.console("mariadb -e \"CHANGE MASTER 'orders' TO MASTER_HOST='127.0.0.1', MASTER_PORT=3999, MASTER_USER='replication', MASTER_PASSWORD='x', MASTER_CONNECT_RETRY=2; START SLAVE 'orders'\"")
        becomes('mysql/is_no_replica', 'failed', 'the MySQL replica does not follow its source of the connection orders: it is not receiving changes (Connecting); it is applying changes')
        vm.console("mariadb -e \"STOP SLAVE 'orders'; RESET SLAVE 'orders' ALL\"")
        becomes('mysql/is_no_replica', 'ok', 'replication_lag_seconds is 0 (fails above 5); the MySQL server answers; it is no replica')
        # The server gets the certificate of the test, which a certificate
        # authority signed that the guest does not know. MariaDB's client
        # program would encrypt through the local socket too, and would
        # fail there for this certificate: the check through the socket
        # tells it not to encrypt, and goes on.
        restart_mariadb(with_the_certificate)
        # The client program itself shows it, where it is one from 11.4.
        release = re.search(r'(\d+)\.(\d+)\.\d+-MariaDB', vm.console('mariadb --version').stdout)
        client_from_11_4 = (int(release.group(1)), int(release.group(2))) >= (11, 4)
        if client_from_11_4:
            assert 'unable to get local issuer certificate' in vm.console('mariadb -e "SELECT 1"', check=False).stderr
        for check, inputs in checks_of_the_connection_security({'login_user': 'mmwatchdog_monitoring'}).items():
            groups['mysql']['checks'][check] = {'mysql': inputs, 'alert_severity': 'info'}
        reload()
        # The words of the client program differ between its releases;
        # the number of the error does not.
        mariadb_says = 'the MySQL server did not answer the check: ERROR 2026 (HY000): '
        becomes('mysql/certificate_is_valid', 'ok', 'the MySQL server answers')
        becomes('mysql/certificate_of_an_unknown_authority', 'failed', mariadb_says + ('TLS/SSL error: unable to get local issuer certificate' if client_from_11_4 else ''))
        becomes('mysql/certificate_for_another_name', 'failed', mariadb_says + ('TLS/SSL error: Hostname verification failed' if client_from_11_4 else ''))
        becomes('mysql/encrypted_without_verification', 'ok', 'the MySQL server answers')
        fresh('mysql/server_on_this_node', 'mysql/through_a_login')
        becomes('mysql/server_on_this_node', 'ok')
        becomes('mysql/through_a_login', 'ok')
        # A server without encryption. MariaDB's client program refuses it
        # only when it verifies a certificate. The TLS login guard blocks
        # its unencrypted login when certificate checking is disabled.
        restart_mariadb('ssl = off\n')
        becomes('mysql/certificate_is_valid', 'failed', mariadb_says + ('TLS/SSL error: SSL is required, but the server does not support it' if client_from_11_4 else ''))
        becomes('mysql/encrypted_without_verification', 'failed', 'the MySQL server did not answer the check:')
        fresh('mysql/through_a_login')
        becomes('mysql/through_a_login', 'ok')
        # A server that takes only encrypted connections over the network
        # refuses the check that states none. The local socket counts as
        # protected, so the check through it goes on.
        restart_mariadb(with_the_certificate + 'require_secure_transport = on\n')
        # A newer server says that it takes no connection without
        # encryption (3159); an older one says only that it refuses (1045).
        refused = becomes('mysql/through_a_login', 'failed', 'the MySQL server did not answer the check: ERROR ')
        assert 'ERROR 3159 ' in refused['message'] or 'ERROR 1045 ' in refused['message'], refused
        becomes('mysql/certificate_is_valid', 'ok')
        becomes('mysql/encrypted_without_verification', 'ok')
        fresh('mysql/server_on_this_node')
        becomes('mysql/server_on_this_node', 'ok')
        restart_mariadb()
        becomes('mysql/through_a_login', 'ok')
        for check in checks_of_the_connection_security({}):
            del groups['mysql']['checks'][check]
        reload()
        # Debian 12 enables mariadb.socket with the package, and the unit
        # listens from the next start of the guest. systemd then starts a
        # stopped server again for the first program that asks, which is
        # the check itself. So the socket unit stops first.
        vm.console(vm.each_unit('stop', ['mariadb.socket', 'mariadb-extra.socket', 'mariadb']))
        gone = becomes('mysql/server_on_this_node', 'failed', 'the MySQL server did not answer the check: ERROR 2002 ')
        vm.console('systemctl start mariadb')
        becomes('mysql/server_on_this_node', 'ok')
        del groups['mysql']
        mark('a mysql check asks the real MariaDB server through its local socket with nothing to set up, also when the server has a certificate that the guest does not know, and through a login with a password; over the network it keeps to its connection_security with MariaDB\'s client program: verified_tls answers for a certificate of the certificate authority that the check trusts and fails for an unknown authority, for another name, and for a server without encryption; dangerously_skip_server_certificate_check takes any certificate and refuses an unencrypted login before it reaches the server; a server that takes only encrypted connections refuses the check that states none; a server that is no replica is 0 seconds behind, and a replica that cannot reach a source that it follows under a name fails the check; a wrong password and a stopped server fail the check with the client\'s own words, and an account without the privilege makes only a limit on the lag unknown')

        # Docker is in no repository of the distribution on the Red Hat family (see LAYOUTS): the scenarios of Docker and of MySQL run in its containers.
        if layout()['docker_scenarios']:
            # Docker, with an image that the test makes from a program of the
            # guest, so that nothing is downloaded.
            vm.console('systemctl start docker; docker rm --force ' + ' '.join(CONTAINERS) + ' >/dev/null 2>&1; '
                       'rm -rf ' + FIXTURE + '/image; mkdir -p ' + FIXTURE + '/image/bin; cp $(command -v busybox busybox-static | head -1) ' + FIXTURE + '/image/bin/busybox; ln -s busybox ' + FIXTURE + '/image/bin/sh; tar -c -C ' + FIXTURE + '/image . | docker import - ' + IMAGE + ' >/dev/null', timeout=600)
            # The program of a container is its process 1, which a TERM
            # signal does not end: Docker kills it after the container's
            # stop timeout, also when Docker itself stops. One second is
            # enough for a program that keeps nothing.
            vm.console('docker run --detach --name mmwatchdog-test-runs --stop-timeout 1 ' + IMAGE + ' /bin/busybox sleep 86400 >/dev/null; '
                       "docker run --detach --name mmwatchdog-test-health --stop-timeout 1 --health-cmd '/bin/busybox test -e /healthy' --health-interval 1s --health-retries 1 --health-start-period 0s " + IMAGE + ' /bin/busybox sleep 86400 >/dev/null; '
                       'docker run --name mmwatchdog-test-ended ' + IMAGE + ' /bin/busybox false; true')
            groups['docker'] = dict(quick, checks={
                'runs': {'docker_container': 'mmwatchdog-test-runs', 'alert_severity': 'sos'},
                'has_a_health_check': {'docker_container': 'mmwatchdog-test-health', 'alert_severity': 'sos'},
                'ended': {'docker_container': 'mmwatchdog-test-ended', 'alert_severity': 'info'},
                'was_never_there': {'docker_container': 'mmwatchdog-test-missing', 'alert_severity': 'info'}})
            reload()
            becomes('docker/runs', 'ok', 'the Docker container mmwatchdog-test-runs runs')
            unhealthy = becomes('docker/has_a_health_check', 'failed', 'the Docker container mmwatchdog-test-health runs, and its own health check fails')
            becomes('docker/ended', 'failed', 'the Docker container mmwatchdog-test-ended does not run: it ended with exit code 1')
            becomes('docker/was_never_there', 'failed', 'Docker has no container named mmwatchdog-test-missing')
            vm.console('docker exec mmwatchdog-test-health /bin/busybox touch /healthy')
            becomes('docker/has_a_health_check', 'ok', 'the Docker container mmwatchdog-test-health runs, and its own health check passes')
            vm.console('docker stop mmwatchdog-test-runs >/dev/null')
            becomes('docker/runs', 'failed', 'the Docker container mmwatchdog-test-runs does not run: it ended with exit code ')
            vm.console('docker start mmwatchdog-test-runs >/dev/null')
            becomes('docker/runs', 'ok')
            # Docker reports a paused container as running. Its programs do no
            # work, so the check fails.
            vm.console('docker pause mmwatchdog-test-runs >/dev/null')
            becomes('docker/runs', 'failed', 'the Docker container mmwatchdog-test-runs is paused: its programs are stopped where they were, and do no work')
            vm.console('docker unpause mmwatchdog-test-runs >/dev/null')
            becomes('docker/runs', 'ok')
            vm.console('docker rm --force mmwatchdog-test-runs >/dev/null')
            becomes('docker/runs', 'failed', 'Docker has no container named mmwatchdog-test-runs')
            vm.console('systemctl stop docker.socket docker.service')
            becomes('docker/has_a_health_check', 'unknown', 'MMWatchdog cannot ask Docker about the container mmwatchdog-test-health: ')
            vm.console('systemctl start docker')
            del groups['docker']
            mark('a docker_container check finds a real container that runs, one that ended, one that is paused, one that was removed, and one whose own health check fails and then passes; a stopped Docker makes the check unknown')

            # MySQL itself: a source and a replica that follows it. The guest
            # asks them with the client program that Debian has, which is
            # MariaDB's, and then with MySQL's own.
            def in_mysql(server, statement):
                """Run a statement as the administrator, inside the container of a MySQL server."""
                return vm.console('docker exec --env MYSQL_PWD=' + shlex.quote(DATABASE_PASSWORD) + ' ' + server + ' mysql --user=root --batch --skip-column-names --execute ' + shlex.quote(statement)).stdout.strip()
            vm.console('docker rm --force ' + ' '.join(MYSQL_SERVERS) + ' >/dev/null 2>&1; docker pull --quiet ' + MYSQL_IMAGE + ' >/dev/null && { docker network inspect ' + DATABASE_NETWORK + ' >/dev/null 2>&1 || docker network create ' + DATABASE_NETWORK + ' >/dev/null; }', timeout=900)
            # The source has the certificate of the test. The replica has the
            # certificate that MySQL makes for itself.
            certificate = {'mmwatchdog-test-mysql-source': ' --ssl-cert=/certificates/server.pem --ssl-key=/tmp/mmwatchdog-server-key.pem --ssl-ca=/certificates/certificate-authority.pem'}
            private_key_setup = 'install -o mysql -g mysql -m 0600 /certificates/server-key.pem /tmp/mmwatchdog-server-key.pem && exec docker-entrypoint.sh mysqld "$@"'
            for server_id, (server, port) in enumerate(MYSQL_SERVERS.items(), 1):
                start_command = ' bash -c ' + shlex.quote(private_key_setup) + ' --' if server in certificate else ''
                vm.console('docker run --detach --name ' + server + ' --network ' + DATABASE_NETWORK + ' --publish 127.0.0.1:' + str(port) + ':3306 --volume ' + DATABASE_CERTIFICATES + ':/certificates:ro --env MYSQL_ROOT_PASSWORD=' + shlex.quote(DATABASE_PASSWORD) + ' ' + MYSQL_IMAGE + start_command +
                           ' --server-id=' + str(server_id) + ' --gtid-mode=ON --enforce-gtid-consistency=ON' + certificate.get(server, '') + ' >/dev/null')
            # A new server answers over the network once it has set itself up.
            for server in MYSQL_SERVERS:
                vm.console('for i in $(seq 120); do docker exec --env MYSQL_PWD=' + shlex.quote(DATABASE_PASSWORD) + ' ' + server + ' mysql --protocol=TCP --host=127.0.0.1 --user=root --execute "SELECT 1" >/dev/null 2>&1 && exit 0; sleep 1; done; exit 1', timeout=180)
            in_mysql('mmwatchdog-test-mysql-source', "CREATE USER 'replication'@'%' IDENTIFIED BY 'a password of the replica'; GRANT REPLICATION SLAVE ON *.* TO 'replication'@'%'")
            in_mysql('mmwatchdog-test-mysql-replica', "CHANGE REPLICATION SOURCE TO SOURCE_HOST='mmwatchdog-test-mysql-source', SOURCE_USER='replication', SOURCE_PASSWORD='a password of the replica', SOURCE_AUTO_POSITION=1, GET_SOURCE_PUBLIC_KEY=1, SOURCE_CONNECT_RETRY=2; START REPLICA")
            # The replica gets the two accounts of the checks from the source.
            in_mysql('mmwatchdog-test-mysql-source', "CREATE USER 'mmwatchdog_monitoring'@'%' IDENTIFIED BY '" + DATABASE_PASSWORD + "'; GRANT REPLICATION CLIENT ON *.* TO 'mmwatchdog_monitoring'@'%'; "
                     "CREATE USER 'mmwatchdog_without_privilege'@'%' IDENTIFIED BY '" + DATABASE_PASSWORD + "'; "
                     "CREATE USER 'mmwatchdog_reads_innodb'@'%' IDENTIFIED BY '" + DATABASE_PASSWORD + "'; GRANT PROCESS ON *.* TO 'mmwatchdog_reads_innodb'@'%'; "
                     "CREATE DATABASE mmwatchdog_test; CREATE TABLE mmwatchdog_test.written (at DATETIME); CREATE TABLE " + HISTORY_TABLE + " (id INT PRIMARY KEY, changes INT); INSERT INTO " + HISTORY_TABLE + " VALUES (1, 0)")
            wait(lambda: in_mysql('mmwatchdog-test-mysql-replica', "SELECT COUNT(*) FROM mysql.user WHERE user = 'mmwatchdog_without_privilege'") == '1', timeout=60)
            # The check of the source verifies the certificate of the test. The
            # checks of the replica state no encryption: a new MySQL server
            # takes the password of a login without encryption only from a
            # client that asks for the server's public key. One check of the
            # replica encrypts and verifies nothing.
            source_port, replica_port = MYSQL_SERVERS.values()
            source = over_the_network('verified_tls', authority=True, login_user='mmwatchdog_monitoring', database_server_port=source_port)
            replica = dict(login, login_user='mmwatchdog_monitoring', database_server_port=replica_port)
            groups['mysql_itself'] = dict(quick, checks={
                'source_answers': {'mysql': source, 'fail_when_measurement_above': lag, 'alert_severity': 'sos'},
                'certificate_of_an_unknown_authority': {'mysql': over_the_network('verified_tls', login_user='mmwatchdog_monitoring', database_server_port=source_port), 'alert_severity': 'info'},
                'certificate_for_another_name': {'mysql': over_the_network('verified_tls', host=ANOTHER_NAME_OF_THE_DATABASE_HOST, authority=True, login_user='mmwatchdog_monitoring', database_server_port=source_port), 'alert_severity': 'info'},
                'replica_follows_its_source': {'mysql': replica, 'fail_when_measurement_above': lag, 'alert_severity': 'sos'},
                'with_another_password': {'mysql': dict(replica, login_password_file='/etc/mmwatchdog/secrets/database-wrong'), 'alert_severity': 'info'},
                'may_not_ask_for_the_replica_status': {'mysql': over_the_network('verified_tls', dangerously_skip_server_certificate_check=True, host=ANOTHER_NAME_OF_THE_DATABASE_HOST, login_user='mmwatchdog_without_privilege', database_server_port=replica_port), 'fail_when_measurement_above': lag, 'alert_severity': 'info'},
                # mmwatchdog_monitoring has no PROCESS: only a limit on the history
                # list length is unknown, and the lag of source_answers and
                # of replica_follows_its_source is still read.
                'innodb_history_is_short': {'mysql': dict(source, login_user='mmwatchdog_reads_innodb'), 'fail_when_measurement_above': {'innodb_history_list_length': 100}, 'alert_severity': 'warn'},
                'may_not_read_the_innodb_history': {'mysql': source, 'fail_when_measurement_above': {'innodb_history_list_length': 100}, 'alert_severity': 'info'}})
            reload()

            def mysql_itself(cannot_connect, unknown_authority, another_name):
                """The checks against the two servers, with the client program that MMWatchdog finds on the guest. Each client program says in its own words that it cannot connect, and that it does not accept a certificate."""
                answers = becomes('mysql_itself/source_answers', 'ok', 'replication_lag_seconds is 0 (fails above 5); the MySQL server answers; it is no replica')
                assert 0 < answers['measurements']['connections_used_percent']['value'] < 50, answers
                becomes('mysql_itself/certificate_of_an_unknown_authority', 'failed', 'the MySQL server did not answer the check: ERROR 2026 (HY000): ' + unknown_authority)
                becomes('mysql_itself/certificate_for_another_name', 'failed', 'the MySQL server did not answer the check: ERROR 2026 (HY000): ' + another_name)
                follows = becomes('mysql_itself/replica_follows_its_source', 'ok', 'replication_lag_seconds is 0 (fails above 5); the MySQL server answers; it is a replica, 0 seconds behind its source')
                assert follows['measurements']['replication_lag_seconds'] == {'value': 0, 'unit': 'seconds'}, follows
                refused = becomes('mysql_itself/with_another_password', 'failed', 'the MySQL server did not answer the check: ERROR 1045 ')
                assert "Access denied for user 'mmwatchdog_monitoring'" in refused['message'] and 'another password' not in json.dumps(refused), refused
                becomes('mysql_itself/may_not_ask_for_the_replica_status', 'unknown', 'the account of the check may not ask for the status of a replica: it needs the privilege REPLICATION CLIENT, or REPLICA MONITOR on MariaDB')
                # mmwatchdog_reads_innodb has PROCESS and not REPLICATION CLIENT, so
                # its message does not say whether the server is a replica.
                short = becomes('mysql_itself/innodb_history_is_short', 'ok', 'innodb_history_list_length is ')
                assert short['message'].endswith(' (fails above 100); the MySQL server answers') and short['measurements']['innodb_history_list_length']['unit'] == 'transactions', short
                becomes('mysql_itself/may_not_read_the_innodb_history', 'unknown', 'the account of the check may not read the InnoDB history list length: it needs the privilege PROCESS')
                # The replica applies each change 30 seconds late: it is behind
                # by the age of the change that waits.
                in_mysql('mmwatchdog-test-mysql-replica', 'STOP REPLICA SQL_THREAD; CHANGE REPLICATION SOURCE TO SOURCE_DELAY=30; START REPLICA SQL_THREAD')
                in_mysql('mmwatchdog-test-mysql-source', 'INSERT INTO mmwatchdog_test.written VALUES (NOW())')
                behind = wait(lambda: last('mysql_itself/replica_follows_its_source').get('measurements', {}).get('replication_lag_seconds', {}).get('value', 0) > 5 and last('mysql_itself/replica_follows_its_source'), timeout=60)
                assert behind['status'] == 'failed' and behind['measurements']['replication_lag_seconds']['value'] <= 31 and behind['message'].endswith(' seconds behind its source'), behind
                in_mysql('mmwatchdog-test-mysql-replica', 'STOP REPLICA SQL_THREAD; CHANGE REPLICATION SOURCE TO SOURCE_DELAY=0; START REPLICA SQL_THREAD')
                becomes('mysql_itself/replica_follows_its_source', 'ok', 'replication_lag_seconds is 0 (fails above 5)')
                # A replica that does not apply changes, and one whose source
                # is gone, give no number: the check fails by itself.
                in_mysql('mmwatchdog-test-mysql-replica', 'STOP REPLICA SQL_THREAD')
                becomes('mysql_itself/replica_follows_its_source', 'failed', 'the MySQL replica does not follow its source: it is receiving changes; it is not applying changes (No)')
                in_mysql('mmwatchdog-test-mysql-replica', 'START REPLICA SQL_THREAD')
                becomes('mysql_itself/replica_follows_its_source', 'ok')
                # The source is gone at once, as after a crash: a stop would
                # wait for the server to shut down, which proves nothing more.
                vm.console('docker kill mmwatchdog-test-mysql-source >/dev/null', timeout=120)
                becomes('mysql_itself/replica_follows_its_source', 'failed', 'the MySQL replica does not follow its source: it is not receiving changes (Connecting); it is applying changes')
                becomes('mysql_itself/source_answers', 'failed', 'the MySQL server did not answer the check: ' + cannot_connect)
                vm.console('docker start mmwatchdog-test-mysql-source >/dev/null')
                wait(lambda: last('mysql_itself/source_answers')['status'] == 'ok' and last('mysql_itself/replica_follows_its_source')['status'] == 'ok', timeout=120)
            # The MariaDB client of openSUSE Leap 16.1 (12.3) has no plugin caching_sha2_password, which MySQL 8.4 asks for:
            # it cannot log in to the MySQL servers. MySQL's own client program, below, can.
            if layout()['mariadb_client_logs_in_to_mysql']:
                assert vm.console('readlink -f "$(command -v mariadb)"').stdout.strip() == '/usr/bin/mariadb'
                mysql_itself("ERROR 2002 (HY000): Can't connect to server on '" + DATABASE_HOST + "'", 'TLS/SSL error: self-signed certificate in certificate chain', 'TLS/SSL error: ' + ('Hostname verification failed' if client_from_11_4 else 'Validation of SSL server certificate failed'))
                mark('a mysql check asks a real MySQL source and its real replica with the client program of MariaDB that Debian has, the source over a connection whose certificate it verifies, the replica without encryption and with encryption that verifies nothing; a certificate of an unknown authority and one for another name fail the check; a server that is no replica is 0 seconds behind, a replica that applies its changes late is behind by that time, a replica that does not apply changes or has lost its source fails the check with the reason, and a wrong password and an account without the privilege are told apart')
            # The same with MySQL's own client program, as on a node that has
            # MySQL and not MariaDB: the program of the image takes the place
            # of MariaDB's for this part.
            vm.console('install -d ' + CLIENT_PROGRAMS + '; docker cp --follow-link mmwatchdog-test-mysql-source:/usr/bin/mysql ' + CLIENT_PROGRAMS + '/mysql >/dev/null; '
                       + layout()['set_aside_mariadb_client'] + '; ln -sf ' + CLIENT_PROGRAMS + '/mysql /usr/local/bin/mysql')
            vm.console(layout()['prepare_the_mysql_client'])
            assert 'MySQL Community Server' in vm.console('runuser -u mmwatchdog -- mysql --version').stdout
            fresh(*('mysql_itself/' + check for check in groups['mysql_itself']['checks']))
            # MySQL's client says the same for both certificates that it does
            # not accept.
            not_accepted = 'SSL connection error: error:0A000086:SSL routines::certificate verify failed'
            mysql_itself("ERROR 2003 (HY000): Can't connect to MySQL server on '" + DATABASE_HOST + ":3307'", not_accepted, not_accepted)
            # A session holds a snapshot open, and another commits 300
            # changes: InnoDB's counter grows by them.
            vm.console('docker exec --detach --env MYSQL_PWD=' + shlex.quote(DATABASE_PASSWORD) + ' mmwatchdog-test-mysql-source mysql --user=root --execute ' + shlex.quote(SNAPSHOT_HELD))
            wait(lambda: in_mysql('mmwatchdog-test-mysql-source', "SELECT COUNT(*) FROM information_schema.PROCESSLIST WHERE INFO = 'SELECT SLEEP(3600)'") == '1')
            in_mysql('mmwatchdog-test-mysql-source', ' '.join(['UPDATE ' + HISTORY_TABLE + ' SET changes = changes + 1 WHERE id = 1;'] * CHANGES_THAT_GROW_THE_HISTORY))
            grown = becomes('mysql_itself/innodb_history_is_short', 'failed', 'innodb_history_list_length is ')
            assert grown['measurements']['innodb_history_list_length']['value'] > 100, grown
            vm.console('rm -f /usr/local/bin/mysql; ' + layout()['put_back_mariadb_client'] + '; docker rm --force ' + ' '.join(MYSQL_SERVERS) + ' >/dev/null')
            del groups['mysql_itself']
            reload()
            mark('the same mysql checks pass with MySQL\'s own client program in the place of MariaDB\'s, which takes other arguments for each kind of connection security; with both client programs, an account with PROCESS reads the InnoDB history list length from InnoDB\'s counter, and an account without it makes only a limit on it unknown and still reads the lag of a source and of a replica; the counter grows past a limit of 100 when another session commits 300 changes while one holds a snapshot open')

        # A SQLite database that a program of another account writes while
        # the check reads it, with a check of its integrity in every run.
        database = FIXTURE + '/sqlite/myapp.db'
        vm.console('rm -rf ' + FIXTURE + '/sqlite; install -d -o mmwatchdog -g mmwatchdog -m 0750 ' + FIXTURE + '/sqlite')
        vm.write(FIXTURE + '/sqlite_writer.py', SQLITE_WRITER)
        vm.console('systemd-run --quiet --unit mmwatchdog-writes-sqlite --uid mmwatchdog --gid mmwatchdog python3 ' + FIXTURE + '/sqlite_writer.py ' + database)
        vm.console("printf 'just text' > " + FIXTURE + '/sqlite/notes.txt')
        groups['sqlite'] = dict(quick, check_every='1s', time_limit_per_run='900ms', checks={
            'database_answers': {'sqlite_database': {'path': database, 'check_integrity': True}, 'fail_when_measurement_above': {'write_ahead_log_size_mib': 500}, 'alert_severity': 'warn'},
            'is_no_database': {'sqlite_database': {'path': FIXTURE + '/sqlite/notes.txt'}, 'alert_severity': 'info'},
            'is_missing': {'sqlite_database': {'path': FIXTURE + '/sqlite/missing.db'}, 'alert_severity': 'info'}})
        reload()
        answers = becomes('sqlite/database_answers', 'ok', 'write_ahead_log_size_mib is ')
        assert answers['message'].endswith(' (fails above 500); ' + database + ' answers a query, and its integrity is intact'), answers
        rows = int(vm.console("python3 -c \"import sqlite3; print(sqlite3.connect('file:" + database + "?mode=ro', uri=True).execute('SELECT count(*) FROM jobs').fetchone()[0])\"").stdout)
        wait(lambda: int(vm.console("python3 -c \"import sqlite3; print(sqlite3.connect('file:" + database + "?mode=ro', uri=True).execute('SELECT count(*) FROM jobs').fetchone()[0])\"").stdout) > rows + 150, timeout=40)
        grown = last('sqlite/database_answers')
        assert grown['status'] == 'ok' and grown['measurements']['database_size_mib']['value'] > 0 and grown['measurements']['write_ahead_log_size_mib']['value'] > 0, grown
        # The program never got an error, and every file of the database
        # still belongs to its account.
        assert vm.console('cat ' + database + '.held', check=False).stdout == '', 'the writing program got an error beside the check'
        assert set(vm.console('stat -c %U ' + database + '*').stdout.split()) == {'mmwatchdog'}
        becomes('sqlite/is_no_database', 'failed', FIXTURE + '/sqlite/notes.txt is no SQLite database')
        becomes('sqlite/is_missing', 'failed', FIXTURE + '/sqlite/missing.db does not exist')
        vm.console('systemctl stop mmwatchdog-writes-sqlite')
        del groups['sqlite']
        reload()
        mark('a sqlite_database check reads the database of a program of another account ten times and more while that program writes 20 rows a second with Python\'s default wait for a lock: the program never gets an error, the files of the database keep their owner, and the sizes of the database and of its write-ahead log are measured')

        vm.install_official_extra_modules()
        # The official extra module apache_status, against Apache as the
        # distribution installs it: the status page is on, and allowed for
        # requests from the same machine.
        apache_unit = layout()['apache_unit']
        vm.console('systemctl start ' + apache_unit)
        groups['apache'] = dict(quick, checks={
            'apache_answers_and_has_free_workers': {'apache_status': {'url': 'http://127.0.0.1/server-status?auto'}, 'fail_when_measurement_above': {'busy_workers_percent': 90}, 'alert_severity': 'warn'},
            'page_for_people': {'apache_status': {'url': 'http://127.0.0.1/server-status'}, 'alert_severity': 'info'},
            'no_status_page': {'apache_status': {'url': 'http://127.0.0.1/no-such-page?auto'}, 'alert_severity': 'info'}})
        reload()
        apache = becomes('apache/apache_answers_and_has_free_workers', 'ok', 'busy_workers_percent is ')
        assert ' (fails above 90); the module reported: Apache answers; ' in apache['message'] and apache['message'].endswith(' workers are busy'), apache
        assert apache['measurements']['busy_workers']['value'] >= 1 and apache['measurements']['idle_workers']['unit'] == 'workers' and 0 < apache['measurements']['busy_workers_percent']['value'] < 50, apache
        assert state('apache/apache_answers_and_has_free_workers')['module_kind'] == 'official_extra'
        becomes('apache/page_for_people', 'unknown', 'the answer is not the status page of Apache in the form for programs')
        becomes('apache/no_status_page', 'failed', 'Apache answered the request for its status page with HTTP 404')
        vm.console('systemctl stop ' + apache_unit)
        becomes('apache/apache_answers_and_has_free_workers', 'failed', 'Apache did not answer the request for its status page: ')
        vm.console('systemctl start ' + apache_unit)
        becomes('apache/apache_answers_and_has_free_workers', 'ok')
        # An Apache that hangs: SIGSTOP pauses its processes, the kernel
        # still accepts each connection for them, and nothing answers.
        # The request ends shortly before the time limit of the run, and
        # the check fails, so a recovery that it drives would restart
        # Apache.
        vm.console('systemctl kill --signal=SIGSTOP ' + apache_unit)
        becomes('apache/apache_answers_and_has_free_workers', 'failed', 'Apache did not answer the request for its status page: MMWatchdog ended the call shortly before the time limit of the run (time_limit_per_run)')
        vm.console('systemctl kill --signal=SIGCONT ' + apache_unit)
        becomes('apache/apache_answers_and_has_free_workers', 'ok')
        del groups['apache']
        mark('the official extra module apache_status reads the status page of the real Apache server, measures its busy and idle workers, and fails when Apache is stopped, hangs, or answers with another page')

        if not has_ready_zfs():
            # The official extra module zfs_pool on a node without ZFS: the
            # check cannot check, and says that the program is missing.
            assert vm.console('command -v zpool', check=False).returncode != 0, 'this guest has a zpool program'
            groups['zfs'] = dict(quick, checks={'pool_is_healthy_and_has_room': {'zfs_pool': {'pool_name': POOL}, 'alert_severity': 'sos'}})
            reload()
            becomes('zfs/pool_is_healthy_and_has_room', 'unknown', 'MMWatchdog cannot run the zpool program: ')
            del groups['zfs']
            reload()
            mark('the official extra module zfs_pool on a node without ZFS cannot check, and says that MMWatchdog cannot run the zpool program')
        else:
            # The official extra module zfs_pool, against a real pool that
            # mirrors two files.
            install_zfs()
            files = [FIXTURE + '/zfs-' + name for name in ['a', 'b']]
            vm.console('zpool destroy ' + POOL + ' 2>/dev/null; truncate -s 128M ' + ' '.join(files) + '; zpool create ' + POOL + ' mirror ' + ' '.join(files))
            groups['zfs'] = dict(quick, checks={
                'pool_is_healthy_and_has_room': {'zfs_pool': {'pool_name': POOL}, 'fail_when_measurement_above': {'space_used_percent': 80}, 'alert_severity': 'sos'},
                'pool_was_scrubbed_within_35_days': {'zfs_pool': {'pool_name': POOL}, 'fail_when_measurement_above': {'days_since_last_scrub': 35}, 'alert_severity': 'warn'},
                'pool_that_is_not_there': {'zfs_pool': {'pool_name': 'nosuchpool'}, 'alert_severity': 'info'}})
            reload()
            pool = becomes('zfs/pool_is_healthy_and_has_room', 'ok', 'space_used_percent is 0 (fails above 80); the module reported: the ZFS pool ' + POOL + ' is ONLINE and healthy')
            assert set(pool['measurements']) == {'space_used_percent', 'free_space_fragmentation_percent', 'days_since_last_scrub'} and pool['measurements']['space_used_percent']['unit'] == 'percent', pool
            becomes('zfs/pool_that_is_not_there', 'failed', 'this node has no ZFS pool named nosuchpool')

            def scrub_says(text):
                """Wait until the check of the scrub is healthy with a message that holds the text, and return its observation: the module's message follows the reading of days_since_last_scrub, which is below a day here."""
                try:
                    found = wait(lambda: last('zfs/pool_was_scrubbed_within_35_days')['status'] == 'ok' and text in last('zfs/pool_was_scrubbed_within_35_days')['message'] and last('zfs/pool_was_scrubbed_within_35_days'))
                except AssertionError:
                    raise AssertionError((text, last('zfs/pool_was_scrubbed_within_35_days'))) from None
                assert found['message'].startswith('days_since_last_scrub is ') and ' (fails above 35); the module reported: the ZFS pool ' + POOL + ' is ONLINE and healthy; ' in found['message'], found
                assert 0 <= found['measurements']['days_since_last_scrub']['value'] < 1 and found['measurements']['days_since_last_scrub']['unit'] == 'days', found
                return found

            def scan_line():
                """The scan line of zpool status, as zpool writes it, and its date with one space between the words."""
                line = vm.console('zpool status ' + POOL + " | sed -n 's/^ *scan: //p'").stdout.strip()
                return line, ' '.join(line.split()[-5:])
            # A new pool was never scrubbed: the days count from its creation.
            scrub_says("; MMWatchdog has not observed a completed scrub, so days_since_last_scrub counts from the pool's creation")
            vm.console('zpool scrub -w ' + POOL)
            line, date = scan_line()
            assert line.startswith('scrub repaired '), line
            scrub_says('; its last scrub ended on ' + date)
            completed_scrub_date = date
            # A scrub that runs, held in place by OpenZFS's setting, then
            # paused, then canceled.
            vm.console('echo 1 > ' + ZFS_SCAN_SUSPEND_PROGRESS + '; zpool scrub ' + POOL)
            line, date = scan_line()
            assert line.startswith('scrub in progress since '), line
            scrub_says('; its last completed scrub observed by MMWatchdog ended on ' + completed_scrub_date)
            vm.console('zpool scrub -p ' + POOL)
            scrub_says('; its last completed scrub observed by MMWatchdog ended on ' + completed_scrub_date)
            vm.console('zpool scrub -s ' + POOL)
            line, date = scan_line()
            assert line.startswith('scrub canceled on '), line
            scrub_says('; its last completed scrub observed by MMWatchdog ended on ' + completed_scrub_date)
            vm.console('echo 0 > ' + ZFS_SCAN_SUSPEND_PROGRESS)
            vm.console('zpool offline ' + POOL + ' ' + files[1])
            becomes('zfs/pool_is_healthy_and_has_room', 'failed', 'the ZFS pool ' + POOL + ' is DEGRADED: One or more devices has been taken offline by the administrator.')
            # A file written while one half is offline: that half gets it
            # by a resilver when it comes back.
            vm.console('head -c 1048576 /dev/urandom > /' + POOL + '/written-while-one-half-was-offline; zpool sync ' + POOL)
            # zpool wait can return before the resilver has started, while the
            # scan line still names the canceled scrub: wait for the line.
            vm.console('zpool online ' + POOL + ' ' + files[1])
            wait(lambda: scan_line()[0].startswith('resilvered '))
            becomes('zfs/pool_is_healthy_and_has_room', 'ok')
            line, date = scan_line()
            scrub_says('; its last completed scrub observed by MMWatchdog ended on ' + completed_scrub_date)
            vm.console('zpool export ' + POOL)
            becomes('zfs/pool_is_healthy_and_has_room', 'failed', 'this node has no ZFS pool named ' + POOL)
            vm.console('zpool import -d ' + FIXTURE + ' ' + POOL + '; zpool destroy ' + POOL)
            del groups['zfs']
            reload()
            mark('the official extra module zfs_pool finds a real mirrored pool healthy and measures its use and its fragmentation, fails with zpool\'s own reason while one half is offline, and fails when the pool is exported; days_since_last_scrub reads the real scan line of zpool status: a new pool that was never scrubbed counts from its creation, then retains the completed scrub date while another scrub runs, is paused, or is canceled, and after a resilver')

        # Docker is in no repository of the distribution on the Red Hat family (see LAYOUTS): the scenario of MongoDB runs in its containers.
        if layout()['docker_scenarios']:
            # The official extra module mongodb, against MongoDB itself and
            # through MongoDB's own shell on the guest: a server of its own, a
            # server that asks for a login, and a replica set of two members and
            # an arbiter.
            def in_mongodb(server, script, login='', check=True):
                """Run JavaScript in the shell inside the container of a MongoDB server, and return what it prints."""
                return vm.console('docker exec ' + server + ' mongosh --quiet --norc ' + login + ' --eval ' + shlex.quote(script), check=check, timeout=120).stdout.strip()
            kernel = tuple(int(number) for number in vm.console('uname -r').stdout.strip().split('-')[0].split('.')[:2])
            mongodb_image = MONGODB_IMAGE if kernel < (6, 19) else MONGODB_IMAGE_FOR_LINUX_6_19_AND_NEWER
            vm.console('docker rm --force ' + ' '.join(MONGODB_SERVERS) + ' >/dev/null 2>&1; rm -f /usr/local/bin/mongosh; docker pull --quiet ' + mongodb_image + ' >/dev/null && { docker network inspect ' + DATABASE_NETWORK + ' >/dev/null 2>&1 || docker network create ' + DATABASE_NETWORK + ' >/dev/null; }', timeout=900)
            administrator = '--env MONGO_INITDB_ROOT_USERNAME=administrator --env MONGO_INITDB_ROOT_PASSWORD=' + shlex.quote(DATABASE_PASSWORD)
            for server, port in MONGODB_SERVERS.items():
                options = administrator if server.endswith('-login') else ''
                # A primary writes an entry of no content to its log of
                # changes every second, and not every 10 seconds, so that a
                # secondary that applies nothing falls behind by a second
                # each second, as under a steady load of writes.
                replica_set = '' if server.endswith(('-alone', '-login')) else ' --replSet mmwatchdogtest --bind_ip_all --setParameter periodicNoopIntervalSecs=1'
                vm.console('docker run --detach --name ' + server + ' --network ' + DATABASE_NETWORK + ' --publish 127.0.0.1:' + str(port) + ':27017 ' + options + ' ' + mongodb_image + ' --wiredTigerCacheSizeGB 0.25' + replica_set + ' >/dev/null')
            # The server with a login sets its administrator up and then starts
            # once more: only then is it the server that the check asks.
            vm.console("for i in $(seq 120); do docker logs mmwatchdog-test-mongodb-login 2>&1 | grep -q 'MongoDB init process complete' && exit 0; sleep 1; done; exit 1", timeout=180)
            as_administrator = '--username administrator --password ' + shlex.quote(DATABASE_PASSWORD) + ' --authenticationDatabase admin'
            for server in MONGODB_SERVERS:
                try:
                    wait(lambda: in_mongodb(server, 'print(db.runCommand({ping: 1}).ok)', as_administrator if server.endswith('-login') else '', check=False) == '1', timeout=120)
                except AssertionError as timed_out:
                    # The container's own log says why its server did not answer.
                    raise AssertionError(server + ' did not answer; the end of its log:\n' + vm.console('docker logs --tail 40 ' + server + ' 2>&1', check=False).stdout) from timed_out
            in_mongodb('mmwatchdog-test-mongodb-login', 'db.getSiblingDB("admin").createUser({user: "mmwatchdog_monitoring", pwd: ' + json.dumps(MONGODB_PASSWORD) + ', roles: ["clusterMonitor"]})', as_administrator)
            # MMWatchdog reads root-owned connection strings and passes their content
            # to the unprivileged module. Passwords use their percent form.
            secrets = '/etc/mmwatchdog/secrets/mongodb-'
            connection_strings = {
                'login': 'mongodb://mmwatchdog_monitoring:' + urllib.parse.quote(MONGODB_PASSWORD, safe='') + '@127.0.0.1:27018/',
                'without-the-login': 'mongodb://127.0.0.1:27018/',
                'another-password': 'mongodb://mmwatchdog_monitoring:another%20password@127.0.0.1:27018/',
                'password-as-it-is': 'mongodb://mmwatchdog_monitoring:' + MONGODB_PASSWORD.replace(' ', '') + '@127.0.0.1:27018/',
                'first-member': 'mongodb://127.0.0.1:27021/', 'second-member': 'mongodb://127.0.0.1:27022/', 'arbiter': 'mongodb://127.0.0.1:27023/'}
            for name, connection_string in connection_strings.items():
                vm.write(secrets + name, connection_string + '\n', '0600')
            vm.console('chown root:root ' + secrets + '*; chmod 0600 ' + secrets + '*')
            # Each run of a check starts the shell, a Node.js program, which
            # takes about a third of a second of processor time on the AMD64
            # guest and longer on a small ARM64 server. So the checks of the
            # servers without a replica set come first, and those of the
            # replica set after them, and each run has 5 seconds: the start
            # of the shell must never decide a scenario.
            mongodb_timings = dict(quick, check_every='5s', time_limit_per_run='5s')
            groups['mongodb'] = dict(mongodb_timings, checks={
                'server_of_its_own': {'mongodb': {}, 'fail_when_measurement_above': {'connections_used_percent': 90}, 'alert_severity': 'sos'},
                'through_a_login': {'mongodb': {'connection_string_file': secrets + 'login'}, 'alert_severity': 'sos'},
                'without_the_login': {'mongodb': {'connection_string_file': secrets + 'without-the-login'}, 'alert_severity': 'info'},
                'with_another_password': {'mongodb': {'connection_string_file': secrets + 'another-password'}, 'alert_severity': 'info'},
                'password_as_it_is': {'mongodb': {'connection_string_file': secrets + 'password-as-it-is'}, 'alert_severity': 'info'}})
            reload()

            def mongodb(check, status, message='', timeout=60):
                try:
                    return wait(lambda: last('mongodb/' + check)['status'] == status and message in last('mongodb/' + check)['message'] and last('mongodb/' + check), timeout=timeout)
                except AssertionError:
                    raise AssertionError((check, status, message, last('mongodb/' + check))) from None
            # A guest without the shell cannot check.
            mongodb('server_of_its_own', 'unknown', 'MMWatchdog cannot run mongosh, the shell of MongoDB: ')
            vm.console('install -d ' + CLIENT_PROGRAMS + '; docker cp --follow-link mmwatchdog-test-mongodb-alone:/usr/bin/mongosh ' + CLIENT_PROGRAMS + '/mongosh >/dev/null; ln -sf ' + CLIENT_PROGRAMS + '/mongosh /usr/local/bin/mongosh')
            alone = mongodb('server_of_its_own', 'ok', ' (fails above 90); the module reported: the MongoDB server answers; it is a server of its own, in no replica set')
            assert set(alone['measurements']) == {'connections_used_percent'} and 0 < alone['measurements']['connections_used_percent']['value'] < 5, alone
            assert mongodb('through_a_login', 'ok')['message'] == 'the MongoDB server answers; it is a server of its own, in no replica set'
            needs_login = mongodb('without_the_login', 'failed', 'the MongoDB server did not answer the check: MongoServerError: ')
            assert 'requires authentication' in needs_login['message'], needs_login
            wrong = mongodb('with_another_password', 'failed', 'the MongoDB server did not answer the check: MongoServerError: Authentication failed')
            assert 'another' not in json.dumps(wrong), wrong
            mongodb('password_as_it_is', 'unknown', 'the connection string in the file that connection_string_file names has a user name or a password with a character that must be written in its percent form there')
            # The three servers of the replica set before anybody set the
            # replica set up: each waits, and takes no changes.
            behind = {'replication_lag_seconds': 5}
            groups['mongodb'] = dict(mongodb_timings, checks={
                'first_member': {'mongodb': {'connection_string_file': secrets + 'first-member'}, 'fail_when_measurement_above': behind, 'alert_severity': 'sos'},
                'second_member': {'mongodb': {'connection_string_file': secrets + 'second-member'}, 'fail_when_measurement_above': behind, 'alert_severity': 'sos'},
                'arbiter': {'mongodb': {'connection_string_file': secrets + 'arbiter'}, 'alert_severity': 'sos'}})
            reload()
            mongodb('first_member', 'failed', 'the MongoDB server answers and takes no changes: it was started for a replica set and is a member of none')
            # The members ask each other every half second, call a member
            # that has not answered for 2 seconds unreachable, and elect a
            # new primary after 2 seconds without one, and not after 10
            # seconds each, MongoDB's own times for a network between
            # machines.
            in_mongodb('mmwatchdog-test-mongodb-1', 'rs.initiate({_id: "mmwatchdogtest", members: [{_id: 0, host: "mmwatchdog-test-mongodb-1:27017", priority: 2}, {_id: 1, host: "mmwatchdog-test-mongodb-2:27017", priority: 1}, {_id: 2, host: "mmwatchdog-test-mongodb-arbiter:27017", arbiterOnly: true}], '
                       'settings: {heartbeatIntervalMillis: 500, heartbeatTimeoutSecs: 2, electionTimeoutMillis: 2000}})')
            primary = mongodb('first_member', 'ok', 'the MongoDB server answers; it is a member of the replica set mmwatchdogtest in the state PRIMARY', timeout=120)
            assert primary['message'].startswith('replication_lag_seconds is 0 (fails above 5); ') and primary['measurements']['replication_lag_seconds'] == {'value': 0, 'unit': 'seconds'}, primary
            secondary = mongodb('second_member', 'ok', 'the MongoDB server answers; it is a member of the replica set mmwatchdogtest in the state SECONDARY', timeout=120)
            assert 0 <= secondary['measurements']['replication_lag_seconds']['value'] <= 5, secondary
            arbiter = mongodb('arbiter', 'ok', 'the MongoDB server answers; it is a member of the replica set mmwatchdogtest in the state ARBITER', timeout=120)
            assert 'replication_lag_seconds' not in arbiter['measurements'], arbiter
            # The secondary stops applying changes while the primary goes on:
            # it falls behind.
            in_mongodb('mmwatchdog-test-mongodb-2', 'db.fsyncLock()')
            in_mongodb('mmwatchdog-test-mongodb-1', 'db.getSiblingDB("mmwatchdog_test").written.insertOne({at: new Date()})')
            fell_behind = mongodb('second_member', 'failed', 'replication_lag_seconds is ', timeout=120)
            assert fell_behind['measurements']['replication_lag_seconds']['value'] > 5 and 'in the state SECONDARY' in fell_behind['message'], fell_behind
            in_mongodb('mmwatchdog-test-mongodb-2', 'db.fsyncUnlock()')
            mongodb('second_member', 'ok')
            # A member that is gone: its own check fails, and the check of the
            # primary names the member that it cannot reach. Each member is
            # gone at once, as after a crash: a stop would wait for the
            # server to shut down, which proves nothing more.
            vm.console('docker kill mmwatchdog-test-mongodb-2 >/dev/null', timeout=120)
            mongodb('second_member', 'failed', 'the MongoDB server did not answer the check: MongoNetworkError: connect ECONNREFUSED 127.0.0.1:27022')
            mongodb('first_member', 'failed', 'in the state PRIMARY; this member cannot reach mmwatchdog-test-mongodb-2:27017')
            vm.console('docker start mmwatchdog-test-mongodb-2 >/dev/null')
            mongodb('first_member', 'ok', timeout=120)
            mongodb('second_member', 'ok', 'in the state SECONDARY', timeout=120)
            # The primary is gone: the secondary takes its place. The same
            # check, with its limit on the lag, fits the new primary.
            vm.console('docker kill mmwatchdog-test-mongodb-1 >/dev/null', timeout=120)
            took_over = mongodb('second_member', 'failed', 'in the state PRIMARY; this member cannot reach mmwatchdog-test-mongodb-1:27017', timeout=120)
            assert took_over['measurements']['replication_lag_seconds'] == {'value': 0, 'unit': 'seconds'}, took_over
            vm.console('docker start mmwatchdog-test-mongodb-1 >/dev/null')
            wait(lambda: all(last('mongodb/' + check)['status'] == 'ok' for check in ['first_member', 'second_member', 'arbiter']), timeout=180)
            # Reuse the completed login fixture as an authenticated replica.
            # The three-member replica above permits every command without
            # authentication and cannot exercise an authorization refusal.
            vm.console('docker rm --force mmwatchdog-test-mongodb-login >/dev/null')
            start_authenticated_replica = (
                'printf %s mmwatchdogAuthenticatedReplicaTestKey > /tmp/mmwatchdog-replica-key; '
                'chmod 0400 /tmp/mmwatchdog-replica-key; chown mongodb:mongodb /tmp/mmwatchdog-replica-key; '
                'exec /usr/local/bin/docker-entrypoint.sh mongod --wiredTigerCacheSizeGB 0.25 '
                '--replSet mmwatchdogrestricted --bind_ip_all --keyFile /tmp/mmwatchdog-replica-key')
            vm.console('docker run --detach --name mmwatchdog-test-mongodb-login --network ' + DATABASE_NETWORK
                       + ' --publish 127.0.0.1:27018:27017 ' + administrator + ' --entrypoint /bin/sh '
                       + mongodb_image + ' -ec ' + shlex.quote(start_authenticated_replica) + ' >/dev/null')
            wait(lambda: 'MongoDB init process complete' in vm.console('docker logs mmwatchdog-test-mongodb-login 2>&1').stdout, timeout=180)
            wait(lambda: in_mongodb('mmwatchdog-test-mongodb-login', 'print(db.adminCommand({hello: 1}).isreplicaset)', as_administrator, check=False) == 'true', timeout=120)
            in_mongodb('mmwatchdog-test-mongodb-login', 'rs.initiate({_id: "mmwatchdogrestricted", members: [{_id: 0, host: "mmwatchdog-test-mongodb-login:27017"}]})', as_administrator)
            wait(lambda: in_mongodb('mmwatchdog-test-mongodb-login', 'print(db.adminCommand({hello: 1}).isWritablePrimary)', as_administrator, check=False) == 'true', timeout=120)
            in_mongodb('mmwatchdog-test-mongodb-login',
                       'const admin = db.getSiblingDB("admin"); '
                       'admin.createRole({role: "server_status_only", privileges: [{resource: {cluster: true}, actions: ["serverStatus"]}], roles: []}); '
                       'admin.createUser({user: "mmwatchdog_server_status_only", pwd: ' + json.dumps(MONGODB_PASSWORD) + ', roles: ["server_status_only"]})',
                       as_administrator)
            limited_login = '--username mmwatchdog_server_status_only --password ' + shlex.quote(MONGODB_PASSWORD) + ' --authenticationDatabase admin'
            assert in_mongodb('mmwatchdog-test-mongodb-login', 'print(db.adminCommand({serverStatus: 1}).ok)', limited_login) == '1'
            restricted_connection_string = ('mongodb://mmwatchdog_server_status_only:' + urllib.parse.quote(MONGODB_PASSWORD, safe='')
                                            + '@127.0.0.1:27018/?authSource=admin&directConnection=true')
            vm.write(secrets + 'restricted-replica-connection-string', restricted_connection_string + '\n', '0600')
            groups['mongodb'] = dict(mongodb_timings, checks={
                'replica_status_is_not_permitted': {'mongodb': {'connection_string_file': secrets + 'restricted-replica-connection-string'}, 'alert_severity': 'info'}})
            reload()
            refused = mongodb('replica_status_is_not_permitted', 'unknown', 'MongoDB could not report its replica set status: ')
            assert 'not authorized' in refused['message'].lower() and 'replSetGetStatus' in refused['message'], refused
            assert restricted_connection_string not in refused['message'] and MONGODB_PASSWORD not in refused['message'], refused
            vm.console('docker rm --force ' + ' '.join(MONGODB_SERVERS) + ' >/dev/null; rm -f /usr/local/bin/mongosh ' + secrets + '*')
            del groups['mongodb']
            reload()
            mark('the official extra module mongodb asks real MongoDB servers through MongoDB\'s own shell: a server of its own, a server with a login whose password is in its percent form, a wrong password that never shows, and a replica set whose primary is 0 seconds behind, whose secondary falls behind while it applies nothing, whose members name the member that they cannot reach, and whose secondary takes the place of a primary that is gone; an authenticated replica account allowed serverStatus but denied replSetGetStatus makes the run unknown with the authorization reason and no connection string; a guest without the shell is unknown')

        # The official extra module mattermost, against a real Mattermost
        # server with a database in the guest's PostgreSQL. It comes
        # last, when the containers of MySQL and MongoDB have given their
        # memory back.
        mattermost_scenarios.suite(cli, status, events, wait, mark, setup_step_between_scenarios_done)
    finally:
        clean_up()
        vm.console('rm -f ' + FRAGMENT)
        cli('reload', 0)


def clean_up():
    """Stop what the scenarios started and start the servers that they stopped. vm.reset calls it after an interruption too."""
    facts = layout()
    vm.console(vm.each_unit('stop', TRANSIENT_UNITS) + '; ' + vm.each_unit('reset-failed', TRANSIENT_UNITS) + '; '
               'rm -f ' + DOVECOT_WITHOUT_ENCRYPTION + "; if command -v postconf >/dev/null; then postconf -e 'smtpd_tls_security_level = may'; fi; "
               'rm -f ' + POSTGRESQL_SETTINGS + ' /etc/mmwatchdog-test/postgresql/*.conf /etc/mmwatchdog-test/postgresql/*.pem ' + ' '.join(mariadb_files(facts['mariadb_settings_directory']).values())
               + "; sed -i '/ # mmwatchdog-test$/d' /etc/postgresql/*/main/pg_hba.conf /var/lib/pgsql/data/pg_hba.conf /var/lib/postgres/data/pg_hba.conf 2>/dev/null; "
               # A server that a package starts by itself is enabled; the others are active.
               'for unit in ' + facts['units'] + '; do if systemctl is-enabled --quiet $unit 2>/dev/null || systemctl is-active --quiet $unit 2>/dev/null; then systemctl restart $unit; fi; done; '
               # After the restart, without its archiving settings, the
               # server no longer hands pgBackRest its write-ahead log.
               'rm -rf ' + pgbackrest_files(facts['pgbackrest_settings']) + '; '
               'rm -f /usr/local/bin/mysql /usr/local/bin/mongosh /usr/local/bin/mmwatchdog-test-dnsmasq; ' + facts['put_back_mariadb_client'] + '; '
               "if command -v mariadb >/dev/null; then mariadb -e \"STOP SLAVE 'orders'; RESET SLAVE 'orders' ALL\" 2>/dev/null; fi; "
               'if command -v pg_lsclusters >/dev/null; then pg_lsclusters --no-header | while read version cluster rest; do if [ "$cluster" = replica ]; then pg_ctlcluster $version replica stop; pg_dropcluster $version replica; fi; done; fi; '
               'if command -v docker >/dev/null; then docker rm --force ' + ' '.join([*CONTAINERS, *MYSQL_SERVERS, *MONGODB_SERVERS]) + ' >/dev/null 2>&1; docker network rm ' + DATABASE_NETWORK + ' >/dev/null 2>&1; fi; '
               'if command -v zpool >/dev/null; then zpool destroy ' + POOL + ' 2>/dev/null; fi; if [ -e ' + ZFS_SCAN_SUSPEND_PROGRESS + ' ]; then echo 0 > ' + ZFS_SCAN_SUSPEND_PROGRESS + '; fi; true', timeout=600)
    mattermost_scenarios.clean_up()
