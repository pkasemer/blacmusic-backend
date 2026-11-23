#!/usr/bin/env bash
# Build the images on THIS machine, copy them to the server and (re)start the stack.
# Building here keeps the compile work off the (small, shared) server.
#
#   deploy/ship.sh pk@178.79.160.251 [path/to/ssh_key]
#
# Needs Docker and ssh locally; the server needs deploy/provision.sh to have been run.
set -euo pipefail
TARGET="${1:?usage: deploy/ship.sh user@host [ssh_key]}"
KEY="${2:-$HOME/.ssh/blac_vps}"
cd "$(dirname "$0")/.."
SSH=(ssh -i "$KEY" -o IdentitiesOnly=yes -o BatchMode=yes)
DIR=/opt/blacmusic
TAG="$(date +%Y%m%d%H%M%S)"

echo "== build images ($TAG)"
docker build --target api    -t "blacmusic-api:$TAG"    .
docker build --target worker -t "blacmusic-worker:$TAG" .

echo "== copy images"
docker save "blacmusic-api:$TAG" "blacmusic-worker:$TAG" | gzip | "${SSH[@]}" "$TARGET" 'gunzip | docker load'

echo "== copy compose files"
scp -i "$KEY" -o IdentitiesOnly=yes deploy/docker-compose.prod.yml deploy/env.example "$TARGET:$DIR/"

echo "== start"
"${SSH[@]}" "$TARGET" "TAG=$TAG DIR=$DIR bash -s" <<'REMOTE'
set -euo pipefail
cd "$DIR"
if [ ! -f .env ]; then
  echo "creating .env with fresh random secrets"
  umask 077
  cp env.example .env
  sed -i "s/^POSTGRES_PASSWORD=.*/POSTGRES_PASSWORD=$(openssl rand -hex 24)/" .env
  sed -i "s/^JWT_SECRET=.*/JWT_SECRET=$(openssl rand -hex 32)/" .env
  sed -i "s/^RUSTFS_ACCESS_KEY=.*/RUSTFS_ACCESS_KEY=$(openssl rand -hex 8)/" .env
  sed -i "s/^RUSTFS_SECRET_KEY=.*/RUSTFS_SECRET_KEY=$(openssl rand -hex 24)/" .env
fi
chmod 600 .env
sed -i "s/^TAG=.*/TAG=$TAG/" .env
docker compose -f docker-compose.prod.yml --env-file .env up -d --remove-orphans
echo "== waiting for the API"
for i in $(seq 1 60); do
  if curl -fsS http://127.0.0.1:8080/readyz >/dev/null 2>&1; then echo "ready after ~${i}s"; break; fi
  sleep 1
done
curl -sS http://127.0.0.1:8080/readyz; echo
docker compose -f docker-compose.prod.yml ps --format 'table {{.Service}}\t{{.Status}}'
# keep the last few image versions only
docker image prune -f >/dev/null
REMOTE
