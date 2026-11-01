-- Run once as the database owner, not as the app user. See README.md.
-- The app can't alter its own schema, and can't rewrite the audit log.
CREATE TABLE IF NOT EXISTS settings (
  account_id   text PRIMARY KEY,
  timezone     text NOT NULL,
  visibility   text NOT NULL,
  language     text NOT NULL,
  content_type text NOT NULL,
  updated_at   timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS audit_log (
  id           bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  op_id        text NOT NULL,
  at           timestamptz NOT NULL DEFAULT now(),
  actor_id     text NOT NULL,
  actor_acct   text NOT NULL,
  target_id    text NOT NULL,
  target_acct  text NOT NULL,
  action       text NOT NULL,
  scheduled_id text NOT NULL DEFAULT '',
  series_id    text NOT NULL DEFAULT '',
  before_at    timestamptz,
  after_at     timestamptz,
  result       text NOT NULL,
  excerpt      bytea
);
CREATE INDEX IF NOT EXISTS audit_log_target_at ON audit_log (target_id, at DESC);
CREATE INDEX IF NOT EXISTS audit_log_actor_at ON audit_log (actor_id, at DESC);

-- Replace agende with your app user if it has another name.
GRANT SELECT, INSERT, UPDATE ON settings TO agende;
GRANT SELECT, INSERT ON audit_log TO agende;
