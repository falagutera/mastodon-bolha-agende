# AGENTS.md

Notes for coding agents (and people) changing agende. The README says what the app does and how
to install it; this file says how to change it without breaking the promises the README makes.

## What it is

One Go binary, server-rendered templates plus one small script (`assets/agende.js`), no ORM. People
sign in with OAuth on **one** Mastodon instance (`AGENDE_INSTANCE`); the instance stores and publishes
the scheduled posts. agende keeps only sessions (Redis, token encrypted), settings, the audit log and,
with delegation on, the owners' tokens (PostgreSQL, encrypted). Every API call goes to that instance.

## Commands

```sh
gofmt -l .            # must print nothing
go vet ./...
go test ./...         # without AGENDE_TEST_DB_HOST and AGENDE_TEST_REDIS ~48 tests skip themselves
```

The skipped tests are most of the rule tests below, so a green run without services proves little:
the run with PostgreSQL and Redis is the one that counts.

With PostgreSQL and Redis (what CI runs). The tests **delete every delegation and `FLUSHDB` the
Redis database they reach**, so use throwaway containers on free ports and chain with `&&`. They
refuse a PostgreSQL database whose name does not end in `_test` (`mustBeTestDB`) and Redis database 0,
the one the app uses (`testRedisDB`, default index 15), both in `agende_test.go`. No psql needed on
the host:

```sh
P=55601 R=55602   # any free ports
docker run -d --name agende-pg-$P -p $P:5432 -e POSTGRES_PASSWORD=pg postgres:17-alpine &&
  docker run -d --name agende-redis-$R -p $R:6379 redis:7.4-alpine &&
  until docker exec agende-pg-$P pg_isready -U postgres -q; do sleep 1; done && sleep 2 &&
  docker exec agende-pg-$P psql -q -U postgres -c "CREATE ROLE agende LOGIN PASSWORD 'agende'" -c "CREATE DATABASE agende_test" &&
  docker exec -i agende-pg-$P psql -q -U postgres -d agende_test -v ON_ERROR_STOP=1 -f - < schema.sql &&
  AGENDE_TEST_DB_HOST=127.0.0.1 AGENDE_TEST_DB_PORT=$P AGENDE_TEST_REDIS=127.0.0.1:$R go test -p 1 -count=1 ./...

docker rm -f agende-pg-$P agende-redis-$R
```

The tests connect as the app role `agende` (password `agende`), so they only have the grants of
`schema.sql`: a query the app user may not run fails here too. Test variables: `AGENDE_TEST_DB_HOST`,
`AGENDE_TEST_DB_PORT` (5432), `AGENDE_TEST_DB_NAME` (agende_test), `AGENDE_TEST_REDIS` (host:port),
`AGENDE_TEST_REDIS_DB` (15; 1 to 15).

To look at a rendered page, use a test: `b.do(http.MethodGet, "/queue", "", nil)` returns status,
`Location` and the HTML body; write the body to a file while you work on it.

## Map

| File | What |
|---|---|
| `main.go` | config from env (`loadConfig`), wiring, HTTP server timeouts, session sweep loop |
| `server.go` | routes, guards (`page`, `action`, `fetchAction`, `sameSite`, `csrfOK`), CSP (`csp`, `securityHeaders`), compose/edit/cancel handlers |
| `auth.go` | OAuth sign-in (state cookie, PKCE S256), token exchange and revocation |
| `sessions.go` | Redis sessions (8 h), token refcount, the sweep that revokes expired tokens (retries for a week) |
| `delegation.go` | delegation flow; `resolve` turns "Post as" into a token, `sameTarget` |
| `store.go` | PostgreSQL: settings, audit writes (`excerpt`, 80 runes, encrypted), delegations (max 10) |
| `audit.go` | the Log page; `auditLog` decides visibility in SQL |
| `mastodon.go` | the instance API client (`do`, `decode` with a 4 MiB limit) |
| `schedule.go` | `series`, `checkDates` (Mastodon's limits), `replyID`, time zones |
| `crypto.go` | `box`: AES-GCM bound to a purpose and an owner; `randomToken`, `hashID` |
| `calendar.go` | week and month views |
| `i18n.go` | every text, in `en` and `pt-BR` |
| `version.go`, `internal/relnotes` | version string, release notes from `CHANGELOG.md` |
| `templates/`, `assets/` | HTML templates; `agende.js` (draft in localStorage, preview, dates), CSS, fonts |
| `schema.sql` | tables and the app user's grants (no DDL, audit log insert-only) |

Tests: `agende_test.go` (`fakeMastodon`, the fake instance: `b.login("ana"|"bia"|"cai")` are login
codes that sign in as accounts 1–3, `@user1`…`@user3`; `user4` is suspended, `user9@other.social` is
remote; `testApp`; `browser` drives sign-in and forms), `delegation_test.go`, `audit_test.go`, `edit_later_test.go`,
`bulk_cancel_test.go`, `changelog_test.go`, `templates_test.go`.

Test helpers: `b.do(method, path, contentType, body)` → (status, `Location`, body); `b.post(path, form)`
(`delegation_test.go`, fills `csrf`); `b.compose(fields, files)`; `scheduleOne`; `baseFields()`;
`future(days)`; `marker(t)` and `b.audit(query)` (`audit_test.go`); the fake's posts are in
`f.scheduled[id]`, read them under `f.mu`.

Audit rows persist across tests (the log is insert-only) and the fake's post ids restart at 100. A
test that reads `/audit` sets `f.next` to something unique and puts `marker(t)` in the text, as
`audit_test.go` does, or it matches rows of another test.

## Rules that must hold

1. **One instance only.** Requests go to `cfg.instance` (`mastodon.do` prefixes `m.base`); ids are
   validated (`idPattern`) and path-escaped. A reply must be an id or a URL of that instance
   (`replyID`, `TestReplyID`). Never fetch a URL a user typed.
2. **Each account acts only on its own posts.** Calls use the session's token (`sess.ActToken`), so
   the instance enforces ownership (`TestOtherAccountCantTouchPost`, `TestEditRefusesAnotherAccountsPost`).
   A form acts only on the account it was shown for (`sameTarget`, `TestAccountChangedInAnotherTab`).
3. **Every state change is guarded.** POSTs go through `action`/`fetchAction` (or the same checks in
   `handleCreate`): `Sec-Fetch-Site` same-origin, CSRF token, a size limit (`TestCSRFRequired`).
   OAuth state works once and uses PKCE (`TestLoginStateWorksOnce`). A resent form does nothing
   (nonce in Redis, `TestResentFormDoesNothing`).
4. **Delegation.** Only the signed-in owner grants, with the password asked again; the account that
   authorized must be the owner (`TestDelegationWithAnotherAccountIsRefused`). Local accounts only,
   no chains, at most 10, checked again at the callback (`TestDelegationRejectsBadDelegates`,
   `TestNoDelegationChains`, `TestDelegationLimit`, `TestLimitIsCheckedAgainAtTheCallback`).
   `resolve` checks the database on every request (`TestActingNeedsADelegation`,
   `TestRevokeEndsAccessAndSharedToken`); the token is bound to its pair (`TestDelegationTokenIsBoundToItsPair`).
5. **Secrets stay encrypted.** Tokens and excerpts are sealed with `box`, bound to purpose and owner
   (`TestBoxBindsOwner`); Redis never holds a token in clear (`TestSweepRevokesExpiredOnce`); another
   key means no session (`TestSessionFromAnotherKeyIsNoSession`). Never log a token or post text.
6. **Who sees the log.** An account sees what was done on it; a delegate sees what it did and the
   delegation events that name it; nobody else, and filters never widen it. Only in `auditLog`'s
   `WHERE` (`TestAuditVisibility`).
7. **No inline script or style.** CSP is `script-src 'self'; style-src 'self'`; only the delegations
   page adds the instance to `form-action` (`TestOnlyTheDelegationsPageAllowsTheInstanceAsFormAction`,
   `TestNoInlineScriptOrStyle`). The Markdown preview escapes HTML (`TestPreviewEscapesHTML`).
8. **Both languages.** Every key exists in `en` and `pt-BR`, and every `.T.key` a template uses
   exists (`TestTextsInBothLanguages`).
9. **Mastodon's limits, before sending.** 6 minutes ahead, 25 a day, 300 in total, series of 2–24:
   a series over the limits sends nothing (`TestCheckDates`, `TestSeriesOverLimitsSendsNothing`,
   `TestDailySeriesStopsAtDailyLimit`). Uploads: 3 concurrent, one per account
   (`TestOneUploadPerAccount`, `TestUploadSlotsDontBlockTextPosts`); edits never lose the original
   (`TestEditFailureKeepsOriginal`).
10. **Schema changes are additive.** `schema.sql` is re-run on upgrades: use `IF NOT EXISTS`, and
    grant the app only what it needs (the audit log stays `SELECT, INSERT`).

## The usual change (a form option or a page)

0. **Check it is not there already.** Grep `i18n.go` (both languages) and `templates/` for the
   label; read the README feature list.
1. Prefer a field on an existing action (e.g. the compose form, `handleCreate`): it keeps the guards,
   the limits and the audit entry. A new route needs its own guards: `page` (GET) or `action` (POST),
   `sameTarget` on forms that post as an account, and calls through `mastodon.go` with `sess.ActToken`.
2. Reusing an `action*` constant (`store.go`) needs no new sentence. A new kind of action writes an
   audit entry (`s.db.audit` / `auditResult`), gets a sentence in `auditLines` (`audit.go`) and texts.
3. Markup in `templates/`, behaviour in `assets/agende.js`, texts in both languages.
4. A test with `testApp` and `browser`, including the account that must be refused.
5. `gofmt`, `go vet`, the tests with PostgreSQL and Redis.

## Releasing

- `VERSION` holds `X.Y.Z`; CI adds the build number (`X.Y.Z-BUILD`). Bump it for user-visible changes:
  a fix is a patch, a new button or option a minor (e.g. 0.4.1 → 0.5.0). Docs- or tests-only changes don't bump.
- Work not released yet can go under `## Unreleased` at the top of `CHANGELOG.md`; the release notes skip it.
- Every version needs its `CHANGELOG.md` entry (`TestChangelogHasVersion`); it is also the
  `/release-notes` page, so write it for the people who use the site.
- A change to `schema.sql` says so in the changelog: installs must apply it again as the owner.
- Code, comments, commits and the README in English. Keep commits small with a message that says why.

## Security checks

CI runs vet, `gofmt -l`, the tests against PostgreSQL 17 and Redis 7.4, govulncheck and the shared scanners
(gitleaks, gosec, semgrep, trivy, hadolint, shellcheck, yamllint, poutine). Before a release that
touches sign-in, delegation, the audit log or templates, also review the change for authorization:
which account the action runs as, and who can see the result. Scanners do not see that.
