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

-- 0.3.0: delegation. The account a delegation event is about, such as the delegate.
ALTER TABLE audit_log ADD COLUMN IF NOT EXISTS subject_id text NOT NULL DEFAULT '';
ALTER TABLE audit_log ADD COLUMN IF NOT EXISTS subject_acct text NOT NULL DEFAULT '';

-- An owner lets a delegate schedule posts as the owner. The token is the owner's,
-- from a separate OAuth app, encrypted with AGENDE_DELEGATION_KEY and bound to the pair.
CREATE TABLE IF NOT EXISTS delegations (
  owner_id      text NOT NULL,
  owner_acct    text NOT NULL,
  delegate_id   text NOT NULL,
  delegate_acct text NOT NULL,
  token         bytea NOT NULL,
  created_at    timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (owner_id, delegate_id),
  CHECK (owner_id <> delegate_id)
);
CREATE INDEX IF NOT EXISTS delegations_delegate ON delegations (delegate_id);

-- Replace agende with your app user if it has another name.
GRANT SELECT, INSERT, UPDATE ON settings TO agende;
GRANT SELECT, INSERT ON audit_log TO agende;
GRANT SELECT, INSERT, UPDATE, DELETE ON delegations TO agende;
