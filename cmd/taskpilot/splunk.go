package main

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"time"

	"github.com/buonnguwaaa/taskpilot/internal/config"
	"github.com/buonnguwaaa/taskpilot/internal/db"
	"github.com/buonnguwaaa/taskpilot/internal/hasakiwork"
	"github.com/buonnguwaaa/taskpilot/internal/splunk"
)

// splunkClient builds a client from stored credentials (token preferred).
func splunkClient(ctx context.Context, cfg *config.Config, consumer string) (*splunk.Client, error) {
	if cfg.Splunk.URL == "" {
		return nil, errors.New("splunk.url is not set in the taskpilot config")
	}
	d, err := db.Open(ctx, cfg.Home)
	if err != nil {
		return nil, err
	}
	defer d.Close()
	c := &splunk.Client{BaseURL: cfg.Splunk.URL, Locale: cfg.Splunk.Locale}
	if tok, meta, err := d.Resolve(ctx, splunk.SecretToken, consumer); err == nil {
		c.BaseURL = cfg.Splunk.APIURL
		if meta.ExpiresAt != nil && meta.ExpiresAt.Before(time.Now()) {
			return nil, fmt.Errorf("the Splunk token expired on %s; create a new one and run `taskpilot auth splunk -token`", meta.ExpiresAt.Local().Format("2006-01-02 15:04"))
		}
		c.Token = tok
		return c, nil
	} else if !errors.Is(err, db.ErrNoSecret) {
		return nil, err
	}
	if ck, meta, err := d.Resolve(ctx, splunk.SecretCookie, consumer); err == nil {
		if meta.ExpiresAt != nil && meta.ExpiresAt.Before(time.Now()) {
			return nil, fmt.Errorf("the Splunk web session expired on %s; run `taskpilot auth splunk`", meta.ExpiresAt.Local().Format("2006-01-02 15:04"))
		}
		c.Cookie = ck
		return c, nil
	} else if !errors.Is(err, db.ErrNoSecret) {
		return nil, err
	}
	return nil, splunk.ErrNoCredentials
}

var splunkSessionCookie = regexp.MustCompile(`^splunkd_\d+$`)

// authSplunk stores Splunk credentials: an API token (-token), a pasted Cookie
// header (-cookie), or (default) a session from logging in through the
// dedicated Chrome profile.
func authSplunk(ctx context.Context, cfg *config.Config, d *db.DB, useToken, useCookie, create bool, tokenTTL string, timeout time.Duration) error {
	if cfg.Splunk.URL == "" {
		return errors.New("set splunk.url in the taskpilot config first")
	}
	c := &splunk.Client{BaseURL: cfg.Splunk.URL, Locale: cfg.Splunk.Locale}
	var secret, value string
	var exp *time.Time
	switch {
	case useToken:
		fmt.Fprintln(os.Stderr, "Tạo token trong Splunk Web: Settings → Tokens → New Token (cần quyền; nếu không có, dùng đăng nhập bằng Chrome).")
		tok, err := readSecret("Splunk token: ")
		if err != nil {
			return err
		}
		c.Token, secret, value = tok, splunk.SecretToken, tok
		switch e, ok := splunk.TokenExpiry(tok); {
		case !ok:
			fmt.Fprintln(os.Stderr, "lưu ý: token không phải JWT nên không đọc được hạn")
		case e == nil:
			fmt.Fprintln(os.Stderr, "token này không có hạn (Expiration để trống khi tạo)")
		case e.Before(time.Now()):
			return fmt.Errorf("token đã hết hạn lúc %s", e.Local().Format("2006-01-02 15:04"))
		default:
			exp = e
		}
	case useCookie:
		ck, err := readSecret("Cookie header của Splunk Web (từ DevTools): ")
		if err != nil {
			return err
		}
		ck = strings.TrimSpace(strings.TrimPrefix(ck, "Cookie:"))
		c.Cookie, secret, value = ck, splunk.SecretCookie, ck
	default:
		hw := cfg.HasakiWork
		if hw.ChromeProfile == "" {
			return errors.New("set hasaki_work.chrome_profile (the dedicated Chrome profile) or use -token / -cookie")
		}
		u, err := url.Parse(cfg.Splunk.URL)
		if err != nil {
			return err
		}
		ch := &hasakiwork.Chrome{Exe: hw.ChromeExe, ProfileDir: hw.ChromeProfile, Port: hw.DebugPort}
		sctx, stop := signal.NotifyContext(ctx, os.Interrupt)
		defer stop()
		if err := ch.Open(sctx, cfg.Splunk.URL+cfg.Splunk.LoginPath); err != nil {
			return err
		}
		fmt.Println("Chrome (profile riêng của taskpilot) đã mở Splunk. Đăng nhập trên cửa sổ đó…")
		wctx, cancel := context.WithTimeout(sctx, timeout)
		defer cancel()
		cookies, err := ch.WaitForCookies(wctx, u.Hostname(), func(cs []hasakiwork.CDPCookie) bool {
			for _, ck := range cs {
				if splunkSessionCookie.MatchString(ck.Name) && ck.Value != "" {
					return true
				}
			}
			return false
		})
		if err != nil {
			return err
		}
		c.Cookie, secret, value = hasakiwork.CookieHeader(cookies), splunk.SecretCookie, hasakiwork.CookieHeader(cookies)
		for _, ck := range cookies {
			if splunkSessionCookie.MatchString(ck.Name) && ck.Expires > 0 {
				t := time.Unix(int64(ck.Expires), 0).UTC()
				exp = &t
			}
		}
		defer func() {
			if err := ch.Close(context.Background()); err != nil {
				fmt.Fprintf(os.Stderr, "warning: could not close Chrome (%v); close it yourself so the debug port goes away\n", err)
			}
		}()
	}
	if value == "" {
		return errors.New("nothing entered")
	}
	user, err := c.WhoAmI(ctx)
	if err != nil {
		return fmt.Errorf("check against %s: %w", cfg.Splunk.URL, err)
	}
	if create && c.Cookie != "" {
		tok, exp2, err := createSplunkToken(ctx, cfg, c, user, tokenTTL)
		if err != nil {
			fmt.Fprintf(os.Stderr, "không tạo được token dùng được (%v);\ngiữ phiên đăng nhập web làm phương án dự phòng.\n", err)
		} else {
			c.Cookie, c.Token = "", tok
			secret, value, exp = splunk.SecretToken, tok, exp2
		}
	}
	if err := d.SetPlain(ctx, secret, value, "Splunk credentials for read-only searches", exp, "operator"); err != nil {
		return err
	}
	// Keep a single active credential kind so the token never silently shadows a fresh session or vice versa.
	other := map[string]string{splunk.SecretToken: splunk.SecretCookie, splunk.SecretCookie: splunk.SecretToken}[secret]
	if _, err := d.Meta(ctx, other); err == nil {
		_ = d.DeleteSecret(ctx, other, "operator")
	}
	fmt.Printf("đã lưu thông tin Splunk (đăng nhập là %s)\n", user)
	return nil
}

// createSplunkToken creates a token with the logged-in web session, checks it
// works against the REST API (api_url, then the management port 8089), and
// deletes it again if it does not, so no unusable credential is left behind.
func createSplunkToken(ctx context.Context, cfg *config.Config, web *splunk.Client, user, ttl string) (string, *time.Time, error) {
	if ttl == "never" {
		ttl = ""
	}
	tok, id, err := web.CreateToken(ctx, user, "taskpilot", ttl)
	if err != nil {
		return "", nil, fmt.Errorf("create token: %w", err)
	}
	candidates := []string{cfg.Splunk.APIURL}
	if u, err := url.Parse(cfg.Splunk.URL); err == nil && u.Port() == "" && cfg.Splunk.APIURL == cfg.Splunk.URL {
		candidates = append(candidates, u.Scheme+"://"+u.Hostname()+":8089")
	}
	var lastErr error
	for _, base := range candidates {
		vctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		_, err := (&splunk.Client{BaseURL: base, Token: tok}).WhoAmI(vctx)
		cancel()
		if err == nil {
			if base != cfg.Splunk.APIURL {
				fmt.Printf("REST API của Splunk nằm ở %s: thêm `api_url: %s` vào mục splunk trong %s\n", base, base, cfg.Path)
			}
			exp, _ := splunk.TokenExpiry(tok)
			fmt.Printf("đã tạo token Splunk cho %s (audience taskpilot, id %s)\n", user, id)
			return tok, exp, nil
		}
		lastErr = fmt.Errorf("%s: %w", base, err)
	}
	if derr := web.DeleteToken(ctx, user, id); derr != nil {
		return "", nil, fmt.Errorf("token created but not usable (%v), and deleting it failed (%v): delete it in Splunk Web → Settings → Tokens (id %s)", lastErr, derr, id)
	}
	return "", nil, fmt.Errorf("token created but the REST API is not reachable with it (%v); the token was deleted again", lastErr)
}

func cmdSplunk(args []string) error {
	if len(args) == 0 || args[0] != "search" {
		fmt.Print("taskpilot splunk search \"<SPL>\" [-earliest -1h] [-latest now] [-max 20] [-project P]\n")
		return nil
	}
	c := newFlags("splunk search")
	earliest := c.fs.String("earliest", "", "start time (default from config, e.g. -1h)")
	latest := c.fs.String("latest", "", "end time (default now)")
	max := c.fs.Int("max", 20, "events to show")
	project := c.fs.String("project", "", "apply this project's splunk_filter")
	query := ""
	rest := args[1:]
	if len(rest) > 0 && !strings.HasPrefix(rest[0], "-") {
		query, rest = rest[0], rest[1:]
	}
	c.fs.Parse(rest)
	if query == "" {
		query = strings.Join(c.fs.Args(), " ")
	}
	if query == "" {
		return errors.New("usage: taskpilot splunk search \"<SPL>\"")
	}
	cfg, err := config.Load(*c.config)
	if err != nil {
		return err
	}
	if err := splunk.Validate(query); err != nil {
		return err
	}
	if *project != "" {
		p, ok := cfg.Projects[*project]
		if !ok {
			return fmt.Errorf("unknown project %q", *project)
		}
		query = splunk.Scope(p.SplunkFilter, query)
	}
	e := *earliest
	if e == "" {
		e = cfg.Splunk.DefaultEarliest
	}
	ctx, cancel := context.WithTimeout(context.Background(), cfg.Splunk.Timeout)
	defer cancel()
	client, err := splunkClient(ctx, cfg, "operator:cli")
	if err != nil {
		return err
	}
	events, err := client.Search(ctx, splunk.Request{SPL: query, Earliest: e, Latest: *latest, MaxEvents: *max})
	if err != nil {
		return err
	}
	fmt.Println(splunk.Format(query, e, events, *max))
	return nil
}
