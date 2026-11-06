package main

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestCancelSelectedFromTheQueue(t *testing.T) {
	f, app, _ := testApp(t)
	f.next = int(time.Now().UnixNano() % 1e12)
	b := &browser{t: t, app: app.URL}
	b.login("ana")
	series := dailySeries(t, f, b, "convites do blog", "4", nil)
	if len(series) != 4 {
		t.Fatalf("series: %d", len(series))
	}
	_, _, page := b.do(http.MethodGet, "/queue", "", nil)
	if !strings.Contains(page, `id="bulk"`) || !strings.Contains(page, `name="id" value="`+series[0].id+`"`) {
		t.Fatal("queue has no bulk form")
	}
	form := url.Values{"csrf": {b.csrf}, "as": {b.as}, "id": {series[0].id, series[2].id, "does-not-exist"}}
	status, loc, _ := b.do(http.MethodPost, "/cancel", "application/x-www-form-urlencoded", strings.NewReader(form.Encode()))
	if status != http.StatusSeeOther || !strings.Contains(loc, "done=cancelled_many") {
		t.Fatalf("cancel: %d %s", status, loc)
	}
	left := f.posts("1")
	if len(left) != 2 || left[0].id != series[1].id || left[1].id != series[3].id {
		t.Errorf("left: %+v", left)
	}

	// Nothing ticked: back to the queue with a message, nothing canceled.
	empty := url.Values{"csrf": {b.csrf}, "as": {b.as}}
	if _, loc, _ := b.do(http.MethodPost, "/cancel", "application/x-www-form-urlencoded", strings.NewReader(empty.Encode())); !strings.Contains(loc, "none_selected") {
		t.Errorf("empty: %s", loc)
	}
	// Without the CSRF token nothing happens.
	bad := url.Values{"csrf": {"x"}, "as": {b.as}, "id": {series[1].id}}
	if status, _, _ := b.do(http.MethodPost, "/cancel", "application/x-www-form-urlencoded", strings.NewReader(bad.Encode())); status != http.StatusForbidden {
		t.Errorf("csrf: %d", status)
	}
	if len(f.posts("1")) != 2 {
		t.Error("canceled without csrf")
	}
}

func TestCancelAPostAndTheLaterOnesWithTheSameText(t *testing.T) {
	f, app, _ := testApp(t)
	f.next = int(time.Now().UnixNano() % 1e12)
	b := &browser{t: t, app: app.URL}
	b.login("ana")
	series := dailySeries(t, f, b, "convites do blog", "5", nil)
	scheduleOne(t, f, b, map[string]string{"text": "outro texto", "visibility": "public", "language": "pt", "when": future(3), "timezone": "UTC", "frequency": "once"}, nil)

	_, _, page := b.do(http.MethodGet, "/post/"+series[1].id, "", nil)
	if !strings.Contains(page, `value="future"`) || !strings.Contains(page, "3 later ones") {
		t.Fatal("post page doesn't offer the later posts")
	}
	form := url.Values{"csrf": {b.csrf}, "as": {b.as}, "scope": {"future"}}
	status, loc, _ := b.do(http.MethodPost, "/post/"+series[1].id+"/cancel", "application/x-www-form-urlencoded", strings.NewReader(form.Encode()))
	if status != http.StatusSeeOther || !strings.Contains(loc, "done=cancelled_many") {
		t.Fatalf("cancel: %d %s", status, loc)
	}
	left := f.posts("1")
	texts := map[string]string{}
	for _, p := range left {
		texts[p.id] = p.text
	}
	if len(left) != 2 || texts[series[0].id] != "convites do blog" || !strings.Contains(strings.Join([]string{left[0].text, left[1].text}, "|"), "outro texto") {
		t.Errorf("left: %+v", left)
	}
}
