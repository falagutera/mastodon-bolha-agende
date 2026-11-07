# Changelog

All notable changes to agende. Versions follow [SemVer](https://semver.org/); images are
published as `registry.gutocarvalho.net/images/agende:<version>-<build>`.

## 0.4.1 — 2026-11-07

Hardening from the updated linters.

- Form handlers read the form their guard already size-limited and parsed, instead of parsing it again on their own.
- Ids sent to the instance are path-escaped.
- The image declares its non-root user (65532) explicitly.

## 0.4.0 — 2026-11-06

- Cancel many posts at once: tick them in the queue (or "Select all") and press "Cancel selected".
- A post's page offers to cancel it together with every later post that has the same text, as editing already did.
- Each canceled post is in the activity log, as before.

## 0.3.2 — 2026-11-06

- Release notes page, linked from the top of every page, and the same data at `/release-notes.json`.

## 0.3.1 — 2026-11-05

- Uploads over 1 MiB no longer leave temporary files behind on the server.

## 0.3.0 — 2026-11-03

- Delegation: let another account schedule posts on yours, and switch between the accounts you can post as.
- Activity log page: what was scheduled, edited, rescheduled or canceled on your account, and by whom.
- Editing a post can also update the later posts of a series that have the same text.

## 0.2.0 — 2026-11-02

- Daily repeat: schedule the same post every day, as a series.

## 0.1.7 — 2026-10-31

First version on agende.bolha.us.

- Schedule posts on the Mastodon instance: text, media, content warning, visibility, language, reply, weekly and monthly series.
- Queue and calendar views; change the date, edit or cancel a scheduled post.
- Time zone picked from a list that shows its offset.
- The unsent draft stays in the browser, bound to the account, and expires.
- Version in the footer.
