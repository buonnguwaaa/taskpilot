package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/buonnguwaaa/taskpilot/internal/claudeai"
	"github.com/buonnguwaaa/taskpilot/internal/config"
	"github.com/buonnguwaaa/taskpilot/internal/db"
	"github.com/buonnguwaaa/taskpilot/internal/hasakiwork"
)

// authClaude stores the operator's claude.ai session cookie, used only to
// read artifacts linked from tickets. Chrome refuses remote debugging on the
// everyday profile and encrypts its cookies, so the cookie is pasted: copy
// sessionKey from DevTools → Application → Cookies → https://claude.ai.
func authClaude(ctx context.Context, d *db.DB) error {
	v, err := readSecret("sessionKey của claude.ai (DevTools → Application → Cookies → https://claude.ai): ")
	if err != nil {
		return err
	}
	v = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(v), "Cookie:"))
	// Accept a whole Cookie header too.
	for _, part := range strings.Split(v, ";") {
		if k, val, ok := strings.Cut(strings.TrimSpace(part), "="); ok && k == claudeai.SessionCookieName {
			v = val
		}
	}
	if v == "" {
		return errors.New("no cookie")
	}
	who, err := claudeai.CheckSession(ctx, nil, v)
	if err != nil {
		return err
	}
	if err := d.SetPlain(ctx, claudeai.SecretSession, v, "claude.ai session cookie (read artifacts linked from tickets)", nil, "operator"); err != nil {
		return err
	}
	fmt.Printf("đã lưu phiên claude.ai (%s)\n", who)
	return nil
}

// claudeStatus is a one-line description for status pages (never the cookie).
func claudeStatus(ctx context.Context, d *db.DB) string {
	m, err := d.Meta(ctx, claudeai.SecretSession)
	if err != nil {
		return "claude.ai (đọc prototype/artifact trong task): chưa có phiên — `taskpilot auth claude`"
	}
	return fmt.Sprintf("claude.ai (đọc prototype/artifact trong task): có phiên từ %s (`taskpilot claude whoami` để kiểm tra)",
		m.UpdatedAt.Local().Format("2006-01-02 15:04"))
}

// claudeFetcher returns an artifact fetcher with the stored session.
func claudeFetcher(ctx context.Context, cfg *config.Config, consumer string) (*claudeai.Fetcher, error) {
	hw := cfg.HasakiWork
	if hw.ChromeProfile == "" {
		return nil, errors.New("set hasaki_work.chrome_profile: claude.ai artifacts are opened in the dedicated Chrome profile")
	}
	s, _, err := storedSecret(ctx, cfg, claudeai.SecretSession, consumer, claudeai.ErrNoSession)
	if err != nil {
		return nil, err
	}
	return &claudeai.Fetcher{
		Chrome:  &hasakiwork.Chrome{Exe: hw.ChromeExe, ProfileDir: hw.ChromeProfile, Port: hw.DebugPort},
		Session: s,
	}, nil
}

const claudeUsage = `taskpilot claude <command>     (claude.ai artifacts linked from tickets, read-only)

  whoami                    check the stored claude.ai session
  artifact URL [-o DIR]     download an artifact (its files and a screenshot) into DIR (default .)
`

func cmdClaude(args []string) error {
	if len(args) == 0 {
		fmt.Print(claudeUsage)
		return nil
	}
	sub, args := args[0], args[1:]
	c := newFlags("claude " + sub)
	outDir := c.fs.String("o", ".", "artifact: directory to download into")
	pos := c.parse(args)
	cfg, err := config.Load(*c.config)
	if err != nil {
		return err
	}
	ctx := context.Background()
	f, err := claudeFetcher(ctx, cfg, "cli:claude")
	if err != nil {
		return err
	}
	switch sub {
	case "whoami":
		who, err := claudeai.CheckSession(ctx, nil, f.Session)
		if err != nil {
			return err
		}
		fmt.Println("claude.ai:", who)
		return nil
	case "artifact":
		if len(pos) != 1 {
			return errors.New("usage: taskpilot claude artifact URL [-o DIR]")
		}
		a, err := f.Fetch(ctx, pos[0], *outDir)
		if err != nil {
			return err
		}
		fmt.Print(a.Format())
		return nil
	}
	fmt.Fprint(os.Stderr, claudeUsage)
	return fmt.Errorf("unknown claude command %q", sub)
}
