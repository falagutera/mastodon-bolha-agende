package main

import (
	"context"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"
)

func marker(t *testing.T) string { return "m" + strings.ToLower(randomToken()[:10]) }

func (b *browser) audit(query string) string {
	b.t.Helper()
	status, _, page := b.do(http.MethodGet, "/audit"+query, "", nil)
	if status != http.StatusOK {
		b.t.Fatalf("audit: %d", status)
	}
	return page
}

func TestAuditReadsLikeSentences(t *testing.T) {
	f, app, _ := testApp(t)
	f.next = int(time.Now().UnixNano() % 1e12)
	m := marker(t)
	ana := &browser{t: t, app: app.URL}
	ana.login("ana")
	fields := baseFields()
	fields["text"] = m + " <b>bold</b>"
	id := scheduleOne(t, f, ana, fields, nil)
	ana.post("/post/"+id+"/date", url.Values{"as": {ana.as}, "when": {future(4)}})
	ana.post("/post/"+id+"/cancel", url.Values{"as": {ana.as}})
	series := baseFields()
	series["text"] = m + " series"
	series["frequency"] = "monthly"
	series["count"] = "3"
	ana.compose(series, nil)

	page := ana.audit("")
	for _, want := range []string{
		"@user1 scheduled “" + m + " &lt;b&gt;bold&lt;/b&gt;” on @user1 for",
		"@user1 moved “" + m,
		"@user1 canceled “" + m,
		"@user1 scheduled a series of 3 posts “" + m + " series” on @user1",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("missing %q", want)
		}
	}
	if strings.Contains(page, "<b>bold") {
		t.Error("HTML not escaped")
	}
	if strings.Count(page, m+" series") != 1 {
		t.Error("series not grouped in one line")
	}
}

func TestAuditVisibility(t *testing.T) {
	f, app, _ := testApp(t)
	f.next = int(time.Now().UnixNano() % 1e12)
	m := marker(t)
	bia := &browser{t: t, app: app.URL}
	bia.login("bia")
	bia.delegate("user1", "bia-d")
	ana := &browser{t: t, app: app.URL}
	ana.login("ana")
	ana.actAs("2")
	fields := baseFields()
	fields["text"] = m
	scheduleOne(t, f, ana, fields, nil)
	bia.post("/delegations/revoke", url.Values{"delegate": {"1"}})

	line := "@user1 scheduled “" + m + "” on @user2"
	if !strings.Contains(bia.audit(""), line) {
		t.Error("owner doesn't see what the delegate did")
	}
	ana.refresh()
	anaPage := ana.audit("")
	if !strings.Contains(anaPage, line) {
		t.Error("delegate doesn't see its own action after revoke")
	}
	if !strings.Contains(anaPage, "@user2 let @user1 post as it") || !strings.Contains(anaPage, "@user2 revoked the access of @user1") {
		t.Error("delegate doesn't see the events that name it")
	}
	cai := &browser{t: t, app: app.URL}
	cai.login("cai")
	if page := cai.audit(""); strings.Contains(page, m) || strings.Contains(page, "@user2 let @user1") {
		t.Error("a third account sees someone else's log")
	}
	// Filtering by another account doesn't widen what cai sees.
	if page := cai.audit("?account=2"); strings.Contains(page, m) {
		t.Error("account filter leaks")
	}
}

func TestAuditFilters(t *testing.T) {
	f, app, _ := testApp(t)
	f.next = int(time.Now().UnixNano() % 1e12)
	m := marker(t)
	ana := &browser{t: t, app: app.URL}
	ana.login("ana")
	fields := baseFields()
	fields["text"] = m
	id := scheduleOne(t, f, ana, fields, nil)
	ana.post("/post/"+id+"/cancel", url.Values{"as": {ana.as}})

	page := ana.audit("?action=cancel")
	if !strings.Contains(page, "canceled “"+m) || strings.Contains(page, "scheduled “"+m) {
		t.Error("action filter")
	}
	tomorrow := time.Now().AddDate(0, 0, 2).Format(time.DateOnly)
	if page := ana.audit("?from=" + tomorrow); strings.Contains(page, m) {
		t.Error("from filter")
	}
	today := time.Now().UTC().Format(time.DateOnly)
	if page := ana.audit("?from=" + today + "&to=" + today); !strings.Contains(page, m) {
		t.Error("today's range")
	}
	// Bad values are ignored, not errors.
	ana.audit("?action=drop&account=x&from=yesterday&before=-1")
}

func TestAuditPages(t *testing.T) {
	_, app, db := testApp(t)
	for range auditPage + 5 {
		op := randomToken()[:22]
		if _, err := db.pool.Exec(context.Background(), `INSERT INTO audit_log (op_id, actor_id, actor_acct, target_id, target_acct, action, result, after_at)
			VALUES ($1, '3', 'user3', '3', 'user3', 'schedule', 'ok', now() + interval '1 day')`, op); err != nil {
			t.Fatal(err)
		}
	}
	cai := &browser{t: t, app: app.URL}
	cai.login("cai")
	page := cai.audit("")
	if n := strings.Count(page, "<li>"); n != auditPage {
		t.Errorf("first page: %d lines", n)
	}
	next := regexp.MustCompile(`href="(/audit\?before=[0-9]+)"`).FindStringSubmatch(page)
	if next == nil {
		t.Fatal("no link to older entries")
	}
	if more := cai.audit(strings.TrimPrefix(next[1], "/audit")); strings.Count(more, "<li>") < 5 {
		t.Error("second page")
	}
}

func TestAuditLines(t *testing.T) {
	tr := strs("en")
	now := time.Date(2027, 3, 1, 12, 0, 0, 0, time.UTC)
	at := func(h int) *time.Time { v := now.Add(time.Duration(h) * time.Hour); return &v }
	rows := []auditRow{
		{Action: actionSchedule, SeriesID: "s", Result: resultFailed, At: now, After: at(72), ActorAcct: "a", TargetAcct: "b", Excerpt: "x"},
		{Action: actionSchedule, SeriesID: "s", Result: resultOK, At: now, After: at(48), ActorAcct: "a", TargetAcct: "b", Excerpt: "x"},
		{Action: actionSchedule, SeriesID: "s", Result: resultOK, At: now, After: at(24), ActorAcct: "a", TargetAcct: "b", Excerpt: "x"},
		{Action: actionCancel, Result: resultIntent, At: now.Add(-time.Hour), Before: at(5), ActorAcct: "a", TargetAcct: "a"},
		{Action: actionCancel, Result: resultIntent, At: now.Add(-time.Minute), Before: at(5), ActorAcct: "a", TargetAcct: "a"},
		{Action: actionLost, Result: resultOK, At: now, SubjectAcct: "a", TargetAcct: "b"},
		{Action: "unknown_action", At: now},
	}
	lines := auditLines(rows, tr, time.UTC, now)
	if len(lines) != 4 {
		t.Fatalf("%d lines: %+v", len(lines), lines)
	}
	if !strings.Contains(lines[0].Text, "series of 3 posts") || !strings.Contains(lines[0].Text, "from Mar 2, 2027") || lines[0].Status != "2 scheduled" {
		t.Errorf("series: %+v", lines[0])
	}
	if lines[1].Status != tr["audit_interrupted"] || !lines[1].Warning || lines[2].Status != tr["audit_in_progress"] {
		t.Errorf("intent: %+v %+v", lines[1], lines[2])
	}
	if !strings.Contains(lines[1].Text, "(no text)") {
		t.Errorf("no text: %s", lines[1].Text)
	}
	if lines[3].Text != "The access of @a to @b ended on the instance" {
		t.Errorf("lost: %s", lines[3].Text)
	}
	for _, lang := range []string{"en", "pt-BR"} {
		for k, v := range strs(lang) {
			if strings.HasPrefix(k, "audit_") && strings.Contains(v, "%!") {
				t.Errorf("%s %s", lang, k)
			}
		}
	}
}
