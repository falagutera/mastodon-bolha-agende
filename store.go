package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
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
	pool *pgxpool.Pool
	box  *box
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
	return s.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM settings WHERE false) + (SELECT count(*) FROM audit_log WHERE false)`).Scan(&n)
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
}

const (
	actionSchedule   = "schedule"
	actionReschedule = "reschedule"
	actionCancel     = "cancel"
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
		(op_id, actor_id, actor_acct, target_id, target_acct, action, scheduled_id, series_id, before_at, after_at, result, excerpt)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`,
		e.OpID, e.ActorID, e.ActorAcct, e.TargetID, e.TargetAcct, e.Action, e.ScheduledID, e.SeriesID, e.Before, e.After, e.Result, sealed)
	return err
}
