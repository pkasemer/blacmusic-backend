#!/usr/bin/env bash
# Cleans the VPS so that it serves only Blac Music. NOTHING CHANGES WITHOUT --apply.
#
# Run it from your PC (Git Bash), as root:
#   ssh -i ~/.ssh/blac_vps root@HOST 'bash -s -- report'         < deploy/vps-cleanup.sh
#   ssh -i ~/.ssh/blac_vps root@HOST 'bash -s -- tidy'           < deploy/vps-cleanup.sh   # shows what it would do
#   ssh -i ~/.ssh/blac_vps root@HOST 'bash -s -- tidy --apply'   < deploy/vps-cleanup.sh   # does it
#
# Commands, in the order to use them:
#   report          sizes and services; changes nothing, with or without --apply
#   tidy            logs, caches and old program images. Touches nobody's content.
#   mwonya-offline  switch the Mwonya sites and their services off. Deletes nothing.
#   mwonya-online   undo mwonya-offline
#   mwonya-delete   delete the Mwonya files. Refused unless deploy/backup-mwonya.sh has verified a copy
#                   off the server, nothing has changed since, the sites are off, and CONFIRM="delete mwonya".
#
# It never touches /opt/blacmusic, the Blac Music containers or their volumes, or the blac* web sites.
set -euo pipefail

ROOT="${CLEANUP_ROOT:-}" # a test can point the script at a copy of the file system
WWW="$ROOT/var/www"
NGINX="$ROOT/etc/nginx"
RETIRED="$NGINX/retired-sites"
MARKER="$ROOT/root/mwonya-backup-verified"
KEEP_IMAGES=3
JOURNAL_LIMIT=300M
MWONYA_DIRS=("$WWW/mwonya_assets" "$WWW/mwonya_scripts"
  "$WWW/html/mwonyaa.com" "$WWW/html/artist.mwonyaa.com" "$WWW/html/xadmin.mwonyaa.com" "$WWW/html/developer.mwonya.com"
  "$WWW/html/events.mwonya.com" "$WWW/html/shop.mwonyaa.com" "$WWW/html/mobile.mwonya.com" "$WWW/html/artistapi.mwonya.com")
# services only the old sites used: PHP, the Redis installed on the host (Blac Music has its own in Docker), and a
# unit that points at a folder that no longer exists
OLD_SERVICES=(php8.1-fpm redis-server app)
MWONYA_CERTS=(mwonya.com-0001 creatorapi.mwonya.com)

APPLY=0
say() { printf '%s\n' "$*"; }
head_() { printf '\n== %s\n' "$*"; }
run() { # run a change, or only say what it would be
  if [ "$APPLY" = 1 ]; then say "doing: $*"; "$@"; else say "would: $*"; fi
}
size() { du -sh "$1" 2>/dev/null | cut -f1; }
die() { say "STOPPED: $*" >&2; exit 1; }

mwonya_totals() { # "files bytes" of everything mwonya-delete would remove
  local d found=()
  for d in "${MWONYA_DIRS[@]}"; do [ -e "$d" ] && found+=("$d"); done
  if [ ${#found[@]} -eq 0 ]; then echo "0 0"; return; fi
  find "${found[@]}" \( -type f -o -type l \) -printf '%s\n' | awk '{n++; s+=$1} END {printf "%d %.0f\n", n, s}'
}

report() {
  head_ "disk"; df -h / | tail -1
  head_ "can be tidied"
  for p in "$ROOT/var/log/journal" "$ROOT/root/.cache/pip" "$ROOT/root/.vscode-server" "$ROOT/var/cache/apt" "$ROOT/var/log/btmp" "$ROOT/var/log/btmp.1"; do
    [ -e "$p" ] && say "$(size "$p")  $p"
  done
  say "old program images:"; old_images | sed 's/^/  /'
  head_ "Mwonya"
  for p in "${MWONYA_DIRS[@]}"; do [ -e "$p" ] && say "$(size "$p")  $p"; done
  say "files and bytes: $(mwonya_totals)"
  say "web sites on: $(ls "$NGINX/sites-enabled" 2>/dev/null | tr '\n' ' ')"
  for s in "${OLD_SERVICES[@]}"; do say "service $s: $(systemctl is-active "$s" 2>/dev/null || true)"; done
  if [ -f "$MARKER" ]; then say "verified backup: $(cat "$MARKER")"; else say "verified backup: none"; fi
}

old_images() { # Blac Music image versions beyond the newest few
  local repo tag
  for repo in blacmusic-api blacmusic-worker blacmusic-admin; do
    { docker images "$repo" --format '{{.Tag}}' 2>/dev/null || true; } | { grep -E '^[0-9]{14}$' || true; } | sort -r | tail -n +$((KEEP_IMAGES + 1)) |
      while read -r tag; do echo "$repo:$tag"; done
  done
}

tidy() {
  head_ "system log files the logger cannot write (this fills the journal with errors)"
  local f fixed=0
  for f in "$ROOT"/var/log/{syslog,auth.log,kern.log,mail.log,user.log,cron.log}; do
    [ -f "$f" ] && [ "$(stat -c %U "$f")" != syslog ] || continue
    run chown syslog:adm "$f"; run chmod 640 "$f"; fixed=1
  done
  if [ "$fixed" = 1 ]; then run systemctl restart rsyslog; else say "nothing to fix"; fi

  head_ "journal: keep at most $JOURNAL_LIMIT"
  # the list of past SSH sign-ins is kept before old entries go
  if [ "$APPLY" = 1 ]; then
    { journalctl -u ssh --no-pager -o short-iso 2>/dev/null | grep 'Accepted ' || true; } >"$ROOT/root/ssh-signins-before-cleanup.log"
    chmod 600 "$ROOT/root/ssh-signins-before-cleanup.log"
    mkdir -p "$ROOT/etc/systemd/journald.conf.d"
    printf '[Journal]\nSystemMaxUse=%s\n' "$JOURNAL_LIMIT" >"$ROOT/etc/systemd/journald.conf.d/size.conf"
    say "saved past SSH sign-ins to /root/ssh-signins-before-cleanup.log"
  else
    say "would: save past SSH sign-ins, then limit the journal"
  fi
  run systemctl restart systemd-journald
  run journalctl --vacuum-size="$JOURNAL_LIMIT"

  head_ "caches"
  [ -d "$ROOT/root/.cache/pip" ] && run rm -rf "$ROOT/root/.cache/pip"
  if [ -d "$ROOT/root/.vscode-server" ]; then
    if pgrep -f '\.vscode-server/' >/dev/null 2>&1; then say "VS Code is connected: leaving $ROOT/root/.vscode-server"
    else run rm -rf "$ROOT/root/.vscode-server"; fi # it is downloaded again the next time VS Code connects
  fi
  run apt-get clean

  head_ "old records of failed sign-ins"
  [ -f "$ROOT/var/log/btmp.1" ] && run rm -f "$ROOT/var/log/btmp.1"
  [ -f "$ROOT/var/log/btmp" ] && run truncate -s 0 "$ROOT/var/log/btmp"

  head_ "old program images (the newest $KEEP_IMAGES of each are kept)"
  local img
  while read -r img; do
    if [ -n "$img" ]; then run docker rmi "$img" || say "kept $img (in use)"; fi
  done < <(old_images)
  run docker image prune -f
}

mwonya_sites() { find "$1" -maxdepth 1 -name '*mwonya*.conf' 2>/dev/null | sort; }

check_web_server() { nginx -t >/dev/null 2>&1; }

mwonya_offline() {
  head_ "switch the Mwonya web sites off"
  local sites f
  mapfile -t sites < <(mwonya_sites "$NGINX/sites-enabled")
  if [ ${#sites[@]} -eq 0 ]; then
    say "no Mwonya site is on"
  else
    check_web_server || die "the web server configuration is already broken; nothing was changed"
    run mkdir -p "$RETIRED"
    for f in "${sites[@]}"; do run mv "$f" "$RETIRED/"; done
    if [ "$APPLY" = 1 ] && ! check_web_server; then
      mv "$RETIRED"/*.conf "$NGINX/sites-enabled/"
      die "the web server did not accept the change; the sites were put back"
    fi
    run systemctl reload nginx
  fi
  head_ "stop the services only they used"
  local s
  for s in "${OLD_SERVICES[@]}"; do run systemctl disable --now "$s" || say "$s was not running"; done
}

mwonya_online() {
  head_ "switch the Mwonya sites back on"
  local sites f s
  mapfile -t sites < <(mwonya_sites "$RETIRED")
  for s in php8.1-fpm redis-server; do run systemctl enable --now "$s" || say "$s could not be started"; done
  for f in "${sites[@]}"; do run mv "$f" "$NGINX/sites-enabled/"; done
  if [ "$APPLY" = 1 ]; then check_web_server || die "the web server configuration has an error: run nginx -t"; fi
  run systemctl reload nginx
}

mwonya_delete() {
  [ -f "$MARKER" ] || die "there is no verified backup. Run deploy/backup-mwonya.sh from your PC first."
  [ -z "$(mwonya_sites "$NGINX/sites-enabled")" ] || die "the Mwonya sites are still on. Run mwonya-offline first."
  local now saved
  now="$(mwonya_totals)"
  saved="$(sed -nE 's/^files=([0-9]+) bytes=([0-9]+).*/\1 \2/p' "$MARKER")"
  [ "$now" = "$saved" ] || die "the files changed since the backup (now '$now', backed up '$saved'). Run the backup again."
  if [ "$APPLY" = 1 ] && [ "${CONFIRM:-}" != "delete mwonya" ]; then
    die 'deleting cannot be undone. Add CONFIRM="delete mwonya" in front of bash to go ahead.'
  fi
  head_ "delete the Mwonya files ($now files and bytes)"
  local p c
  for p in "${MWONYA_DIRS[@]}"; do [ -e "$p" ] && run rm -rf -- "$p"; done
  for p in "$RETIRED"/*mwonya*.conf "$NGINX"/sites-available/*mwonya*.conf; do [ -e "$p" ] || [ -L "$p" ] && run rm -f -- "$p"; done
  for p in "$ROOT"/root/{album_notification,artist_statistics_updater,playlist_generator,update_track_plays}.log; do [ -f "$p" ] && run rm -f -- "$p"; done
  [ -f "$ROOT/etc/systemd/system/app.service" ] && run rm -f -- "$ROOT/etc/systemd/system/app.service"
  for c in "${MWONYA_CERTS[@]}"; do run certbot delete --non-interactive --cert-name "$c" || say "certificate $c was not there"; done
  head_ "remove the programs only Mwonya used"
  run apt-get purge -y 'php8.1*' redis-server redis-tools || say "some packages were not installed"
  run systemctl daemon-reload
}

main() {
  local cmd="${1:-report}"
  [ "${2:-}" = "--apply" ] && APPLY=1
  [ -n "$ROOT" ] || [ "$(id -u)" = 0 ] || die "run this as root"
  case "$cmd" in
    report) report ;;
    tidy) tidy ;;
    mwonya-offline) mwonya_offline ;;
    mwonya-online) mwonya_online ;;
    mwonya-delete) mwonya_delete ;;
    *) die "unknown command $cmd (report, tidy, mwonya-offline, mwonya-online, mwonya-delete)" ;;
  esac
  if [ "$cmd" != report ]; then
    if [ "$APPLY" = 1 ]; then head_ "done"; df -h / | tail -1; else say ""; say "Nothing was changed. Add --apply to do this."; fi
  fi
}

# Everything is read before anything runs, and no command can swallow the rest of a script sent over stdin.
main "$@" </dev/null; exit
