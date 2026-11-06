# agende

Schedule posts on one Mastodon instance. People sign in with OAuth, write a post and pick a date. The instance stores and publishes it, so scheduled posts go out even if this app is down.

- Full composer: Markdown, media with descriptions, content warnings, visibility, language, replies
- Queue, weekly and monthly calendar, change date, cancel, duplicate
- Repeat daily, weekly or monthly: creates up to 24 regular scheduled posts at once
- Editing a post finds the later posts with exactly the same text, and offers to change them too: each keeps its own date and files
- Optional self-destruct and instance-only posts, for forks that support them
- Activity log page: who scheduled, moved, edited or canceled what, on which account, with filters by account, action and period
- Optional delegation: an account lets another account of the instance schedule posts as it

## Install

1. Register the OAuth application:

   ```sh
   curl -F client_name=agende \
     -F "scopes=profile read:statuses write:statuses write:media" \
     -F redirect_uris=https://agende.example.social/auth/callback \
     https://example.social/api/v1/apps
   ```

   For delegation, register a second application with the same scopes and its own redirect, and set `AGENDE_DELEGATION_CLIENT_ID`, `AGENDE_DELEGATION_CLIENT_SECRET` and `AGENDE_DELEGATION_KEY` (`openssl rand -hex 32`, not the same as the encryption key):

   ```sh
   curl -F "client_name=agende delegation" \
     -F "scopes=profile read:statuses write:statuses write:media" \
     -F redirect_uris=https://agende.example.social/delegations/callback \
     https://example.social/api/v1/apps
   ```

   It has to be a separate app: Mastodon gives one token per account and app, so with a single app, signing out would revoke the delegation too.

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

What changed in each version is in [CHANGELOG.md](CHANGELOG.md), which the app also serves as its release notes page (`/release-notes`, linked at the top of the page) and as `/release-notes.json`. Add an entry when you bump `VERSION`: a test fails without it.

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
| PostgreSQL | Settings per account, the audit log and, with delegation on, the owners' tokens, encrypted |

The audit log covers scheduling, date changes, edits, cancellations and delegation events, and the Log page shows it as sentences. An account sees everything done on it, by itself or a delegate; a delegate sees what it did on other accounts, even after losing access, and the delegation events that name it. It keeps who did what, on which account and when, plus an encrypted excerpt of up to 80 characters, because a canceled post no longer exists on the instance. It only sees actions made here, not ones made directly on Mastodon. Remove old rows with a separate database user: the app can't.

Losing the encryption key or the Redis data signs everyone out, and those tokens can't be revoked from here: people can remove them under Authorized apps on the instance. A lost key also makes stored excerpts unreadable. Scheduled posts aren't affected.

## Delegation

An account, say @news, opens Delegations, names a local account, say @ana, and confirms with its password on the instance. From then on @ana sees "Post as" and can schedule, duplicate, edit and cancel scheduled posts of @news. Posts go out from @news.

- Only the owner grants it, signed in as the owner. The instance asks for the password again, and the app checks that the account that authorized is the one signed in.
- Linked by account id, so a renamed or recreated account doesn't inherit it. Local, active accounts only, up to 10 per owner. A delegate can't pass access on.
- The owner revokes it under Delegations, and the delegate can give it up. Removing the delegation app under Authorized apps on the instance ends every delegation of that account. Scheduled posts stay in the owner's queue.
- Every request checks the delegation in the database again. Every action is in the audit log with the account that did it and the account it was done for.
- The owner's token is stored in PostgreSQL, encrypted with `AGENDE_DELEGATION_KEY` and bound to the pair of accounts. Unlike sessions, it doesn't expire until revoked. It has the same scopes as sign-in, so a leaked token could also read the owner's private posts: there is no smaller scope that lists scheduled posts.

Upgrading from 0.2: apply `schema.sql` again as the database owner before starting 0.3. It adds the `delegations` table and two audit columns, and leaves the rest as it is.

## Limits

These come from Mastodon: at least 5 minutes ahead, 25 scheduled posts per day (UTC) and 300 in total. Mastodon only lets the date of a scheduled post change. Editing anything else creates a new scheduled post and then cancels the old one, keeping its media; the new post gets a new id. Media isn't copied when duplicating.

## Development

```sh
go test ./...
```

Tests that need PostgreSQL and Redis skip themselves unless `AGENDE_TEST_DB_HOST` and `AGENDE_TEST_REDIS` are set, see `.gitlab-ci.yml`.

## License

AGPL-3.0, see `LICENSE` and `NOTICE`.
