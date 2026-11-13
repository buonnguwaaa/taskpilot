package main

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/buonnguwaaa/taskpilot/internal/config"
	"github.com/buonnguwaaa/taskpilot/internal/db"
	"github.com/buonnguwaaa/taskpilot/internal/hasakiwork"
	"github.com/buonnguwaaa/taskpilot/internal/vault"
)

// vaultTokenJS finds the token the Vault UI keeps in local/session storage
// after a login (keys like "vault-oidc☃1" holding {"token": "hvs.…"}).
const vaultTokenJS = `(() => {
  for (const st of [window.localStorage, window.sessionStorage]) {
    for (let i = 0; i < st.length; i++) {
      const v = st.getItem(st.key(i));
      if (!v || v.indexOf('"token"') < 0) continue;
      try { const o = JSON.parse(v); if (o && typeof o.token === 'string' && o.token.length > 10) return o.token; } catch (e) {}
    }
  }
  return "";
})()`

// vaultDropTokenJS removes the Vault UI's stored login that holds the given
// token and reloads, so the UI shows the login form instead of a dead session.
const vaultDropTokenJS = `((bad) => {
  for (const st of [window.localStorage, window.sessionStorage]) {
    for (let i = st.length - 1; i >= 0; i--) {
      const k = st.key(i), v = st.getItem(k);
      if (v && v.indexOf(bad) >= 0) st.removeItem(k);
    }
  }
  setTimeout(() => location.reload(), 100);
  return "ok";
})(%q)`

// waitVaultToken polls the Vault page for a token that Vault accepts. A token
// left in the profile by an earlier login (expired or revoked) is dropped from
// the page and the wait goes on until the operator logs in again.
func waitVaultToken(ctx context.Context, ch *hasakiwork.Chrome, baseURL, host string) (string, error) {
	rejected := map[string]bool{}
	for {
		tok, err := ch.WaitForEval(ctx, host, vaultTokenJS)
		if err != nil {
			return "", err
		}
		if !rejected[tok] {
			_, lerr := (&vault.Client{BaseURL: baseURL, Token: tok}).LookupSelf(ctx)
			if lerr == nil {
				return tok, nil
			}
			if !errors.Is(lerr, vault.ErrUnauthorized) {
				return "", fmt.Errorf("check against %s: %w", baseURL, lerr)
			}
			rejected[tok] = true
			fmt.Println("Token Vault cũ trong Chrome đã hết hạn — đã xoá, hãy đăng nhập OIDC lại trên cửa sổ đó…")
			_, _ = ch.EvalInPage(ctx, host, fmt.Sprintf(vaultDropTokenJS, tok))
		}
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("hết thời gian chờ đăng nhập trên %s", host)
		case <-time.After(2 * time.Second):
		}
	}
}

// vaultClient returns a client with the operator's stored Vault token.
func vaultClient(ctx context.Context, cfg *config.Config, consumer string) (*vault.Client, error) {
	if cfg.Vault.URL == "" {
		return nil, errors.New("vault.url is not set in the taskpilot config")
	}
	tok, meta, err := storedSecret(ctx, cfg, vault.SecretToken, consumer, vault.ErrNoCredentials)
	if err != nil {
		return nil, err
	}
	if meta.ExpiresAt != nil && meta.ExpiresAt.Before(time.Now()) {
		return nil, fmt.Errorf("token Vault %s đã hết hạn lúc %s: `taskpilot auth vault`", cfg.Vault.Env, meta.ExpiresAt.Local().Format("2006-01-02 15:04"))
	}
	return &vault.Client{BaseURL: cfg.Vault.URL, Token: tok}, nil
}

// authVault logs in to the QC Vault with SSO in the dedicated Chrome profile
// (or a pasted token) and stores the token.
func authVault(ctx context.Context, cfg *config.Config, d *db.DB, paste bool, timeout time.Duration) error {
	if cfg.Vault.URL == "" {
		return errors.New("set vault.url in the taskpilot config first (e.g. https://vault-qc-v2.hasaki.dev)")
	}
	var tok string
	if paste {
		v, err := readSecret("Vault token (UI: menu tài khoản → Copy token): ")
		if err != nil {
			return err
		}
		tok = strings.TrimSpace(v)
	} else {
		hw := cfg.HasakiWork
		if hw.ChromeProfile == "" {
			return errors.New("set hasaki_work.chrome_profile (the dedicated Chrome profile) or use -token")
		}
		u, err := url.Parse(cfg.Vault.URL)
		if err != nil {
			return err
		}
		ch := &hasakiwork.Chrome{Exe: hw.ChromeExe, ProfileDir: hw.ChromeProfile, Port: hw.DebugPort}
		sctx, stop := signal.NotifyContext(ctx, os.Interrupt)
		defer stop()
		if err := ch.Open(sctx, cfg.Vault.URL+cfg.Vault.LoginPath); err != nil {
			return err
		}
		defer closeChrome(ch)
		fmt.Println("Chrome (profile riêng của taskpilot) đã mở Vault " + cfg.Vault.Env + ". Chọn OIDC và đăng nhập SSO trên cửa sổ đó…")
		wctx, cancel := context.WithTimeout(sctx, timeout)
		defer cancel()
		tok, err = waitVaultToken(wctx, ch, cfg.Vault.URL, u.Hostname())
		if err != nil {
			return err
		}
	}
	if tok == "" {
		return errors.New("no token")
	}
	info, err := (&vault.Client{BaseURL: cfg.Vault.URL, Token: tok}).LookupSelf(ctx)
	if err != nil {
		return fmt.Errorf("check against %s: %w", cfg.Vault.URL, err)
	}
	if err := d.SetPlain(ctx, vault.SecretToken, tok, "Vault "+cfg.Vault.Env+" token (read-only use)", info.Expires, "operator"); err != nil {
		return err
	}
	when := "không có hạn"
	if info.Expires != nil {
		when = "hết hạn " + info.Expires.Local().Format("2006-01-02 15:04")
	}
	fmt.Printf("đã lưu token Vault %s (%s, policies %s, %s)\n", cfg.Vault.Env, info.DisplayName, strings.Join(info.Policies, ","), when)
	return nil
}

// vaultStatus is a one-line description for status pages (never the token).
func vaultStatus(ctx context.Context, cfg *config.Config, d *db.DB) string {
	if cfg.Vault.URL == "" {
		return "Vault: chưa cấu hình (vault.url)"
	}
	m, err := d.Meta(ctx, vault.SecretToken)
	if err != nil {
		return "Vault " + cfg.Vault.Env + ": chưa đăng nhập — phím v (tab Status) hoặc `taskpilot auth vault`"
	}
	switch {
	case m.ExpiresAt == nil:
		return "Vault " + cfg.Vault.Env + ": có token, không có hạn"
	case m.ExpiresAt.Before(time.Now()):
		return "Vault " + cfg.Vault.Env + ": token ĐÃ HẾT HẠN — phím v để đăng nhập lại"
	default:
		return fmt.Sprintf("Vault %s: token còn %s", cfg.Vault.Env, time.Until(*m.ExpiresAt).Round(time.Minute))
	}
}

const vaultUsage = `taskpilot vault <command>     (QC Vault, read-only)

  whoami          the stored token: user, policies, expiry
  ls [PATH]       list the secret engines, or the keys under PATH (kv/payment/)
  get PATH        print a secret's keys and values
`

func cmdVault(args []string) error {
	if len(args) == 0 {
		fmt.Print(vaultUsage)
		return nil
	}
	sub, args := args[0], args[1:]
	c := newFlags("vault " + sub)
	pos := c.parse(args)
	cfg, err := config.Load(*c.config)
	if err != nil {
		return err
	}
	ctx := context.Background()
	cl, err := vaultClient(ctx, cfg, "cli:vault")
	if err != nil {
		return err
	}
	switch sub {
	case "whoami":
		info, err := cl.LookupSelf(ctx)
		if err != nil {
			return err
		}
		exp := "không có hạn"
		if info.Expires != nil {
			exp = info.Expires.Local().Format("2006-01-02 15:04")
		}
		fmt.Printf("%s · policies %s · hết hạn %s\n", info.DisplayName, strings.Join(info.Policies, ","), exp)
		return nil
	case "ls":
		p := ""
		if len(pos) > 0 {
			p = pos[0]
		}
		keys, err := cl.List(ctx, p)
		if err != nil {
			return err
		}
		for _, k := range keys {
			fmt.Println(strings.TrimSuffix(p, "/") + map[bool]string{true: "/", false: ""}[p != ""] + k)
		}
		return nil
	case "get":
		if len(pos) != 1 {
			return errors.New("usage: taskpilot vault get PATH")
		}
		kv, err := cl.Read(ctx, pos[0])
		if err != nil {
			return err
		}
		fmt.Print(vault.Format(kv))
		return nil
	}
	fmt.Print(vaultUsage)
	return fmt.Errorf("unknown vault command %q", sub)
}
