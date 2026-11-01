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
)

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
	flashMessages = []string{"created", "rescheduled", "cancelled", "saved"}
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
		if err := r.ParseForm(); err != nil || !csrfOK(sess, r.PostFormValue("csrf")) {
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
	mux.HandleFunc("GET /queue", s.page(s.handleQueue))
	mux.HandleFunc("GET /calendar", s.page(s.handleCalendar))
	mux.HandleFunc("GET /new", s.page(s.handleNew))
	mux.HandleFunc("POST /new", s.handleCreate)
	mux.HandleFunc("GET /result/{id}", s.page(s.handleResult))
	mux.HandleFunc("GET /post/{id}", s.page(s.handlePost))
	mux.HandleFunc("POST /post/{id}/date", s.action(s.handleReschedule))
	mux.HandleFunc("POST /post/{id}/cancel", s.action(s.handleCancel))
	mux.HandleFunc("GET /settings", s.page(s.handleSettings))
	mux.HandleFunc("POST /settings", s.action(s.handleSaveSettings))
	mux.HandleFunc("POST /preview", s.fetchAction(s.handlePreview))
	mux.HandleFunc("POST /dates", s.fetchAction(s.handleDates))
	return securityHeaders(mux)
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
		h.Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; connect-src 'self'; style-src 'self'; font-src 'self'; img-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'")
		next.ServeHTTP(w, r)
	})
}

type viewData struct {
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
}

type seriesResult struct {
	When string `json:"when"`
	OK   bool   `json:"ok"`
}

func (s *server) view(r *http.Request, sess session, nav string) viewData {
	lang := language(r, s.cfg.lang)
	v := viewData{T: strs(lang), Lang: lang, Domain: s.cfg.domain, Acct: sess.Acct, CSRF: sess.CSRF, Nav: nav,
		Markdown: s.cfg.markdown, LocalOnly: s.cfg.localOnly, Expiring: s.cfg.expiring, MaxSeries: maxSeries}
	if key := r.URL.Query().Get("done"); slices.Contains(flashMessages, key) {
		v.Flash = v.T["flash_"+key]
	}
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

func (s *server) instanceFailed(w http.ResponseWriter, r *http.Request, err error) {
	var api *apiError
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
	posts, err := s.api.scheduled(r.Context(), sess.Token)
	if err != nil {
		s.instanceFailed(w, r, err)
		return
	}
	sort.Slice(posts, func(i, j int) bool { return posts[i].ScheduledAt.Before(posts[j].ScheduledAt) })
	v := s.view(r, sess, "queue")
	v.Settings = st
	v.Remaining = totalLimit - len(posts)
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
	posts, err := s.api.scheduled(r.Context(), sess.Token)
	if err != nil {
		s.instanceFailed(w, r, err)
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
	for i := range v.Limits.MaxMedia {
		v.MaxMedia = append(v.MaxMedia, i)
	}
	for _, sec := range v.Limits.Expirations {
		v.Expires = append(v.Expires, expiryOption{Seconds: sec, Label: expiryLabel(sec, v.T), On: sec == form.ExpiresIn})
	}
	return v
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
		p, err := s.api.scheduledByID(r.Context(), sess.Token, from)
		if err != nil {
			s.instanceFailed(w, r, err)
			return
		}
		form.Text, form.CW = p.Params.Text, p.Params.SpoilerText
		if slices.Contains(visibilities, p.Params.Visibility) {
			form.Visibility = p.Params.Visibility
		}
		if p.Params.InReplyToID != nil {
			form.Reply = fmt.Sprint(p.Params.InReplyToID)
		}
		form.Plain = p.Params.ContentType != "text/markdown"
		form.LocalOnly = isSet(p.Params.LocalOnly)
		if p.Params.Language != "" {
			form.Language = p.Params.Language
		}
		if n, err := strconv.Atoi(fmt.Sprint(p.Params.ExpiresIn)); err == nil {
			form.ExpiresIn = n
		}
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

func (r *composeForm) load(req *http.Request) {
	r.Text = strings.TrimSpace(req.PostFormValue("text"))
	r.CW = strings.TrimSpace(req.PostFormValue("cw"))
	r.Visibility = req.PostFormValue("visibility")
	r.Language = req.PostFormValue("language")
	r.Reply = strings.TrimSpace(req.PostFormValue("reply"))
	r.When = req.PostFormValue("when")
	r.Timezone = req.PostFormValue("timezone")
	r.Plain = req.PostFormValue("plain") != ""
	r.LocalOnly = req.PostFormValue("local_only") != ""
	r.ExpiresIn, _ = strconv.Atoi(req.PostFormValue("expires_in"))
	r.Frequency = req.PostFormValue("frequency")
	r.Count, _ = strconv.Atoi(req.PostFormValue("count"))
	r.Nonce = req.PostFormValue("nonce")
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
		out = append(out, upload{Header: files[0], Alt: strings.TrimSpace(r.PostFormValue("alt_" + strconv.Itoa(i)))})
	}
	if len(r.MultipartForm.File) > max {
		return nil, errors.New("too_many_media")
	}
	return out, nil
}

func (s *server) validate(f *composeForm, lim limits, files []upload) (newStatus, []time.Time, error) {
	var zero newStatus
	if f.Text == "" && len(files) == 0 {
		return zero, nil, errors.New("empty")
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
	status := newStatus{Text: f.Text, SpoilerText: f.CW, Visibility: f.Visibility, Language: f.Language, InReplyToID: reply}
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
	if !csrfOK(sess, r.PostFormValue("csrf")) {
		s.renderError(w, r, http.StatusForbidden, "expired")
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
	status, dates, err := s.validate(&form, lim, files)
	if err != nil {
		fail(http.StatusUnprocessableEntity, err.Error())
		return
	}
	existing, err := s.api.scheduled(r.Context(), sess.Token)
	if err != nil {
		s.instanceFailed(w, r, err)
		return
	}
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
	entry := auditEntry{OpID: randomToken()[:22], ActorID: sess.AccountID, ActorAcct: sess.Acct, TargetID: sess.AccountID, TargetAcct: sess.Acct,
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
	return s.api.schedule(ctx, sess.Token, key, status)
}

func (s *server) sendMedia(ctx context.Context, sess session, f upload) (string, error) {
	file, err := f.Header.Open()
	if err != nil {
		return "", err
	}
	defer file.Close()
	return s.api.uploadMedia(ctx, sess.Token, f.Header.Filename, f.Header.Header.Get("Content-Type"), f.Alt, file)
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
	p, err := s.api.scheduledByID(r.Context(), sess.Token, r.PathValue("id"))
	if err != nil {
		s.instanceFailed(w, r, err)
		return
	}
	v := s.view(r, sess, "queue")
	v.Settings = st
	v.Post = s.describe(p, loc, v.T["date_format"])
	if q := r.URL.Query().Get("error"); q != "" && slices.Contains([]string{"too_soon", "daily_limit", "bad_date"}, q) {
		v.Error = v.T["form_"+q]
	}
	s.render(w, "post.html", http.StatusOK, v)
}

func (s *server) handleReschedule(w http.ResponseWriter, r *http.Request, sess session) {
	id := r.PathValue("id")
	_, loc, err := s.userSettings(r.Context(), sess)
	if err != nil {
		log.Printf("settings: %v", err)
		s.renderError(w, r, http.StatusServiceUnavailable, "unavailable")
		return
	}
	p, err := s.api.scheduledByID(r.Context(), sess.Token, id)
	if err != nil {
		s.instanceFailed(w, r, err)
		return
	}
	back := func(key string) {
		http.Redirect(w, r, "/post/"+id+"?error="+url.QueryEscape(key), http.StatusSeeOther)
	}
	at, err := parseLocal(r.PostFormValue("when"), loc)
	if err != nil {
		back(errBadDate.Error())
		return
	}
	existing, err := s.api.scheduled(r.Context(), sess.Token)
	if err != nil {
		s.instanceFailed(w, r, err)
		return
	}
	others := slices.DeleteFunc(existing, func(x scheduledStatus) bool { return x.ID == id })
	if err := checkDates([]time.Time{at}, others, time.Now()); err != nil {
		back(err.Error())
		return
	}
	before := p.ScheduledAt
	entry := auditEntry{OpID: randomToken()[:22], ActorID: sess.AccountID, ActorAcct: sess.Acct, TargetID: sess.AccountID, TargetAcct: sess.Acct,
		Action: actionReschedule, ScheduledID: id, Before: &before, After: &at, Result: resultIntent, Excerpt: p.Params.Text}
	if err := s.db.audit(r.Context(), entry); err != nil {
		log.Printf("audit: %v", err)
		s.renderError(w, r, http.StatusServiceUnavailable, "unavailable")
		return
	}
	err = s.api.reschedule(r.Context(), sess.Token, id, at)
	entry.Result = outcome(err)
	s.auditResult(r.Context(), entry)
	if err != nil {
		s.instanceFailed(w, r, err)
		return
	}
	http.Redirect(w, r, "/queue?done=rescheduled", http.StatusSeeOther)
}

func (s *server) handleCancel(w http.ResponseWriter, r *http.Request, sess session) {
	id := r.PathValue("id")
	p, err := s.api.scheduledByID(r.Context(), sess.Token, id)
	if err != nil {
		s.instanceFailed(w, r, err)
		return
	}
	before := p.ScheduledAt
	entry := auditEntry{OpID: randomToken()[:22], ActorID: sess.AccountID, ActorAcct: sess.Acct, TargetID: sess.AccountID, TargetAcct: sess.Acct,
		Action: actionCancel, ScheduledID: id, Before: &before, Result: resultIntent, Excerpt: p.Params.Text}
	if err := s.db.audit(r.Context(), entry); err != nil {
		log.Printf("audit: %v", err)
		s.renderError(w, r, http.StatusServiceUnavailable, "unavailable")
		return
	}
	err = s.api.cancel(r.Context(), sess.Token, id)
	entry.Result = outcome(err)
	s.auditResult(r.Context(), entry)
	if err != nil {
		s.instanceFailed(w, r, err)
		return
	}
	http.Redirect(w, r, "/queue?done=cancelled", http.StatusSeeOther)
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
	s.render(w, "settings.html", http.StatusOK, v)
}

func (s *server) handleSaveSettings(w http.ResponseWriter, r *http.Request, sess session) {
	st := settings{Timezone: r.PostFormValue("timezone"), Visibility: r.PostFormValue("visibility"),
		Language: r.PostFormValue("language"), ContentType: "text/plain"}
	if s.cfg.markdown && r.PostFormValue("markdown") != "" {
		st.ContentType = "text/markdown"
	}
	_, tzErr := time.LoadLocation(st.Timezone)
	if tzErr != nil || st.Timezone == "" || !slices.Contains(visibilities, st.Visibility) || !langPattern.MatchString(st.Language) {
		v := s.view(r, sess, "settings")
		v.Settings = st
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
