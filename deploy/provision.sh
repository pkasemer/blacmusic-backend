#!/usr/bin/env bash
# One-time server preparation. Run as root on the VPS:
#
#   ssh root@HOST 'bash -s' -- "<ssh public key for pk>" < deploy/provision.sh
#
# It is safe to run again. It only ADDS things: it never restarts nginx, php,
# redis or any other service that is already running on the machine.
#
#   1. creates the "pk" user (no password: set one yourself with `sudo passwd pk`),
#      key-only login with the given public key, member of sudo and docker
#   2. installs Docker Engine + the compose plugin from Docker's own apt repository
#      and turns on log rotation so container logs cannot fill the disk
#   3. creates /opt/blacmusic for the application files
set -euo pipefail

PUBKEY="${1:-}"
[ "$(id -u)" = 0 ] || { echo "run as root"; exit 1; }

export DEBIAN_FRONTEND=noninteractive
export NEEDRESTART_MODE=l          # only LIST services that would need a restart, never restart them

echo "== user pk"
if ! id pk >/dev/null 2>&1; then
  adduser --disabled-password --gecos "" pk
fi
usermod -aG sudo pk
install -d -m 700 -o pk -g pk /home/pk/.ssh
if [ -n "$PUBKEY" ]; then
  touch /home/pk/.ssh/authorized_keys
  grep -qxF "$PUBKEY" /home/pk/.ssh/authorized_keys || echo "$PUBKEY" >> /home/pk/.ssh/authorized_keys
  chown pk:pk /home/pk/.ssh/authorized_keys; chmod 600 /home/pk/.ssh/authorized_keys
fi

echo "== docker"
if ! command -v docker >/dev/null 2>&1; then
  apt-get update -qq
  apt-get install -y -qq --no-install-recommends ca-certificates curl gnupg >/dev/null
  install -m 0755 -d /etc/apt/keyrings
  curl -fsSL https://download.docker.com/linux/ubuntu/gpg | gpg --dearmor --yes -o /etc/apt/keyrings/docker.gpg
  chmod a+r /etc/apt/keyrings/docker.gpg
  . /etc/os-release
  echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.gpg] https://download.docker.com/linux/ubuntu ${VERSION_CODENAME} stable" \
    > /etc/apt/sources.list.d/docker.list
  apt-get update -qq
  apt-get install -y -qq --no-install-recommends docker-ce docker-ce-cli containerd.io docker-compose-plugin >/dev/null
fi
if [ ! -f /etc/docker/daemon.json ]; then
  cat > /etc/docker/daemon.json <<'JSON'
{
  "log-driver": "json-file",
  "log-opts": { "max-size": "10m", "max-file": "3" }
}
JSON
  systemctl restart docker
fi
systemctl enable --now docker >/dev/null 2>&1
usermod -aG docker pk
docker --version
docker compose version

echo "== application directory"
install -d -m 750 -o pk -g pk /opt/blacmusic

echo "== done (services that apt says need a restart are listed below and were NOT restarted)"
(command -v needrestart >/dev/null && needrestart -b 2>/dev/null | grep -E 'NEEDRESTART-(SVC|KSTA)' ) || true
