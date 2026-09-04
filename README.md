# DumpKeeper

Веб-панель для резервного копирования PostgreSQL: бэкапы `pg_dump` по cron-расписанию, хранение локально и в S3-совместимых хранилищах, восстановление через `pg_restore`, retention «держать последние N».

## Возможности

- **Задания (jobs)** — каждое задание target'ит один профиль подключения к PostgreSQL. Расписание — стандартный cron (5 полей, `robfig/cron`); плюс кнопка ручного запуска.
- **Хранение** — локально в `DATA_DIR/backups` и/или несколько S3-совместимых назначений (MinIO, AWS S3, …). Объекты кладутся как `{prefix}/{filename}`.
- **Retention** — после каждого успешного запуска удаляются завершённые бэкапы сверх `keep_last` (и локальные файлы, и объекты во всех S3, где они лежат). `keep_last = 0` — без ограничений.
- **Восстановление** — `pg_restore --clean --if-exists --no-owner --no-privileges --exit-on-error` в базу из профиля задания. Берётся локальная копия, при её отсутствии — S3-назначения по порядку.
- **История запусков** — статус (`running` / `completed` / `failed`), размер, триггер (`manual`/`cron`), хвост stderr при ошибке, скачивание файла.
- **Авторизация** — один пользователь (логин/пароль из env), сессии в cookie + CSRF.
- **Метаданные** — SQLite (чисто-Go драйвер, без CGO). Сами дампы в SQLite не хранятся, только файлы.
- **Мягкая остановка** — HTTP → cron → ожидание идущих `pg_dump` (обрыв дампа грозит битым файлом).

Имя файла дампа: `{job}-{YYYYMMDDTHHMMSSZ}.dump`, формат `pg_dump --format=custom`.

Частичный успех — не провал: если локальная копия или хотя бы одно S3-назначение сработали, запуск считается `completed`, ошибки отдельных назначений видны в `backups.error`. Провалом считается только «не сохранилось нигде».

## Быстрый старт (dev-стек из репозитория)

```bash
docker compose up -d --build
```

Поднимаются DumpKeeper + одноразовый PostgreSQL 17 + MinIO:

| Что | Где | Доступы |
|---|---|---|
| UI | http://127.0.0.1:18080 | `admin` / `admin123` |
| PostgreSQL (с хоста) | `127.0.0.1:15432` | `postgres` / `pgpass` |
| PostgreSQL (в формах UI) | host `postgres`, port `5432` | — |
| MinIO | http://127.0.0.1:9000 | `minio` / `minio12345`, бакет `dk-backups` создан автоматически |
| S3-назначение (в форме UI) | endpoint `minio:9000`, HTTPS выключен | `minio` / `minio12345` |

Тестовые данные:

```bash
docker compose exec postgres psql -U postgres -c \
  'CREATE TABLE demo AS SELECT generate_series(1,10) i'
```

## Подключение в свой docker-compose

Важно: `pg_dump` выполняется **внутри контейнера DumpKeeper**, поэтому база должна быть доступна по сети из него. Указывайте сервисное имя и внутренний порт (например `postgres:5432`), а не проброшенный на хост.

Вариант 1 — PostgreSQL в том же compose-проекте (сеть общая по умолчанию):

```yaml
services:
  dumpkeeper:
    build: /path/to/DumpKeeper   # либо image: dumpkeeper (см. ниже)
    restart: unless-stopped
    environment:
      AUTH_LOGIN: admin
      AUTH_PASSWORD: change-me   # только эти две переменные обязательны
    ports:
      - "8080:8080"
    volumes:
      - dumpkeeper-data:/data    # метаданные SQLite + локальные бэкапы
    # Если Postgres в этом же файле — depends_on с healthcheck, как в
    # docker-compose.yml репозитория, необязателен, но полезен.

volumes:
  dumpkeeper-data:
```

В базе DumpKeeper (в UI) подключение к Postgres задаётся так:

| Поле | Значение |
|---|---|
| Host | `postgres` (имя сервиса) |
| Port | `5432` (внутренний порт контейнера, не хостовой) |
| Username / Password / DB name | ваши |

Вариант 2 — база крутится в другом compose-проекте: подключите оба проекта к одной внешней сети.

```yaml
services:
  dumpkeeper:
    networks: [dbnet]

networks:
  dbnet:
    external: true   # второму проекту — та же сеть, host = имя сервиса той БД
```

Вариант 3 — PostgreSQL установлен на хосте: используйте `host.docker.internal` (на Linux добавьте `extra_hosts`).

```yaml
services:
  dumpkeeper:
    extra_hosts:
      - "host.docker.internal:host-gateway"
```

и в профиле базы Host = `host.docker.internal`, Port = порт PostgreSQL на хосте.

Образ собирается из этого репозитория. Чтобы не тянуть исходники в чужой compose-файл, соберите образ заранее и используйте его:

```bash
docker build -t dumpkeeper /path/to/DumpKeeper
```

```yaml
services:
  dumpkeeper:
    image: dumpkeeper
```

Для MinIO/S3-назначения в UI указывайте endpoint, доступный из контейнера (`minio:9000`, а не `127.0.0.1:9000`), при необходимости выключите HTTPS.

## Переменные окружения

| Переменная | Обязательна | По умолчанию | Описание |
|---|---|---|---|
| `AUTH_LOGIN` | да | — | Логин единственного пользователя UI |
| `AUTH_PASSWORD` | да | — | Его пароль |
| `LISTEN_ADDR` | нет | `:8080` | Адрес HTTP-сервера |
| `DATA_DIR` | нет | `/data` | Каталог метаданных и локальных бэкапов |

## Структура данных

```
$DATA_DIR/
├── dumpkeeper.db      # SQLite: базы, назначения, задания, история запусков, сессии
└── backups/           # локальные копии дампов (*.dump, custom format)
```

Схема SQLite применяется при старте автоматически; мигрирует раскладку до-2.0 (задания со встроенными кредами и глобальным S3) на текущую.

## Сборка и запуск без Docker

Нужны Go 1.25+ и `pg_dump`/`pg_restore` (`postgresql-client`) в `PATH`:

```bash
go build ./cmd/dumpkeeper
AUTH_LOGIN=admin AUTH_PASSWORD=admin123 DATA_DIR=./data LISTEN_ADDR=:8080 ./dumpkeeper
```

## Замечания по эксплуатации

- Версия клиента `pg_dump` в образе должна быть **не старше** мажорной версии сервера. Образ (alpine 3.22) несёт клиент PostgreSQL 17 — для серверов 18+ пересоберите образ на более свежем alpine.
- Пароли к PostgreSQL передаются через `PGPASSWORD`/`PGSSLMODE`, не через argv; креды S3 хранятся в SQLite в открытом виде — ограничивайте доступ к `DATA_DIR`.
- Порт 8080 отдаёт только UI и healthcheck (`/login`); наружу имеет смысл публиковать за reverse proxy с HTTPS.
