package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type settings struct {
	Timezone    string
	Visibility  string
	Language    string
	ContentType string
}

type store struct {
	pool          *pgxpool.Pool
	box           *box
	delegationBox *box // nil when delegation is off
}

func openStore(ctx context.Context, c config, b *box) (*store, error) {
	u := url.URL{
		Scheme:   "postgres",
		User:     url.UserPassword(c.dbUser, c.dbPassword),
		Host:     net.JoinHostPort(c.dbHost, c.dbPort),
		Path:     "/" + c.dbName,
		RawQuery: url.Values{"sslmode": {c.dbSSLMode}}.Encode(),
	}
	cfg, err := pgxpool.ParseConfig(u.String())
	if err != nil {
		return nil, fmt.Errorf("database settings: %w", err)
	}
	cfg.MaxConns = 3
	// Simple protocol so it also works behind pgbouncer in transaction mode.
	cfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("database: %w", err)
	}
	return &store{pool: pool, box: b}, nil
}

// check fails at startup when schema.sql wasn't applied.
func (s *store) check(ctx context.Context) error {
	var n int
	return s.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM settings WHERE false) + (SELECT count(*) FROM audit_log WHERE subject_id <> '' AND false)
		+ (SELECT count(*) FROM delegations WHERE false)`).Scan(&n)
}

func (s *store) settings(ctx context.Context, accountID string, fallback settings) (settings, error) {
	var v settings
	err := s.pool.QueryRow(ctx, `SELECT timezone, visibility, language, content_type FROM settings WHERE account_id = $1`, accountID).
		Scan(&v.Timezone, &v.Visibility, &v.Language, &v.ContentType)
	if errors.Is(err, pgx.ErrNoRows) {
		return fallback, nil
	}
	return v, err
}

func (s *store) saveSettings(ctx context.Context, accountID string, v settings) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO settings (account_id, timezone, visibility, language, content_type)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (account_id) DO UPDATE SET timezone = $2, visibility = $3, language = $4, content_type = $5, updated_at = now()`,
		accountID, v.Timezone, v.Visibility, v.Language, v.ContentType)
	return err
}

// auditEntry is one line of the log. The actor and the target are separate from
// the start: they are the same account until delegation exists.
type auditEntry struct {
	OpID        string
	ActorID     string
	ActorAcct   string
	TargetID    string
	TargetAcct  string
	Action      string
	ScheduledID string
	SeriesID    string
	Before      *time.Time
	After       *time.Time
	Result      string
	Excerpt     string
	// SubjectID is the other account of a delegation event: the delegate.
	SubjectID   string
	SubjectAcct string
}

const (
	actionSchedule   = "schedule"
	actionReschedule = "reschedule"
	actionCancel     = "cancel"
	actionEdit       = "edit"
	actionDelegate   = "delegate"
	actionRevoke     = "revoke_delegation"
	actionLeave      = "leave_delegation"
	actionLost       = "delegation_lost"
	resultIntent     = "intent"
	resultOK         = "ok"
	resultFailed     = "failed"
	resultUnknown    = "unknown"
	resultPublished  = "published_now"
	excerptRunes     = 80
)

func excerpt(text string) string {
	r := []rune(text)
	if len(r) > excerptRunes {
		r = r[:excerptRunes]
	}
	return string(r)
}

// audit appends one row. The table can't be updated by this user, so an action
// takes two rows with the same op_id: the intent and then its result. An intent
// with no result is an action interrupted midway.
func (s *store) audit(ctx context.Context, e auditEntry) error {
	var sealed []byte
	if e.Excerpt != "" {
		sealed = s.box.seal([]byte(excerpt(e.Excerpt)), []byte(e.TargetID))
	}
	_, err := s.pool.Exec(ctx, `INSERT INTO audit_log
		(op_id, actor_id, actor_acct, target_id, target_acct, action, scheduled_id, series_id, before_at, after_at, result, excerpt, subject_id, subject_acct)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)`,
		e.OpID, e.ActorID, e.ActorAcct, e.TargetID, e.TargetAcct, e.Action, e.ScheduledID, e.SeriesID, e.Before, e.After, e.Result, sealed,
		e.SubjectID, e.SubjectAcct)
	return err
}

// delegation lets Delegate schedule posts as Owner. The token never leaves this
// file in a listing: only delegationToken decrypts it.
type delegation struct {
	OwnerID      string
	OwnerAcct    string
	DelegateID   string
	DelegateAcct string
	Created      time.Time
}

const maxDelegations = 10

var (
	errNoDelegation = errors.New("no delegation")
	errTooMany      = errors.New("too many delegations")
)

// pair is the associated data of a delegation token: a value copied to another
// row doesn't decrypt.
func pair(ownerID, delegateID string) []byte { return []byte(ownerID + "\x00" + delegateID) }

// putDelegation stores a delegation and gives every delegation of the owner the
// new token, so they all share one, as on the instance. It returns the tokens the
// rows held before, if different: the caller revokes them, or they would stay
// valid with nothing here to revoke them later.
func (s *store) putDelegation(ctx context.Context, d delegation, token string) (stale []string, err error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	rows, err := tx.Query(ctx, `SELECT delegate_id, token FROM delegations WHERE owner_id = $1 FOR UPDATE`, d.OwnerID)
	if err != nil {
		return nil, err
	}
	type held struct {
		delegate string
		sealed   []byte
	}
	others, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (held, error) {
		var h held
		err := row.Scan(&h.delegate, &h.sealed)
		return h, err
	})
	if err != nil {
		return nil, err
	}
	// Checked again here: several flows may have started before any finished.
	if len(others) >= maxDelegations && !slices.ContainsFunc(others, func(h held) bool { return h.delegate == d.DelegateID }) {
		return nil, errTooMany
	}
	seen := map[string]bool{token: true}
	for _, h := range others {
		if old, err := s.delegationBox.open(h.sealed, pair(d.OwnerID, h.delegate)); err == nil && !seen[string(old)] {
			seen[string(old)] = true
			stale = append(stale, string(old))
		}
		if h.delegate == d.DelegateID {
			continue
		}
		if _, err := tx.Exec(ctx, `UPDATE delegations SET token = $3, owner_acct = $4 WHERE owner_id = $1 AND delegate_id = $2`,
			d.OwnerID, h.delegate, s.delegationBox.seal([]byte(token), pair(d.OwnerID, h.delegate)), d.OwnerAcct); err != nil {
			return nil, err
		}
	}
	if _, err := tx.Exec(ctx, `INSERT INTO delegations (owner_id, owner_acct, delegate_id, delegate_acct, token)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (owner_id, delegate_id) DO UPDATE SET owner_acct = $2, delegate_acct = $4, token = $5`,
		d.OwnerID, d.OwnerAcct, d.DelegateID, d.DelegateAcct, s.delegationBox.seal([]byte(token), pair(d.OwnerID, d.DelegateID))); err != nil {
		return nil, err
	}
	return stale, tx.Commit(ctx)
}

func (s *store) listDelegations(ctx context.Context, column, accountID string) ([]delegation, error) {
	if column != "owner_id" && column != "delegate_id" {
		return nil, errors.New("bad column")
	}
	rows, err := s.pool.Query(ctx, `SELECT owner_id, owner_acct, delegate_id, delegate_acct, created_at FROM delegations
		WHERE `+column+` = $1 ORDER BY owner_acct, delegate_acct`, accountID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (delegation, error) {
		var d delegation
		err := row.Scan(&d.OwnerID, &d.OwnerAcct, &d.DelegateID, &d.DelegateAcct, &d.Created)
		return d, err
	})
}

// grantedBy lists who may act as the owner.
func (s *store) grantedBy(ctx context.Context, ownerID string) ([]delegation, error) {
	return s.listDelegations(ctx, "owner_id", ownerID)
}

// grantedTo lists the accounts the delegate may act as.
func (s *store) grantedTo(ctx context.Context, delegateID string) ([]delegation, error) {
	return s.listDelegations(ctx, "delegate_id", delegateID)
}

// delegationToken is the only check of "may this account act as that one". A row
// that doesn't decrypt counts as no delegation.
func (s *store) delegationToken(ctx context.Context, ownerID, delegateID string) (delegation, string, error) {
	d := delegation{OwnerID: ownerID, DelegateID: delegateID}
	var sealed []byte
	err := s.pool.QueryRow(ctx, `SELECT owner_acct, delegate_acct, created_at, token FROM delegations
		WHERE owner_id = $1 AND delegate_id = $2`, ownerID, delegateID).Scan(&d.OwnerAcct, &d.DelegateAcct, &d.Created, &sealed)
	if errors.Is(err, pgx.ErrNoRows) {
		return d, "", errNoDelegation
	}
	if err != nil {
		return d, "", err
	}
	token, err := s.delegationBox.open(sealed, pair(ownerID, delegateID))
	if err != nil {
		return d, "", errNoDelegation
	}
	return d, string(token), nil
}

// deleteDelegation removes one delegation. Mastodon gives the same token to every
// authorization of an account in one app, so the token may only be revoked when
// the owner has no delegation left: last says so.
func (s *store) deleteDelegation(ctx context.Context, ownerID, delegateID string) (d delegation, token string, last bool, err error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return d, "", false, err
	}
	defer tx.Rollback(ctx)
	var sealed []byte
	d = delegation{OwnerID: ownerID, DelegateID: delegateID}
	err = tx.QueryRow(ctx, `DELETE FROM delegations WHERE owner_id = $1 AND delegate_id = $2
		RETURNING owner_acct, delegate_acct, token`, ownerID, delegateID).Scan(&d.OwnerAcct, &d.DelegateAcct, &sealed)
	if errors.Is(err, pgx.ErrNoRows) {
		return d, "", false, errNoDelegation
	}
	if err != nil {
		return d, "", false, err
	}
	var left int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM delegations WHERE owner_id = $1`, ownerID).Scan(&left); err != nil {
		return d, "", false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return d, "", false, err
	}
	if plain, err := s.delegationBox.open(sealed, pair(ownerID, delegateID)); err == nil {
		token = string(plain)
	}
	return d, token, left == 0, nil
}

func (s *store) countDelegations(ctx context.Context, ownerID string) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `SELECT count(*) FROM delegations WHERE owner_id = $1`, ownerID).Scan(&n)
	return n, err
}
