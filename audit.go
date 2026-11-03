package main

// The audit page. Each action is two rows sharing op_id (intent, then result),
// shown as one sentence. Who sees what:
//   - an account sees everything done on it (target), by itself or a delegate;
//   - a delegate sees what it did on other accounts (actor);
//   - a delegate sees the delegation events that name it (subject).
// It only covers actions made here, never ones made directly on Mastodon.

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
)

const (
	auditPage = 50
	// An intent older than this with no result was cut midway.
	auditInterrupted = createTimeout + time.Minute
)

var (
	postActions       = []string{actionSchedule, actionReschedule, actionCancel, actionEdit}
	delegationActions = []string{actionDelegate, actionRevoke, actionLeave, actionLost}
	auditFilters      = []string{"schedule", "reschedule", "cancel", "edit", "delegation"}
)

type auditRow struct {
	ID          int64
	At          time.Time // when the action started
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
	SubjectID   string
	SubjectAcct string
}

type auditFilter struct {
	Account string // target account id, or empty for all
	Action  string // one of auditFilters, or empty
	From    *time.Time
	To      *time.Time // exclusive
	Before  int64      // page: rows older than this id
}

// auditLog reads one page for viewer. Visibility is decided here, in SQL, and
// nowhere else.
func (s *store) auditLog(ctx context.Context, viewer string, f auditFilter) ([]auditRow, error) {
	args := []any{viewer, delegationActions}
	where := ""
	add := func(cond string, v any) {
		args = append(args, v)
		where += fmt.Sprintf(" AND "+cond, len(args))
	}
	if f.Account != "" {
		add("target_id = $%d", f.Account)
	}
	switch f.Action {
	case "":
	case "delegation":
		add("action = ANY($%d)", delegationActions)
	default:
		add("action = $%d", f.Action)
	}
	if f.From != nil {
		add("started >= $%d", *f.From)
	}
	if f.To != nil {
		add("started < $%d", *f.To)
	}
	if f.Before > 0 {
		add("id < $%d", f.Before)
	}
	args = append(args, auditPage+1)
	// DISTINCT ON keeps the last row of each action: its result, or the intent alone.
	query := `SELECT id, started, actor_id, actor_acct, target_id, target_acct, action, scheduled_id, series_id,
			before_at, after_at, result, excerpt, subject_id, subject_acct
		FROM (
			SELECT DISTINCT ON (op_id) *, min(at) OVER (PARTITION BY op_id) AS started
			FROM audit_log
			WHERE target_id = $1 OR actor_id = $1 OR (subject_id = $1 AND action = ANY($2))
			ORDER BY op_id, id DESC
		) ops
		WHERE true` + where + `
		ORDER BY id DESC
		LIMIT $` + strconv.Itoa(len(args))
	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (auditRow, error) {
		var a auditRow
		var sealed []byte
		err := row.Scan(&a.ID, &a.At, &a.ActorID, &a.ActorAcct, &a.TargetID, &a.TargetAcct, &a.Action, &a.ScheduledID, &a.SeriesID,
			&a.Before, &a.After, &a.Result, &sealed, &a.SubjectID, &a.SubjectAcct)
		if err == nil && len(sealed) > 0 {
			// Excerpts are bound to the target account; one that doesn't open is left out.
			if plain, err := s.box.open(sealed, []byte(a.TargetID)); err == nil {
				a.Excerpt = string(plain)
			}
		}
		return a, err
	})
}

// auditAccounts lists the accounts the viewer's log mentions as targets, for the filter.
func (s *store) auditAccounts(ctx context.Context, viewer string) ([]actOption, error) {
	rows, err := s.pool.Query(ctx, `SELECT DISTINCT ON (target_id) target_id, target_acct FROM audit_log
		WHERE target_id = $1 OR actor_id = $1 OR (subject_id = $1 AND action = ANY($2))
		ORDER BY target_id, id DESC`, viewer, delegationActions)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (actOption, error) {
		var o actOption
		err := row.Scan(&o.ID, &o.Acct)
		return o, err
	})
}

// auditLine is one sentence of the page.
type auditLine struct {
	When    string
	Text    string
	Status  string // empty when it worked
	Warning bool
}

// auditLines turns rows into sentences. Consecutive posts of one series become
// a single line.
func auditLines(rows []auditRow, t map[string]string, loc *time.Location, now time.Time) []auditLine {
	acct := func(a string) string { return "@" + a }
	date := func(at *time.Time) string {
		if at == nil {
			return "?"
		}
		return at.In(loc).Format(t["date_format"])
	}
	quote := func(text string) string {
		if text == "" {
			return t["audit_no_text"]
		}
		if len([]rune(text)) >= excerptRunes {
			text += "…"
		}
		return "“" + text + "”"
	}
	var out []auditLine
	for i := 0; i < len(rows); i++ {
		r := rows[i]
		line := auditLine{When: r.At.In(loc).Format(t["date_format"])}
		switch r.Action {
		case actionSchedule:
			// rows are newest first, so a series runs from j back to i.
			j := i
			for r.SeriesID != "" && j+1 < len(rows) && rows[j+1].Action == actionSchedule && rows[j+1].SeriesID == r.SeriesID {
				j++
			}
			if j > i {
				ok := 0
				for _, x := range rows[i : j+1] {
					if x.Result == resultOK {
						ok++
					}
				}
				first := rows[j]
				line.When = first.At.In(loc).Format(t["date_format"])
				line.Text = fmt.Sprintf(t["audit_series"], acct(r.ActorAcct), j-i+1, quote(first.Excerpt), acct(r.TargetAcct), date(first.After), date(r.After))
				if ok < j-i+1 {
					line.Status, line.Warning = fmt.Sprintf(t["audit_series_partial"], ok), true
				}
				out = append(out, line)
				i = j
				continue
			}
			line.Text = fmt.Sprintf(t["audit_schedule"], acct(r.ActorAcct), quote(r.Excerpt), acct(r.TargetAcct), date(r.After))
		case actionReschedule:
			line.Text = fmt.Sprintf(t["audit_reschedule"], acct(r.ActorAcct), quote(r.Excerpt), acct(r.TargetAcct), date(r.Before), date(r.After))
		case actionCancel:
			line.Text = fmt.Sprintf(t["audit_cancel"], acct(r.ActorAcct), quote(r.Excerpt), acct(r.TargetAcct), date(r.Before))
		case actionEdit:
			line.Text = fmt.Sprintf(t["audit_edit"], acct(r.ActorAcct), quote(r.Excerpt), acct(r.TargetAcct), date(r.After))
		case actionDelegate:
			line.Text = fmt.Sprintf(t["audit_delegate"], acct(r.TargetAcct), acct(r.SubjectAcct))
		case actionRevoke:
			line.Text = fmt.Sprintf(t["audit_revoke"], acct(r.TargetAcct), acct(r.SubjectAcct))
		case actionLeave:
			line.Text = fmt.Sprintf(t["audit_leave"], acct(r.SubjectAcct), acct(r.TargetAcct))
		case actionLost:
			line.Text = fmt.Sprintf(t["audit_lost"], acct(r.SubjectAcct), acct(r.TargetAcct))
		default:
			continue
		}
		line.Status, line.Warning = resultLabel(r, t, now)
		out = append(out, line)
	}
	return out
}

func resultLabel(r auditRow, t map[string]string, now time.Time) (string, bool) {
	switch r.Result {
	case resultOK:
		return "", false
	case resultFailed:
		return t["audit_failed"], true
	case resultPublished:
		return t["audit_published_now"], true
	case resultUnknown:
		return t["audit_unknown"], true
	case resultIntent:
		if now.Sub(r.At) < auditInterrupted {
			return t["audit_in_progress"], false
		}
		return t["audit_interrupted"], true
	}
	return t["audit_unknown"], true
}

// parseAuditFilter reads the query string. Anything invalid is ignored rather
// than reported: it is only a filter.
func parseAuditFilter(q url.Values, loc *time.Location) auditFilter {
	var f auditFilter
	if a := q.Get("account"); idPattern.MatchString(a) {
		f.Account = a
	}
	if a := q.Get("action"); slices.Contains(auditFilters, a) {
		f.Action = a
	}
	if d, err := time.ParseInLocation(time.DateOnly, q.Get("from"), loc); err == nil {
		f.From = &d
	}
	if d, err := time.ParseInLocation(time.DateOnly, q.Get("to"), loc); err == nil {
		end := d.AddDate(0, 0, 1)
		f.To = &end
	}
	if b, err := strconv.ParseInt(q.Get("before"), 10, 64); err == nil && b > 0 {
		f.Before = b
	}
	return f
}

func (s *server) handleAudit(w http.ResponseWriter, r *http.Request, sess session) {
	st, loc, err := s.userSettings(r.Context(), sess)
	if err != nil {
		log.Printf("settings: %v", err)
		s.renderError(w, r, http.StatusServiceUnavailable, "unavailable")
		return
	}
	q := r.URL.Query()
	f := parseAuditFilter(q, loc)
	// The page always shows the signed-in account's own log, whoever it is acting as.
	rows, err := s.db.auditLog(r.Context(), sess.AccountID, f)
	var accounts []actOption
	if err == nil {
		accounts, err = s.db.auditAccounts(r.Context(), sess.AccountID)
	}
	if errors.Is(err, context.Canceled) {
		return
	}
	if err != nil {
		log.Printf("audit: %v", err)
		s.renderError(w, r, http.StatusServiceUnavailable, "unavailable")
		return
	}
	v := s.view(r, sess, "audit")
	v.Settings = st
	if len(rows) > auditPage {
		rows = rows[:auditPage]
		next := url.Values{}
		for _, k := range []string{"account", "action", "from", "to"} {
			if q.Get(k) != "" {
				next.Set(k, q.Get(k))
			}
		}
		next.Set("before", strconv.FormatInt(rows[len(rows)-1].ID, 10))
		v.Next = "/audit?" + next.Encode()
	}
	v.Audit = auditLines(rows, v.T, loc, time.Now())
	for i := range accounts {
		accounts[i].On = accounts[i].ID == f.Account
	}
	v.AuditAccounts = accounts
	v.AuditFilter = auditFilterView{Account: f.Account, Action: f.Action, From: q.Get("from"), To: q.Get("to"), Paged: f.Before > 0}
	if f.From == nil {
		v.AuditFilter.From = ""
	}
	if f.To == nil {
		v.AuditFilter.To = ""
	}
	v.AuditActions = auditFilters
	s.render(w, "audit.html", http.StatusOK, v)
}

type auditFilterView struct {
	Account, Action, From, To string
	Paged                     bool
}
