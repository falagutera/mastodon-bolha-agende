package main

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	sessionTTL  = 8 * time.Hour
	expiryIndex = "agende:expiry"
	sessionKey  = "agende:session:"
	// Kept past expiry so the sweep can keep trying to revoke the token.
	sweepGrace    = 7 * 24 * time.Hour
	tokenRefs     = "agende:token:"
	revokeLock    = "agende:revoking:"
	revokeLockTTL = 2 * time.Minute
	// "Post as" lives apart from the session, so switching never rewrites the
	// session itself, which could undo a sign-out happening at the same time.
	actingKey = "agende:acting:"
)

var (
	errNoSession  = errors.New("no session")
	errUnreadable = errors.New("session encrypted with another key")
)

type session struct {
	ID        string    `json:"-"`
	AccountID string    `json:"account_id"`
	Acct      string    `json:"acct"`
	CSRF      string    `json:"csrf"`
	Token     string    `json:"-"`
	Sealed    []byte    `json:"token"`
	Expires   time.Time `json:"expires"`
	// ActingAs is the owner chosen in "Post as", read from its own key. It is only
	// a wish: every request checks the delegation again in the database.
	ActingAs string `json:"-"`

	// Filled per request by server.resolve, never stored. Post actions use these;
	// the account's own pages (settings, delegations, sign-out) use the fields above.
	ActToken   string `json:"-"`
	TargetID   string `json:"-"`
	TargetAcct string `json:"-"`
	Delegated  bool   `json:"-"`
	Lost       bool   `json:"-"` // the chosen delegation was revoked
}

type sessions struct {
	rdb *redis.Client
	box *box
}

func (s *sessions) create(ctx context.Context, accountID, acct, token string) (session, error) {
	id := randomToken()
	key := hashID(id)
	sess := session{ID: id, AccountID: accountID, Acct: acct, CSRF: randomToken(), Token: token, Expires: time.Now().Add(sessionTTL)}
	sess.Sealed = s.box.seal([]byte(token), []byte(key+"\x00"+accountID))
	raw, err := json.Marshal(sess)
	if err != nil {
		return session{}, err
	}
	_, err = s.rdb.TxPipelined(ctx, func(p redis.Pipeliner) error {
		p.Set(ctx, sessionKey+key, raw, sessionTTL+sweepGrace)
		p.ZAdd(ctx, expiryIndex, redis.Z{Score: float64(sess.Expires.Unix()), Member: key})
		p.SAdd(ctx, tokenRefs+hashID(token), key)
		p.Expire(ctx, tokenRefs+hashID(token), sessionTTL+sweepGrace)
		return nil
	})
	return sess, err
}

func (s *sessions) get(ctx context.Context, id string) (session, error) {
	if id == "" {
		return session{}, errNoSession
	}
	sess, err := s.load(ctx, hashID(id))
	if errors.Is(err, errUnreadable) {
		return session{}, errNoSession
	}
	if err != nil {
		return session{}, err
	}
	if time.Now().After(sess.Expires) {
		return session{}, errNoSession
	}
	acting, err := s.rdb.Get(ctx, actingKey+hashID(id)).Result()
	if err != nil && !errors.Is(err, redis.Nil) {
		return session{}, err
	}
	sess.ID, sess.ActingAs = id, acting
	return sess, nil
}

func (s *sessions) load(ctx context.Context, key string) (session, error) {
	raw, err := s.rdb.Get(ctx, sessionKey+key).Bytes()
	if errors.Is(err, redis.Nil) {
		return session{}, errNoSession
	}
	if err != nil {
		return session{}, err
	}
	var sess session
	if err := json.Unmarshal(raw, &sess); err != nil {
		return session{}, err
	}
	token, err := s.box.open(sess.Sealed, []byte(key+"\x00"+sess.AccountID))
	if err != nil {
		return session{}, errUnreadable
	}
	sess.Token = string(token)
	return sess, nil
}

// setActing records the account chosen in "Post as". Empty means the account itself.
func (s *sessions) setActing(ctx context.Context, id, ownerID string) error {
	key := hashID(id)
	if ownerID == "" {
		return s.rdb.Del(ctx, actingKey+key).Err()
	}
	n, err := s.rdb.Exists(ctx, sessionKey+key).Result()
	if err != nil {
		return err
	}
	if n == 0 {
		return errNoSession
	}
	// Outlives the session at most by the sweep grace, and is removed with it.
	return s.rdb.Set(ctx, actingKey+key, ownerID, sessionTTL+sweepGrace).Err()
}

// expire makes a session unusable at once and leaves it for finish to revoke.
func (s *sessions) expire(ctx context.Context, key string) error {
	raw, err := s.rdb.Get(ctx, sessionKey+key).Bytes()
	if errors.Is(err, redis.Nil) {
		return s.rdb.ZRem(ctx, expiryIndex, key).Err()
	}
	if err != nil {
		return err
	}
	var sess session
	if err := json.Unmarshal(raw, &sess); err != nil {
		return err
	}
	sess.Expires = time.Now().Add(-time.Second)
	updated, err := json.Marshal(sess)
	if err != nil {
		return err
	}
	_, err = s.rdb.TxPipelined(ctx, func(p redis.Pipeliner) error {
		p.Set(ctx, sessionKey+key, updated, redis.KeepTTL)
		p.ZAdd(ctx, expiryIndex, redis.Z{Score: float64(sess.Expires.Unix()), Member: key})
		return nil
	})
	return err
}

// finish revokes an expired session's token and only then forgets it, so a failed
// revoke is tried again. The lock keeps two replicas from revoking the same token.
func (s *sessions) finish(ctx context.Context, key string, revoke func(string) error) {
	got, err := s.rdb.SetNX(ctx, revokeLock+key, 1, revokeLockTTL).Result()
	if err != nil {
		log.Printf("sweep: %v", err)
		return
	}
	if !got {
		return
	}
	defer s.rdb.Del(ctx, revokeLock+key)
	sess, err := s.load(ctx, key)
	var redisDown bool
	if err != nil && !errors.Is(err, errNoSession) && !errors.Is(err, errUnreadable) {
		redisDown = true
	}
	if redisDown {
		log.Printf("sweep: %v", err)
		return
	}
	if err == nil {
		// Mastodon gives the same token to every login of an account in this app,
		// so it can only be revoked with the last session using it.
		last, err := s.lastUser(ctx, sess.Token, key)
		if err != nil {
			log.Printf("sweep: %v", err)
			return
		}
		if last {
			if err := revoke(sess.Token); err != nil {
				log.Printf("sweep: revoke: %v", err)
				return
			}
		}
		s.rdb.SRem(ctx, tokenRefs+hashID(sess.Token), key)
	} else if errors.Is(err, errUnreadable) {
		log.Print("sweep: dropping a session encrypted with another key")
	}
	if _, err := s.rdb.TxPipelined(ctx, func(p redis.Pipeliner) error {
		p.ZRem(ctx, expiryIndex, key)
		p.Del(ctx, sessionKey+key, actingKey+key)
		return nil
	}); err != nil {
		log.Printf("sweep: %v", err)
	}
}

func (s *sessions) lastUser(ctx context.Context, token, key string) (bool, error) {
	keys, err := s.rdb.SMembers(ctx, tokenRefs+hashID(token)).Result()
	if err != nil {
		return false, err
	}
	for _, other := range keys {
		if other == key {
			continue
		}
		sess, err := s.load(ctx, other)
		if err == nil && time.Now().Before(sess.Expires) {
			return false, nil
		}
		if err != nil && !errors.Is(err, errNoSession) && !errors.Is(err, errUnreadable) {
			return false, err
		}
	}
	return true, nil
}

// sweep revokes the tokens of expired sessions: Mastodon's tokens never expire.
func (s *sessions) sweep(ctx context.Context, revoke func(string) error) {
	keys, err := s.rdb.ZRangeByScore(ctx, expiryIndex, &redis.ZRangeBy{
		Min: "-inf", Max: strconv.FormatInt(time.Now().Unix(), 10), Count: 100,
	}).Result()
	if err != nil {
		log.Printf("sweep: %v", err)
		return
	}
	for _, key := range keys {
		s.finish(ctx, key, revoke)
	}
}
