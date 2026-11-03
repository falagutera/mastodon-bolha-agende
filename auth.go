package main

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	sessionCookie = "__Host-agende"
	stateCookie   = "__Host-agende-state"
	stateTTL      = 10 * time.Minute
	stateKey      = "agende:state:"
	oauthScope    = "profile read:statuses write:statuses write:media"
)

func (s *server) setCookie(w http.ResponseWriter, name, value string, ttl time.Duration) {
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    value,
		Path:     "/",
		MaxAge:   int(ttl.Seconds()),
		HttpOnly: true,
		Secure:   true,
		// Lax: the cookie must survive the top-level redirect back from the instance.
		SameSite: http.SameSiteLaxMode,
	})
}

func (s *server) clearCookie(w http.ResponseWriter, name string) {
	s.setCookie(w, name, "", -time.Second)
}

// oauthApp is one of the two OAuth applications: sign-in, or delegation.
type oauthApp struct {
	id, secret, redirect string
}

func (s *server) loginApp() oauthApp {
	return oauthApp{id: s.cfg.clientID, secret: s.cfg.clientSecret, redirect: s.cfg.baseURL + "/auth/callback"}
}

func (s *server) delegationApp() oauthApp {
	return oauthApp{id: s.cfg.delegationClientID, secret: s.cfg.delegationClientSecret, redirect: s.cfg.baseURL + "/delegations/callback"}
}

// authorizeURL starts an authorization code flow with PKCE. extra adds parameters
// such as prompt=login.
func (s *server) authorizeURL(app oauthApp, state, verifier string, extra url.Values) string {
	challenge := sha256.Sum256([]byte(verifier))
	q := url.Values{
		"response_type":         {"code"},
		"client_id":             {app.id},
		"redirect_uri":          {app.redirect},
		"scope":                 {oauthScope},
		"state":                 {state},
		"code_challenge":        {base64.RawURLEncoding.EncodeToString(challenge[:])},
		"code_challenge_method": {"S256"},
	}
	for k, v := range extra {
		q[k] = v
	}
	return s.cfg.instance + "/oauth/authorize?" + q.Encode()
}

func (s *server) handleLogin(w http.ResponseWriter, r *http.Request) {
	state, verifier := randomToken(), randomToken()
	if err := s.rdb.Set(r.Context(), stateKey+hashID(state), verifier, stateTTL).Err(); err != nil {
		log.Printf("login: %v", err)
		s.renderError(w, r, http.StatusServiceUnavailable, "unavailable")
		return
	}
	s.setCookie(w, stateCookie, state, stateTTL)
	http.Redirect(w, r, s.authorizeURL(s.loginApp(), state, verifier, nil), http.StatusFound)
}

func (s *server) handleCallback(w http.ResponseWriter, r *http.Request) {
	c, err := r.Cookie(stateCookie)
	if err != nil {
		s.renderError(w, r, http.StatusBadRequest, "expired")
		return
	}
	s.clearCookie(w, stateCookie)
	q := r.URL.Query()
	if subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(c.Value)) != 1 {
		s.renderError(w, r, http.StatusBadRequest, "expired")
		return
	}
	// GetDel: a state works once.
	verifier, err := s.rdb.GetDel(r.Context(), stateKey+hashID(c.Value)).Result()
	if errors.Is(err, redis.Nil) {
		s.renderError(w, r, http.StatusBadRequest, "expired")
		return
	}
	if err != nil {
		log.Printf("callback: %v", err)
		s.renderError(w, r, http.StatusServiceUnavailable, "unavailable")
		return
	}
	if q.Get("code") == "" {
		s.renderError(w, r, http.StatusForbidden, "denied")
		return
	}
	token, err := s.exchangeCode(r.Context(), s.loginApp(), q.Get("code"), verifier)
	if err != nil {
		log.Printf("token exchange: %v", err)
		s.renderError(w, r, http.StatusBadGateway, "instance")
		return
	}
	acct, err := s.api.verify(r.Context(), token)
	if err != nil {
		log.Printf("verify_credentials: %v", err)
		s.revokeNow(token)
		s.renderError(w, r, http.StatusBadGateway, "instance")
		return
	}
	sess, err := s.sessions.create(r.Context(), acct.ID, acct.Username, token)
	if err != nil {
		log.Printf("session: %v", err)
		s.revokeNow(token)
		s.renderError(w, r, http.StatusServiceUnavailable, "unavailable")
		return
	}
	s.setCookie(w, sessionCookie, sess.ID, sessionTTL)
	http.Redirect(w, r, "/queue", http.StatusFound)
}

func (s *server) handleLogout(w http.ResponseWriter, r *http.Request, sess session) {
	key := hashID(sess.ID)
	if err := s.sessions.expire(r.Context(), key); err != nil {
		log.Printf("logout: %v", err)
		s.renderError(w, r, http.StatusServiceUnavailable, "unavailable")
		return
	}
	s.sessions.finish(r.Context(), key, s.revokeToken)
	s.clearCookie(w, sessionCookie)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *server) exchangeCode(ctx context.Context, app oauthApp, code, verifier string) (string, error) {
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"client_id":     {app.id},
		"client_secret": {app.secret},
		"redirect_uri":  {app.redirect},
		"code_verifier": {verifier},
		"scope":         {oauthScope},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.cfg.instance+"/oauth/token", bodyOf(form))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := s.api.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("status %d", resp.StatusCode)
	}
	var out struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&out); err != nil {
		return "", err
	}
	if out.AccessToken == "" {
		return "", errors.New("empty access token")
	}
	return out.AccessToken, nil
}

func (s *server) revokeToken(token string) error { return s.revokeWith(s.loginApp(), token) }

func (s *server) revokeWith(app oauthApp, token string) error {
	form := url.Values{"client_id": {app.id}, "client_secret": {app.secret}, "token": {token}}
	resp, err := s.api.client.PostForm(s.cfg.instance+"/oauth/revoke", form)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	return nil
}

// revokeNow is for a token that never became a session; the instance keeps it if this fails.
func (s *server) revokeNow(token string) {
	if err := s.revokeToken(token); err != nil {
		log.Printf("revoke: %v", err)
	}
}
