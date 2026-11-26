#!/usr/bin/env bash
# Copies everything Mwonya from the VPS to this PC and proves the copy is complete. Run it on your PC (Git Bash):
#
#   deploy/backup-mwonya.sh root@178.79.160.251 ~/.ssh/blac_vps /c/Backups/mwonya
#
# The files travel as a series of archives of about 1 GB. If the connection drops, run the same command again: parts
# already verified are skipped. Each part is checked by its SHA-256 and its number of files. When all of them pass,
# the server gets a marker that lets "vps-cleanup.sh mwonya-delete" run.
#
# The archives contain passwords and private keys from the old sites. Keep them somewhere private.
#
# It reads from the server and writes only its own working files in /root. Switch the sites off first
# (vps-cleanup.sh mwonya-offline --apply) so that files do not change while they are copied.
set -euo pipefail
TARGET="${1:?usage: backup-mwonya.sh user@host ssh_key local_folder}"
KEY="${2:?ssh key}"
DEST="${3:?local folder}"
PART_MB="${PART_MB:-1024}"
ROOT="${CLEANUP_ROOT:-}" # tests only

remote() { # run a bash script (on stdin) on the server
  if [ "$TARGET" = local-test ]; then CLEANUP_ROOT="$ROOT" bash -s -- "$@"
  else ssh -i "$KEY" -o IdentitiesOnly=yes -o BatchMode=yes -o ServerAliveInterval=30 "$TARGET" "bash -s -- $(printf '%q ' "$@")"; fi
}

mkdir -p "$DEST"
WORK="$ROOT/root/mwonya-backup"

echo "== listing what to copy"
# the same folders vps-cleanup.sh would delete, plus the web server files for those sites
plan="$(remote "$ROOT" "$WORK" "$PART_MB" <<'REMOTE'
set -euo pipefail
ROOT="$1"; WORK="$2"; PART=$(( $3 * 1048576 ))
rm -rf "$WORK"; mkdir -p "$WORK"; chmod 700 "$WORK"
cd "${ROOT:-/}"
paths=()
for d in var/www/mwonya_assets var/www/mwonya_scripts var/www/html/{mwonyaa.com,artist.mwonyaa.com,xadmin.mwonyaa.com,developer.mwonya.com,events.mwonya.com,shop.mwonyaa.com,mobile.mwonya.com,artistapi.mwonya.com}; do
  [ -e "$d" ] && paths+=("$d")
done
[ ${#paths[@]} -gt 0 ] || { echo "nothing to copy" >&2; exit 1; }
[ -z "$(find "${paths[@]}" -name $'*\n*' -print -quit)" ] || { echo "a file name contains a line break; copy it by hand" >&2; exit 1; }
# what the delete step will compare against
find "${paths[@]}" \( -type f -o -type l \) -printf '%s\n' | awk '{n++; s+=$1} END {printf "%d %.0f\n", n, s}' >"$WORK/totals"
extra=()
for f in etc/nginx/sites-available/*mwonya* etc/systemd/system/app.service root/{album_notification,artist_statistics_updater,playlist_generator,update_track_plays}.log; do
  [ -f "$f" ] && extra+=("$f")
done
{ find "${paths[@]}" \( -type f -o -type l \) -printf '%s\t%p\n'
  if [ ${#extra[@]} -gt 0 ]; then find "${extra[@]}" -printf '%s\t%p\n'; fi; } | LC_ALL=C sort -t "$(printf '\t')" -k2 |
  awk -F '\t' -v part="$PART" -v work="$WORK" 'BEGIN {n=1} { if (size > 0 && size + $1 > part) {n++; size=0}; size+=$1; print $2 > sprintf("%s/part-%04d.list", work, n) }'
echo "$(ls "$WORK"/part-*.list | wc -l) $(cat "$WORK/totals")"
REMOTE
)"
read -r parts files bytes <<<"$plan"
echo "$files files, $(( bytes / 1048576 )) MB, in $parts parts"
free_kb=$(df -Pk "$DEST" | awk 'NR==2 {print $4}')
have_kb=$(du -sk "$DEST" | cut -f1)
need_kb=$(( bytes / 1024 + 1048576 - have_kb ))
[ "$free_kb" -gt "$need_kb" ] || { echo "not enough free space in $DEST: need about $(( need_kb / 1048576 )) GB"; exit 1; }

for i in $(seq 1 "$parts"); do
  name="$(printf 'part-%04d' "$i")"
  list_sum="$(remote "$WORK/$name.list" <<'REMOTE'
set -euo pipefail
# names, sizes and change times: a part is copied again if any of its files changed
cd "$(dirname "$1")"; list="$1"; root="${CLEANUP_ROOT:-/}"
(cd "$root" && while IFS= read -r f; do stat -c '%s %Y %n' -- "$f"; done <"$list") | sha256sum | cut -d' ' -f1
REMOTE
)"
  if [ -f "$DEST/$name.ok" ] && [ -f "$DEST/$name.tar" ] && [ "$(cat "$DEST/$name.ok")" = "$list_sum" ]; then
    echo "$name already verified"
    continue
  fi
  echo "$name of $parts: copying"
  rm -f "$DEST/$name.ok"
  remote "$ROOT" "$WORK" "$name" >"$DEST/$name.tar" <<'REMOTE'
set -euo pipefail
ROOT="$1"; WORK="$2"; name="$3"
pipe="$WORK/$name.pipe"; rm -f "$pipe"; mkfifo "$pipe"
sha256sum <"$pipe" | cut -d' ' -f1 >"$WORK/$name.sha256" &
# low priority, so Blac Music stays responsive
nice -n 19 tar -C "${ROOT:-/}" --verbatim-files-from -cf - -T "$WORK/$name.list" | tee "$pipe"
wait
rm -f "$pipe"
REMOTE
  info="$(remote "$WORK" "$name" <<'REMOTE'
set -euo pipefail
echo "$(cat "$1/$2.sha256") $(wc -l <"$1/$2.list")"
REMOTE
)"
  read -r want_sum want_count <<<"$info"
  got_sum="$(sha256sum "$DEST/$name.tar" | cut -d' ' -f1)"
  got_count="$(tar --force-local -tf "$DEST/$name.tar" | wc -l)"
  [ "$got_sum" = "$want_sum" ] || { echo "$name arrived damaged: run the command again"; exit 1; }
  [ "$got_count" -eq "$want_count" ] || { echo "$name has $got_count files, expected $want_count: run the command again"; exit 1; }
  echo "$list_sum" >"$DEST/$name.ok"
done

echo "== every part verified"
remote "$ROOT" "$WORK" "$files" "$bytes" "$parts" <<'REMOTE'
set -euo pipefail
ROOT="$1"; WORK="$2"
echo "files=$3 bytes=$4 parts=$5 verified=$(date -u +%FT%TZ)" >"$ROOT/root/mwonya-backup-verified"
rm -rf "$WORK"
REMOTE
echo "Backup complete in $DEST. Open an archive or two and check the music plays before deleting anything."
