package main

import (
	"context"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

func (b *browser) post(path string, form url.Values) (int, string, string) {
	b.t.Helper()
	form.Set("csrf", b.csrf)
	return b.do(http.MethodPost, path, "application/x-www-form-urlencoded", strings.NewReader(form.Encode()))
}

// startDelegation asks to delegate to handle and returns the authorize URL.
func (b *browser) startDelegation(handle string) (int, *url.URL) {
	b.t.Helper()
	status, loc, _ := b.post("/delegations", url.Values{"handle": {handle}})
	u, err := url.Parse(loc)
	if err != nil {
		b.t.Fatal(err)
	}
	return status, u
}

// delegate runs the whole flow; code is what the instance returns, so "bia-d"
// means bia typed her password.
func (b *browser) delegate(handle, code string) string {
	b.t.Helper()
	status, u := b.startDelegation(handle)
	if status != http.StatusSeeOther || !strings.HasSuffix(u.Path, "/oauth/authorize") {
		b.t.Fatalf("start: %d %s", status, u)
	}
	_, loc, _ := b.do(http.MethodGet, "/delegations/callback?state="+u.Query().Get("state")+"&code="+code, "", nil)
	return loc
}

func (b *browser) actAs(id string) int {
	b.t.Helper()
	status, _, _ := b.post("/acting", url.Values{"account": {id}})
	b.refresh()
	return status
}

func delegationCount(t *testing.T, db *store, owner, delegate string) int {
	t.Helper()
	var n int
	if err := db.pool.QueryRow(context.Background(), `SELECT count(*) FROM delegations WHERE owner_id = $1 AND delegate_id = $2`, owner, delegate).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestDelegationGrantAndPostAs(t *testing.T) {
	f, app, db := testApp(t)
	// The audit log only grows, so keep these ids apart from earlier runs.
	f.next = int(time.Now().UnixNano() % 1e12)
	bia := &browser{t: t, app: app.URL}
	bia.login("bia")
	status, u := bia.startDelegation("@user1")
	q := u.Query()
	if status != http.StatusSeeOther || q.Get("client_id") != "did" || q.Get("prompt") != "login" || q.Get("force_login") != "true" ||
		!strings.HasSuffix(q.Get("redirect_uri"), "/delegations/callback") || q.Get("code_challenge_method") != "S256" {
		t.Fatalf("authorize: %d %s", status, u)
	}
	_, loc, _ := bia.do(http.MethodGet, "/delegations/callback?state="+q.Get("state")+"&code=bia-d", "", nil)
	if !strings.Contains(loc, "done=delegated") || delegationCount(t, db, "2", "1") != 1 {
		t.Fatalf("callback: %s", loc)
	}
	if got := f.tokenClients[len(f.tokenClients)-1]; got != "did" {
		t.Errorf("exchanged with %q, not the delegation app", got)
	}
	var stored []byte
	db.pool.QueryRow(context.Background(), `SELECT token FROM delegations WHERE owner_id = '2'`).Scan(&stored)
	if strings.Contains(string(stored), "token-bia-d") {
		t.Error("token stored in clear")
	}
	if _, _, page := bia.do(http.MethodGet, "/delegations", "", nil); !strings.Contains(page, "@user1") {
		t.Error("delegate not listed for the owner")
	}

	ana := &browser{t: t, app: app.URL}
	ana.login("ana")
	if ana.as != "1" {
		t.Fatalf("own account first: %q", ana.as)
	}
	if _, _, page := ana.do(http.MethodGet, "/queue", "", nil); !strings.Contains(page, `<option value="2"`) {
		t.Fatal("no Post as option")
	}
	if status := ana.actAs("2"); status != http.StatusSeeOther || ana.as != "2" {
		t.Fatalf("act as: %d %q", status, ana.as)
	}
	if _, _, page := ana.do(http.MethodGet, "/queue", "", nil); !strings.Contains(page, "acting as @user2") {
		t.Error("no acting banner")
	}
	id := scheduleOne(t, f, ana, baseFields(), nil)
	if f.owner[id] != "2" {
		t.Fatalf("posted as %q", f.owner[id])
	}
	var n int
	db.pool.QueryRow(context.Background(), `SELECT count(*) FROM audit_log WHERE scheduled_id = $1 AND actor_id = '1' AND target_id = '2' AND result = 'ok'`, id).Scan(&n)
	if n != 1 {
		t.Errorf("audit: %d", n)
	}
	db.pool.QueryRow(context.Background(), `SELECT count(*) FROM audit_log WHERE action = 'delegate' AND target_id = '2' AND subject_id = '1'`).Scan(&n)
	if n == 0 {
		t.Error("delegation not audited")
	}
	// The owner sees the post in her own queue and can cancel it.
	bia.refresh()
	if status, _, _ := bia.post("/post/"+id+"/cancel", url.Values{"as": {bia.as}}); status != http.StatusSeeOther || f.scheduled[id] != nil {
		t.Errorf("owner cancel: %d", status)
	}
}

func TestDelegationRejectsBadDelegates(t *testing.T) {
	_, app, db := testApp(t)
	bia := &browser{t: t, app: app.URL}
	bia.login("bia")
	for handle, want := range map[string]string{
		"user9@other.social": "error=remote",
		"user9":              "error=no_account", // the instance says it's remote
		"user4":              "error=no_account", // suspended
		"nobody":             "error=no_account",
		"@user2":             "error=self",
		"bad name":           "error=remote",
	} {
		_, loc, _ := bia.post("/delegations", url.Values{"handle": {handle}})
		if !strings.Contains(loc, want) {
			t.Errorf("%q: %s, want %s", handle, loc, want)
		}
	}
	if _, u := bia.startDelegation("user1@example.social"); !strings.HasSuffix(u.Path, "/oauth/authorize") {
		t.Errorf("own domain: %s", u)
	}
	if delegationCount(t, db, "2", "1") != 0 {
		t.Error("stored before authorization")
	}
}

func TestDelegationWithAnotherAccountIsRefused(t *testing.T) {
	f, app, db := testApp(t)
	bia := &browser{t: t, app: app.URL}
	bia.login("bia")
	// bia started, but ana's password was typed on the instance.
	if loc := bia.delegate("user3", "ana-d"); !strings.Contains(loc, "error=wrong_account") {
		t.Fatalf("callback: %s", loc)
	}
	if delegationCount(t, db, "1", "3") != 0 || delegationCount(t, db, "2", "3") != 0 {
		t.Error("stored a delegation")
	}
	if len(f.revoked) != 1 || f.revoked[0] != "token-ana-d" || f.revokedBy[0] != "did" {
		t.Errorf("revoked: %v by %v", f.revoked, f.revokedBy)
	}
}

func TestDelegationCallbackNeedsItsState(t *testing.T) {
	_, app, db := testApp(t)
	bia := &browser{t: t, app: app.URL}
	bia.login("bia")
	_, u := bia.startDelegation("user1")
	state := u.Query().Get("state")
	bia.dstate = ""
	if status, _, _ := bia.do(http.MethodGet, "/delegations/callback?state="+state+"&code=bia-d", "", nil); status != http.StatusBadRequest {
		t.Errorf("no cookie: %d", status)
	}
	// Another session can't finish bia's flow, even with her state.
	ana := &browser{t: t, app: app.URL}
	ana.login("ana")
	ana.dstate = state
	if status, _, _ := ana.do(http.MethodGet, "/delegations/callback?state="+state+"&code=bia-d", "", nil); status != http.StatusBadRequest {
		t.Errorf("other session: %d", status)
	}
	if delegationCount(t, db, "2", "1") != 0 {
		t.Error("stored")
	}
}

func TestActingNeedsADelegation(t *testing.T) {
	f, app, _ := testApp(t)
	ana := &browser{t: t, app: app.URL}
	ana.login("ana")
	if status := ana.actAs("2"); status != http.StatusForbidden || ana.as != "1" {
		t.Errorf("act as: %d %q", status, ana.as)
	}
	// Forging the form field doesn't change the account either.
	fields := baseFields()
	fields["as"] = "2"
	if status, _, _ := ana.compose(fields, nil); status != http.StatusConflict || len(f.forms) != 0 {
		t.Errorf("forged as: %d, %d sent", status, len(f.forms))
	}
}

func TestAccountChangedInAnotherTab(t *testing.T) {
	f, app, _ := testApp(t)
	bia := &browser{t: t, app: app.URL}
	bia.login("bia")
	bia.delegate("user1", "bia-d")
	ana := &browser{t: t, app: app.URL}
	ana.login("ana")
	ana.actAs("2")
	stale := ana.as // the compose page of the first tab
	ana.actAs("1")  // another tab switched back
	fields := baseFields()
	fields["as"] = stale
	if status, _, _ := ana.compose(fields, nil); status != http.StatusConflict || len(f.forms) != 0 {
		t.Errorf("stale tab: %d, %d sent", status, len(f.forms))
	}
}

func TestRevokeEndsAccessAndSharedToken(t *testing.T) {
	f, app, db := testApp(t)
	bia := &browser{t: t, app: app.URL}
	bia.login("bia")
	bia.delegate("user1", "bia-d")
	bia.delegate("user3", "bia-d") // Mastodon hands out the same token again
	ana := &browser{t: t, app: app.URL}
	ana.login("ana")
	ana.actAs("2")

	bia.post("/delegations/revoke", url.Values{"delegate": {"1"}})
	if delegationCount(t, db, "2", "1") != 0 {
		t.Fatal("still delegated")
	}
	if len(f.revoked) != 0 {
		t.Errorf("revoked a token cai still uses: %v", f.revoked)
	}
	if _, _, page := ana.do(http.MethodGet, "/queue", "", nil); !strings.Contains(page, "no longer valid") {
		t.Error("no notice after revoke")
	}
	if ana.refresh(); ana.as != "1" {
		t.Errorf("still acting as %q", ana.as)
	}
	if status, _, _ := ana.post("/post/1/cancel", url.Values{"as": {"2"}}); status != http.StatusConflict {
		t.Errorf("post as revoked owner: %d", status)
	}

	_, loc, _ := bia.post("/delegations/revoke", url.Values{"delegate": {"3"}})
	if !strings.Contains(loc, "done=removed") || len(f.revoked) != 1 || f.revoked[0] != "token-bia-d" || f.revokedBy[0] != "did" {
		t.Errorf("last revoke: %s %v %v", loc, f.revoked, f.revokedBy)
	}
	var n int
	db.pool.QueryRow(context.Background(), `SELECT count(*) FROM audit_log WHERE action = 'revoke_delegation' AND target_id = '2'`).Scan(&n)
	if n < 2 {
		t.Errorf("revokes audited: %d", n)
	}
}

func TestOnlyTheOwnerRevokes(t *testing.T) {
	f, app, db := testApp(t)
	bia := &browser{t: t, app: app.URL}
	bia.login("bia")
	bia.delegate("user1", "bia-d")
	cai := &browser{t: t, app: app.URL}
	cai.login("cai")
	// cai names bia's delegate, but revoke always works on cai's own account.
	cai.post("/delegations/revoke", url.Values{"delegate": {"1"}})
	if delegationCount(t, db, "2", "1") != 1 || len(f.revoked) != 0 {
		t.Error("someone else revoked bia's delegation")
	}
}

func TestLostTokenDropsOnlyTheDelegation(t *testing.T) {
	f, app, db := testApp(t)
	bia := &browser{t: t, app: app.URL}
	bia.login("bia")
	bia.delegate("user1", "bia-d")
	ana := &browser{t: t, app: app.URL}
	ana.login("ana")
	ana.actAs("2")
	f.mu.Lock()
	f.dead["token-bia-d"] = true // bia removed the app under Authorized apps
	f.mu.Unlock()
	if _, loc, _ := ana.do(http.MethodGet, "/queue", "", nil); !strings.Contains(loc, "delegation_lost") {
		t.Fatalf("queue: %s", loc)
	}
	if delegationCount(t, db, "2", "1") != 0 {
		t.Error("dead delegation kept")
	}
	if status, _, _ := ana.do(http.MethodGet, "/queue", "", nil); status != http.StatusOK {
		t.Errorf("ana signed out: %d", status)
	}
}

func TestActingChecksTheTokenFirst(t *testing.T) {
	f, app, db := testApp(t)
	bia := &browser{t: t, app: app.URL}
	bia.login("bia")
	bia.delegate("user1", "bia-d")
	f.mu.Lock()
	f.dead["token-bia-d"] = true
	f.mu.Unlock()
	ana := &browser{t: t, app: app.URL}
	ana.login("ana")
	ana.actAs("2")
	if ana.as != "1" || delegationCount(t, db, "2", "1") != 0 {
		t.Errorf("acting with a dead token: %q", ana.as)
	}
}

func TestDelegateLeaves(t *testing.T) {
	f, app, db := testApp(t)
	bia := &browser{t: t, app: app.URL}
	bia.login("bia")
	bia.delegate("user1", "bia-d")
	ana := &browser{t: t, app: app.URL}
	ana.login("ana")
	ana.actAs("2")
	ana.post("/delegations/leave", url.Values{"owner": {"2"}})
	if delegationCount(t, db, "2", "1") != 0 || len(f.revoked) != 1 {
		t.Errorf("leave: %v", f.revoked)
	}
	if ana.refresh(); ana.as != "1" {
		t.Errorf("still acting as %q", ana.as)
	}
}

// A delegate acting as the owner still grants only for their own account.
func TestNoDelegationChains(t *testing.T) {
	_, app, db := testApp(t)
	bia := &browser{t: t, app: app.URL}
	bia.login("bia")
	bia.delegate("user1", "bia-d")
	ana := &browser{t: t, app: app.URL}
	ana.login("ana")
	ana.actAs("2")
	if loc := ana.delegate("user3", "bia-d"); !strings.Contains(loc, "error=wrong_account") {
		t.Errorf("chain with bia's password: %s", loc)
	}
	if delegationCount(t, db, "2", "3") != 0 {
		t.Fatal("ana passed bia's access on")
	}
	ana.delegate("user3", "ana-d")
	if delegationCount(t, db, "1", "3") != 1 {
		t.Error("ana can still delegate her own account")
	}
}

func TestDelegationTokenIsBoundToItsPair(t *testing.T) {
	_, app, db := testApp(t)
	bia := &browser{t: t, app: app.URL}
	bia.login("bia")
	bia.delegate("user1", "bia-d")
	// A copied row, as if someone wrote to the database, doesn't grant anything.
	if _, err := db.pool.Exec(context.Background(), `INSERT INTO delegations (owner_id, owner_acct, delegate_id, delegate_acct, token)
		SELECT owner_id, owner_acct, '3', 'user3', token FROM delegations WHERE owner_id = '2' AND delegate_id = '1'`); err != nil {
		t.Fatal(err)
	}
	cai := &browser{t: t, app: app.URL}
	cai.login("cai")
	if status := cai.actAs("2"); status != http.StatusForbidden || cai.as != "3" {
		t.Errorf("copied row: %d %q", status, cai.as)
	}
}

func TestDelegationLimit(t *testing.T) {
	_, app, db := testApp(t)
	for i := range maxDelegations {
		if _, err := db.pool.Exec(context.Background(), `INSERT INTO delegations (owner_id, owner_acct, delegate_id, delegate_acct, token)
			VALUES ('2', 'user2', $1, 'x', '\x00')`, "90"+string(rune('0'+i))); err != nil {
			t.Fatal(err)
		}
	}
	bia := &browser{t: t, app: app.URL}
	bia.login("bia")
	if _, loc, _ := bia.post("/delegations", url.Values{"handle": {"user1"}}); !strings.Contains(loc, "too_many") {
		t.Errorf("limit: %s", loc)
	}
}

func TestDelegationConfig(t *testing.T) {
	base := map[string]string{"AGENDE_INSTANCE": "https://example.social", "AGENDE_BASE_URL": "https://agende.example.social",
		"AGENDE_CLIENT_ID": "id", "AGENDE_CLIENT_SECRET": "s", "AGENDE_ENCRYPTION_KEY": strings.Repeat("e", 32),
		"AGENDE_DB_HOST": "db", "AGENDE_DB_PASSWORD": "p", "AGENDE_REDIS_ADDR": "r:6379"}
	for name, extra := range map[string]map[string]string{
		"partial":  {"AGENDE_DELEGATION_CLIENT_ID": "did"},
		"short":    {"AGENDE_DELEGATION_CLIENT_ID": "did", "AGENDE_DELEGATION_CLIENT_SECRET": "x", "AGENDE_DELEGATION_KEY": "short"},
		"same key": {"AGENDE_DELEGATION_CLIENT_ID": "did", "AGENDE_DELEGATION_CLIENT_SECRET": "x", "AGENDE_DELEGATION_KEY": strings.Repeat("e", 32)},
		"same app": {"AGENDE_DELEGATION_CLIENT_ID": "id", "AGENDE_DELEGATION_CLIENT_SECRET": "x", "AGENDE_DELEGATION_KEY": strings.Repeat("d", 32)},
	} {
		for k, v := range base {
			t.Setenv(k, v)
		}
		for _, k := range []string{"AGENDE_DELEGATION_CLIENT_ID", "AGENDE_DELEGATION_CLIENT_SECRET", "AGENDE_DELEGATION_KEY"} {
			t.Setenv(k, extra[k])
		}
		if _, err := loadConfig(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	t.Setenv("AGENDE_DELEGATION_CLIENT_ID", "did")
	t.Setenv("AGENDE_DELEGATION_CLIENT_SECRET", "x")
	t.Setenv("AGENDE_DELEGATION_KEY", strings.Repeat("d", 32))
	if c, err := loadConfig(); err != nil || !c.delegation() {
		t.Errorf("valid: %v", err)
	}
}

func TestNormalizeHandle(t *testing.T) {
	for in, want := range map[string]string{"@ana": "ana", "ana": "ana", " ana@Example.Social ": "ana", "ana@other.social": "", "a b": "", "@": ""} {
		got, ok := normalizeHandle(in, "example.social")
		if (want == "") == ok || (ok && got != want) {
			t.Errorf("%q: %q %v", in, got, ok)
		}
	}
}

// If the instance hands out a new token, every delegation of the owner moves to
// it and the old one is revoked, so revoking the last delegation leaves nothing.
func TestNewTokenReplacesTheOldEverywhere(t *testing.T) {
	f, app, db := testApp(t)
	bia := &browser{t: t, app: app.URL}
	bia.login("bia")
	bia.delegate("user1", "bia-d")
	bia.delegate("user3", "bia") // a different token for the same account
	if len(f.revoked) != 1 || f.revoked[0] != "token-bia-d" {
		t.Fatalf("old token: %v", f.revoked)
	}
	for _, delegate := range []string{"1", "3"} {
		if _, token, err := db.delegationToken(context.Background(), "2", delegate); err != nil || token != "token-bia" {
			t.Errorf("%s holds %q %v", delegate, token, err)
		}
	}
	bia.post("/delegations/revoke", url.Values{"delegate": {"1"}})
	bia.post("/delegations/revoke", url.Values{"delegate": {"3"}})
	if len(f.revoked) != 2 || f.revoked[1] != "token-bia" {
		t.Errorf("revoked: %v", f.revoked)
	}
}

// A delegate whose own sign-in died (suspended, or removed this app) stops acting
// as the owner, even though acting never uses the delegate's own token.
func TestDeadDelegateStopsActing(t *testing.T) {
	f, app, _ := testApp(t)
	bia := &browser{t: t, app: app.URL}
	bia.login("bia")
	bia.delegate("user1", "bia-d")
	ana := &browser{t: t, app: app.URL}
	ana.login("ana")
	ana.actAs("2")
	f.mu.Lock()
	f.dead["token-ana"] = true
	f.mu.Unlock()
	rdb := redis.NewClient(&redis.Options{Addr: os.Getenv("AGENDE_TEST_REDIS"), DB: testRedisDB(t)})
	keys, _ := rdb.Keys(context.Background(), aliveKey+"*").Result()
	if len(keys) == 0 {
		t.Fatal("the check is not cached")
	}
	rdb.Del(context.Background(), keys...) // as if the cache expired
	if status, loc, _ := ana.do(http.MethodGet, "/queue", "", nil); status != http.StatusFound || loc != "/" {
		t.Errorf("still signed in: %d %s", status, loc)
	}
	if n := len(f.forms); n != 0 {
		t.Errorf("%d posts", n)
	}
}

func TestOnlyTheDelegationsPageAllowsTheInstanceAsFormAction(t *testing.T) {
	_, app, _ := testApp(t)
	bia := &browser{t: t, app: app.URL}
	bia.login("bia")
	policy := func(path string) string {
		req, _ := http.NewRequest(http.MethodGet, app.URL+path, nil)
		req.Header.Set("Cookie", bia.cookie)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.Header.Get("Content-Security-Policy")
	}
	got := policy("/delegations")
	if !strings.Contains(got, "form-action 'self' http://127.0.0.1") || strings.Count(got, "form-action") != 1 {
		t.Errorf("delegations: %s", got)
	}
	if got := policy("/queue"); !strings.Contains(got, "form-action 'self';") {
		t.Errorf("queue: %s", got)
	}
}

func TestLimitIsCheckedAgainAtTheCallback(t *testing.T) {
	_, app, db := testApp(t)
	bia := &browser{t: t, app: app.URL}
	bia.login("bia")
	_, u := bia.startDelegation("user1") // started below the limit
	for i := range maxDelegations {
		db.pool.Exec(context.Background(), `INSERT INTO delegations (owner_id, owner_acct, delegate_id, delegate_acct, token)
			VALUES ('2', 'user2', $1, 'x', '\x00')`, "80"+string(rune('0'+i)))
	}
	_, loc, _ := bia.do(http.MethodGet, "/delegations/callback?state="+u.Query().Get("state")+"&code=bia-d", "", nil)
	if !strings.Contains(loc, "too_many") || delegationCount(t, db, "2", "1") != 0 {
		t.Errorf("callback: %s", loc)
	}
}
