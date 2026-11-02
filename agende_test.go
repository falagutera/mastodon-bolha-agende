package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

func TestSeriesMonthlyKeepsDayAndClamps(t *testing.T) {
	loc, _ := time.LoadLocation("America/Sao_Paulo")
	first := time.Date(2027, 1, 31, 10, 0, 0, 0, loc)
	got, err := series(first, "monthly", 4)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"2027-01-31", "2027-02-28", "2027-03-31", "2027-04-30"}
	for i, d := range got {
		if d.Format(time.DateOnly) != want[i] || d.Hour() != 10 {
			t.Errorf("%d: got %v, want %s at 10h", i, d, want[i])
		}
	}
}

func TestSeriesDailyKeepsTimeAcrossMonths(t *testing.T) {
	loc, _ := time.LoadLocation("America/Sao_Paulo")
	first := time.Date(2027, 1, 30, 10, 0, 0, 0, loc)
	got, err := series(first, "daily", 5)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"2027-01-30", "2027-01-31", "2027-02-01", "2027-02-02", "2027-02-03"}
	if len(got) != len(want) {
		t.Fatalf("got %d dates, want %d", len(got), len(want))
	}
	for i, d := range got {
		if d.Format(time.DateOnly) != want[i] || d.Hour() != 10 || d.Minute() != 0 {
			t.Errorf("%d: got %v, want %s at 10:00", i, d, want[i])
		}
	}
}

// A daily series follows the wall clock. On the day a zone skips the chosen
// time, Go moves the post back by the offset change instead of skipping the day.
func TestSeriesDailyAcrossDSTGap(t *testing.T) {
	loc, _ := time.LoadLocation("America/New_York")
	first := time.Date(2027, 3, 13, 2, 30, 0, 0, loc)
	got, err := series(first, "daily", 3)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"2027-03-13 02:30", "2027-03-14 01:30", "2027-03-15 02:30"}
	for i, d := range got {
		if s := d.Format("2006-01-02 15:04"); s != want[i] {
			t.Errorf("%d: got %s, want %s", i, s, want[i])
		}
	}
}

func TestSeriesRejectsBadInput(t *testing.T) {
	now := time.Now()
	for _, c := range []struct {
		freq  string
		count int
	}{{"monthly", 1}, {"monthly", maxSeries + 1}, {"hourly", 3}, {"yearly", 3}, {"monthly", -2}} {
		if _, err := series(now, c.freq, c.count); err == nil {
			t.Errorf("%v accepted", c)
		}
	}
	if got, err := series(now, "once", 0); err != nil || len(got) != 1 {
		t.Errorf("once: %v %v", got, err)
	}
}

func TestCheckDates(t *testing.T) {
	now := time.Date(2027, 3, 1, 12, 0, 0, 0, time.UTC)
	if err := checkDates([]time.Time{now.Add(5 * time.Minute)}, nil, now); err != errTooSoon {
		t.Errorf("5 min: %v", err)
	}
	if err := checkDates([]time.Time{now.Add(-time.Hour)}, nil, now); err != errTooSoon {
		t.Errorf("past: %v", err)
	}
	day := now.AddDate(0, 0, 2)
	var full []scheduledStatus
	for range dailyLimit {
		full = append(full, scheduledStatus{ScheduledAt: day})
	}
	if err := checkDates([]time.Time{day}, full, now); err != errDailyLimit {
		t.Errorf("daily: %v", err)
	}
	if err := checkDates([]time.Time{day.AddDate(0, 0, 1)}, full, now); err != nil {
		t.Errorf("next day: %v", err)
	}
	many := make([]scheduledStatus, totalLimit)
	if err := checkDates([]time.Time{day}, many, now); err != errTotalLimit {
		t.Errorf("total: %v", err)
	}
}

func TestReplyID(t *testing.T) {
	inst := "https://example.social"
	for in, want := range map[string]string{
		"":                  "",
		"123":               "123",
		inst + "/@ana/456":  "456",
		inst + "/@ana/456/": "456",
	} {
		if got, err := replyID(in, inst); err != nil || got != want {
			t.Errorf("%q: %q %v", in, got, err)
		}
	}
	for _, in := range []string{"https://evil.example/@a/1", inst + ".evil.example/@a/1", "abc", "1/../2"} {
		if _, err := replyID(in, inst); err == nil {
			t.Errorf("%q accepted", in)
		}
	}
}

func TestBoxBindsOwner(t *testing.T) {
	b, _ := newBox(strings.Repeat("k", 32), "test")
	sealed := b.seal([]byte("secret"), []byte("a"))
	if got, err := b.open(sealed, []byte("a")); err != nil || string(got) != "secret" {
		t.Fatalf("open: %q %v", got, err)
	}
	if _, err := b.open(sealed, []byte("b")); err == nil {
		t.Error("opened with another owner")
	}
	other, _ := newBox(strings.Repeat("k", 32), "other")
	if _, err := other.open(sealed, []byte("a")); err == nil {
		t.Error("opened with another purpose")
	}
}

func TestCalendarMonth(t *testing.T) {
	anchor := time.Date(2027, 2, 15, 0, 0, 0, 0, time.UTC)
	post := scheduledStatus{ID: "9", ScheduledAt: time.Date(2027, 2, 5, 23, 30, 0, 0, time.UTC)}
	post.Params.Text = "hello"
	c := buildCalendar("month", anchor, anchor, []scheduledStatus{post})
	found := false
	for _, week := range c.Weeks {
		if len(week) != 7 {
			t.Fatalf("week of %d", len(week))
		}
		for _, d := range week {
			if len(d.Items) > 0 {
				found = d.Date.Day() == 5 && d.Items[0].Time == "23:30"
			}
		}
	}
	if !found {
		t.Error("post not on its day")
	}
	if c.Weeks[0][0].Date.Weekday() != time.Sunday {
		t.Error("weeks must start on Sunday")
	}
	loc := time.FixedZone("x", -4*3600)
	c = buildCalendar("week", time.Date(2027, 2, 6, 12, 0, 0, 0, loc), time.Now(), []scheduledStatus{
		{ID: "1", ScheduledAt: time.Date(2027, 2, 6, 2, 0, 0, 0, time.UTC)},
	})
	if len(c.Weeks) != 1 || len(c.Weeks[0][5].Items) != 1 {
		t.Errorf("post must follow the zone: %+v", c.Weeks)
	}
}

// fakeMastodon is the instance's API with two accounts, one per token.
type fakeMastodon struct {
	mu        sync.Mutex
	scheduled map[string]map[string]any
	owner     map[string]string
	next      int
	forms     []url.Values
	media     int
	revoked   []string
	alts      []string
}

var tokenOwner = map[string]string{"token-ana": "1", "token-bia": "2"}

func newFakeMastodon() (*fakeMastodon, *httptest.Server) {
	f := &fakeMastodon{scheduled: map[string]map[string]any{}, owner: map[string]string{}, next: 100}
	mux := http.NewServeMux()
	who := func(r *http.Request) string {
		return tokenOwner[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")]
	}
	mux.HandleFunc("POST /oauth/token", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"access_token":%q}`, "token-"+r.FormValue("code"))
	})
	mux.HandleFunc("POST /oauth/revoke", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.revoked = append(f.revoked, r.FormValue("token"))
		f.mu.Unlock()
	})
	mux.HandleFunc("GET /api/v2/instance", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"configuration":{"statuses":{"max_characters":1200,"max_media_attachments":4,"expirations":[900,3600]}}}`)
	})
	mux.HandleFunc("PUT /api/v1/media/{id}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.alts = append(f.alts, r.FormValue("description"))
		fmt.Fprint(w, `{}`)
	})
	mux.HandleFunc("GET /api/v1/accounts/verify_credentials", func(w http.ResponseWriter, r *http.Request) {
		id := who(r)
		fmt.Fprintf(w, `{"id":%q,"username":"user%s"}`, id, id)
	})
	mux.HandleFunc("POST /api/v2/media", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.media++
		fmt.Fprintf(w, `{"id":"%d","type":"image"}`, f.media)
	})
	mux.HandleFunc("POST /api/v1/statuses", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		f.mu.Lock()
		defer f.mu.Unlock()
		f.forms = append(f.forms, r.PostForm)
		f.next++
		id := strconv.Itoa(f.next)
		if r.PostFormValue("status") == "REFUSE" {
			w.WriteHeader(http.StatusUnprocessableEntity)
			fmt.Fprint(w, `{"error":"refused"}`)
			return
		}
		media := []any{}
		for _, m := range r.PostForm["media_ids[]"] {
			media = append(media, map[string]any{"id": m, "type": "image", "description": "alt"})
		}
		item := map[string]any{"id": id, "scheduled_at": r.PostFormValue("scheduled_at"), "params": map[string]any{"text": r.PostFormValue("status"), "visibility": r.PostFormValue("visibility")}, "media_attachments": media}
		f.scheduled[id] = item
		f.owner[id] = who(r)
		json.NewEncoder(w).Encode(item)
	})
	mux.HandleFunc("GET /api/v1/scheduled_statuses", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		list := []map[string]any{}
		for id, item := range f.scheduled {
			if f.owner[id] == who(r) {
				list = append(list, item)
			}
		}
		json.NewEncoder(w).Encode(list)
	})
	one := func(w http.ResponseWriter, r *http.Request) map[string]any {
		id := r.PathValue("id")
		item, ok := f.scheduled[id]
		if !ok || f.owner[id] != who(r) {
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"error":"Record not found"}`)
			return nil
		}
		return item
	}
	mux.HandleFunc("GET /api/v1/scheduled_statuses/{id}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if item := one(w, r); item != nil {
			json.NewEncoder(w).Encode(item)
		}
	})
	mux.HandleFunc("PUT /api/v1/scheduled_statuses/{id}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if item := one(w, r); item != nil {
			item["scheduled_at"] = r.FormValue("scheduled_at")
			json.NewEncoder(w).Encode(item)
		}
	})
	mux.HandleFunc("DELETE /api/v1/scheduled_statuses/{id}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if item := one(w, r); item != nil {
			delete(f.scheduled, r.PathValue("id"))
			fmt.Fprint(w, `{}`)
		}
	})
	return f, httptest.NewServer(mux)
}

// testApp needs Postgres and Redis: set AGENDE_TEST_DATABASE and AGENDE_TEST_REDIS.
func testApp(t *testing.T) (*fakeMastodon, *httptest.Server, *store) {
	t.Helper()
	dsn, addr := os.Getenv("AGENDE_TEST_DB_HOST"), os.Getenv("AGENDE_TEST_REDIS")
	if dsn == "" || addr == "" {
		t.Skip("AGENDE_TEST_DB_HOST and AGENDE_TEST_REDIS not set")
	}
	f, inst := newFakeMastodon()
	t.Cleanup(inst.Close)
	cfg := config{instance: inst.URL, domain: "example.social", baseURL: "https://agende.example.social", clientID: "id", clientSecret: "secret",
		lang: "en", timezone: "UTC", markdown: true, localOnly: true, expiring: true, maxUpload: 8 << 20,
		dbHost: dsn, dbPort: env("AGENDE_TEST_DB_PORT", "5432"), dbUser: "agende", dbPassword: "agende", dbName: "agende", dbSSLMode: "disable"}
	key := strings.Repeat("k", 32)
	auditBox, _ := newBox(key, "audit")
	tokenBox, _ := newBox(key, "session-token")
	db, err := openStore(context.Background(), cfg, auditBox)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.check(context.Background()); err != nil {
		t.Fatal(err)
	}
	rdb := redis.NewClient(&redis.Options{Addr: addr})
	rdb.FlushDB(context.Background())
	tmpl, err := parseTemplates()
	if err != nil {
		t.Fatal(err)
	}
	api := &mastodon{base: inst.URL, client: inst.Client(), upload: inst.Client()}
	s := newServer(cfg, tmpl, api, db, &sessions{rdb: rdb, box: tokenBox}, rdb)
	app := httptest.NewServer(s.routes())
	t.Cleanup(app.Close)
	return f, app, db
}

type browser struct {
	t      *testing.T
	app    string
	cookie string
	csrf   string
}

var csrfField = regexp.MustCompile(`name="csrf" value="([^"]+)"`)

func (b *browser) do(method, path, kind string, body io.Reader) (int, string, string) {
	b.t.Helper()
	req, _ := http.NewRequest(method, b.app+path, body)
	if kind != "" {
		req.Header.Set("Content-Type", kind)
	}
	if b.cookie != "" {
		req.Header.Set("Cookie", b.cookie)
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		b.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	for _, c := range resp.Cookies() {
		if c.Name == sessionCookie && c.Value != "" {
			b.cookie = c.Name + "=" + c.Value
		}
		if c.Name == stateCookie && c.Value != "" {
			b.cookie = c.Name + "=" + c.Value
		}
	}
	return resp.StatusCode, resp.Header.Get("Location"), string(raw)
}

func (b *browser) login(code string) {
	b.t.Helper()
	_, loc, _ := b.do(http.MethodGet, "/login", "", nil)
	u, err := url.Parse(loc)
	if err != nil {
		b.t.Fatal(err)
	}
	q := u.Query()
	if q.Get("code_challenge_method") != "S256" || q.Get("state") == "" {
		b.t.Fatalf("no PKCE or state: %s", loc)
	}
	status, _, _ := b.do(http.MethodGet, "/auth/callback?state="+q.Get("state")+"&code="+code, "", nil)
	if status != http.StatusFound {
		b.t.Fatalf("callback: %d", status)
	}
	_, _, page := b.do(http.MethodGet, "/new", "", nil)
	m := csrfField.FindStringSubmatch(page)
	if m == nil {
		b.t.Fatal("no csrf field")
	}
	b.csrf = m[1]
}

func (b *browser) compose(fields map[string]string, files map[string]string) (int, string, string) {
	b.t.Helper()
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	w.WriteField("csrf", b.csrf)
	w.WriteField("nonce", randomToken())
	for k, v := range fields {
		w.WriteField(k, v)
	}
	for field, kind := range files {
		h := map[string][]string{"Content-Disposition": {`form-data; name="` + field + `"; filename="a.png"`}, "Content-Type": {kind}}
		part, _ := w.CreatePart(h)
		part.Write([]byte("data"))
	}
	w.Close()
	return b.do(http.MethodPost, "/new", w.FormDataContentType(), &body)
}

func future(days int) string { return time.Now().UTC().AddDate(0, 0, days).Format(dateTimeForm) }

func baseFields() map[string]string {
	return map[string]string{"text": "# Hi", "visibility": "public", "language": "pt", "when": future(2), "timezone": "UTC", "frequency": "once"}
}

func TestLoginStateWorksOnce(t *testing.T) {
	_, app, _ := testApp(t)
	b := &browser{t: t, app: app.URL}
	_, loc, _ := b.do(http.MethodGet, "/login", "", nil)
	u, _ := url.Parse(loc)
	state := u.Query().Get("state")
	saved := b.cookie
	if status, _, _ := b.do(http.MethodGet, "/auth/callback?state=wrong&code=ana", "", nil); status != http.StatusBadRequest {
		t.Errorf("wrong state: %d", status)
	}
	b.cookie = saved
	if status, _, _ := b.do(http.MethodGet, "/auth/callback?state="+state+"&code=ana", "", nil); status != http.StatusFound {
		t.Errorf("right state: %d", status)
	}
	b.cookie = saved
	if status, _, _ := b.do(http.MethodGet, "/auth/callback?state="+state+"&code=ana", "", nil); status != http.StatusBadRequest {
		t.Errorf("reused state: %d", status)
	}
}

func TestComposeSendsEveryOption(t *testing.T) {
	f, app, _ := testApp(t)
	b := &browser{t: t, app: app.URL}
	b.login("ana")
	fields := baseFields()
	fields["cw"] = "aviso"
	fields["local_only"] = "on"
	fields["expires_in"] = "900"
	fields["alt_0"] = "uma foto"
	status, loc, _ := b.compose(fields, map[string]string{"media_0": "image/png"})
	if status != http.StatusSeeOther || loc != "/queue?done=created" {
		t.Fatalf("create: %d %s", status, loc)
	}
	got := f.forms[0]
	for k, want := range map[string]string{"content_type": "text/markdown", "local_only": "true", "expires_in": "900", "spoiler_text": "aviso", "media_ids[]": "1"} {
		if got.Get(k) != want {
			t.Errorf("%s: %q", k, got.Get(k))
		}
	}
}

func TestPlainTextSkipsMarkdown(t *testing.T) {
	f, app, _ := testApp(t)
	b := &browser{t: t, app: app.URL}
	b.login("ana")
	fields := baseFields()
	fields["plain"] = "on"
	b.compose(fields, nil)
	if f.forms[0].Get("content_type") != "" {
		t.Error("content_type sent")
	}
}

func TestMonthlySeriesAuditsEachPost(t *testing.T) {
	f, app, db := testApp(t)
	b := &browser{t: t, app: app.URL}
	b.login("ana")
	marker := randomToken()
	fields := baseFields()
	fields["text"] = marker
	fields["frequency"] = "monthly"
	fields["count"] = "6"
	if status, _, _ := b.compose(fields, map[string]string{"media_0": "image/png"}); status != http.StatusSeeOther {
		t.Fatalf("series: %d", status)
	}
	if len(f.forms) != 6 || f.media != 6 {
		t.Fatalf("posts %d, media %d", len(f.forms), f.media)
	}
	var results int
	db.pool.QueryRow(context.Background(), `SELECT count(*) FROM audit_log WHERE series_id <> '' AND result = 'ok' AND at > now() - interval '1 minute'`).Scan(&results)
	if results < 6 {
		t.Errorf("audit rows: %d", results)
	}
	var plain int
	db.pool.QueryRow(context.Background(), `SELECT count(*) FROM audit_log WHERE position($1::bytea in excerpt) > 0`, marker).Scan(&plain)
	if plain != 0 {
		t.Error("excerpt stored in clear")
	}
}

func TestDailySeriesCreatesOnePostPerDay(t *testing.T) {
	f, app, db := testApp(t)
	f.next = 5000 // keep these ids out of the other tests' range
	b := &browser{t: t, app: app.URL}
	b.login("ana")
	fields := baseFields()
	fields["frequency"] = "daily"
	fields["count"] = "3"
	if status, _, _ := b.compose(fields, nil); status != http.StatusSeeOther {
		t.Fatalf("series: %d", status)
	}
	if len(f.forms) != 3 {
		t.Fatalf("posts %d", len(f.forms))
	}
	days := make([]time.Time, 0, len(f.forms))
	for _, form := range f.forms {
		at, err := time.Parse(time.RFC3339, form.Get("scheduled_at"))
		if err != nil {
			t.Fatalf("scheduled_at: %v", err)
		}
		days = append(days, at.UTC())
	}
	for i := 1; i < len(days); i++ {
		if days[i].Sub(days[i-1]) != 24*time.Hour {
			t.Errorf("%d: %v after %v", i, days[i], days[i-1])
		}
	}
	var results, seriesIDs int
	db.pool.QueryRow(context.Background(), `SELECT count(DISTINCT scheduled_id), count(DISTINCT series_id) FROM audit_log
		WHERE scheduled_id IN ('5001', '5002', '5003') AND series_id <> '' AND result = 'ok'`).Scan(&results, &seriesIDs)
	if results != 3 || seriesIDs != 1 {
		t.Errorf("audit rows: %d schedules, %d series", results, seriesIDs)
	}
}

func TestDailySeriesStopsAtDailyLimit(t *testing.T) {
	f, app, _ := testApp(t)
	at := time.Now().UTC().AddDate(0, 0, 2)
	for i := range dailyLimit {
		id := strconv.Itoa(1000 + i)
		f.scheduled[id] = map[string]any{"id": id, "scheduled_at": at.Format(time.RFC3339), "params": map[string]any{"text": "full"}}
		f.owner[id] = "1"
	}
	b := &browser{t: t, app: app.URL}
	b.login("ana")
	fields := baseFields()
	fields["frequency"] = "daily"
	fields["count"] = "3"
	if status, _, _ := b.compose(fields, nil); status != http.StatusUnprocessableEntity {
		t.Errorf("over the daily limit: %d", status)
	}
	if len(f.forms) != 0 {
		t.Errorf("sent %d posts", len(f.forms))
	}
}

func TestSeriesOverLimitsSendsNothing(t *testing.T) {
	f, app, _ := testApp(t)
	b := &browser{t: t, app: app.URL}
	b.login("ana")
	fields := baseFields()
	fields["frequency"] = "monthly"
	fields["count"] = "24"
	files := map[string]string{"media_0": "image/png", "media_1": "image/png"}
	if status, _, _ := b.compose(fields, files); status != http.StatusUnprocessableEntity {
		t.Errorf("uploads over limit: %d", status)
	}
	if len(f.forms) != 0 || f.media != 0 {
		t.Errorf("sent %d posts, %d media", len(f.forms), f.media)
	}
}

func TestRejectsBadInput(t *testing.T) {
	f, app, _ := testApp(t)
	b := &browser{t: t, app: app.URL}
	b.login("ana")
	for key, value := range map[string]string{
		"when": time.Now().UTC().Add(2 * time.Minute).Format(dateTimeForm), "visibility": "everyone", "timezone": "Mars/Base",
		"expires_in": "60", "reply": "https://evil.example/@a/1", "language": "<x>",
	} {
		fields := baseFields()
		fields[key] = value
		if status, _, _ := b.compose(fields, nil); status != http.StatusUnprocessableEntity {
			t.Errorf("%s=%s: %d", key, value, status)
		}
	}
	if status, _, _ := b.compose(baseFields(), map[string]string{"media_0": "text/html"}); status != http.StatusUnprocessableEntity {
		t.Errorf("html upload: %d", status)
	}
	if len(f.forms) != 0 {
		t.Errorf("%d posts sent", len(f.forms))
	}
}

func TestCSRFRequired(t *testing.T) {
	f, app, _ := testApp(t)
	b := &browser{t: t, app: app.URL}
	b.login("ana")
	good := b.csrf
	b.csrf = "wrong"
	if status, _, _ := b.compose(baseFields(), nil); status != http.StatusForbidden {
		t.Errorf("wrong token: %d", status)
	}
	b.csrf = good
	req, _ := http.NewRequest(http.MethodPost, app.URL+"/logout", strings.NewReader("csrf="+good))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	req.Header.Set("Cookie", b.cookie)
	resp, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("cross-site: %d", resp.StatusCode)
	}
	status, _, _ := b.do(http.MethodPost, "/preview", "text/plain", strings.NewReader("# x"))
	if status != http.StatusForbidden {
		t.Errorf("preview without token: %d", status)
	}
	if len(f.forms) != 0 {
		t.Error("post sent")
	}
}

func TestOtherAccountCantTouchPost(t *testing.T) {
	f, app, _ := testApp(t)
	ana := &browser{t: t, app: app.URL}
	ana.login("ana")
	ana.compose(baseFields(), nil)
	bia := &browser{t: t, app: app.URL}
	bia.login("bia")
	var id string
	for k := range f.scheduled {
		id = k
	}
	form := url.Values{"csrf": {bia.csrf}, "when": {future(3)}}
	for _, path := range []string{"/post/" + id + "/cancel", "/post/" + id + "/date"} {
		if status, _, _ := bia.do(http.MethodPost, path, "application/x-www-form-urlencoded", strings.NewReader(form.Encode())); status != http.StatusNotFound {
			t.Errorf("%s: %d", path, status)
		}
	}
	if status, _, _ := bia.do(http.MethodGet, "/post/"+id, "", nil); status != http.StatusNotFound {
		t.Errorf("view: %d", status)
	}
	if len(f.scheduled) != 1 {
		t.Error("post was removed")
	}
}

func TestRescheduleCancelLogout(t *testing.T) {
	f, app, _ := testApp(t)
	b := &browser{t: t, app: app.URL}
	b.login("ana")
	b.compose(baseFields(), nil)
	var id string
	for k := range f.scheduled {
		id = k
	}
	form := url.Values{"csrf": {b.csrf}, "when": {future(5)}}
	if status, loc, _ := b.do(http.MethodPost, "/post/"+id+"/date", "application/x-www-form-urlencoded", strings.NewReader(form.Encode())); status != http.StatusSeeOther || !strings.Contains(loc, "rescheduled") {
		t.Errorf("reschedule: %d %s", status, loc)
	}
	soon := url.Values{"csrf": {b.csrf}, "when": {time.Now().UTC().Format(dateTimeForm)}}
	if _, loc, _ := b.do(http.MethodPost, "/post/"+id+"/date", "application/x-www-form-urlencoded", strings.NewReader(soon.Encode())); !strings.Contains(loc, "too_soon") {
		t.Errorf("too soon: %s", loc)
	}
	if status, _, _ := b.do(http.MethodPost, "/post/"+id+"/cancel", "application/x-www-form-urlencoded", strings.NewReader(url.Values{"csrf": {b.csrf}}.Encode())); status != http.StatusSeeOther {
		t.Errorf("cancel: %d", status)
	}
	if len(f.scheduled) != 0 {
		t.Error("not canceled")
	}
	b.do(http.MethodPost, "/logout", "application/x-www-form-urlencoded", strings.NewReader(url.Values{"csrf": {b.csrf}}.Encode()))
	if len(f.revoked) != 1 || f.revoked[0] != "token-ana" {
		t.Errorf("revoked: %v", f.revoked)
	}
	if status, _, _ := b.do(http.MethodGet, "/queue", "", nil); status != http.StatusFound {
		t.Errorf("after logout: %d", status)
	}
}

func TestSweepRevokesExpiredOnce(t *testing.T) {
	addr := os.Getenv("AGENDE_TEST_REDIS")
	if addr == "" {
		t.Skip("AGENDE_TEST_REDIS not set")
	}
	ctx := context.Background()
	rdb := redis.NewClient(&redis.Options{Addr: addr})
	rdb.FlushDB(ctx)
	bx, _ := newBox(strings.Repeat("k", 32), "t")
	store := &sessions{rdb: rdb, box: bx}
	sess, err := store.create(ctx, "1", "ana", "tok")
	if err != nil {
		t.Fatal(err)
	}
	rdb.ZAdd(ctx, expiryIndex, redis.Z{Score: 1, Member: hashID(sess.ID)})
	var mu sync.Mutex
	var got []string
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			store.sweep(ctx, func(token string) error {
				mu.Lock()
				got = append(got, token)
				mu.Unlock()
				return nil
			})
		})
	}
	wg.Wait()
	if len(got) != 1 || got[0] != "tok" {
		t.Errorf("revoked %v", got)
	}
	raw, _ := rdb.Keys(ctx, "*").Result()
	for _, k := range raw {
		v, _ := rdb.Get(ctx, k).Bytes()
		if bytes.Contains(v, []byte("tok")) {
			t.Error("token in clear")
		}
	}
}

func TestPreviewEscapesHTML(t *testing.T) {
	_, app, _ := testApp(t)
	b := &browser{t: t, app: app.URL}
	b.login("ana")
	req, _ := http.NewRequest(http.MethodPost, app.URL+"/preview", strings.NewReader("<script>alert(1)</script>\n\n[x](javascript:alert(1))"))
	req.Header.Set("X-CSRF-Token", b.csrf)
	req.Header.Set("Cookie", b.cookie)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if strings.Contains(string(raw), "<script") || strings.Contains(string(raw), "javascript:") {
		t.Errorf("unsafe preview: %s", raw)
	}
}

func TestFailedRevokeIsRetried(t *testing.T) {
	addr := os.Getenv("AGENDE_TEST_REDIS")
	if addr == "" {
		t.Skip("AGENDE_TEST_REDIS not set")
	}
	ctx := context.Background()
	rdb := redis.NewClient(&redis.Options{Addr: addr})
	rdb.FlushDB(ctx)
	bx, _ := newBox(strings.Repeat("k", 32), "t")
	store := &sessions{rdb: rdb, box: bx}
	sess, _ := store.create(ctx, "1", "ana", "tok")
	if err := store.expire(ctx, hashID(sess.ID)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.get(ctx, sess.ID); err != errNoSession {
		t.Errorf("expired session still works: %v", err)
	}
	calls := 0
	store.sweep(ctx, func(string) error { calls++; return fmt.Errorf("down") })
	store.sweep(ctx, func(string) error { calls++; return nil })
	store.sweep(ctx, func(string) error { calls++; return nil })
	if calls != 2 {
		t.Errorf("revoke calls: %d", calls)
	}
}

func TestSessionFromAnotherKeyIsNoSession(t *testing.T) {
	addr := os.Getenv("AGENDE_TEST_REDIS")
	if addr == "" {
		t.Skip("AGENDE_TEST_REDIS not set")
	}
	ctx := context.Background()
	rdb := redis.NewClient(&redis.Options{Addr: addr})
	rdb.FlushDB(ctx)
	a, _ := newBox(strings.Repeat("a", 32), "t")
	b, _ := newBox(strings.Repeat("b", 32), "t")
	sess, _ := (&sessions{rdb: rdb, box: a}).create(ctx, "1", "ana", "tok")
	if _, err := (&sessions{rdb: rdb, box: b}).get(ctx, sess.ID); err != errNoSession {
		t.Errorf("got %v", err)
	}
}

func TestUploadSlotsDontBlockTextPosts(t *testing.T) {
	_, app, _ := testApp(t)
	b := &browser{t: t, app: app.URL}
	b.login("ana")
	done := make(chan int, 1)
	go func() {
		status, _, _ := b.compose(baseFields(), nil)
		done <- status
	}()
	select {
	case status := <-done:
		if status != http.StatusSeeOther {
			t.Errorf("status %d", status)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("text post blocked")
	}
}

func TestOneUploadPerAccount(t *testing.T) {
	s := &server{uploads: make(chan struct{}, 1), uploading: map[string]bool{}}
	release, ok := s.acquireUpload(context.Background(), "1")
	if !ok {
		t.Fatal("first upload refused")
	}
	if _, ok := s.acquireUpload(context.Background(), "1"); ok {
		t.Error("same account got a second slot")
	}
	start := time.Now()
	if _, ok := s.acquireUpload(context.Background(), "2"); ok {
		t.Error("slot given twice")
	}
	if time.Since(start) > uploadWait+time.Second {
		t.Error("waited too long")
	}
	release()
	if release, ok := s.acquireUpload(context.Background(), "2"); !ok {
		t.Error("slot not released")
	} else {
		release()
	}
}

func TestResentFormDoesNothing(t *testing.T) {
	f, app, _ := testApp(t)
	b := &browser{t: t, app: app.URL}
	b.login("ana")
	nonce := randomToken()
	send := func() int {
		var body bytes.Buffer
		w := multipart.NewWriter(&body)
		w.WriteField("csrf", b.csrf)
		w.WriteField("nonce", nonce)
		for k, v := range baseFields() {
			w.WriteField(k, v)
		}
		w.Close()
		status, _, _ := b.do(http.MethodPost, "/new", w.FormDataContentType(), &body)
		return status
	}
	send()
	send()
	if len(f.forms) != 1 {
		t.Errorf("posts: %d", len(f.forms))
	}
}

func TestStaleDateIsNotSent(t *testing.T) {
	f, srv := newFakeMastodon()
	defer srv.Close()
	s := &server{api: &mastodon{base: srv.URL, client: srv.Client(), upload: srv.Client()}}
	_, err := s.sendPost(context.Background(), session{Token: "token-ana"}, newStatus{Text: "x", Visibility: "public", ScheduledAt: time.Now().Add(time.Minute)}, nil, "k")
	if err != errTooSoon || len(f.forms) != 0 {
		t.Errorf("%v, %d posts", err, len(f.forms))
	}
}

func TestSharedTokenSurvivesOneSession(t *testing.T) {
	addr := os.Getenv("AGENDE_TEST_REDIS")
	if addr == "" {
		t.Skip("AGENDE_TEST_REDIS not set")
	}
	ctx := context.Background()
	rdb := redis.NewClient(&redis.Options{Addr: addr})
	rdb.FlushDB(ctx)
	bx, _ := newBox(strings.Repeat("k", 32), "t")
	store := &sessions{rdb: rdb, box: bx}
	first, _ := store.create(ctx, "1", "ana", "same")
	second, _ := store.create(ctx, "1", "ana", "same")
	var revoked []string
	revoke := func(token string) error { revoked = append(revoked, token); return nil }
	store.expire(ctx, hashID(first.ID))
	store.sweep(ctx, revoke)
	if len(revoked) != 0 {
		t.Fatal("token of a live session revoked")
	}
	if _, err := store.get(ctx, second.ID); err != nil {
		t.Errorf("second session: %v", err)
	}
	store.expire(ctx, hashID(second.ID))
	store.sweep(ctx, revoke)
	if len(revoked) != 1 {
		t.Errorf("revoked %v", revoked)
	}
}

func TestZoneOptions(t *testing.T) {
	now := time.Date(2027, 1, 15, 12, 0, 0, 0, time.UTC)
	loc, _ := time.LoadLocation("America/Campo_Grande")
	if got := zoneLabel("America/Campo_Grande", loc, now); got != "America/Campo_Grande (UTC-4)" {
		t.Error(got)
	}
	india, _ := time.LoadLocation("Asia/Kolkata")
	if got := zoneLabel("Asia/Kolkata", india, now); got != "Asia/Kolkata (UTC+5:30)" {
		t.Error(got)
	}
	opts := zoneOptions("Asia/Kolkata", now)
	if last := opts[len(opts)-1]; last.Name != "Asia/Kolkata" || !last.On {
		t.Errorf("current zone missing: %+v", last)
	}
	for _, o := range zoneOptions("Mars/Base", now) {
		if o.Name == "Mars/Base" {
			t.Error("invalid zone offered")
		}
	}
}

func scheduleOne(t *testing.T, f *fakeMastodon, b *browser, fields map[string]string, files map[string]string) string {
	t.Helper()
	b.compose(fields, files)
	f.mu.Lock()
	defer f.mu.Unlock()
	for id := range f.scheduled {
		return id
	}
	t.Fatal("not scheduled")
	return ""
}

func TestEditCreatesNewThenCancelsOld(t *testing.T) {
	f, app, _ := testApp(t)
	b := &browser{t: t, app: app.URL}
	b.login("ana")
	id := scheduleOne(t, f, b, baseFields(), map[string]string{"media_0": "image/png"})
	status, _, page := b.do(http.MethodGet, "/post/"+id+"/edit", "", nil)
	if status != http.StatusOK || !strings.Contains(page, `name="keep_1"`) || !strings.Contains(page, "edit_id") {
		t.Fatalf("edit form: %d", status)
	}
	fields := baseFields()
	fields["text"] = "novo"
	fields["edit_id"] = id
	fields["keep_1"] = "on"
	fields["keep_alt_1"] = "nova descricao"
	if status, loc, _ := b.compose(fields, nil); status != http.StatusSeeOther || !strings.Contains(loc, "edited") {
		t.Fatalf("save: %d %s", status, loc)
	}
	if len(f.scheduled) != 1 {
		t.Fatalf("queue: %d", len(f.scheduled))
	}
	if _, ok := f.scheduled[id]; ok {
		t.Error("old post still there")
	}
	last := f.forms[len(f.forms)-1]
	if last.Get("status") != "novo" || last.Get("media_ids[]") != "1" {
		t.Errorf("sent: %v", last)
	}
	if len(f.alts) != 1 || f.alts[0] != "nova descricao" {
		t.Errorf("alts: %v", f.alts)
	}
}

func TestEditFailureKeepsOriginal(t *testing.T) {
	f, app, _ := testApp(t)
	b := &browser{t: t, app: app.URL}
	b.login("ana")
	id := scheduleOne(t, f, b, baseFields(), nil)
	fields := baseFields()
	fields["text"] = "REFUSE"
	fields["edit_id"] = id
	status, _, page := b.compose(fields, nil)
	if status != http.StatusBadGateway || !strings.Contains(page, "REFUSE") {
		t.Errorf("failure: %d", status)
	}
	if _, ok := f.scheduled[id]; !ok {
		t.Error("original lost")
	}
}

func TestEditRefusesAnotherAccountsPost(t *testing.T) {
	f, app, _ := testApp(t)
	ana := &browser{t: t, app: app.URL}
	ana.login("ana")
	id := scheduleOne(t, f, ana, baseFields(), nil)
	bia := &browser{t: t, app: app.URL}
	bia.login("bia")
	fields := baseFields()
	fields["edit_id"] = id
	if status, _, _ := bia.compose(fields, nil); status != http.StatusNotFound {
		t.Errorf("save: %d", status)
	}
	if status, _, _ := bia.do(http.MethodGet, "/post/"+id+"/edit", "", nil); status != http.StatusNotFound {
		t.Errorf("form: %d", status)
	}
	if len(f.scheduled) != 1 || len(f.forms) != 1 {
		t.Errorf("queue %d, sent %d", len(f.scheduled), len(f.forms))
	}
}

func TestPollsAreNotEditable(t *testing.T) {
	var p scheduledStatus
	if !editable(p) {
		t.Error("plain post refused")
	}
	p.Params.Poll = map[string]any{"options": []any{"a"}}
	if editable(p) {
		t.Error("poll accepted")
	}
}
