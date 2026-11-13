// Package hasakiwork integrates Hasaki Work (work.hasaki.vn / wshr.hasaki.vn).
package hasakiwork

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/buonnguwaaa/taskpilot/internal/db"
)

// Secret names in the database.
const (
	SecretToken  = "hasaki-work/jwt"
	SecretCookie = "hasaki-work/session-cookie"
)

// DefaultTokenURL is the same-origin endpoint the web app calls to turn its
// SSO session cookie into a short-lived JWT.
const DefaultTokenURL = "https://work.hasaki.vn/api/auth/token"

// ErrAuthExpired means no valid token is available and none could be refreshed.
var ErrAuthExpired = errors.New("hasaki work token expired; run `taskpilot auth login` (or `taskpilot auth set-token`)")

// Auth hands out a valid JWT, refreshing it from the session cookie when it
// is close to expiry (option A) and reporting expiry otherwise (option C).
type Auth struct {
	DB            *db.DB
	HTTP          *http.Client
	TokenURL      string
	RefreshBefore time.Duration
	Now           func() time.Time
}

// NewAuth returns an Auth with defaults: refresh when less than 6h remain.
func NewAuth(d *db.DB) *Auth {
	return &Auth{
		DB:            d,
		HTTP:          &http.Client{Timeout: 20 * time.Second},
		TokenURL:      DefaultTokenURL,
		RefreshBefore: 6 * time.Hour,
		Now:           time.Now,
	}
}

// Claims are the JWT fields taskpilot reads. The signature is not verified:
// the token is only forwarded to the API, which verifies it.
type Claims struct {
	Exp int64  `json:"exp"`
	Iat int64  `json:"iat"`
	Sub any    `json:"sub"`
	Aud string `json:"aud"`
}

// ParseClaims decodes (without verifying) the claims of a JWT.
func ParseClaims(token string) (*Claims, error) {
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) != 3 {
		return nil, errors.New("not a JWT (expected 3 dot-separated parts)")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, fmt.Errorf("JWT payload: %w", err)
	}
	var c Claims
	if err := json.Unmarshal(payload, &c); err != nil {
		return nil, fmt.Errorf("JWT payload: %w", err)
	}
	if c.Exp == 0 {
		return nil, errors.New("JWT has no exp claim")
	}
	return &c, nil
}

// SetToken stores a JWT supplied by the operator (option C).
func (a *Auth) SetToken(ctx context.Context, token, actor string) (time.Time, error) {
	token = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(token), "Bearer "))
	c, err := ParseClaims(token)
	if err != nil {
		return time.Time{}, err
	}
	exp := time.Unix(c.Exp, 0).UTC()
	if !exp.After(a.Now()) {
		return exp, fmt.Errorf("token already expired at %s", exp.Format(time.RFC3339))
	}
	return exp, a.DB.SetPlain(ctx, SecretToken, token, "Hasaki Work API JWT", &exp, actor)
}

// SetCookie stores the work.hasaki.vn session cookie header (option A).
// expiresAt is the session cookie's own expiry, when known.
func (a *Auth) SetCookie(ctx context.Context, cookie string, expiresAt *time.Time, actor string) error {
	cookie = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(cookie), "Cookie:"))
	if !strings.Contains(cookie, "=") {
		return errors.New("expected a Cookie header value like `name=value; name2=value2`")
	}
	return a.DB.SetPlain(ctx, SecretCookie, strings.TrimSpace(cookie), "work.hasaki.vn session cookie (refreshes the JWT)", expiresAt, actor)
}

// Token returns a JWT valid for at least a while, refreshing it if possible.
func (a *Auth) Token(ctx context.Context) (string, error) {
	tok, meta, err := a.DB.Resolve(ctx, SecretToken, "source:hasaki-work")
	if err != nil && !errors.Is(err, db.ErrNoSecret) {
		return "", err
	}
	now := a.Now()
	valid := err == nil && meta.ExpiresAt != nil && meta.ExpiresAt.After(now)
	if valid && meta.ExpiresAt.Sub(now) > a.RefreshBefore {
		return tok, nil
	}
	fresh, rerr := a.Refresh(ctx)
	if rerr == nil {
		return fresh, nil
	}
	if valid {
		return tok, nil // refresh failed, but the current token still works
	}
	return "", fmt.Errorf("%w (refresh: %v)", ErrAuthExpired, rerr)
}

// Refresh exchanges the stored session cookie for a new JWT and stores it.
// Cookies the server rotates in the response are merged back.
func (a *Auth) Refresh(ctx context.Context) (string, error) {
	cookie, _, err := a.DB.Resolve(ctx, SecretCookie, "source:hasaki-work")
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.TokenURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Cookie", cookie)
	req.Header.Set("Accept", "application/json")
	resp, err := a.HTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("token endpoint returned HTTP %d (session cookie expired? log in again and re-import)", resp.StatusCode)
	}
	var out struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(body, &out); err != nil || out.Token == "" {
		return "", fmt.Errorf("token endpoint returned no token")
	}
	if _, err := a.SetToken(ctx, out.Token, "daemon"); err != nil {
		return "", err
	}
	if rotated := resp.Cookies(); len(rotated) > 0 {
		var exp *time.Time
		for _, c := range rotated {
			if c.Name == SessionCookieName && !c.Expires.IsZero() {
				e := c.Expires.UTC()
				exp = &e
			}
		}
		if exp == nil {
			if m, err := a.DB.Meta(ctx, SecretCookie); err == nil {
				exp = m.ExpiresAt
			}
		}
		if err := a.DB.SetPlain(ctx, SecretCookie, MergeCookies(cookie, rotated), "", exp, "daemon"); err != nil {
			return "", err
		}
	}
	return out.Token, nil
}

// MergeCookies applies Set-Cookie updates to a Cookie header value.
func MergeCookies(header string, updates []*http.Cookie) string {
	jar := map[string]string{}
	var order []string
	for _, part := range strings.Split(header, ";") {
		name, value, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok || name == "" {
			continue
		}
		if _, seen := jar[name]; !seen {
			order = append(order, name)
		}
		jar[name] = value
	}
	for _, c := range updates {
		if c.MaxAge < 0 || (!c.Expires.IsZero() && c.Expires.Before(time.Now())) {
			delete(jar, c.Name)
			continue
		}
		if _, seen := jar[c.Name]; !seen {
			order = append(order, c.Name)
		}
		jar[c.Name] = c.Value
	}
	var parts []string
	for _, n := range order {
		if v, ok := jar[n]; ok {
			parts = append(parts, n+"="+v)
		}
	}
	return strings.Join(parts, "; ")
}

// Status summarizes the stored credentials without revealing them.
type Status struct {
	TokenExpiresAt *time.Time
	HasToken       bool
	HasCookie      bool
	CookieExpires  *time.Time
}

// Remaining is how long the token stays valid (negative when expired).
func (s Status) Remaining(now time.Time) time.Duration {
	if s.TokenExpiresAt == nil {
		return -1
	}
	return s.TokenExpiresAt.Sub(now)
}

// Summary is a one-line, operator-facing description (Vietnamese).
func (s Status) Summary(now time.Time) string {
	refresh := "không có cookie để tự gia hạn"
	if s.HasCookie {
		refresh = "tự gia hạn bằng cookie"
		if s.CookieExpires != nil {
			if left := s.CookieExpires.Sub(now); left > 0 {
				refresh += fmt.Sprintf(", phiên đăng nhập còn %s", round(left))
			} else {
				refresh = "phiên đăng nhập ĐÃ HẾT HẠN — chạy `taskpilot auth login`"
			}
		}
	}
	switch {
	case !s.HasToken && s.HasCookie:
		return "Hasaki Work: chưa có token, sẽ lấy bằng cookie"
	case !s.HasToken:
		return "Hasaki Work: chưa có token — chạy `taskpilot auth login` hoặc `auth set-token`"
	case s.Remaining(now) <= 0:
		return "Hasaki Work: token ĐÃ HẾT HẠN (" + refresh + ")"
	default:
		return fmt.Sprintf("Hasaki Work: token còn %s (%s)", round(s.Remaining(now)), refresh)
	}
}

func round(d time.Duration) string {
	if d >= time.Hour {
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
	return fmt.Sprintf("%dm", int(d.Minutes()))
}

// ReadStatus loads the credential status from the database.
func ReadStatus(ctx context.Context, d *db.DB) (Status, error) {
	var s Status
	if m, err := d.Meta(ctx, SecretToken); err == nil {
		s.HasToken, s.TokenExpiresAt = true, m.ExpiresAt
	} else if !errors.Is(err, db.ErrNoSecret) {
		return s, err
	}
	if m, err := d.Meta(ctx, SecretCookie); err == nil {
		s.HasCookie, s.CookieExpires = true, m.ExpiresAt
	} else if !errors.Is(err, db.ErrNoSecret) {
		return s, err
	}
	return s, nil
}

// CookieNames lists cookie names in a header value (for display; never values).
func CookieNames(header string) []string {
	var names []string
	for _, part := range strings.Split(header, ";") {
		if n, _, ok := strings.Cut(strings.TrimSpace(part), "="); ok && n != "" {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	return names
}
