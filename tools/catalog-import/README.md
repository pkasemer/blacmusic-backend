# Catalog import

Creates an artist profile and publishes their releases from a manifest plus audio files, for catalogs the rights
holder has authorised us to host. It uses the public API as the account that owns the artist, so verified email,
quota, audio validation, transcoding and the rights acceptance all apply. Python 3 standard library only.

## Run

1. Register the account that will own the artist and verify its email (one account per artist profile).
2. Put the audio, cover art and avatar named in the manifest into one folder (WAV/FLAC masters are best; files over
   16 MiB use resumable multipart uploads automatically).
3. Dry run (changes nothing, shows what is ready, missing or already on the server):

```powershell
$env:BLAC_EMAIL = "owner@example.com"; $env:BLAC_PASSWORD = "..."
python tools/catalog-import/import_catalog.py --api https://blacmusic.pkasemer.com --manifest tools/catalog-import/akeine.json --audio-dir C:\path\to\akeine-audio
```

4. Add `--apply` to import. `--no-publish` leaves releases as drafts for review.

## Duplicates

A track listed twice, two tracks with identical audio (SHA-256), or a title that already exists in another release
of the artist stop the run or are skipped. Re-running skips what is already on the server, so a failed run resumes
where it stopped.

## Manifest

`artist {name, bio, avatar}`, then `releases[] {key, type: single|album, title, genre (from GET /v1/genres),
releaseDate YYYY-MM-DD, cover, tracks[] {title, file, explicit?, allowDuplicateTitle?}}`. Other keys (`youtube`,
`note`, `excluded`) are documentation only: `youtube` lists the channel videos that were folded into each song.
`akeine.json` was built from titles and upload dates on Akeine's channel; dates and genres are approximate.
