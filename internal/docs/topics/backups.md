# Backups

*Every night, every database and DokWalt's own state go to Cloudflare R2 (or any S3 storage), off the server.*

The daemon runs the backup on the server, so it works while your laptop is
closed. Each night it makes one backup containing:

- **Every database of every Postgres service**, of every app and stage:
  `pg_dump --format=custom`, run inside the database container. A shared
  server (`x-dokwalt.uses`) gives one dump per app database.
- **The roles of each Postgres server** (`pg_dumpall --globals-only`), with
  their password hashes, so a fresh server can get the apps' roles back.
- **DokWalt's state** (`dokwalt.db`): apps, domains, releases, config. Config
  values stay encrypted with `secret.key`, which is **not** uploaded: keep a
  copy of it somewhere safe, apart from the bucket.

Other stateful services (Redis, MySQL, uploaded files in volumes…) aren't
included: see `dokwalt docs databases`.

## 1. Create a bucket and a token on Cloudflare

1. In the Cloudflare dashboard → **R2** → **Create bucket**, e.g. `backups`.
   Keep the default location and storage class.
2. **R2** → **Manage API tokens** → **Create API token**:
   - Permissions: **Object Read & Write**.
   - Apply to: **specific bucket only** → `backups`.
3. Copy the **Access Key ID**, the **Secret Access Key** and your
   **Account ID** (also on the R2 overview page). The secret is shown once.

R2's free tier covers 10 GB of storage, and downloads are free.

## 2. Set up DokWalt

```text
$ dokwalt backup:setup --r2-account 1a2b3c4d… --bucket backups --access-key 7f8e…
? Secret access key: ••••••••
✓ Backups set up on prod

Storage     backups/dokwalt/prod  https://1a2b3c4d….r2.cloudflarestorage.com
Schedule    every day at 03:00 CEST · next Sun 11 Oct 03:00
Retention   the newest backup of the last 7 days and 4 weeks
Last run    never

  Make the first one now: dokwalt backup:now
```

- Setup checks the bucket by writing, listing and deleting a test object,
  and refuses settings that don't work.
- The secret key is asked at a prompt, or read from `--secret-key-stdin` or
  `DOKWALT_BACKUP_SECRET_KEY`, never from a flag (shell history). It's stored
  encrypted on the server, like config values, and never shown again.
- `--time 04:30` changes the time (server local time), `--keep-daily` and
  `--keep-weekly` the retention, `--prefix` the folder in the bucket
  (default `dokwalt/<server hostname>`, so several servers can share a
  bucket).
- Run `backup:setup` again to change a setting: flags you leave out keep
  their value. A new `--access-key` asks for its secret.
- Other S3 providers: `--endpoint https://… --region …` instead of
  `--r2-account` (AWS S3, Backblaze B2, MinIO…).
- `dokwalt backup:disable` stops the nightly run and forgets the settings.
  Backups already in the bucket stay.

## Schedule, failures and retention

- **Once a day**, at the set time. A server that was off at that time
  catches up when it comes back, the same day. A failed run isn't retried:
  you get a **"Backup failed" alert** (see `dokwalt docs alerts`), and a
  "resolved" one after the next good backup. `dokwalt backup:now` retries
  right away.
- **Incomplete backups.** When one database can't be dumped (a stopped
  service, an error), the others are still uploaded, and the backup is
  marked incomplete. Stopped stages (`dokwalt stop`) are skipped on purpose.
- **Retention** runs after each complete backup: DokWalt keeps the newest
  backup of each of the last 7 days that have one, and of each of the last 4
  weeks (the two overlap), and deletes the others. An incomplete backup
  never deletes anything, so a run of bad nights can't remove your good
  backups. Interrupted uploads are removed after a day.
- **One dump at a time** is written to the server's disk
  (`/var/lib/dokwalt/backup-tmp`) and deleted once uploaded, so the server
  needs free space for its largest database, not for all of them.

## See what's there

```text
$ dokwalt backups
…
ID                 STARTED           TRIGGER    SIZE     FILES  STATUS
20261011T010000Z   Sun 11 Oct 03:00  schedule   18.2 MB  6      complete
20261010T143012Z   Sat 10 Oct 16:30  manual     18.1 MB  6      complete

$ dokwalt backups latest
FILE                                 CONTENT            FROM                    SIZE
dokwalt.db                           DokWalt state                              412 kB
postgres-production-db/globals.sql   roles              postgres/production db  2.1 kB
postgres-production-db/blog.dump     database blog      postgres/production db  9.8 MB
…
```

IDs are the start time in UTC. `latest` means the newest backup.
`dokwalt server info` shows the last run too.

## Download

```bash
dokwalt backup:download latest                                     # the whole backup, in <host>-<id>/
dokwalt backup:download latest postgres-production-db/blog.dump    # one file, here
```

Files come through the server (the bucket's credentials stay there) and are
checked against the checksum recorded at backup time. A dump restores
anywhere with `pg_restore`.

## Restore a database

```text
$ dokwalt backup:restore latest --database blog
! This replaces database blog in postgres/production (service db) with its copy from 20261011T010000Z (Sun 11 Oct 03:00).
  Type blog to confirm: blog
✓ Restored postgres-production-db/blog.dump from backup 20261011T010000Z
```

- The server downloads the dump, checks its checksum, and runs `pg_restore`
  in the service it came from, as that server's superuser: objects are
  replaced (`--clean --if-exists`) and get back their original owner. A
  database that no longer exists is created.
- The app keeps running during the restore. If it writes a lot, stop it
  first (`dokwalt stop -a blog`, then `dokwalt start -a blog`).
- Pass a file path instead of `--database` when several services have a
  database with the same name. `--confirm <database>` skips the question
  (scripts).

## Rebuild a lost server

1. Prepare the new server and run `dokwalt server init` (`dokwalt docs
   server`).
2. Point the new server at the old backups:
   `dokwalt backup:setup … --prefix dokwalt/<old hostname>`, then
   `dokwalt backup:download latest dokwalt.db` (or download it from the R2
   dashboard).
3. Restore DokWalt's state: copy `dokwalt.db` and your copy of `secret.key`
   to the server, then `sudo systemctl stop dokwalt`, put both in
   `/var/lib/dokwalt/` (owned by root, mode 0600; delete any
   `dokwalt.db-wal` and `dokwalt.db-shm` there), and
   `sudo systemctl start dokwalt`. Every app, domain and config var is back,
   and so are the backup settings.
4. `dokwalt deploy` from each app folder (images aren't in the backup),
   providers like the shared Postgres first.
5. For each Postgres server: `dokwalt backup:restore <id> <dir>/globals.sql`
   (roles first), then each database with `--database`.

See also: `dokwalt docs databases`, `dokwalt docs alerts`, `dokwalt docs security`
