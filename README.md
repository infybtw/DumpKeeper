# DumpKeeper

A web panel for PostgreSQL backups: `pg_dump` runs on a cron schedule, results go to local storage and/or S3-compatible stores, restore via `psql`, keep-last-N retention.

## Features
- **Dashboard** — the main page summarizes databases by availability, jobs by activity, executions by status, and restorations as donut cards, plus per-database uptime over the last 24h and the latest executions. The jobs list lives on its own **Jobs** tab.
- **Availability monitoring** — DumpKeeper probes every configured database with `psql SELECT 1` on an interval set in **Settings** (default: every minute, `0` disables it). The **Availability** tab shows the current status and latency per database plus a downtime history: every period from the first failed probe to the first success after it, with duration and the last error.
- **Storage** — locally in `DATA_DIR/backups` and/or several S3-compatible destinations (MinIO, AWS S3, …). Objects are stored as `{prefix}/{filename}`.
- **Retention** — after every successful run, completed backups beyond `keep_last` are pruned (local files and objects in every S3 destination holding them). `keep_last = 0` means unlimited.
- **Restore** — the stored `.sql` dump is replayed with `psql --set=ON_ERROR_STOP=1` into the database from the job's profile. Dumps are produced with `--clean --if-exists`, so restore drops and recreates existing objects. Prefers the local copy; otherwise stored S3 destinations are tried in order. Restores run in the background — the executions row shows a live progress bar (phase and %) and the outcome.
- **Manual restore page** — upload a plain-text `.sql` on the **Restore** tab and replay it into any database profile. Options: an automatic pre-restore dump of the target, create-if-missing (from `template0`), **Clear database before restore** (on by default — drops the existing target, force-closing connections, and recreates it empty from `template0`, so re-imports of dumps without `DROP` statements never hit `already exists`), and **Keep access rights** (replays `OWNER TO`/GRANT/REVOKE as-is; by default they are dropped — the plain-text equivalent of `--no-owner --no-privileges`). Imports and safety dumps are recorded in Executions with triggers `import` / `pre-restore`.
- **Execution history** — status (`running` / `completed` / `failed`), size, trigger (`manual`/`cron`), stderr tail on failure, file download.
- **Auth** — a single user (login/password from env), session cookies + CSRF.
- **Metadata** — SQLite (pure-Go driver, no CGO). Dumps themselves are not stored in SQLite, only referenced as files.
- **Graceful shutdown** — HTTP → cron → wait for in-flight `pg_dump` runs (killing a dump mid-write risks a corrupt file).

Dump file name: `{job}-{YYYYMMDDTHHMMSSZ}.sql`, plain-text format (`pg_dump --format=plain --clean --if-exists --no-owner --no-privileges`). Backups created before this change keep the `{job}-*.dump` custom format and still restore, via `pg_restore`.

Partial success is not a failure: if the local copy or at least one S3 destination succeeded, the run counts as `completed`, with individual destination failures visible in `backups.error`. Only "stored nowhere" counts as `failed`.

## Quick start (dev stack from this repo)

```bash
docker compose up -d --build
```

Starts DumpKeeper + a throwaway PostgreSQL 17 + MinIO:

| What | Where | Credentials |
|---|---|---|
| UI | http://127.0.0.1:18080 | `admin` / `admin123` |
| PostgreSQL (from the host) | `127.0.0.1:15432` | `postgres` / `pgpass` |
| PostgreSQL (in UI forms) | host `postgres`, port `5432` | — |
| MinIO | http://127.0.0.1:9000 | `minio` / `minio12345`, bucket `dk-backups` is created automatically |
| S3 destination (in UI form) | endpoint `minio:9000`, HTTPS off | `minio` / `minio12345` |

Seed test data:

```bash
docker compose exec postgres psql -U postgres -c \
  'CREATE TABLE demo AS SELECT generate_series(1,10) i'
```

## Adding to your own docker-compose

Important: `pg_dump` runs **inside the DumpKeeper container**, so the database must be reachable from it over the network. Use the service name and the internal port (e.g. `postgres:5432`), not the host-published one.

Variant 1 — PostgreSQL in the same compose project (default network is shared):

```yaml
services:
  dumpkeeper:
    build: /path/to/DumpKeeper   # or image: dumpkeeper (see below)
    restart: unless-stopped
    environment:
      AUTH_LOGIN: admin
      AUTH_PASSWORD: change-me   # the only two required variables
    ports:
      - "8080:8080"
    volumes:
      - dumpkeeper-data:/data    # SQLite metadata + local backups
    # If Postgres is in the same file, depends_on with a healthcheck (as in
    # this repo's docker-compose.yml) is optional but useful.

volumes:
  dumpkeeper-data:
```

The PostgreSQL connection in DumpKeeper (via the UI) is then:

| Field | Value |
|---|---|
| Host | `postgres` (the service name) |
| Port | `5432` (the container-internal port, not the host-published one) |
| Username / Password / DB name | yours |

Variant 2 — the database runs in another compose project: attach both projects to one external network.

```yaml
services:
  dumpkeeper:
    networks: [dbnet]

networks:
  dbnet:
    external: true   # the second project uses the same network; host = that DB's service name
```

Variant 3 — PostgreSQL installed on the host: use `host.docker.internal` (on Linux add `extra_hosts`).

```yaml
services:
  dumpkeeper:
    extra_hosts:
      - "host.docker.internal:host-gateway"
```

and in the database profile set Host = `host.docker.internal`, Port = the host's PostgreSQL port.

The image is built from this repo. To avoid dragging the sources into someone else's compose file, build the image once and reference it:

```bash
docker build -t dumpkeeper /path/to/DumpKeeper
```

```yaml
services:
  dumpkeeper:
    image: dumpkeeper
```

For a MinIO/S3 destination in the UI, use an endpoint reachable from the container (`minio:9000`, not `127.0.0.1:9000`) and turn HTTPS off if needed.

## Environment variables

| Variable | Required | Default | Description |
|---|---|---|---|
| `AUTH_LOGIN` | yes | — | Login of the single UI user |
| `AUTH_PASSWORD` | yes | — | Their password |
| `LISTEN_ADDR` | no | `:8080` | HTTP server address |
| `DATA_DIR` | no | `/data` | Directory for metadata and local backups |

## Data layout

```
$DATA_DIR/
├── dumpkeeper.db      # SQLite: databases, destinations, jobs, execution history, sessions, availability monitoring, settings
└── backups/           # local dump copies (*.sql, plain text; legacy *.dump)
```

The SQLite schema is applied on startup automatically; it also migrates the pre-2.0 layout (jobs with embedded credentials and a single global S3 setting) to the current one.

## Building and running without Docker
Requires Go 1.25+ and `pg_dump`/`psql`/`createdb` (`postgresql-client`) in `PATH`:

```bash
go build ./cmd/dumpkeeper
AUTH_LOGIN=admin AUTH_PASSWORD=admin123 DATA_DIR=./data LISTEN_ADDR=:8080 ./dumpkeeper
```

## Operational notes

- The `pg_dump` client version in the image must be **no older than** the server's major version. The image (alpine 3.22) ships the PostgreSQL 17 client — for 18+ servers, rebuild the image on a newer alpine.
- PostgreSQL passwords are passed via `PGPASSWORD`/`PGSSLMODE`, not argv; S3 credentials are stored in SQLite in plain text — restrict access to `DATA_DIR`.
- Port 8080 serves only the UI and the healthcheck (`/login`); expose it publicly behind a reverse proxy with TLS.
