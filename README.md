# DumpKeeper

A web UI for PostgreSQL backups. Run backups manually or on a cron schedule, store them locally or in S3-compatible storage, and restore them from the browser.

- Manage database connections, backup jobs, and multiple storage destinations.
- Keep the latest N backups per job, or keep everything.
- View execution history, download dumps, and track restore progress.
- Restore a saved backup or upload a SQL dump.
- Monitor database availability, latency, and downtime.

## Try it locally

From this repository, run:

```bash
docker compose up -d --build
```

Open **http://127.0.0.1:18080/dk/** and sign in with **`admin` / `admin123`**.

The included stack runs DumpKeeper, PostgreSQL 17, and MinIO. It is for local testing, not production.

Use these values in DumpKeeper:

| Connection | Settings |
|---|---|
| PostgreSQL | Host `postgres`, port `5432`, database `postgres`, user `postgres`, password `pgpass` |
| S3 destination | Endpoint `minio:9000`, HTTPS off, access key `minio`, secret key `minio12345`, bucket `dk-backups` |

The bucket is created automatically. From the host, PostgreSQL is available at `127.0.0.1:15432` and the MinIO S3 API at `http://127.0.0.1:9000`.

To add sample data:

```bash
docker compose exec postgres psql -U postgres -c \
  'CREATE TABLE demo AS SELECT generate_series(1,10) i'
```

## Run with Docker Compose

Add this service and volume to your Compose file:

```yaml
services:
  dumpkeeper:
    image: ghcr.io/infybtw/dumpkeeper:latest
    restart: unless-stopped
    environment:
      AUTH_LOGIN: admin
      AUTH_PASSWORD: change-me
    ports:
      - "127.0.0.1:8080:8080"
    volumes:
      - dumpkeeper-data:/data

volumes:
  dumpkeeper-data:
```

Set a strong password, start the service, and open **http://127.0.0.1:8080**. For remote access, use a reverse proxy with TLS.

**Database and S3 addresses must be reachable from the DumpKeeper container.** For services on the same Compose network, use service names and internal ports, such as `postgres:5432` or `minio:9000`. Do not use `localhost` or host-published ports for those services.

## Set up backups

1. Add a connection profile under **Databases**.
2. If you use S3, add it under **Destinations**.
3. Create a job under **Jobs**: choose the database, storage destinations, schedule, and **Keep last** count.

Schedules use five-field cron syntax. For example, `0 2 * * *` runs daily at 02:00. Leave the schedule empty for manual backups only. Set **Keep last** to `0` to disable retention.

Backups are plain-text files named `{job}-{YYYYMMDDTHHMMSSZ}.sql`. DumpKeeper runs `pg_dump` with `--clean --if-exists --no-owner --no-privileges`: dumps contain statements to drop and recreate objects, but omit ownership and access rights.

A run is marked `completed` if at least one local or S3 copy is saved, even if other destinations fail. Destination errors are recorded with the execution. After each completed run, retention removes older completed backups from local storage and all S3 destinations that hold them.

## Restore a database

**Restores can destroy existing data. Check the target database before starting.**

To restore a saved job backup, use **Executions**. It restores into the database configured for that job, using the local copy first and S3 if needed. SQL dumps run through `psql`, which stops on the first error. Older custom-format `.dump` backups use `pg_restore`.

To upload a plain-text `.sql` dump, open **Restore** and select a database profile. The options are:

| Option | Default | Behavior |
|---|---|---|
| Back up the current database first | On | Saves a local safety dump before restoring. If the backup fails, the restore is skipped. |
| Create the database if it does not exist | Off | Creates the missing database from `template0`; skips the safety dump. |
| Clear database before restore | On | Disconnects clients, drops the target database, and recreates it from `template0`. |
| Keep access rights | Off | Preserves ownership and GRANT/REVOKE statements. Referenced roles must already exist. |

The upload and any safety dump are saved locally and listed in **Executions**. Restores run in the background, with progress and results shown there.

## Availability monitoring

DumpKeeper checks each database with `psql SELECT 1` every 15 minutes by default. Change the interval in **Settings**, or set it to `0` to disable checks.

**Availability** shows status, latency, and downtime history. **Dashboard** shows a summary, uptime over the last 24 hours, and recent executions.

## Configuration

| Environment variable | Default | Purpose |
|---|---|---|
| `AUTH_LOGIN` | Required | Username for the single UI account |
| `AUTH_PASSWORD` | Required | Password for that account |
| `LISTEN_ADDR` | `:8080` | HTTP listen address |
| `DATA_DIR` | `/data` | Metadata and local backup directory |
| `BASE_PATH` | Empty | URL prefix, such as `/dumpkeeper` |

With `BASE_PATH=/dumpkeeper`, the UI is served at `/dumpkeeper/` and `/` redirects there. Use this when a reverse proxy routes by path.

### Stored data

```text
$DATA_DIR/
├── dumpkeeper.db   # SQLite: configuration, history, and sessions
└── backups/        # Local dump files
```

Database setup and migrations run automatically on startup. Keep `/data` on a persistent volume.

**SQLite contains saved PostgreSQL passwords, S3 credentials, and session tokens in plain text. Restrict access to the data directory and configuration backups.**

### Back up and restore configuration

In **Settings → Configuration backup**, click **Download configuration backup**. This creates a consistent SQLite snapshot without stopping DumpKeeper.

The snapshot includes database profiles, destinations, jobs, settings, execution and availability history, and sessions. It does **not** include dump files or environment variables such as `AUTH_LOGIN` and `AUTH_PASSWORD`.

To restore it:

1. Stop DumpKeeper.
2. Move the existing `DATA_DIR/dumpkeeper.db` and any `dumpkeeper.db-wal` / `dumpkeeper.db-shm` files to a safe location together. Do not leave old sidecar files beside the restored database.
3. Copy the snapshot to `DATA_DIR/dumpkeeper.db` and make sure DumpKeeper can read and write it.
4. Restore local dumps to `DATA_DIR/backups` separately if needed, and set the required environment variables.
5. Start DumpKeeper. Restored schedules become active immediately; history entries still need their referenced local or S3 dump files.

## Run without Docker

Requires Go 1.25+ and PostgreSQL client tools (`pg_dump`, `psql`, `createdb`, and `pg_restore`) in `PATH`.

```bash
go build ./cmd/dumpkeeper
AUTH_LOGIN=admin AUTH_PASSWORD=change-me DATA_DIR=./data LISTEN_ADDR=127.0.0.1:8080 ./dumpkeeper
```

Use a strong password instead of `change-me`.

The `pg_dump` major version must be at least as new as the PostgreSQL server. The Dockerfile uses Alpine 3.22 with PostgreSQL 17 client tools; for PostgreSQL 18 or newer, rebuild with a base image that provides a matching or newer client.
