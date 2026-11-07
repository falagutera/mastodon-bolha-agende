package main

// Delegation: an owner lets another local account (the delegate) schedule posts as
// the owner. Mastodon has no such thing, so the owner authorizes a separate OAuth
// app while signed in here as themself, and the token is kept encrypted.
//
// Rules, from the plan:
//   - Only the owner grants, signed in as the owner. The OAuth step asks for the
//     password again (prompt=login) and the callback checks the account.
//   - Bound by numeric account id, never by @name.
//   - Local, active accounts only. No chains: a delegate never grants for the owner,
//     because the delegations page always works on the signed-in account.
//   - A delegate schedules, duplicates, edits and cancels. Nothing else is exposed.
//   - Every check of "may this account act as that one" goes through resolve,
//     which reads the database on every request.

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	delegateStateCookie = "__Host-agende-dstate"
	delegateStateKey    = "agende:delegate:"
	// While acting as an owner, a request never uses the delegate's own token, so
	// it is checked with the instance at least this often.
	aliveKey   = "agende:alive:"
	aliveTTL   = 3 * time.Minute
	ownerLock  = "agende:dlock:"
	ownerLockT = 30 * time.Second
)

var errBusy = errors.New("busy")

// lockOwner serializes changes to one owner's delegations across replicas: they
// share one token, so a revoke must not race a new authorization.
func (s *server) lockOwner(ctx context.Context, owner string) (func(), error) {
	key, mine := ownerLock+owner, randomToken()
	ok, err := s.rdb.SetNX(ctx, key, mine, ownerLockT).Result()
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, errBusy
	}
	return func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		// Delete only our own lock, not one taken after ours expired.
		unlock.Run(ctx, s.rdb, []string{key}, mine)
	}, nil
}

var unlock = redis.NewScript(`if redis.call("GET", KEYS[1]) == ARGV[1] then return redis.call("DEL", KEYS[1]) end return 0`)

// ownTokenAlive checks the delegate's own sign-in: a suspended delegate, or one
// that removed this app on the instance, must stop acting as anyone.
func (s *server) ownTokenAlive(ctx context.Context, sess session) (bool, error) {
	key := aliveKey + hashID(sess.ID)
	n, err := s.rdb.Exists(ctx, key).Result()
	if err != nil {
		return false, err
	}
	if n == 1 {
		return true, nil
	}
	acct, err := s.api.verify(ctx, sess.Token)
	var api *apiError
	if errors.As(err, &api) && api.Status == http.StatusUnauthorized {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if acct.ID != sess.AccountID {
		return false, nil
	}
	return true, s.rdb.Set(ctx, key, 1, aliveTTL).Err()
}

type actOption struct {
	ID   string
	Acct string
	On   bool
}

// resolve fills the account a request acts on. It is the single place that turns
// the "Post as" choice into a token, and it checks the database each time.
func (s *server) resolve(ctx context.Context, sess *session) error {
	sess.ActToken, sess.TargetID, sess.TargetAcct, sess.Delegated = sess.Token, sess.AccountID, sess.Acct, false
	if sess.ActingAs == "" || sess.ActingAs == sess.AccountID || !s.cfg.delegation() {
		return nil
	}
	d, token, err := s.db.delegationToken(ctx, sess.ActingAs, sess.AccountID)
	if errors.Is(err, errNoDelegation) {
		// Revoked meanwhile: fall back to the account itself and say so.
		if err := s.sessions.setActing(ctx, sess.ID, ""); err != nil && !errors.Is(err, errNoSession) {
			return err
		}
		sess.ActingAs, sess.Lost = "", true
		return nil
	}
	if err != nil {
		return err
	}
	alive, err := s.ownTokenAlive(ctx, *sess)
	if err != nil {
		return err
	}
	if !alive {
		// The session is dead: let the sweep revoke it, and sign out.
		if err := s.sessions.expire(ctx, hashID(sess.ID)); err != nil {
			log.Printf("session: %v", err)
		}
		return errNoSession
	}
	sess.ActToken, sess.TargetID, sess.TargetAcct, sess.Delegated = token, d.OwnerID, d.OwnerAcct, true
	return nil
}

// sameTarget makes a post form act only on the account it was shown for. If
// "Post as" changed in another tab, the form is refused instead of posting as the
// wrong account.
func sameTarget(r *http.Request, sess session) bool {
	return subtle.ConstantTimeCompare([]byte(r.PostForm.Get("as")), []byte(sess.TargetID)) == 1
}

// dropLost forgets a delegation whose token the instance no longer accepts: the
// owner removed it under Authorized apps, or the account was suspended.
func (s *server) dropLost(ctx context.Context, sess session) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	d, _, _, err := s.db.deleteDelegation(ctx, sess.TargetID, sess.AccountID)
	if err != nil && !errors.Is(err, errNoDelegation) {
		log.Printf("delegation: %v", err)
	}
	if err == nil {
		s.auditDelegation(ctx, sess.AccountID, sess.Acct, d, actionLost)
	}
	if err := s.sessions.setActing(ctx, sess.ID, ""); err != nil && !errors.Is(err, errNoSession) {
		log.Printf("delegation: %v", err)
	}
}

func (s *server) auditDelegation(ctx context.Context, actorID, actorAcct string, d delegation, action string) {
	s.auditResult(ctx, auditEntry{OpID: randomToken()[:22], ActorID: actorID, ActorAcct: actorAcct,
		TargetID: d.OwnerID, TargetAcct: d.OwnerAcct, SubjectID: d.DelegateID, SubjectAcct: d.DelegateAcct,
		Action: action, Result: resultOK})
}

func (s *server) actOptions(ctx context.Context, sess session) []actOption {
	if !s.cfg.delegation() || sess.AccountID == "" {
		return nil
	}
	list, err := s.db.grantedTo(ctx, sess.AccountID)
	if err != nil {
		log.Printf("delegation: %v", err)
		return nil
	}
	if len(list) == 0 {
		return nil
	}
	opts := []actOption{{ID: sess.AccountID, Acct: sess.Acct, On: !sess.Delegated}}
	for _, d := range list {
		opts = append(opts, actOption{ID: d.OwnerID, Acct: d.OwnerAcct, On: sess.Delegated && d.OwnerID == sess.TargetID})
	}
	return opts
}

func (s *server) handleDelegations(w http.ResponseWriter, r *http.Request, sess session) {
	v := s.view(r, sess, "delegations")
	var err error
	if v.Granted, err = s.db.grantedBy(r.Context(), sess.AccountID); err == nil {
		v.Received, err = s.db.grantedTo(r.Context(), sess.AccountID)
	}
	if err != nil {
		log.Printf("delegation: %v", err)
		s.renderError(w, r, http.StatusServiceUnavailable, "unavailable")
		return
	}
	if key := r.URL.Query().Get("error"); key != "" {
		if msg := v.T["delegation_"+key]; msg != "" {
			v.Error = msg
		}
	}
	v.MaxDelegations = maxDelegations
	// The grant form answers with a redirect to the instance, and browsers apply
	// form-action to that redirect too.
	w.Header().Set("Content-Security-Policy", csp("'self' "+s.cfg.instance))
	s.render(w, "delegations.html", http.StatusOK, v)
}

// normalizeHandle turns "@ana", "ana" or "ana@this.domain" into "ana". Another
// domain is a remote account, which can't be a delegate.
func normalizeHandle(in, domain string) (string, bool) {
	h := strings.TrimPrefix(strings.TrimSpace(in), "@")
	if name, host, ok := strings.Cut(h, "@"); ok {
		if !strings.EqualFold(host, domain) {
			return "", false
		}
		h = name
	}
	return h, handlePattern.MatchString(h)
}

type pendingDelegation struct {
	Verifier     string `json:"verifier"`
	Owner        string `json:"owner"`
	Session      string `json:"session"`
	DelegateID   string `json:"delegate_id"`
	DelegateAcct string `json:"delegate_acct"`
}

// handleDelegate starts a delegation: it checks the delegate, then sends the owner
// to authorize the delegation app, typing the password again.
func (s *server) handleDelegate(w http.ResponseWriter, r *http.Request, sess session) {
	back := func(key string) {
		http.Redirect(w, r, "/delegations?error="+url.QueryEscape(key), http.StatusSeeOther)
	}
	name, ok := normalizeHandle(r.PostForm.Get("handle"), s.cfg.domain)
	if !ok {
		back("remote")
		return
	}
	n, err := s.db.countDelegations(r.Context(), sess.AccountID)
	if err != nil {
		log.Printf("delegation: %v", err)
		s.renderError(w, r, http.StatusServiceUnavailable, "unavailable")
		return
	}
	if n >= maxDelegations {
		back("too_many")
		return
	}
	acct, err := s.api.localAccount(r.Context(), name)
	var api *apiError
	if errors.As(err, &api) && api.Status == http.StatusNotFound {
		back("no_account")
		return
	}
	if err != nil {
		log.Printf("lookup: %v", err)
		s.renderError(w, r, http.StatusBadGateway, "instance")
		return
	}
	if acct.ID == sess.AccountID {
		back("self")
		return
	}
	state, verifier := randomToken(), randomToken()
	raw, _ := json.Marshal(pendingDelegation{Verifier: verifier, Owner: sess.AccountID, Session: hashID(sess.ID),
		DelegateID: acct.ID, DelegateAcct: acct.Acct})
	if err := s.rdb.Set(r.Context(), delegateStateKey+hashID(state), raw, stateTTL).Err(); err != nil {
		log.Printf("delegation: %v", err)
		s.renderError(w, r, http.StatusServiceUnavailable, "unavailable")
		return
	}
	s.setCookie(w, delegateStateCookie, state, stateTTL)
	// prompt=login asks for the password even with a session open on the instance,
	// and force_login shows the authorization screen instead of skipping it.
	extra := url.Values{"prompt": {"login"}, "force_login": {"true"}}
	http.Redirect(w, r, s.authorizeURL(s.delegationApp(), state, verifier, extra), http.StatusSeeOther)
}

func (s *server) handleDelegationCallback(w http.ResponseWriter, r *http.Request, sess session) {
	c, err := r.Cookie(delegateStateCookie)
	if err != nil {
		s.renderError(w, r, http.StatusBadRequest, "expired")
		return
	}
	s.clearCookie(w, delegateStateCookie)
	q := r.URL.Query()
	if subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(c.Value)) != 1 {
		s.renderError(w, r, http.StatusBadRequest, "expired")
		return
	}
	raw, err := s.rdb.GetDel(r.Context(), delegateStateKey+hashID(c.Value)).Bytes()
	if errors.Is(err, redis.Nil) {
		s.renderError(w, r, http.StatusBadRequest, "expired")
		return
	}
	if err != nil {
		log.Printf("delegation: %v", err)
		s.renderError(w, r, http.StatusServiceUnavailable, "unavailable")
		return
	}
	var p pendingDelegation
	// The flow belongs to the session that started it.
	if json.Unmarshal(raw, &p) != nil || p.Owner != sess.AccountID || p.Session != hashID(sess.ID) || p.DelegateID == sess.AccountID {
		s.renderError(w, r, http.StatusBadRequest, "expired")
		return
	}
	if q.Get("code") == "" {
		s.renderError(w, r, http.StatusForbidden, "denied")
		return
	}
	release, err := s.lockOwner(r.Context(), sess.AccountID)
	if errors.Is(err, errBusy) {
		s.renderError(w, r, http.StatusServiceUnavailable, "busy")
		return
	}
	if err != nil {
		log.Printf("delegation: %v", err)
		s.renderError(w, r, http.StatusServiceUnavailable, "unavailable")
		return
	}
	defer release()
	app := s.delegationApp()
	token, err := s.exchangeCode(r.Context(), app, q.Get("code"), p.Verifier)
	if err != nil {
		log.Printf("delegation token: %v", err)
		s.renderError(w, r, http.StatusBadGateway, "instance")
		return
	}
	acct, err := s.api.verify(r.Context(), token)
	if err != nil {
		log.Printf("delegation verify: %v", err)
		s.renderError(w, r, http.StatusBadGateway, "instance")
		return
	}
	if acct.ID != sess.AccountID {
		// Someone authorized with another account. Its token is shared by all of its
		// delegations, so revoke it only if it has none.
		if release, err := s.lockOwner(r.Context(), acct.ID); err == nil {
			if n, err := s.db.countDelegations(r.Context(), acct.ID); err == nil && n == 0 {
				if err := s.revokeWith(app, token); err != nil {
					log.Printf("delegation revoke: %v", err)
				}
			}
			release()
		}
		http.Redirect(w, r, "/delegations?error=wrong_account", http.StatusSeeOther)
		return
	}
	d := delegation{OwnerID: sess.AccountID, OwnerAcct: acct.Username, DelegateID: p.DelegateID, DelegateAcct: p.DelegateAcct}
	stale, err := s.db.putDelegation(r.Context(), d, token)
	if err != nil {
		// Nothing holds the new token: don't leave it valid on the instance.
		if n, cerr := s.db.countDelegations(r.Context(), sess.AccountID); cerr == nil && n == 0 {
			if err := s.revokeWith(app, token); err != nil {
				log.Printf("delegation revoke: %v", err)
			}
		}
		if errors.Is(err, errTooMany) {
			http.Redirect(w, r, "/delegations?error=too_many", http.StatusSeeOther)
			return
		}
		log.Printf("delegation: %v", err)
		s.renderError(w, r, http.StatusServiceUnavailable, "unavailable")
		return
	}
	for _, old := range stale {
		if err := s.revokeWith(app, old); err != nil {
			log.Printf("delegation revoke old token: %v", err)
		}
	}
	s.auditDelegation(r.Context(), sess.AccountID, sess.Acct, d, actionDelegate)
	http.Redirect(w, r, "/delegations?done=delegated", http.StatusSeeOther)
}

// removeDelegation deletes one delegation and revokes the token when it was the
// owner's last. revoked is false when the instance didn't confirm.
func (s *server) removeDelegation(ctx context.Context, ownerID, delegateID string) (delegation, bool, error) {
	release, err := s.lockOwner(ctx, ownerID)
	if err != nil {
		return delegation{}, false, err
	}
	defer release()
	d, token, last, err := s.db.deleteDelegation(ctx, ownerID, delegateID)
	if err != nil {
		return d, false, err
	}
	if !last || token == "" {
		return d, true, nil
	}
	if err := s.revokeWith(s.delegationApp(), token); err != nil {
		log.Printf("delegation revoke: %v", err)
		return d, false, nil
	}
	return d, true, nil
}

// handleRevoke is the owner taking access away from a delegate.
func (s *server) handleRevoke(w http.ResponseWriter, r *http.Request, sess session) {
	delegate := r.PostForm.Get("delegate")
	if !idPattern.MatchString(delegate) {
		s.renderError(w, r, http.StatusNotFound, "not_found")
		return
	}
	d, revoked, err := s.removeDelegation(r.Context(), sess.AccountID, delegate)
	s.afterRemove(w, r, sess, d, revoked, err, actionRevoke)
}

// handleLeave is the delegate giving up access to an owner.
func (s *server) handleLeave(w http.ResponseWriter, r *http.Request, sess session) {
	owner := r.PostForm.Get("owner")
	if !idPattern.MatchString(owner) {
		s.renderError(w, r, http.StatusNotFound, "not_found")
		return
	}
	d, revoked, err := s.removeDelegation(r.Context(), owner, sess.AccountID)
	if err == nil && sess.ActingAs == owner {
		if err := s.sessions.setActing(r.Context(), sess.ID, ""); err != nil {
			log.Printf("delegation: %v", err)
		}
	}
	s.afterRemove(w, r, sess, d, revoked, err, actionLeave)
}

func (s *server) afterRemove(w http.ResponseWriter, r *http.Request, sess session, d delegation, revoked bool, err error, action string) {
	if errors.Is(err, errNoDelegation) {
		http.Redirect(w, r, "/delegations", http.StatusSeeOther)
		return
	}
	if errors.Is(err, errBusy) {
		s.renderError(w, r, http.StatusServiceUnavailable, "busy")
		return
	}
	if err != nil {
		log.Printf("delegation: %v", err)
		s.renderError(w, r, http.StatusServiceUnavailable, "unavailable")
		return
	}
	s.auditDelegation(r.Context(), sess.AccountID, sess.Acct, d, action)
	done := "removed"
	if !revoked {
		done = "removed_unconfirmed"
	}
	http.Redirect(w, r, "/delegations?done="+done, http.StatusSeeOther)
}

// handleActing switches "Post as". The choice is checked here and again on every
// request, and the token is confirmed with the instance before it is used.
func (s *server) handleActing(w http.ResponseWriter, r *http.Request, sess session) {
	owner := r.PostForm.Get("account")
	if owner == "" || owner == sess.AccountID {
		if err := s.sessions.setActing(r.Context(), sess.ID, ""); err != nil {
			log.Printf("acting: %v", err)
			s.renderError(w, r, http.StatusServiceUnavailable, "unavailable")
			return
		}
		http.Redirect(w, r, "/queue", http.StatusSeeOther)
		return
	}
	if !s.cfg.delegation() || !idPattern.MatchString(owner) {
		s.renderError(w, r, http.StatusForbidden, "not_delegated")
		return
	}
	_, token, err := s.db.delegationToken(r.Context(), owner, sess.AccountID)
	if errors.Is(err, errNoDelegation) {
		s.renderError(w, r, http.StatusForbidden, "not_delegated")
		return
	}
	if err != nil {
		log.Printf("acting: %v", err)
		s.renderError(w, r, http.StatusServiceUnavailable, "unavailable")
		return
	}
	acct, err := s.api.verify(r.Context(), token)
	var api *apiError
	if (errors.As(err, &api) && api.Status == http.StatusUnauthorized) || (err == nil && acct.ID != owner) {
		lost := sess
		lost.TargetID = owner
		s.dropLost(r.Context(), lost)
		http.Redirect(w, r, "/queue?done=delegation_lost", http.StatusSeeOther)
		return
	}
	if err != nil {
		log.Printf("acting verify: %v", err)
		s.renderError(w, r, http.StatusBadGateway, "instance")
		return
	}
	if err := s.sessions.setActing(r.Context(), sess.ID, owner); err != nil {
		log.Printf("acting: %v", err)
		s.renderError(w, r, http.StatusServiceUnavailable, "unavailable")
		return
	}
	http.Redirect(w, r, "/queue", http.StatusSeeOther)
}
