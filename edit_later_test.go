package main

import (
	"net/http"
	"sort"
	"strings"
	"testing"
	"time"
)

type fakePost struct {
	id, text, at string
	media        int
}

// posts lists the account's queue, by date.
func (f *fakeMastodon) posts(owner string) []fakePost {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []fakePost
	for id, it := range f.scheduled {
		if f.owner[id] != owner {
			continue
		}
		p := it["params"].(map[string]any)
		out = append(out, fakePost{id: id, text: p["text"].(string), at: it["scheduled_at"].(string), media: len(it["media_attachments"].([]any))})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].at < out[j].at })
	return out
}

func dailySeries(t *testing.T, f *fakeMastodon, b *browser, text string, n string, files map[string]string) []fakePost {
	t.Helper()
	fields := baseFields()
	fields["text"], fields["frequency"], fields["count"] = text, "daily", n
	if status, _, _ := b.compose(fields, files); status != http.StatusSeeOther {
		t.Fatalf("series: %d", status)
	}
	return f.posts("1")
}

func editFields(p fakePost, text, scope string) map[string]string {
	fields := baseFields()
	at, _ := time.Parse(time.RFC3339, p.at)
	fields["text"], fields["edit_id"], fields["when"] = text, p.id, at.UTC().Format(dateTimeForm)
	if scope != "" {
		fields["scope"] = scope
	}
	return fields
}

func TestEditOffersAndAppliesToLaterPostsWithTheSameText(t *testing.T) {
	f, app, _ := testApp(t)
	f.next = int(time.Now().UnixNano() % 1e12)
	b := &browser{t: t, app: app.URL}
	b.login("ana")
	series := dailySeries(t, f, b, "campanha de novembro", "4", nil)
	other := scheduleOne(t, f, b, map[string]string{"text": "outro texto", "visibility": "public", "language": "pt", "when": future(3), "timezone": "UTC", "frequency": "once"}, nil)
	_ = other
	if len(series) != 4 {
		t.Fatalf("series: %d", len(series))
	}
	// The second post knows about the two after it, not the one before.
	_, _, page := b.do(http.MethodGet, "/post/"+series[1].id+"/edit", "", nil)
	if !strings.Contains(page, "2 later posts have the same text") || !strings.Contains(page, `value="future"`) {
		t.Fatal("no offer to change the later posts")
	}
	before := f.posts("1")
	if status, loc, _ := b.compose(editFields(series[1], "campanha nova", "future"), nil); status != http.StatusSeeOther || !strings.Contains(loc, "edited_all") {
		t.Fatalf("save: %d %s", status, loc)
	}
	after := f.posts("1")
	if len(after) != len(before) {
		t.Fatalf("queue %d -> %d", len(before), len(after))
	}
	var changed, kept []string
	for _, p := range after {
		switch p.text {
		case "campanha nova":
			changed = append(changed, p.at)
		case "campanha de novembro":
			kept = append(kept, p.at)
		}
	}
	// Dates stay; only the text changes, from the edited post on.
	want := []string{series[1].at, series[2].at, series[3].at}
	if strings.Join(changed, ",") != strings.Join(want, ",") || len(kept) != 1 || kept[0] != series[0].at {
		t.Errorf("changed %v kept %v, want %v changed and %s kept", changed, kept, want, series[0].at)
	}
	for _, p := range after {
		if p.text == "outro texto" {
			return
		}
	}
	t.Error("a post with another text was touched")
}

func TestEditOnlyThisOne(t *testing.T) {
	f, app, _ := testApp(t)
	f.next = int(time.Now().UnixNano() % 1e12)
	b := &browser{t: t, app: app.URL}
	b.login("ana")
	series := dailySeries(t, f, b, "aviso semanal", "3", nil)
	for _, scope := range []string{"", "one"} {
		target := f.posts("1")[0]
		if status, loc, _ := b.compose(editFields(target, "aviso "+scope+"x", scope), nil); status != http.StatusSeeOther || !strings.Contains(loc, "done=edited") || strings.Contains(loc, "edited_all") {
			t.Fatalf("%q: %d %s", scope, status, loc)
		}
	}
	n := 0
	for _, p := range f.posts("1") {
		if p.text == "aviso semanal" {
			n++
		}
	}
	if n != len(series)-1 {
		t.Errorf("%d posts kept the old text, want %d", n, len(series)-1)
	}
}

func TestLaterPostsKeepTheirOwnFiles(t *testing.T) {
	f, app, _ := testApp(t)
	f.next = int(time.Now().UnixNano() % 1e12)
	b := &browser{t: t, app: app.URL}
	b.login("ana")
	series := dailySeries(t, f, b, "com imagem", "3", map[string]string{"media_0": "image/png"})
	for _, p := range series {
		if p.media != 1 {
			t.Fatalf("setup: %+v", p)
		}
	}
	fields := editFields(series[0], "com imagem nova legenda", "future")
	fields["keep_"+firstMedia(f, series[0].id)] = "on"
	b.compose(fields, nil)
	for _, p := range f.posts("1") {
		if p.media != 1 || p.text != "com imagem nova legenda" {
			t.Errorf("after: %+v", p)
		}
	}
}

func firstMedia(f *fakeMastodon, id string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.scheduled[id]["media_attachments"].([]any)[0].(map[string]any)["id"].(string)
}

func TestSiblings(t *testing.T) {
	at := func(d int) time.Time { return time.Date(2027, 1, d, 10, 0, 0, 0, time.UTC) }
	p := func(id, text string, d int) scheduledStatus {
		s := scheduledStatus{ID: id, ScheduledAt: at(d)}
		s.Params.Text = text
		return s
	}
	posts := []scheduledStatus{p("1", "x", 1), p("2", "x", 2), p("3", "y", 3), p("4", "x", 4), p("5", "x", 5)}
	poll := p("6", "x", 6)
	poll.Params.Poll = map[string]any{"options": []string{"a"}}
	posts = append(posts, poll)
	got := siblings(posts, posts[1])
	if len(got) != 2 || got[0].ID != "4" || got[1].ID != "5" {
		t.Errorf("siblings: %+v", got)
	}
	if len(siblings(posts, p("9", "", 0))) != 0 {
		t.Error("empty text has siblings")
	}
}
