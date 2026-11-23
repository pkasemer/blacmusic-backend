# Deploying Blac Music to a server

This is the setup used for the first server (Ubuntu 22.04, 1 core, 2 GB RAM, shared with other
websites behind the same nginx). Everything here only **adds** to the machine: it never edits another
site's nginx config and never restarts a running service.

```
Cloudflare ──> nginx :443 ──> api      127.0.0.1:8080  (blacmusic.pkasemer.com)
               nginx :443 ──> storage  127.0.0.1:9000  (media.blacmusic.pkasemer.com, presigned uploads/downloads)
                                 docker compose: postgres, redis, storage (RustFS), api, worker (ffmpeg)
```

## One-time setup

1. **DNS** (Cloudflare): `blacmusic` and `media.blacmusic` as A records to the server.
   - `blacmusic` can be proxied (orange cloud).
   - `media.blacmusic` must be **DNS only** (grey cloud). Cloudflare's free certificate covers only one
     level (`*.pkasemer.com`), not `media.blacmusic.pkasemer.com`, and large audio is better served
     directly. Alternatively use a one-level name such as `blacmusic-media.pkasemer.com`
     (then change `S3_PUBLIC_ENDPOINT` and the nginx `server_name`).
   - SSL/TLS mode: **Full** or **Full (strict)**, never Flexible.
2. **Prepare the server** (as root; adds user `pk`, Docker, `/opt/blacmusic`):
   ```bash
   ssh root@HOST 'bash -s' -- "$(cat ~/.ssh/blac_vps.pub)" < deploy/provision.sh
   ```
   `pk` has no password and logs in with the key only. Set a password yourself if you want one for
   `sudo`: `sudo passwd pk`.
3. **nginx and certificates** (as root):
   ```bash
   scp deploy/nginx/blacmusic.conf root@HOST:/tmp/blacmusic.conf
   ssh root@HOST 'bash -s' < deploy/nginx-setup.sh
   ssh root@HOST 'certbot certonly --webroot -w /var/www/certbot -d blacmusic.pkasemer.com \
       -d media.blacmusic.pkasemer.com --cert-name blacmusic --deploy-hook "systemctl reload nginx"'
   ssh root@HOST 'ln -sfn /etc/letsencrypt/live/blacmusic/fullchain.pem /etc/nginx/blacmusic/fullchain.pem; \
       ln -sfn /etc/letsencrypt/live/blacmusic/privkey.pem /etc/nginx/blacmusic/privkey.pem; \
       nginx -t && systemctl reload nginx'
   ```
   Certbot renews the certificate by itself (the server's existing certbot timer handles it).

## Every deploy

```bash
deploy/ship.sh pk@HOST ~/.ssh/blac_vps
```
Builds the images on your machine, copies them over, writes `/opt/blacmusic/.env` with fresh random
secrets the first time, and restarts the stack. Database and files live in Docker volumes and survive.

## Settings in `/opt/blacmusic/.env`
See `deploy/env.example`. Notable:
- `APP_ENV=development` until a mail provider exists: verification codes are then written to the API log
  (`docker compose -f docker-compose.prod.yml logs api`) instead of emailed. Set `SMTP_*` and
  `APP_ENV=production` for real use (the API refuses to start in production without SMTP).
- `FREE_STORAGE_GB`: free space per artist. Keep it small while the disk is shared and small.
- `TRUST_PROXY=true` (set in the compose file): the API trusts `X-Real-IP` because nginx overwrites it and
  the API port is bound to 127.0.0.1. nginx takes the visitor's address from `CF-Connecting-IP` only when the
  connection comes from a Cloudflare range (refreshed by `nginx-setup.sh`).

## Operating it
```bash
cd /opt/blacmusic
docker compose -f docker-compose.prod.yml ps
docker compose -f docker-compose.prod.yml logs -f api worker
docker stats --no-stream
```
Not set up yet (worth doing before real users): backups of the `pgdata` and `storagedata` volumes, an
uptime check on `https://blacmusic.pkasemer.com/readyz`, and disk monitoring.
