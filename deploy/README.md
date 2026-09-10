# Deployment

Everything here targets one VPS running the app behind Nginx, per spec §7.4 and §7.5.

## First time

1. **DNS.** Point `cloud.faidz.fun` at the VPS. Separately, `cdn.faidz.fun` is a
   *proxied* CNAME to the B2 host — that orange cloud is what activates the
   Bandwidth Alliance and makes storage egress free (§2.5).

2. **Certificate.**

   ```bash
   sudo certbot certonly --webroot -w /var/www/certbot -d cloud.faidz.fun
   ```

3. **Credentials.** On the server, never from a developer machine:

   ```bash
   sudo install -d -m 700 /etc/cloudapp
   sudo cp backend/.env.example /etc/cloudapp/env
   sudo chmod 600 /etc/cloudapp/env
   sudo nano /etc/cloudapp/env
   ```

   Both B2 keys go in here. The upload key must be the `writeFiles`-only,
   prefix-restricted one — the server refuses to hand a browser anything else
   (§2.3), and `/api/health` says so if it is wrong.

4. **ffmpeg**, for the tier 3 thumbnail worker (§5.4):

   ```bash
   sudo apt install ffmpeg
   ```

   Optional. Without it, the rare files no browser can decode keep a
   placeholder instead of getting a server-side thumbnail; nothing else changes.

5. **Deploy and create the account.**

   ```bash
   ./deploy/deploy.sh faidz@43.157.230.218
   ssh faidz@43.157.230.218 'sudo -u cloudapp /opt/cloudapp/cloudapp createuser --username faidz'
   ```

   There is no registration endpoint (D7), so this is the only way an account
   comes into existence.

## Every time after

```bash
./deploy/deploy.sh faidz@43.157.230.218
```

Cross-compiles the binary here and ships it. No Go toolchain, no Node and no
cgo on the server — that is what the pure-Go SQLite driver buys (D6).

## Checking on it

```bash
curl -s https://cloud.faidz.fun/api/health | jq
journalctl -u cloudapp -f
```

Health reports honestly rather than a flat `ok`: `"b2": {"state": "error"}` with
a reason when storage is misconfigured, and it names an over-privileged upload
key rather than letting that pass silently.

## Background jobs

They run inside the server process, not from cron, so they share its
configuration and its log. Each decides for itself whether it is due, so a
restart at 03:05 still runs that night's work.

| Job | Interval | What it does |
|---|---|---|
| Thumbnails | 2 min | One ffmpeg extraction for a file the browser could not decode |
| Orphans | hourly | Sweeps uploads abandoned over 24h ago — B2 bills unfinished parts |
| Tokens | hourly | Renews the 7-day prefix download authorization before it lapses |
| Purge | daily | Deletes storage for files trashed over 30 days ago |
| Backup | daily | `VACUUM INTO` a copy, uploads it, keeps 14 |
| Reconcile | weekly | Reports drift between `storage_used` and the rows |

**The purge is the one that matters financially.** A trashed row whose storage
object is never deleted is billed forever, invisibly.

## Recovery

Two independent layers (§7.3).

**Lost database, backups intact** — the ordinary case:

```bash
sudo systemctl stop cloudapp
# fetch users/1/db/cloud-YYYYMMDD.sqlite from the bucket
sudo -u cloudapp cp cloud-20260910.sqlite /var/lib/cloudapp/cloud.db
sudo systemctl start cloudapp
```

**Lost database and every backup** — rebuild from the per-file sidecars:

```bash
sudo -u cloudapp /opt/cloudapp/cloudapp createuser --username faidz
sudo -u cloudapp /opt/cloudapp/cloudapp reindex --from-b2 --username faidz --dry-run
sudo -u cloudapp /opt/cloudapp/cloudapp reindex --from-b2 --username faidz
```

Filenames, sizes, dates and dimensions come back. Folders do not — sidecars do
not record them — so everything lands in the root, and thumbnails are marked
unsupported so the worker regenerates them.

Run the dry run at least once against the real bucket. An untested recovery
procedure is not a recovery procedure.

## Local development

`tools/fakeb2` stands in for storage, so the whole app runs with no Backblaze
account and no spend. See `tools/README.md`.
