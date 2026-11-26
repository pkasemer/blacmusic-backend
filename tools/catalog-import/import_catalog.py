#!/usr/bin/env python3
"""Import an artist and their releases from a manifest plus audio files.

For catalogs the rights holder has authorised us to host. It drives the normal
public API as the account that owns the artist profile, so every check the app
gets (verified email, quota, ffprobe validation, transcoding, rights
acceptance) applies here too.

  BLAC_EMAIL=... BLAC_PASSWORD=... python import_catalog.py \
      --api https://blacmusic.pkasemer.com --manifest akeine.json --audio-dir ./akeine-audio          # dry run
  ... same ... --apply                                                                                 # do it

Duplicates are avoided in four ways:
  1. a track listed twice in the manifest (same normalised title) is an error;
  2. two manifest tracks pointing at identical audio (same SHA-256) are an error;
  3. a release or track that already exists on the server (same normalised
     title, and for releases the same type) is skipped, so re-running resumes
     where a failed run stopped;
  4. a track title that already exists in ANOTHER release of the artist
     (a single later also on an album) is refused unless the manifest sets
     "allowDuplicateTitle": true on that track.

Only Python's standard library is needed.
"""
import argparse
import hashlib
import json
import os
import re
import sys
import time
import urllib.error
import urllib.request

TERMS_VERSION = "2026-11"
UA = "blacmusic-catalog-import/1.0"  # Cloudflare blocks the default Python user agent
PART_FALLBACK = 8 * 1024 * 1024


def norm(s):
    """Normalise a title for comparison: case, punctuation, feat. credits."""
    s = s.lower()
    s = re.sub(r"[\(\[]\s*(feat\.?|ft\.?|featuring)[^\)\]]*[\)\]]", "", s)
    s = re.sub(r"\b(feat\.?|ft\.?|featuring)\b.*$", "", s)
    s = re.sub(r"[^a-z0-9]+", " ", s)
    return s.strip()


class Api:
    def __init__(self, base, email, password):
        self.base = base.rstrip("/")
        self.email, self.password = email, password
        self.token = None

    def login(self):
        r = self.raw("POST", "/v1/auth/login", {"email": self.email, "password": self.password}, auth=False)
        self.token = r["accessToken"]

    def raw(self, method, path, body=None, auth=True, ok=(200, 201, 202, 204)):
        data = json.dumps(body).encode() if body is not None else None
        req = urllib.request.Request(self.base + path, data=data, method=method)
        req.add_header("User-Agent", UA)
        if body is not None:
            req.add_header("Content-Type", "application/json")
        if auth:
            req.add_header("Authorization", "Bearer " + self.token)
        try:
            with urllib.request.urlopen(req, timeout=120) as resp:
                raw = resp.read()
                return json.loads(raw) if raw else {}
        except urllib.error.HTTPError as e:
            raw = e.read()
            try:
                err = json.loads(raw)
            except Exception:
                err = {"raw": raw[:200].decode("utf-8", "replace")}
            raise ApiError(e.code, err, method, path)

    def call(self, method, path, body=None):
        try:
            return self.raw(method, path, body)
        except ApiError as e:
            if e.status == 401:
                self.login()
                return self.raw(method, path, body)
            raise


class ApiError(Exception):
    def __init__(self, status, body, method, path):
        super().__init__(f"{method} {path} -> {status} {json.dumps(body)[:300]}")
        self.status, self.body = status, body

    @property
    def code(self):
        return (self.body.get("error") or {}).get("code")


def put(url, data):
    req = urllib.request.Request(url, data=data, method="PUT")
    req.add_header("User-Agent", UA)
    req.add_header("Content-Length", str(len(data)))
    with urllib.request.urlopen(req, timeout=600) as resp:
        resp.read()


def sha256(path):
    h = hashlib.sha256()
    with open(path, "rb") as f:
        for chunk in iter(lambda: f.read(1 << 20), b""):
            h.update(chunk)
    return h.hexdigest()


def upload_image(api, path, purpose):
    size = os.path.getsize(path)
    img = api.call("POST", "/v1/images", {"purpose": purpose, "sizeBytes": size})
    with open(path, "rb") as f:
        put(img["upload"]["url"], f.read())
    api.call("POST", f"/v1/images/{img['id']}/complete")
    return img["id"]


def upload_track(api, release_id, track, path):
    size = os.path.getsize(path)
    body = {"releaseId": release_id, "title": track["title"], "explicit": bool(track.get("explicit", False)),
            "filename": os.path.basename(path), "sizeBytes": size,
            "rightsAccepted": True, "termsVersion": TERMS_VERSION}
    if track.get("genre"):
        body["genre"] = track["genre"]
    if track.get("trackNumber"):
        body["trackNumber"] = track["trackNumber"]
    up = api.call("POST", "/v1/uploads", body)
    target = up["upload"]
    if target["type"] == "single":
        with open(path, "rb") as f:
            put(target["url"], f.read())
    else:
        part = target.get("partSize", PART_FALLBACK)
        with open(path, "rb") as f:
            for p in target["parts"]:
                f.seek((p["number"] - 1) * part)
                put(p["url"], f.read(part))
    api.call("POST", f"/v1/uploads/{up['id']}/complete")
    for _ in range(300):
        st = api.call("GET", f"/v1/uploads/{up['id']}")
        if st["status"] == "ready":
            return st["trackId"]
        if st["status"] == "failed":
            raise RuntimeError(f"processing failed: {st.get('error')}")
        time.sleep(2)
    raise RuntimeError("timed out waiting for processing")


def validate(manifest, audio_dir):
    problems, titles, hashes = [], {}, {}
    for rel in manifest["releases"]:
        if rel["type"] not in ("single", "album"):
            problems.append(f"{rel['key']}: type must be single or album")
        if rel["type"] == "single" and len(rel["tracks"]) != 1:
            problems.append(f"{rel['key']}: a single has exactly one track")
        if not re.fullmatch(r"\d{4}-\d\d-\d\d", rel.get("releaseDate", "")):
            problems.append(f"{rel['key']}: releaseDate must be YYYY-MM-DD")
        for t in rel["tracks"]:
            n = norm(t["title"])
            if n in titles and not t.get("allowDuplicateTitle"):
                problems.append(f"duplicate track title '{t['title']}' in {rel['key']} (also in {titles[n]})")
            titles.setdefault(n, rel["key"])
            path = os.path.join(audio_dir, t["file"])
            if not os.path.isfile(path):
                continue
            h = sha256(path)
            if h in hashes:
                problems.append(f"'{t['title']}' has the same audio as '{hashes[h]}'")
            hashes.setdefault(h, t["title"])
    return problems


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--api", required=True)
    ap.add_argument("--manifest", required=True)
    ap.add_argument("--audio-dir", required=True, help="folder with the audio, cover and avatar files named in the manifest")
    ap.add_argument("--apply", action="store_true", help="do it (default: only show the plan)")
    ap.add_argument("--no-publish", action="store_true", help="leave releases as drafts")
    args = ap.parse_args()

    manifest = json.load(open(args.manifest, encoding="utf-8"))
    problems = validate(manifest, args.audio_dir)
    if problems:
        print("Manifest problems:")
        for p in problems:
            print("  -", p)
        sys.exit(1)

    email, password = os.environ.get("BLAC_EMAIL"), os.environ.get("BLAC_PASSWORD")
    if not email or not password:
        sys.exit("set BLAC_EMAIL and BLAC_PASSWORD (the account that owns the artist profile)")
    api = Api(args.api, email, password)
    api.login()

    art = manifest["artist"]
    try:
        artist = api.call("GET", "/v1/me/artist")
        print(f"artist exists: {artist['name']} ({artist['id']})")
    except ApiError as e:
        if e.code != "no_artist":
            raise
        artist = None
        print(f"artist to create: {art['name']}")
        if args.apply:
            artist = api.call("POST", "/v1/artists", {"name": art["name"], "bio": art.get("bio", "")})
            print(f"  created {artist['id']}")

    existing = {}
    if artist:
        for r in api.call("GET", "/v1/me/releases").get("releases", []):
            detail = api.call("GET", f"/v1/releases/{r['id']}")
            existing[(norm(detail["title"]), detail["type"])] = detail
    have_titles = {norm(t["title"]): d["title"] for d in existing.values() for t in d.get("tracks", [])}

    missing = []
    for rel in manifest["releases"]:
        cur = existing.get((norm(rel["title"]), rel["type"]))
        print(f"\n{rel['releaseDate']} {rel['type']:6} {rel['title']}" + ("  [exists: " + cur["visibility"] + "]" if cur else ""))
        for t in rel["tracks"]:
            path = os.path.join(args.audio_dir, t["file"])
            has_file = os.path.isfile(path)
            on_server = cur and any(norm(x["title"]) == norm(t["title"]) for x in cur.get("tracks", []))
            elsewhere = (not on_server) and norm(t["title"]) in have_titles and not t.get("allowDuplicateTitle")
            state = "on server" if on_server else ("DUPLICATE of '%s' in another release" % have_titles[norm(t["title"])] if elsewhere else ("ready to upload" if has_file else "MISSING FILE " + t["file"]))
            print(f"    - {t['title']}: {state}")
            if not has_file and not on_server:
                missing.append(t["file"])
    if not args.apply:
        print(f"\nDry run. {len(missing)} audio file(s) missing." if missing else "\nDry run: everything is in place. Add --apply to import.")
        return
    if missing:
        sys.exit("Refusing to import with missing audio: " + ", ".join(missing))

    if art.get("avatar") and not artist.get("imageUrl"):
        path = os.path.join(args.audio_dir, art["avatar"])
        if os.path.isfile(path):
            api.call("PATCH", "/v1/me", {"avatarImageId": upload_image(api, path, "avatar")})
            print("avatar set")

    for rel in manifest["releases"]:
        cur = existing.get((norm(rel["title"]), rel["type"]))
        if cur and cur["visibility"] == "public" and all(
                any(norm(x["title"]) == norm(t["title"]) for x in cur.get("tracks", [])) for t in rel["tracks"]):
            continue
        if not cur:
            body = {"type": rel["type"], "title": rel["title"], "description": rel.get("description", ""),
                    "genre": rel.get("genre", ""), "releaseDate": rel["releaseDate"]}
            cover = rel.get("cover")
            if cover and os.path.isfile(os.path.join(args.audio_dir, cover)):
                body["coverImageId"] = upload_image(api, os.path.join(args.audio_dir, cover), "cover")
            cur = api.call("POST", "/v1/releases", body)
            cur.setdefault("tracks", [])
            print(f"created release {rel['title']} ({cur['id']})")
        for i, t in enumerate(rel["tracks"], 1):
            if any(norm(x["title"]) == norm(t["title"]) for x in cur.get("tracks", [])):
                continue
            if norm(t["title"]) in have_titles and not t.get("allowDuplicateTitle"):
                print(f"  skipped duplicate track '{t['title']}'")
                continue
            t = dict(t, genre=t.get("genre", rel.get("genre", "")), trackNumber=t.get("trackNumber", i if rel["type"] == "album" else 0))
            tid = upload_track(api, cur["id"], t, os.path.join(args.audio_dir, t["file"]))
            have_titles[norm(t["title"])] = t["title"]
            print(f"  uploaded {t['title']} -> {tid}")
        if not args.no_publish:
            api.call("POST", f"/v1/releases/{cur['id']}/publish")
            print(f"  published {rel['title']}")
    print("\nDone.")


if __name__ == "__main__":
    try:
        main()
    except (ApiError, RuntimeError, OSError) as e:
        sys.exit(f"stopped: {e}\nFix the cause and run again: finished work is skipped.")
