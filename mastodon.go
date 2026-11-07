package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	responseLimit = 4 << 20
	pageSize      = 40
	mediaWait     = 90 * time.Second
)

// apiError carries Mastodon's message, which is safe to show to the author.
type apiError struct {
	Status  int
	Message string
}

func (e *apiError) Error() string { return fmt.Sprintf("status %d: %s", e.Status, e.Message) }

type mastodon struct {
	base   string
	client *http.Client // API calls
	upload *http.Client // media, with a longer timeout

	mu       sync.Mutex
	limits   limits
	limitsAt time.Time
}

type limits struct {
	MaxCharacters int
	MaxMedia      int
	Expirations   []int
}

type account struct {
	ID        string `json:"id"`
	Username  string `json:"username"`
	Acct      string `json:"acct"`
	Suspended bool   `json:"suspended"`
}

type mediaAttachment struct {
	ID          string `json:"id"`
	Type        string `json:"type"`
	PreviewURL  string `json:"preview_url"`
	Description string `json:"description"`
}

type scheduledParams struct {
	Text        string `json:"text"`
	SpoilerText string `json:"spoiler_text"`
	Visibility  string `json:"visibility"`
	Language    string `json:"language"`
	ContentType string `json:"content_type"`
	InReplyToID any    `json:"in_reply_to_id"`
	LocalOnly   any    `json:"local_only"`
	ExpiresIn   any    `json:"expires_in"`
	Sensitive   any    `json:"sensitive"`
	Poll        any    `json:"poll"`
	QuotedID    any    `json:"quoted_status_id"`
}

type scheduledStatus struct {
	ID          string            `json:"id"`
	ScheduledAt time.Time         `json:"scheduled_at"`
	Params      scheduledParams   `json:"params"`
	Media       []mediaAttachment `json:"media_attachments"`
}

type newStatus struct {
	Text        string
	SpoilerText string
	Visibility  string
	Language    string
	ContentType string
	InReplyToID string
	LocalOnly   bool
	ExpiresIn   int
	Sensitive   bool
	MediaIDs    []string
	ScheduledAt time.Time
}

func (m *mastodon) do(ctx context.Context, client *http.Client, token, method, path, kind string, headers map[string]string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, m.base+path, body) // #nosec G704 -- m.base is the configured instance; ids are path-escaped
	if err != nil {
		return nil, err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if kind != "" {
		req.Header.Set("Content-Type", kind)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return client.Do(req) // #nosec G704 -- requests only go to the configured instance; ids are path-escaped
}

// decode fills out from a response and turns error statuses into apiError.
func decode(resp *http.Response, out any) error {
	defer resp.Body.Close()
	body := io.LimitReader(resp.Body, responseLimit)
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		var e struct {
			Error string `json:"error"`
		}
		json.NewDecoder(body).Decode(&e)
		return &apiError{Status: resp.StatusCode, Message: e.Error}
	}
	if out == nil {
		io.Copy(io.Discard, body)
		return nil
	}
	return json.NewDecoder(body).Decode(out)
}

func (m *mastodon) verify(ctx context.Context, token string) (account, error) {
	var a account
	resp, err := m.do(ctx, m.client, token, http.MethodGet, "/api/v1/accounts/verify_credentials", "", nil, nil)
	if err != nil {
		return a, err
	}
	if err := decode(resp, &a); err != nil {
		return a, err
	}
	if a.ID == "" || a.Username == "" {
		return a, errors.New("empty account")
	}
	return a, nil
}

var handlePattern = regexp.MustCompile(`^[A-Za-z0-9_]{1,30}$`)

// localAccount finds an active account of this instance by username. It sends no
// token: lookup is public, and the sign-in scopes don't include read:accounts.
func (m *mastodon) localAccount(ctx context.Context, username string) (account, error) {
	var a account
	if !handlePattern.MatchString(username) {
		return a, &apiError{Status: http.StatusNotFound}
	}
	resp, err := m.do(ctx, m.client, "", http.MethodGet, "/api/v1/accounts/lookup?acct="+url.QueryEscape(username), "", nil, nil)
	if err != nil {
		return a, err
	}
	if err := decode(resp, &a); err != nil {
		return a, err
	}
	// acct has no domain only for local accounts.
	if !idPattern.MatchString(a.ID) || a.Suspended || a.Acct == "" || strings.Contains(a.Acct, "@") || !strings.EqualFold(a.Acct, username) {
		return a, &apiError{Status: http.StatusNotFound}
	}
	return a, nil
}

// instanceLimits reads the instance's own limits, cached for an hour.
func (m *mastodon) instanceLimits(ctx context.Context) limits {
	m.mu.Lock()
	defer m.mu.Unlock()
	if time.Since(m.limitsAt) < time.Hour {
		return m.limits
	}
	fallback := limits{MaxCharacters: 500, MaxMedia: 4}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	resp, err := m.do(ctx, m.client, "", http.MethodGet, "/api/v2/instance", "", nil, nil)
	if err != nil {
		if m.limits.MaxCharacters > 0 {
			return m.limits
		}
		return fallback
	}
	var out struct {
		Configuration struct {
			Statuses struct {
				MaxCharacters int   `json:"max_characters"`
				MaxMedia      int   `json:"max_media_attachments"`
				Expirations   []int `json:"expirations"`
			} `json:"statuses"`
		} `json:"configuration"`
	}
	if err := decode(resp, &out); err != nil || out.Configuration.Statuses.MaxCharacters == 0 {
		if m.limits.MaxCharacters > 0 {
			return m.limits
		}
		return fallback
	}
	c := out.Configuration.Statuses
	m.limits = limits{MaxCharacters: c.MaxCharacters, MaxMedia: c.MaxMedia, Expirations: c.Expirations}
	m.limitsAt = time.Now()
	return m.limits
}

var nextLink = regexp.MustCompile(`[?&]max_id=([0-9]+)[^>]*>;\s*rel="next"`)

// scheduled lists every scheduled post of the token's account. The instance holds
// at most 300, so this is a few pages at worst.
func (m *mastodon) scheduled(ctx context.Context, token string) ([]scheduledStatus, error) {
	var all []scheduledStatus
	maxID := ""
	for page := 0; page <= totalLimit/pageSize+1; page++ {
		path := "/api/v1/scheduled_statuses?limit=" + strconv.Itoa(pageSize)
		if maxID != "" {
			path += "&max_id=" + maxID
		}
		resp, err := m.do(ctx, m.client, token, http.MethodGet, path, "", nil, nil)
		if err != nil {
			return nil, err
		}
		link := resp.Header.Get("Link")
		var items []scheduledStatus
		if err := decode(resp, &items); err != nil {
			return nil, err
		}
		all = append(all, items...)
		next := nextLink.FindStringSubmatch(link)
		if next == nil || len(items) == 0 {
			break
		}
		maxID = next[1]
	}
	return all, nil
}

func (m *mastodon) scheduledByID(ctx context.Context, token, id string) (scheduledStatus, error) {
	var out scheduledStatus
	if !idPattern.MatchString(id) {
		return out, &apiError{Status: http.StatusNotFound}
	}
	resp, err := m.do(ctx, m.client, token, http.MethodGet, "/api/v1/scheduled_statuses/"+url.PathEscape(id), "", nil, nil)
	if err != nil {
		return out, err
	}
	return out, decode(resp, &out)
}

func (s newStatus) form() url.Values {
	f := url.Values{
		"status":       {s.Text},
		"visibility":   {s.Visibility},
		"scheduled_at": {s.ScheduledAt.UTC().Format(time.RFC3339)},
	}
	set := func(key, value string) {
		if value != "" {
			f.Set(key, value)
		}
	}
	set("spoiler_text", s.SpoilerText)
	set("language", s.Language)
	set("content_type", s.ContentType)
	set("in_reply_to_id", s.InReplyToID)
	if s.LocalOnly {
		f.Set("local_only", "true")
	}
	if s.Sensitive {
		f.Set("sensitive", "true")
	}
	if s.ExpiresIn > 0 {
		f.Set("expires_in", strconv.Itoa(s.ExpiresIn))
	}
	for _, id := range s.MediaIDs {
		f.Add("media_ids[]", id)
	}
	return f
}

// errPublishedNow means the instance published the post instead of scheduling it.
var errPublishedNow = errors.New("published_now")

// schedule creates a scheduled post. The idempotency key makes a resent form safe.
func (m *mastodon) schedule(ctx context.Context, token, key string, s newStatus) (string, error) {
	resp, err := m.do(ctx, m.client, token, http.MethodPost, "/api/v1/statuses", "application/x-www-form-urlencoded",
		map[string]string{"Idempotency-Key": key}, strings.NewReader(s.form().Encode()))
	if err != nil {
		return "", err
	}
	var out struct {
		ID          string     `json:"id"`
		ScheduledAt *time.Time `json:"scheduled_at"`
	}
	if err := decode(resp, &out); err != nil {
		return "", err
	}
	if out.ScheduledAt == nil {
		return out.ID, errPublishedNow
	}
	return out.ID, nil
}

func (m *mastodon) reschedule(ctx context.Context, token, id string, at time.Time) error {
	form := url.Values{"scheduled_at": {at.UTC().Format(time.RFC3339)}}
	resp, err := m.do(ctx, m.client, token, http.MethodPut, "/api/v1/scheduled_statuses/"+url.PathEscape(id),
		"application/x-www-form-urlencoded", nil, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	return decode(resp, nil)
}

// describeMedia changes the description of an attachment.
func (m *mastodon) describeMedia(ctx context.Context, token, id, alt string) error {
	form := url.Values{"description": {alt}}
	resp, err := m.do(ctx, m.client, token, http.MethodPut, "/api/v1/media/"+url.PathEscape(id),
		"application/x-www-form-urlencoded", nil, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	return decode(resp, nil)
}

func (m *mastodon) cancel(ctx context.Context, token, id string) error {
	resp, err := m.do(ctx, m.client, token, http.MethodDelete, "/api/v1/scheduled_statuses/"+url.PathEscape(id), "", nil, nil)
	if err != nil {
		return err
	}
	return decode(resp, nil)
}

// uploadMedia streams a file to the instance and waits until it is usable,
// because a post can't reference media still being processed.
func (m *mastodon) uploadMedia(ctx context.Context, token, name, kind, alt string, file io.Reader) (string, error) {
	reader, writer := io.Pipe()
	form := multipart.NewWriter(writer)
	go func() {
		err := writeMedia(form, name, kind, alt, file)
		writer.CloseWithError(err)
	}()
	resp, err := m.do(ctx, m.upload, token, http.MethodPost, "/api/v2/media", form.FormDataContentType(), nil, reader)
	if err != nil {
		reader.Close()
		return "", err
	}
	processing := resp.StatusCode == http.StatusAccepted
	var out mediaAttachment
	if err := decode(resp, &out); err != nil {
		return "", err
	}
	if out.ID == "" {
		return "", errors.New("empty media id")
	}
	if !processing {
		return out.ID, nil
	}
	deadline := time.Now().Add(mediaWait)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(2 * time.Second):
		}
		resp, err := m.do(ctx, m.client, token, http.MethodGet, "/api/v1/media/"+url.PathEscape(out.ID), "", nil, nil)
		if err != nil {
			return "", err
		}
		ready := resp.StatusCode == http.StatusOK
		if err := decodeProcessing(resp); err != nil {
			return "", err
		}
		if ready {
			return out.ID, nil
		}
	}
	return "", &apiError{Status: http.StatusGatewayTimeout, Message: "media still processing"}
}

func decodeProcessing(resp *http.Response) error {
	if resp.StatusCode == http.StatusPartialContent {
		resp.Body.Close()
		return nil
	}
	return decode(resp, nil)
}

func writeMedia(form *multipart.Writer, name, kind, alt string, file io.Reader) error {
	if alt != "" {
		if err := form.WriteField("description", alt); err != nil {
			return err
		}
	}
	part, err := form.CreatePart(mediaHeader(name, kind))
	if err != nil {
		return err
	}
	if _, err := io.Copy(part, file); err != nil {
		return err
	}
	return form.Close()
}

func mediaHeader(name, kind string) map[string][]string {
	safe := strings.Map(func(r rune) rune {
		if r == '"' || r == '\\' || r < 0x20 {
			return '_'
		}
		return r
	}, name)
	return map[string][]string{
		"Content-Disposition": {`form-data; name="file"; filename="` + safe + `"`},
		"Content-Type":        {kind},
	}
}
