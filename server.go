package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"log"
	"mime/multipart"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/yuin/goldmark"
	"gitlab.gutocarvalho.net/open/bolha/agende/internal/relnotes"
)

var errOldNotCancelled = errors.New("old post not cancelled")

const (
	formMemory      = 1 << 20
	maxMediaUploads = 25 // the instance allows 30 per 30 minutes
	maxLoginBody    = 1 << 16
	finalLead       = 5*time.Minute + 20*time.Second
	previewLimit    = 16 << 10
	uploadWait      = 3 * time.Second
	uploadRead      = 2 * time.Minute
	createTimeout   = 10 * time.Minute
	resultTTL       = time.Hour
	formKey         = "agende:form:"
	resultKey       = "agende:result:"
)

var (
	visibilities  = []string{"public", "unlisted", "private", "direct"}
	langPattern   = regexp.MustCompile(`^[a-z]{2,3}$`)
	allowedMedia  = []string{"image/", "video/", "audio/"}
	flashMessages = []string{"created", "edited", "edited_all", "rescheduled", "cancelled", "cancelled_many", "saved", "delegated", "removed", "removed_unconfirmed", "delegation_lost"}
)

type server struct {
	cfg      config
	tmpl     *template.Template
	api      *mastodon
	db       *store
	sessions *sessions
	rdb      *redis.Client
	uploads  chan struct{} // bounds concurrent uploads, which use temporary disk
	previews chan struct{} // bounds Markdown rendering

	uploadMu  sync.Mutex
	uploading map[string]bool
}

func newServer(cfg config, tmpl *template.Template, api *mastodon, db *store, sess *sessions, rdb *redis.Client) *server {
	return &server{cfg: cfg, tmpl: tmpl, api: api, db: db, sessions: sess, rdb: rdb, uploads: make(chan struct{}, 3), previews: make(chan struct{}, 4), uploading: map[string]bool{}}
}

type authed func(http.ResponseWriter, *http.Request, session)

// page requires a session. Anonymous visitors go to the sign-in page.
func (s *server) page(next authed) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess, ok, failed := s.current(w, r)
		if failed {
			return
		}
		if !ok {
			http.Redirect(w, r, "/", http.StatusFound)
			return
		}
		next(w, r, sess)
	}
}

// current reads the session. failed means an error page was already written.
func (s *server) current(w http.ResponseWriter, r *http.Request) (sess session, ok, failed bool) {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return session{}, false, false
	}
	sess, err = s.sessions.get(r.Context(), c.Value)
	if errors.Is(err, errNoSession) {
		s.clearCookie(w, sessionCookie)
		return session{}, false, false
	}
	if err == nil {
		err = s.resolve(r.Context(), &sess)
		if errors.Is(err, errNoSession) {
			s.clearCookie(w, sessionCookie)
			return session{}, false, false
		}
	}
	if err != nil {
		log.Printf("session: %v", err)
		s.renderError(w, r, http.StatusServiceUnavailable, "unavailable")
		return session{}, false, true
	}
	return sess, true, false
}

// sameSite rejects requests started by another site, before reading any body.
func sameSite(r *http.Request) bool {
	site := r.Header.Get("Sec-Fetch-Site")
	return site == "" || site == "same-origin"
}

func csrfOK(sess session, value string) bool {
	return subtle.ConstantTimeCompare([]byte(sess.CSRF), []byte(value)) == 1
}

// action guards a POST of a regular form.
func (s *server) action(next authed) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !sameSite(r) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		sess, ok, failed := s.current(w, r)
		if failed {
			return
		}
		if !ok {
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxLoginBody)
		if err := r.ParseForm(); err != nil || !csrfOK(sess, r.PostForm.Get("csrf")) {
			s.renderError(w, r, http.StatusForbidden, "expired")
			return
		}
		next(w, r, sess)
	}
}

// fetchAction guards the helper endpoints the page calls from its script.
func (s *server) fetchAction(next authed) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !sameSite(r) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		sess, ok, failed := s.current(w, r)
		if failed {
			return
		}
		if !ok || !csrfOK(sess, r.Header.Get("X-CSRF-Token")) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, previewLimit)
		next(w, r, sess)
	}
}

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := s.rdb.Ping(ctx).Err(); err != nil {
			http.Error(w, "redis", http.StatusServiceUnavailable)
			return
		}
		fmt.Fprintln(w, "ok")
	})
	mux.HandleFunc("GET /robots.txt", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprint(w, "User-agent: *\nDisallow: /\n")
	})
	assets, err := fs.Sub(assetFS, "assets")
	if err != nil {
		panic(err)
	}
	mux.Handle("GET /assets/", http.StripPrefix("/assets/", http.FileServerFS(assets)))
	mux.HandleFunc("GET /login", s.handleLogin)
	mux.HandleFunc("GET /auth/callback", s.handleCallback)
	mux.HandleFunc("POST /logout", s.action(s.handleLogout))
	mux.HandleFunc("GET /{$}", s.handleIndex)
	mux.HandleFunc("GET /release-notes", s.handleNotes)
	mux.Handle("GET /release-notes.json", relnotes.JSONHandler("agende", version, releases))
	mux.HandleFunc("GET /queue", s.page(s.handleQueue))
	mux.HandleFunc("GET /calendar", s.page(s.handleCalendar))
	mux.HandleFunc("GET /new", s.page(s.handleNew))
	mux.HandleFunc("POST /new", s.handleCreate)
	mux.HandleFunc("GET /result/{id}", s.page(s.handleResult))
	mux.HandleFunc("GET /post/{id}", s.page(s.handlePost))
	mux.HandleFunc("GET /post/{id}/edit", s.page(s.handleEdit))
	mux.HandleFunc("POST /post/{id}/date", s.action(s.handleReschedule))
	mux.HandleFunc("POST /post/{id}/cancel", s.action(s.handleCancel))
	mux.HandleFunc("POST /cancel", s.action(s.handleCancelMany))
	mux.HandleFunc("GET /settings", s.page(s.handleSettings))
	mux.HandleFunc("POST /settings", s.action(s.handleSaveSettings))
	mux.HandleFunc("POST /preview", s.fetchAction(s.handlePreview))
	mux.HandleFunc("POST /dates", s.fetchAction(s.handleDates))
	mux.HandleFunc("GET /audit", s.page(s.handleAudit))
	if s.cfg.delegation() {
		mux.HandleFunc("GET /delegations", s.page(s.handleDelegations))
		mux.HandleFunc("POST /delegations", s.action(s.handleDelegate))
		mux.HandleFunc("GET /delegations/callback", s.page(s.handleDelegationCallback))
		mux.HandleFunc("POST /delegations/revoke", s.action(s.handleRevoke))
		mux.HandleFunc("POST /delegations/leave", s.action(s.handleLeave))
		mux.HandleFunc("POST /acting", s.action(s.handleActing))
	}
	return securityHeaders(mux)
}

// csp is the page policy. Only form-action varies: the delegations page sends a
// form that ends at the instance.
func csp(formAction string) string {
	return "default-src 'none'; script-src 'self'; connect-src 'self'; style-src 'self'; font-src 'self'; img-src 'self'; base-uri 'none'; form-action " +
		formAction + "; frame-ancestors 'none'"
}

// securityHeaders allows same-origin scripts for the page and the bar the edge
// injects on bolha sites. Nothing inline runs.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Cache-Control", "no-store")
		h.Set("X-Robots-Tag", "noindex, nofollow")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Content-Security-Policy", csp("'self'"))
		next.ServeHTTP(w, r)
	})
}

type viewData struct {
	Version   string
	T         map[string]string
	Domain    string
	Acct      string
	CSRF      string
	Flash     string
	Error     string
	Nav       string
	Lang      string
	Limits    limits
	Settings  settings
	Markdown  bool
	LocalOnly bool
	Expiring  bool
	MaxSeries int
	Posts     []postView
	Post      postView
	Form      composeForm
	Calendar  calendar
	Weekdays  []string
	View      string
	Heading   string
	Prev      string
	Next      string
	Results   []seriesResult
	Remaining int
	Now       string
	Message   string
	MaxMedia  []int
	Expires   []expiryOption
	Zones     []zoneOption
	Siblings  []string // dates of later posts with the same text, when editing
	SameText  int      // later posts with the same text, on the post page

	// Delegation. TargetAcct is the account posts go out as.
	DelegationOn   bool
	TargetID       string
	TargetAcct     string
	Delegated      bool
	Acting         []actOption
	Granted        []delegation
	Received       []delegation
	MaxDelegations int

	Audit         []auditLine
	AuditAccounts []actOption
	AuditFilter   auditFilterView
	AuditActions  []string

	Releases template.HTML // release notes page
}

type postView struct {
	ID         string
	When       string
	WhenInput  string
	Text       string
	CW         string
	Visibility string
	Media      int
	LocalOnly  bool
	Expires    bool
	Markdown   bool
	Editable   bool
}

type expiryOption struct {
	Seconds int
	Label   string
	On      bool
}

type composeForm struct {
	Text       string
	CW         string
	Visibility string
	Language   string
	Reply      string
	When       string
	Timezone   string
	Plain      bool
	LocalOnly  bool
	ExpiresIn  int
	Frequency  string
	Count      int
	Nonce      string
	Notice     string
	Edit       string
	Sensitive  bool
	Kept       []keptMedia
	Scope      string // "future" also edits the later posts with the same text
}

type keptMedia struct {
	ID   string
	Type string
	Alt  string
}

type seriesResult struct {
	When string `json:"when"`
	OK   bool   `json:"ok"`
}

func (s *server) view(r *http.Request, sess session, nav string) viewData {
	lang := language(r, s.cfg.lang)
	v := viewData{T: strs(lang), Lang: lang, Version: version, Domain: s.cfg.domain, Acct: sess.Acct, CSRF: sess.CSRF, Nav: nav,
		Markdown: s.cfg.markdown, LocalOnly: s.cfg.localOnly, Expiring: s.cfg.expiring, MaxSeries: maxSeries,
		DelegationOn: s.cfg.delegation(), TargetID: sess.TargetID, TargetAcct: sess.TargetAcct, Delegated: sess.Delegated}
	if key := r.URL.Query().Get("done"); slices.Contains(flashMessages, key) {
		v.Flash = v.T["flash_"+key]
	}
	if sess.Lost {
		v.Flash = v.T["flash_delegation_lost"]
	}
	v.Acting = s.actOptions(r.Context(), sess)
	return v
}

func (s *server) render(w http.ResponseWriter, name string, status int, v viewData) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := s.tmpl.ExecuteTemplate(w, name, v); err != nil {
		log.Printf("render %s: %v", name, err)
	}
}

func (s *server) renderError(w http.ResponseWriter, r *http.Request, status int, key string) {
	v := s.view(r, session{}, "")
	v.Message = v.T["error_"+key]
	s.render(w, "error.html", status, v)
}

func (s *server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if q := r.URL.Query().Get("lang"); q != "" {
		setLanguage(w, q)
	}
	_, ok, failed := s.current(w, r)
	if failed {
		return
	}
	if ok {
		http.Redirect(w, r, "/queue", http.StatusFound)
		return
	}
	s.render(w, "index.html", http.StatusOK, s.view(r, session{}, ""))
}

// handleNotes is public: signed-in visitors keep their menu.
func (s *server) handleNotes(w http.ResponseWriter, r *http.Request) {
	if q := r.URL.Query().Get("lang"); q != "" {
		setLanguage(w, q)
	}
	sess, ok, failed := s.current(w, r)
	if failed {
		return
	}
	if !ok {
		sess = session{}
	}
	v := s.view(r, sess, "notes")
	v.Releases = relnotes.HTML(releases)
	s.render(w, "notes.html", http.StatusOK, v)
}

func (s *server) defaults() settings {
	content := "text/plain"
	if s.cfg.markdown {
		content = "text/markdown"
	}
	return settings{Timezone: s.cfg.timezone, Visibility: "public", Language: strings.SplitN(s.cfg.lang, "-", 2)[0], ContentType: content}
}

func (s *server) userSettings(ctx context.Context, sess session) (settings, *time.Location, error) {
	v, err := s.db.settings(ctx, sess.AccountID, s.defaults())
	if err != nil {
		return v, nil, err
	}
	loc, err := time.LoadLocation(v.Timezone)
	if err != nil {
		loc = time.UTC
	}
	return v, loc, nil
}

func (s *server) describe(p scheduledStatus, loc *time.Location, layout string) postView {
	at := p.ScheduledAt.In(loc)
	return postView{
		ID: p.ID, When: at.Format(layout), WhenInput: at.Format(dateTimeForm),
		Text: p.Params.Text, CW: p.Params.SpoilerText, Visibility: p.Params.Visibility, Media: len(p.Media),
		LocalOnly: isSet(p.Params.LocalOnly), Expires: isSet(p.Params.ExpiresIn), Markdown: p.Params.ContentType == "text/markdown",
		Editable: editable(p),
	}
}

// isSet reads params that Mastodon stores as sent: strings, numbers or booleans.
func isSet(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case string:
		return x != "" && x != "false" && x != "0"
	default:
		return true
	}
}

func (s *server) instanceFailed(w http.ResponseWriter, r *http.Request, sess session, err error) {
	var api *apiError
	if errors.As(err, &api) && api.Status == http.StatusUnauthorized && sess.Delegated {
		// The owner's token is gone, not this session: drop the delegation only.
		s.dropLost(r.Context(), sess)
		http.Redirect(w, r, "/queue?done=delegation_lost", http.StatusSeeOther)
		return
	}
	if errors.As(err, &api) && api.Status == http.StatusUnauthorized {
		// The token was revoked on the instance: this session is useless.
		s.clearCookie(w, sessionCookie)
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	if errors.As(err, &api) && api.Status == http.StatusNotFound {
		s.renderError(w, r, http.StatusNotFound, "not_found")
		return
	}
	log.Printf("instance: %v", err)
	s.renderError(w, r, http.StatusBadGateway, "instance")
}

func (s *server) handleQueue(w http.ResponseWriter, r *http.Request, sess session) {
	st, loc, err := s.userSettings(r.Context(), sess)
	if err != nil {
		log.Printf("settings: %v", err)
		s.renderError(w, r, http.StatusServiceUnavailable, "unavailable")
		return
	}
	posts, err := s.api.scheduled(r.Context(), sess.ActToken)
	if err != nil {
		s.instanceFailed(w, r, sess, err)
		return
	}
	sort.Slice(posts, func(i, j int) bool { return posts[i].ScheduledAt.Before(posts[j].ScheduledAt) })
	v := s.view(r, sess, "queue")
	v.Settings = st
	v.Remaining = totalLimit - len(posts)
	if q := r.URL.Query().Get("error"); q == "none_selected" || q == "cancel_partial" {
		v.Error = v.T["form_"+q]
	}
	for _, p := range posts {
		v.Posts = append(v.Posts, s.describe(p, loc, v.T["date_format"]))
	}
	s.render(w, "queue.html", http.StatusOK, v)
}

func (s *server) handleCalendar(w http.ResponseWriter, r *http.Request, sess session) {
	st, loc, err := s.userSettings(r.Context(), sess)
	if err != nil {
		log.Printf("settings: %v", err)
		s.renderError(w, r, http.StatusServiceUnavailable, "unavailable")
		return
	}
	posts, err := s.api.scheduled(r.Context(), sess.ActToken)
	if err != nil {
		s.instanceFailed(w, r, sess, err)
		return
	}
	now := time.Now().In(loc)
	anchor := now
	if q := r.URL.Query().Get("date"); q != "" {
		if d, err := time.ParseInLocation(time.DateOnly, q, loc); err == nil {
			anchor = d
		}
	}
	view := r.URL.Query().Get("view")
	if view != "week" {
		view = "month"
	}
	v := s.view(r, sess, "calendar")
	v.Settings = st
	v.View = view
	v.Calendar = buildCalendar(view, anchor, now, posts)
	v.Weekdays = strings.Split(v.T["weekdays"], ",")
	if view == "month" {
		first := time.Date(anchor.Year(), anchor.Month(), 1, 0, 0, 0, 0, loc)
		v.Heading = strings.Split(v.T["months"], ",")[anchor.Month()-1] + " " + strconv.Itoa(anchor.Year())
		v.Prev, v.Next = first.AddDate(0, -1, 0).Format(time.DateOnly), first.AddDate(0, 1, 0).Format(time.DateOnly)
	} else {
		start := weekStart(anchor)
		v.Heading = start.Format(v.T["day_format"]) + " – " + start.AddDate(0, 0, 6).Format(v.T["day_format"])
		v.Prev, v.Next = start.AddDate(0, 0, -7).Format(time.DateOnly), start.AddDate(0, 0, 7).Format(time.DateOnly)
	}
	s.render(w, "calendar.html", http.StatusOK, v)
}

func (s *server) composeView(r *http.Request, sess session, st settings, form composeForm) viewData {
	v := s.view(r, sess, "new")
	v.Settings = st
	v.Limits = s.api.instanceLimits(r.Context())
	v.Form = form
	v.Now = time.Now().Format(dateTimeForm)
	v.Zones = zoneOptions(form.Timezone, time.Now())
	for i := range v.Limits.MaxMedia - len(form.Kept) {
		v.MaxMedia = append(v.MaxMedia, i)
	}
	for _, sec := range v.Limits.Expirations {
		v.Expires = append(v.Expires, expiryOption{Seconds: sec, Label: expiryLabel(sec, v.T), On: sec == form.ExpiresIn})
	}
	if form.Edit != "" {
		if posts, err := s.api.scheduled(r.Context(), sess.ActToken); err == nil {
			if i := slices.IndexFunc(posts, func(p scheduledStatus) bool { return p.ID == form.Edit }); i >= 0 {
				loc, err := time.LoadLocation(form.Timezone)
				if err != nil {
					loc = time.UTC
				}
				for _, sib := range siblings(posts, posts[i]) {
					v.Siblings = append(v.Siblings, sib.ScheduledAt.In(loc).Format(v.T["date_format"]))
				}
			}
		}
	}
	return v
}

// siblings are the later posts with exactly the same text as p: a series made
// here, or repeated by hand. Edits may apply to them too.
func siblings(posts []scheduledStatus, p scheduledStatus) []scheduledStatus {
	var out []scheduledStatus
	if strings.TrimSpace(p.Params.Text) == "" {
		return out
	}
	for _, x := range posts {
		if x.ID != p.ID && x.Params.Text == p.Params.Text && x.ScheduledAt.After(p.ScheduledAt) && editable(x) {
			out = append(out, x)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ScheduledAt.Before(out[j].ScheduledAt) })
	if len(out) > maxSeries {
		out = out[:maxSeries]
	}
	return out
}

func expiryLabel(sec int, t map[string]string) string {
	if sec%3600 == 0 {
		return strconv.Itoa(sec/3600) + " " + t["hours"]
	}
	return strconv.Itoa(sec/60) + " " + t["minutes"]
}

func (s *server) handleNew(w http.ResponseWriter, r *http.Request, sess session) {
	st, _, err := s.userSettings(r.Context(), sess)
	if err != nil {
		log.Printf("settings: %v", err)
		s.renderError(w, r, http.StatusServiceUnavailable, "unavailable")
		return
	}
	form := composeForm{Visibility: st.Visibility, Language: st.Language, Timezone: st.Timezone,
		Plain: st.ContentType != "text/markdown", Frequency: "once", Count: 6, Nonce: randomToken()}
	if from := r.URL.Query().Get("from"); from != "" {
		p, err := s.api.scheduledByID(r.Context(), sess.ActToken, from)
		if err != nil {
			s.instanceFailed(w, r, sess, err)
			return
		}
		form.prefill(p)
		if len(p.Media) > 0 {
			form.Notice = "duplicate_media"
		}
	}
	v := s.composeView(r, sess, st, form)
	if form.Notice != "" {
		v.Error = v.T["notice_"+form.Notice]
	}
	s.render(w, "new.html", http.StatusOK, v)
}

// prefill copies a scheduled post into the form, for Duplicate and Edit.
func (f *composeForm) prefill(p scheduledStatus) {
	f.Text, f.CW = p.Params.Text, p.Params.SpoilerText
	if slices.Contains(visibilities, p.Params.Visibility) {
		f.Visibility = p.Params.Visibility
	}
	if p.Params.InReplyToID != nil {
		f.Reply = fmt.Sprint(p.Params.InReplyToID)
	}
	f.Plain = p.Params.ContentType != "text/markdown"
	f.LocalOnly = isSet(p.Params.LocalOnly)
	f.Sensitive = isSet(p.Params.Sensitive)
	if p.Params.Language != "" {
		f.Language = p.Params.Language
	}
	if n, err := strconv.Atoi(fmt.Sprint(p.Params.ExpiresIn)); err == nil {
		f.ExpiresIn = n
	}
}

// editable leaves out what this composer can't reproduce, such as polls and quotes
// created in another app: recreating those posts would lose them.
func editable(p scheduledStatus) bool {
	return p.Params.Poll == nil && p.Params.QuotedID == nil
}

func (s *server) editForm(st settings, p scheduledStatus, loc *time.Location) composeForm {
	form := composeForm{Language: st.Language, Timezone: loc.String(), Visibility: st.Visibility,
		Frequency: "once", Count: 6, Nonce: randomToken(), Edit: p.ID, When: p.ScheduledAt.In(loc).Format(dateTimeForm)}
	form.prefill(p)
	for _, m := range p.Media {
		form.Kept = append(form.Kept, keptMedia{ID: m.ID, Type: m.Type, Alt: m.Description})
	}
	return form
}

func (s *server) handleEdit(w http.ResponseWriter, r *http.Request, sess session) {
	st, loc, err := s.userSettings(r.Context(), sess)
	if err != nil {
		log.Printf("settings: %v", err)
		s.renderError(w, r, http.StatusServiceUnavailable, "unavailable")
		return
	}
	p, err := s.api.scheduledByID(r.Context(), sess.ActToken, r.PathValue("id"))
	if err != nil {
		s.instanceFailed(w, r, sess, err)
		return
	}
	if !editable(p) {
		s.renderError(w, r, http.StatusConflict, "not_editable")
		return
	}
	s.render(w, "new.html", http.StatusOK, s.composeView(r, sess, st, s.editForm(st, p, loc)))
}

// saveEdit replaces a scheduled post. Mastodon can't change one, so it creates the
// new post first and only then cancels the old: if anything fails, the old one is
// still in the queue. A media attachment moves to the new post when it is reused.
// keepMedia says whether an attachment of the old post moves to the new one, and
// with which description.
type keepMedia func(m mediaAttachment) (keep bool, alt string)

func formKeep(r *http.Request) keepMedia {
	return func(m mediaAttachment) (bool, string) {
		return r.PostForm.Get("keep_"+m.ID) != "", strings.TrimSpace(r.PostForm.Get("keep_alt_" + m.ID))
	}
}

// keepAll is for the later posts of an edit: each keeps its own attachments.
func keepAll(m mediaAttachment) (bool, string) { return true, m.Description }

func (s *server) saveEdit(ctx context.Context, sess session, keep keepMedia, old scheduledStatus, status newStatus, files []upload, key string) error {
	before, at := old.ScheduledAt, status.ScheduledAt
	entry := auditEntry{OpID: randomToken()[:22], ActorID: sess.AccountID, ActorAcct: sess.Acct, TargetID: sess.TargetID, TargetAcct: sess.TargetAcct,
		Action: actionEdit, ScheduledID: old.ID, Before: &before, After: &at, Result: resultIntent, Excerpt: status.Text}
	if err := s.db.audit(ctx, entry); err != nil {
		return fmt.Errorf("audit: %w", err)
	}
	for _, m := range old.Media {
		if k, _ := keep(m); k {
			status.MediaIDs = append(status.MediaIDs, m.ID)
		}
	}
	id, err := s.sendPost(ctx, sess, status, files, key)
	if err != nil {
		entry.Result = outcome(err)
		if errors.Is(err, errPublishedNow) {
			entry.ScheduledID, entry.Result = id, resultPublished
		}
		s.auditResult(ctx, entry)
		return err
	}
	entry.ScheduledID, entry.Result = id, resultOK
	s.auditResult(ctx, entry)
	for _, m := range old.Media {
		k, alt := keep(m)
		if !k {
			continue
		}
		if alt != m.Description {
			if err := s.api.describeMedia(ctx, sess.ActToken, m.ID, alt); err != nil {
				log.Printf("edit: description: %v", err)
			}
		}
	}
	cancelEntry := auditEntry{OpID: randomToken()[:22], ActorID: sess.AccountID, ActorAcct: sess.Acct, TargetID: sess.TargetID, TargetAcct: sess.TargetAcct,
		Action: actionCancel, ScheduledID: old.ID, Before: &before, Result: resultIntent, Excerpt: old.Params.Text}
	if err := s.db.audit(ctx, cancelEntry); err != nil {
		log.Printf("audit: %v", err)
	}
	err = s.api.cancel(ctx, sess.ActToken, old.ID)
	cancelEntry.Result = outcome(err)
	s.auditResult(ctx, cancelEntry)
	if err != nil {
		return fmt.Errorf("%w: %v", errOldNotCancelled, err)
	}
	return nil
}

func (r *composeForm) load(req *http.Request) {
	r.Text = strings.TrimSpace(req.PostForm.Get("text"))
	r.CW = strings.TrimSpace(req.PostForm.Get("cw"))
	r.Visibility = req.PostForm.Get("visibility")
	r.Language = req.PostForm.Get("language")
	r.Reply = strings.TrimSpace(req.PostForm.Get("reply"))
	r.When = req.PostForm.Get("when")
	r.Timezone = req.PostForm.Get("timezone")
	r.Plain = req.PostForm.Get("plain") != ""
	r.LocalOnly = req.PostForm.Get("local_only") != ""
	r.ExpiresIn, _ = strconv.Atoi(req.PostForm.Get("expires_in"))
	r.Frequency = req.PostForm.Get("frequency")
	r.Count, _ = strconv.Atoi(req.PostForm.Get("count"))
	r.Nonce = req.PostForm.Get("nonce")
	r.Edit = req.PostForm.Get("edit_id")
	r.Sensitive = req.PostForm.Get("sensitive") != ""
	r.Scope = req.PostForm.Get("scope")
}

type upload struct {
	Header *multipart.FileHeader
	Alt    string
}

func collectUploads(r *http.Request, max int) ([]upload, error) {
	if r.MultipartForm == nil {
		return nil, nil
	}
	var out []upload
	for i := range max {
		files := r.MultipartForm.File["media_"+strconv.Itoa(i)]
		if len(files) == 0 {
			continue
		}
		kind := files[0].Header.Get("Content-Type")
		if !slices.ContainsFunc(allowedMedia, func(p string) bool { return strings.HasPrefix(kind, p) }) {
			return nil, errors.New("bad_media")
		}
		out = append(out, upload{Header: files[0], Alt: strings.TrimSpace(r.PostForm.Get("alt_" + strconv.Itoa(i)))})
	}
	if len(r.MultipartForm.File) > max {
		return nil, errors.New("too_many_media")
	}
	return out, nil
}

func (s *server) validate(f *composeForm, lim limits, files []upload) (newStatus, []time.Time, error) {
	var zero newStatus
	if f.Text == "" && len(files)+len(f.Kept) == 0 {
		return zero, nil, errors.New("empty")
	}
	if len(files)+len(f.Kept) > lim.MaxMedia {
		return zero, nil, errors.New("too_many_media")
	}
	if f.Edit != "" {
		f.Frequency = "once"
	}
	if !slices.Contains(visibilities, f.Visibility) {
		return zero, nil, errors.New("bad_visibility")
	}
	if f.Language != "" && !langPattern.MatchString(f.Language) {
		return zero, nil, errors.New("bad_language")
	}
	if len(f.Nonce) < 20 || len(f.Nonce) > 64 {
		return zero, nil, errors.New("bad_form")
	}
	loc, err := time.LoadLocation(f.Timezone)
	if err != nil {
		return zero, nil, errors.New("bad_timezone")
	}
	first, err := parseLocal(f.When, loc)
	if err != nil {
		return zero, nil, err
	}
	dates, err := series(first, f.Frequency, f.Count)
	if err != nil {
		return zero, nil, err
	}
	if len(dates)*len(files) > maxMediaUploads {
		return zero, nil, errors.New("too_many_uploads")
	}
	reply, err := replyID(f.Reply, s.cfg.instance)
	if err != nil {
		return zero, nil, errBadReply
	}
	status := newStatus{Text: f.Text, SpoilerText: f.CW, Visibility: f.Visibility, Language: f.Language, InReplyToID: reply, Sensitive: f.Sensitive}
	if s.cfg.markdown && !f.Plain {
		status.ContentType = "text/markdown"
	}
	if s.cfg.localOnly {
		status.LocalOnly = f.LocalOnly
	}
	if f.ExpiresIn != 0 {
		if !s.cfg.expiring || !slices.Contains(lim.Expirations, f.ExpiresIn) {
			return zero, nil, errors.New("bad_expiry")
		}
		status.ExpiresIn = f.ExpiresIn
	}
	return status, dates, nil
}

// acquireUpload bounds what uploads use: small forms never come here, one account
// holds at most one slot, and waiting is short so nobody queues behind a slow body.
func (s *server) acquireUpload(ctx context.Context, account string) (func(), bool) {
	s.uploadMu.Lock()
	busy := s.uploading[account]
	if !busy {
		s.uploading[account] = true
	}
	s.uploadMu.Unlock()
	if busy {
		return nil, false
	}
	free := func() {
		s.uploadMu.Lock()
		delete(s.uploading, account)
		s.uploadMu.Unlock()
	}
	select {
	case s.uploads <- struct{}{}:
		return func() { <-s.uploads; free() }, true
	case <-time.After(uploadWait):
	case <-ctx.Done():
	}
	free()
	return nil, false
}

// handleCreate takes the multipart form, so it does its own guarding: the session
// and origin are checked before the body is read, the CSRF token after.
func (s *server) handleCreate(w http.ResponseWriter, r *http.Request) {
	if !sameSite(r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	sess, ok, failed := s.current(w, r)
	if failed {
		return
	}
	if !ok {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	limit := int64(formMemory)
	if r.ContentLength < 0 || r.ContentLength > formMemory {
		release, ok := s.acquireUpload(r.Context(), sess.AccountID)
		if !ok {
			s.renderError(w, r, http.StatusServiceUnavailable, "busy")
			return
		}
		defer release()
		limit = s.cfg.maxUpload
	}
	// A stalled upload can't hold a slot: the body has a deadline, the handler another.
	if err := http.NewResponseController(w).SetReadDeadline(time.Now().Add(uploadRead)); err != nil {
		log.Printf("read deadline: %v", err)
	}
	ctx, cancel := context.WithTimeout(r.Context(), createTimeout)
	defer cancel()
	r = r.WithContext(ctx)
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	if err := r.ParseMultipartForm(formMemory); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			s.renderError(w, r, http.StatusRequestEntityTooLarge, "too_large")
			return
		}
		s.renderError(w, r, http.StatusBadRequest, "bad_form")
		return
	}
	// The multipart parse ran on this context-bound copy, so the net/http server's
	// own cleanup never removes the temp files it created for uploads over 1 MiB.
	// Remove them here, or every such upload leaks a file into the tmpfs /tmp.
	defer func() {
		if r.MultipartForm != nil {
			r.MultipartForm.RemoveAll()
		}
	}()
	if !csrfOK(sess, r.PostForm.Get("csrf")) {
		s.renderError(w, r, http.StatusForbidden, "expired")
		return
	}
	if !sameTarget(r, sess) {
		s.renderError(w, r, http.StatusConflict, "account_changed")
		return
	}
	st, _, err := s.userSettings(r.Context(), sess)
	if err != nil {
		log.Printf("settings: %v", err)
		s.renderError(w, r, http.StatusServiceUnavailable, "unavailable")
		return
	}
	var form composeForm
	form.load(r)
	lim := s.api.instanceLimits(r.Context())
	fail := func(status int, key string) {
		v := s.composeView(r, sess, st, form)
		v.Error = v.T["form_"+key]
		if v.Error == "" {
			v.Error = v.T["form_invalid"]
		}
		s.render(w, "new.html", status, v)
	}
	files, err := collectUploads(r, lim.MaxMedia)
	if err != nil {
		fail(http.StatusUnprocessableEntity, err.Error())
		return
	}
	var old scheduledStatus
	if form.Edit != "" {
		// Read with this session's token: another account's post answers 404.
		old, err = s.api.scheduledByID(r.Context(), sess.ActToken, form.Edit)
		if err != nil {
			s.instanceFailed(w, r, sess, err)
			return
		}
		if !editable(old) {
			s.renderError(w, r, http.StatusConflict, "not_editable")
			return
		}
		for _, m := range old.Media {
			if r.PostForm.Get("keep_"+m.ID) != "" {
				form.Kept = append(form.Kept, keptMedia{ID: m.ID, Type: m.Type, Alt: strings.TrimSpace(r.PostForm.Get("keep_alt_" + m.ID))})
			}
		}
	}
	status, dates, err := s.validate(&form, lim, files)
	if err != nil {
		fail(http.StatusUnprocessableEntity, err.Error())
		return
	}
	existing, err := s.api.scheduled(r.Context(), sess.ActToken)
	if err != nil {
		s.instanceFailed(w, r, sess, err)
		return
	}
	existing = slices.DeleteFunc(existing, func(x scheduledStatus) bool { return x.ID == form.Edit })
	if err := checkDates(dates, existing, time.Now()); err != nil {
		fail(http.StatusUnprocessableEntity, err.Error())
		return
	}
	// A resent form must not upload everything again.
	var fresh bool
	fresh, err = s.rdb.SetNX(r.Context(), formKey+hashID(sess.AccountID+":"+form.Nonce), 1, resultTTL).Result()
	if err != nil {
		log.Printf("form: %v", err)
		s.renderError(w, r, http.StatusServiceUnavailable, "unavailable")
		return
	}
	if !fresh {
		http.Redirect(w, r, s.resultPath(sess, form.Nonce), http.StatusSeeOther)
		return
	}
	if form.Edit != "" {
		status.ScheduledAt = dates[0]
		err := s.saveEdit(r.Context(), sess, formKeep(r), old, status, files, form.Nonce+":0")
		if err == nil && form.Scope == "future" {
			s.editLater(w, r, sess, old, status, existing, form.Nonce)
			return
		}
		if err == nil {
			http.Redirect(w, r, "/queue?done=edited", http.StatusSeeOther)
			return
		}
		log.Printf("edit: %v", err)
		// Let the form be sent again.
		s.rdb.Del(context.WithoutCancel(r.Context()), formKey+hashID(sess.AccountID+":"+form.Nonce))
		form.Nonce = randomToken()
		switch {
		case errors.Is(err, errOldNotCancelled):
			fail(http.StatusOK, "old_not_cancelled")
		case errors.Is(err, errPublishedNow):
			fail(http.StatusOK, "published_now")
		case errors.Is(err, errTooSoon):
			fail(http.StatusOK, "stale")
		default:
			fail(http.StatusBadGateway, "edit_failed")
		}
		return
	}
	seriesID := ""
	if len(dates) > 1 {
		seriesID = randomToken()[:16]
	}
	loc, _ := time.LoadLocation(form.Timezone)
	v := s.view(r, sess, "new")
	stopped := false
	for i, at := range dates {
		res := seriesResult{When: at.In(loc).Format(v.T["date_format"])}
		if stopped {
			v.Results = append(v.Results, res)
			continue
		}
		next := status
		next.ScheduledAt = at
		if err := s.createOne(r.Context(), sess, next, files, form.Nonce+":"+strconv.Itoa(i), seriesID); err != nil {
			log.Printf("schedule: %v", err)
			stopped = true
			v.Error = v.T["form_stopped"]
			var api *apiError
			if errors.As(err, &api) && api.Message != "" {
				v.Error += " " + api.Message
			}
			if errors.Is(err, errPublishedNow) {
				v.Error = v.T["form_published_now"]
			}
			if errors.Is(err, errTooSoon) {
				v.Error = v.T["form_stale"]
			}
			v.Results = append(v.Results, res)
			continue
		}
		res.OK = true
		v.Results = append(v.Results, res)
	}
	if !stopped && len(dates) == 1 {
		http.Redirect(w, r, "/queue?done=created", http.StatusSeeOther)
		return
	}
	s.saveResult(r.Context(), sess, form.Nonce, resultPage{Error: v.Error, Results: v.Results})
	http.Redirect(w, r, s.resultPath(sess, form.Nonce), http.StatusSeeOther)
}

// editLater applies an edit to the later posts with the same text. Each keeps its
// own date and attachments; new files only went to the post being edited.
func (s *server) editLater(w http.ResponseWriter, r *http.Request, sess session, old scheduledStatus, status newStatus, existing []scheduledStatus, nonce string) {
	sibs := siblings(append(existing, old), old)
	if len(sibs) == 0 {
		http.Redirect(w, r, "/queue?done=edited", http.StatusSeeOther)
		return
	}
	v := s.view(r, sess, "new")
	_, loc, err := s.userSettings(r.Context(), sess)
	if err != nil {
		loc = time.UTC
	}
	failed := 0
	for i, sib := range sibs {
		next := status
		next.ScheduledAt, next.MediaIDs = sib.ScheduledAt, nil
		res := seriesResult{When: sib.ScheduledAt.In(loc).Format(v.T["date_format"]), OK: true}
		if err := s.saveEdit(r.Context(), sess, keepAll, sib, next, nil, nonce+":"+strconv.Itoa(i+1)); err != nil {
			log.Printf("edit later: %v", err)
			res.OK = false
			failed++
		}
		v.Results = append(v.Results, res)
	}
	if failed == 0 {
		http.Redirect(w, r, "/queue?done=edited_all", http.StatusSeeOther)
		return
	}
	s.saveResult(r.Context(), sess, nonce, resultPage{Error: v.T["form_later_partial"], Results: v.Results})
	http.Redirect(w, r, s.resultPath(sess, nonce), http.StatusSeeOther)
}

type resultPage struct {
	Error   string         `json:"error"`
	Results []seriesResult `json:"results"`
}

func (s *server) resultPath(sess session, nonce string) string {
	return "/result/" + url.PathEscape(hashID(sess.AccountID+":"+nonce))
}

func (s *server) saveResult(ctx context.Context, sess session, nonce string, page resultPage) {
	raw, err := json.Marshal(page)
	if err == nil {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		err = s.rdb.Set(ctx, resultKey+hashID(sess.AccountID+":"+nonce), raw, resultTTL).Err()
	}
	if err != nil {
		log.Printf("result: %v", err)
	}
}

// handleResult needs the id derived from the account, so it shows only your own series.
func (s *server) handleResult(w http.ResponseWriter, r *http.Request, sess session) {
	raw, err := s.rdb.Get(r.Context(), resultKey+r.PathValue("id")).Bytes()
	if err != nil {
		http.Redirect(w, r, "/queue", http.StatusSeeOther)
		return
	}
	var page resultPage
	if err := json.Unmarshal(raw, &page); err != nil {
		http.Redirect(w, r, "/queue", http.StatusSeeOther)
		return
	}
	v := s.view(r, sess, "new")
	v.Error, v.Results = page.Error, page.Results
	s.render(w, "result.html", http.StatusOK, v)
}

// createOne uploads a post's media and schedules it. Nothing is sent unless the
// intent was recorded first.
func (s *server) createOne(ctx context.Context, sess session, status newStatus, files []upload, key, seriesID string) error {
	at := status.ScheduledAt
	entry := auditEntry{OpID: randomToken()[:22], ActorID: sess.AccountID, ActorAcct: sess.Acct, TargetID: sess.TargetID, TargetAcct: sess.TargetAcct,
		Action: actionSchedule, SeriesID: seriesID, After: &at, Result: resultIntent, Excerpt: status.Text}
	if err := s.db.audit(ctx, entry); err != nil {
		return fmt.Errorf("audit: %w", err)
	}
	id, err := s.sendPost(ctx, sess, status, files, key)
	entry.ScheduledID, entry.Result = id, resultOK
	if errors.Is(err, errPublishedNow) {
		entry.Result = resultPublished
	} else if err != nil {
		entry.Result = outcome(err)
	}
	s.auditResult(ctx, entry)
	return err
}

func (s *server) sendPost(ctx context.Context, sess session, status newStatus, files []upload, key string) (string, error) {
	for _, f := range files {
		id, err := s.sendMedia(ctx, sess, f)
		if err != nil {
			return "", err
		}
		status.MediaIDs = append(status.MediaIDs, id)
	}
	// Uploads can take minutes: never send a date that went stale meanwhile.
	if time.Until(status.ScheduledAt) < finalLead {
		return "", errTooSoon
	}
	return s.api.schedule(ctx, sess.ActToken, key, status)
}

func (s *server) sendMedia(ctx context.Context, sess session, f upload) (string, error) {
	file, err := f.Header.Open()
	if err != nil {
		return "", err
	}
	defer file.Close()
	return s.api.uploadMedia(ctx, sess.ActToken, f.Header.Filename, f.Header.Header.Get("Content-Type"), f.Alt, file)
}

// outcome tells a refusal from an answer that never came: after a timeout the
// instance may have applied the action.
func outcome(err error) string {
	var api *apiError
	switch {
	case err == nil:
		return resultOK
	case errors.Is(err, errTooSoon), errors.As(err, &api):
		return resultFailed
	default:
		return resultUnknown
	}
}

// auditResult survives the request being cancelled after the instance answered.
func (s *server) auditResult(ctx context.Context, entry auditEntry) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := s.db.audit(ctx, entry); err != nil {
		log.Printf("audit result: %v", err)
	}
}

func (s *server) handlePost(w http.ResponseWriter, r *http.Request, sess session) {
	st, loc, err := s.userSettings(r.Context(), sess)
	if err != nil {
		log.Printf("settings: %v", err)
		s.renderError(w, r, http.StatusServiceUnavailable, "unavailable")
		return
	}
	p, err := s.api.scheduledByID(r.Context(), sess.ActToken, r.PathValue("id"))
	if err != nil {
		s.instanceFailed(w, r, sess, err)
		return
	}
	v := s.view(r, sess, "queue")
	v.Settings = st
	v.Post = s.describe(p, loc, v.T["date_format"])
	if posts, err := s.api.scheduled(r.Context(), sess.ActToken); err == nil {
		v.SameText = len(sameTextLater(posts, p))
	}
	if q := r.URL.Query().Get("error"); q != "" && slices.Contains([]string{"too_soon", "daily_limit", "bad_date"}, q) {
		v.Error = v.T["form_"+q]
	}
	s.render(w, "post.html", http.StatusOK, v)
}

func (s *server) handleReschedule(w http.ResponseWriter, r *http.Request, sess session) {
	id := r.PathValue("id")
	if !sameTarget(r, sess) {
		s.renderError(w, r, http.StatusConflict, "account_changed")
		return
	}
	_, loc, err := s.userSettings(r.Context(), sess)
	if err != nil {
		log.Printf("settings: %v", err)
		s.renderError(w, r, http.StatusServiceUnavailable, "unavailable")
		return
	}
	p, err := s.api.scheduledByID(r.Context(), sess.ActToken, id)
	if err != nil {
		s.instanceFailed(w, r, sess, err)
		return
	}
	back := func(key string) {
		http.Redirect(w, r, "/post/"+id+"?error="+url.QueryEscape(key), http.StatusSeeOther)
	}
	at, err := parseLocal(r.PostForm.Get("when"), loc)
	if err != nil {
		back(errBadDate.Error())
		return
	}
	existing, err := s.api.scheduled(r.Context(), sess.ActToken)
	if err != nil {
		s.instanceFailed(w, r, sess, err)
		return
	}
	others := slices.DeleteFunc(existing, func(x scheduledStatus) bool { return x.ID == id })
	if err := checkDates([]time.Time{at}, others, time.Now()); err != nil {
		back(err.Error())
		return
	}
	before := p.ScheduledAt
	entry := auditEntry{OpID: randomToken()[:22], ActorID: sess.AccountID, ActorAcct: sess.Acct, TargetID: sess.TargetID, TargetAcct: sess.TargetAcct,
		Action: actionReschedule, ScheduledID: id, Before: &before, After: &at, Result: resultIntent, Excerpt: p.Params.Text}
	if err := s.db.audit(r.Context(), entry); err != nil {
		log.Printf("audit: %v", err)
		s.renderError(w, r, http.StatusServiceUnavailable, "unavailable")
		return
	}
	err = s.api.reschedule(r.Context(), sess.ActToken, id, at)
	entry.Result = outcome(err)
	s.auditResult(r.Context(), entry)
	if err != nil {
		s.instanceFailed(w, r, sess, err)
		return
	}
	http.Redirect(w, r, "/queue?done=rescheduled", http.StatusSeeOther)
}

func (s *server) handleCancel(w http.ResponseWriter, r *http.Request, sess session) {
	id := r.PathValue("id")
	if !sameTarget(r, sess) {
		s.renderError(w, r, http.StatusConflict, "account_changed")
		return
	}
	if r.PostForm.Get("scope") == "future" {
		posts, err := s.api.scheduled(r.Context(), sess.ActToken)
		if err != nil {
			s.instanceFailed(w, r, sess, err)
			return
		}
		i := slices.IndexFunc(posts, func(p scheduledStatus) bool { return p.ID == id })
		if i < 0 {
			s.renderError(w, r, http.StatusNotFound, "not_found")
			return
		}
		s.cancelAll(w, r, sess, append([]scheduledStatus{posts[i]}, sameTextLater(posts, posts[i])...))
		return
	}
	p, err := s.api.scheduledByID(r.Context(), sess.ActToken, id)
	if err != nil {
		s.instanceFailed(w, r, sess, err)
		return
	}
	if err := s.cancelOne(r.Context(), sess, p); err != nil {
		if errors.Is(err, errAudit) {
			s.renderError(w, r, http.StatusServiceUnavailable, "unavailable")
			return
		}
		s.instanceFailed(w, r, sess, err)
		return
	}
	http.Redirect(w, r, "/queue?done=cancelled", http.StatusSeeOther)
}

// handleCancelMany cancels the posts ticked on the queue page. IDs that are no
// longer in the queue (already published or canceled) are skipped.
func (s *server) handleCancelMany(w http.ResponseWriter, r *http.Request, sess session) {
	if !sameTarget(r, sess) {
		s.renderError(w, r, http.StatusConflict, "account_changed")
		return
	}
	ids := r.PostForm["id"]
	if len(ids) == 0 {
		http.Redirect(w, r, "/queue?error=none_selected", http.StatusSeeOther)
		return
	}
	if len(ids) > totalLimit {
		ids = ids[:totalLimit]
	}
	posts, err := s.api.scheduled(r.Context(), sess.ActToken)
	if err != nil {
		s.instanceFailed(w, r, sess, err)
		return
	}
	var picked []scheduledStatus
	for _, p := range posts {
		if slices.Contains(ids, p.ID) {
			picked = append(picked, p)
		}
	}
	s.cancelAll(w, r, sess, picked)
}

var errAudit = errors.New("audit unavailable")

// cancelOne cancels a scheduled post, with the intent logged before the call.
func (s *server) cancelOne(ctx context.Context, sess session, p scheduledStatus) error {
	before := p.ScheduledAt
	entry := auditEntry{OpID: randomToken()[:22], ActorID: sess.AccountID, ActorAcct: sess.Acct, TargetID: sess.TargetID, TargetAcct: sess.TargetAcct,
		Action: actionCancel, ScheduledID: p.ID, Before: &before, Result: resultIntent, Excerpt: p.Params.Text}
	if err := s.db.audit(ctx, entry); err != nil {
		log.Printf("audit: %v", err)
		return errAudit
	}
	err := s.api.cancel(ctx, sess.ActToken, p.ID)
	entry.Result = outcome(err)
	s.auditResult(ctx, entry)
	return err
}

// cancelAll cancels the posts one by one and stops at the first failure, so a
// revoked token or an instance outage doesn't produce a long list of errors.
func (s *server) cancelAll(w http.ResponseWriter, r *http.Request, sess session, posts []scheduledStatus) {
	done := 0
	for _, p := range posts {
		err := s.cancelOne(r.Context(), sess, p)
		var api *apiError
		if errors.As(err, &api) && api.Status == http.StatusNotFound {
			continue // published or canceled meanwhile
		}
		if err != nil {
			if done == 0 {
				if errors.Is(err, errAudit) {
					s.renderError(w, r, http.StatusServiceUnavailable, "unavailable")
					return
				}
				s.instanceFailed(w, r, sess, err)
				return
			}
			log.Printf("cancel many: %v", err)
			http.Redirect(w, r, "/queue?error=cancel_partial", http.StatusSeeOther)
			return
		}
		done++
	}
	http.Redirect(w, r, "/queue?done=cancelled_many", http.StatusSeeOther)
}

// sameTextLater lists every later post with exactly the same text as p, to
// cancel a whole series at once. Unlike siblings, it isn't capped and includes
// posts that can't be edited here.
func sameTextLater(posts []scheduledStatus, p scheduledStatus) []scheduledStatus {
	var out []scheduledStatus
	if strings.TrimSpace(p.Params.Text) == "" {
		return out
	}
	for _, x := range posts {
		if x.ID != p.ID && x.Params.Text == p.Params.Text && x.ScheduledAt.After(p.ScheduledAt) {
			out = append(out, x)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ScheduledAt.Before(out[j].ScheduledAt) })
	return out
}

func (s *server) handleSettings(w http.ResponseWriter, r *http.Request, sess session) {
	st, _, err := s.userSettings(r.Context(), sess)
	if err != nil {
		log.Printf("settings: %v", err)
		s.renderError(w, r, http.StatusServiceUnavailable, "unavailable")
		return
	}
	v := s.view(r, sess, "settings")
	v.Settings = st
	v.Zones = zoneOptions(st.Timezone, time.Now())
	s.render(w, "settings.html", http.StatusOK, v)
}

func (s *server) handleSaveSettings(w http.ResponseWriter, r *http.Request, sess session) {
	st := settings{Timezone: r.PostForm.Get("timezone"), Visibility: r.PostForm.Get("visibility"),
		Language: r.PostForm.Get("language"), ContentType: "text/plain"}
	if s.cfg.markdown && r.PostForm.Get("markdown") != "" {
		st.ContentType = "text/markdown"
	}
	_, tzErr := time.LoadLocation(st.Timezone)
	if tzErr != nil || st.Timezone == "" || !slices.Contains(visibilities, st.Visibility) || !langPattern.MatchString(st.Language) {
		v := s.view(r, sess, "settings")
		v.Settings = st
		v.Zones = zoneOptions(s.defaults().Timezone, time.Now())
		v.Error = v.T["form_invalid"]
		s.render(w, "settings.html", http.StatusUnprocessableEntity, v)
		return
	}
	if err := s.db.saveSettings(r.Context(), sess.AccountID, st); err != nil {
		log.Printf("settings: %v", err)
		s.renderError(w, r, http.StatusServiceUnavailable, "unavailable")
		return
	}
	http.Redirect(w, r, "/settings?done=saved", http.StatusSeeOther)
}

// handlePreview renders Markdown for the composer. goldmark's defaults escape raw
// HTML and drop dangerous links, so the output is safe to place in the page.
func (s *server) handlePreview(w http.ResponseWriter, r *http.Request, _ session) {
	select {
	case s.previews <- struct{}{}:
		defer func() { <-s.previews }()
	default:
		http.Error(w, "busy", http.StatusTooManyRequests)
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "too large", http.StatusRequestEntityTooLarge)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := goldmark.Convert(body, w); err != nil {
		log.Printf("preview: %v", err)
	}
}

// handleDates lists the dates a series would use, before the author confirms it.
func (s *server) handleDates(w http.ResponseWriter, r *http.Request, _ session) {
	var in struct {
		When      string `json:"when"`
		Timezone  string `json:"timezone"`
		Frequency string `json:"frequency"`
		Count     int    `json:"count"`
	}
	w.Header().Set("Content-Type", "application/json")
	out := struct {
		Dates []string `json:"dates"`
	}{Dates: []string{}}
	if json.NewDecoder(r.Body).Decode(&in) == nil {
		if loc, err := time.LoadLocation(in.Timezone); err == nil {
			if first, err := parseLocal(in.When, loc); err == nil {
				if dates, err := series(first, in.Frequency, in.Count); err == nil {
					t := strs(language(r, s.cfg.lang))
					for _, d := range dates {
						out.Dates = append(out.Dates, d.Format(t["date_format"]))
					}
				}
			}
		}
	}
	json.NewEncoder(w).Encode(out)
}

func bodyOf(form url.Values) io.Reader { return strings.NewReader(form.Encode()) }
