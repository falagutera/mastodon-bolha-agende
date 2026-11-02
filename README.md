# agende

Schedule posts on one Mastodon instance. People sign in with OAuth, write a post and pick a date. The instance stores and publishes it, so scheduled posts go out even if this app is down.

- Full composer: Markdown, media with descriptions, content warnings, visibility, language, replies
- Queue, weekly and monthly calendar, change date, cancel, duplicate
- Repeat daily, weekly or monthly: creates up to 24 regular scheduled posts at once
- Optional self-destruct and instance-only posts, for forks that support them
- Audit log of every change to a scheduled post, recorded from day one

## Install

1. Register the OAuth application:

   ```sh
   curl -F client_name=agende \
     -F "scopes=profile read:statuses write:statuses write:media" \
     -F redirect_uris=https://agende.example.social/auth/callback \
     https://example.social/api/v1/apps
   ```

2. Copy `.env.example` to `.env` and fill it in. Put the database admin password in `postgres.env` as `POSTGRES_PASSWORD=...`, so the app never receives it.

3. Start the database and apply the schema as its owner. The app user can't change the schema, and can only add to the audit log:

   ```sh
   docker compose up -d postgres redis
   docker compose exec postgres psql -U postgres -d agende -c "CREATE ROLE agende LOGIN PASSWORD 'change-me'"
   docker compose exec -T postgres psql -U postgres -d agende < schema.sql
   ```

4. `docker compose up -d`, and point your reverse proxy at `127.0.0.1:8080`. It must serve HTTPS, because the session cookie needs it, and accept uploads as large as `AGENDE_MAX_UPLOAD_MB`.

## Versions

Versions look like `X.Y.Z-BUILD`. `X.Y.Z` is in the `VERSION` file and follows SemVer, and `BUILD` is the GitLab pipeline number. A local build shows `X.Y.Z-dev`. The page footer shows the version and links to the [container registry](https://gitlab.gutocarvalho.net/images/agende/container_registry).

| Image tag | Moves? |
|---|---|
| `X.Y.Z-BUILD` | Never. Pin this one in production. |
| `X.Y.Z` | On each build of that version. |
| `latest` | On each build. |

## How it stores data

| Where | What |
|---|---|
| Mastodon | The scheduled posts, including text and media |
| Redis | Sessions, with the access token encrypted. A session lasts 8 hours. The token is revoked at sign-out or when it expires, retrying for a week if the instance is down |
| PostgreSQL | Settings per account, and the audit log |

The audit log covers scheduling, date changes and cancellations. It keeps who did what, on which account and when, plus an encrypted excerpt of up to 80 characters, because a canceled post no longer exists on the instance. It only sees actions made here, not ones made directly on Mastodon. Remove old rows with a separate database user: the app can't.

Losing the encryption key or the Redis data signs everyone out, and those tokens can't be revoked from here: people can remove them under Authorized apps on the instance. A lost key also makes stored excerpts unreadable. Scheduled posts aren't affected.

## Limits

These come from Mastodon: at least 5 minutes ahead, 25 scheduled posts per day (UTC) and 300 in total. Mastodon only lets the date of a scheduled post change. Editing anything else creates a new scheduled post and then cancels the old one, keeping its media; the new post gets a new id. Media isn't copied when duplicating.

## Development

```sh
go test ./...
```

Tests that need PostgreSQL and Redis skip themselves unless `AGENDE_TEST_DB_HOST` and `AGENDE_TEST_REDIS` are set, see `.gitlab-ci.yml`.

## License

AGPL-3.0, see `LICENSE` and `NOTICE`.
