package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/buonnguwaaa/taskpilot/internal/config"
	"github.com/buonnguwaaa/taskpilot/internal/db"
	"github.com/buonnguwaaa/taskpilot/internal/jenkins"
)

// jenkinsClient builds a read-only client from the stored user and API token.
func jenkinsClient(ctx context.Context, cfg *config.Config, consumer string) (*jenkins.Client, error) {
	if cfg.Jenkins.URL == "" {
		return nil, errors.New("jenkins.url is not set in the taskpilot config")
	}
	user, _, err := storedSecret(ctx, cfg, jenkins.SecretUser, consumer, jenkins.ErrNoCredentials)
	if err != nil {
		return nil, err
	}
	tok, meta, err := storedSecret(ctx, cfg, jenkins.SecretToken, consumer, jenkins.ErrNoCredentials)
	if err != nil {
		return nil, err
	}
	if meta.ExpiresAt != nil && meta.ExpiresAt.Before(time.Now()) {
		return nil, fmt.Errorf("the Jenkins API token expired on %s; run `taskpilot auth jenkins`", meta.ExpiresAt.Format("2006-01-02"))
	}
	return jenkins.New(cfg.Jenkins.URL, user, tok), nil
}

// authJenkins stores the Jenkins user and API token after checking them.
func authJenkins(ctx context.Context, cfg *config.Config, d *db.DB) error {
	if cfg.Jenkins.URL == "" {
		return errors.New("set jenkins.url in the taskpilot config first (e.g. jenkins: {url: https://jenkins.hasaki.vn})")
	}
	fmt.Printf("Tạo API token: %s/me/configure → API Token → Add new Token (token chỉ dùng để đọc job, build, log).\n", cfg.Jenkins.URL)
	user, err := readLine("User Jenkins (ID đăng nhập, xem ở góc phải trên): ")
	if err != nil {
		return err
	}
	token, err := readSecret("API token: ")
	if err != nil {
		return err
	}
	if user == "" || token == "" {
		return errors.New("user and token are required")
	}
	name, err := jenkins.New(cfg.Jenkins.URL, user, token).WhoAmI(ctx)
	if err != nil {
		return fmt.Errorf("check against %s: %w", cfg.Jenkins.URL, err)
	}
	if err := d.SetPlain(ctx, jenkins.SecretUser, user, "Jenkins user (read-only access for agents)", nil, "operator"); err != nil {
		return err
	}
	if err := d.SetPlain(ctx, jenkins.SecretToken, token, "Jenkins API token", nil, "operator"); err != nil {
		return err
	}
	fmt.Printf("đã lưu thông tin Jenkins (đăng nhập là %s: OK)\n", name)
	return nil
}

func jenkinsStatus(ctx context.Context, cfg *config.Config, d *db.DB) string {
	if cfg.Jenkins.URL == "" {
		return "Jenkins: chưa cấu hình (jenkins.url)"
	}
	if _, err := d.Meta(ctx, jenkins.SecretToken); err != nil {
		return "Jenkins: chưa có API token — `taskpilot auth jenkins`"
	}
	return "Jenkins: có API token (" + cfg.Jenkins.URL + ")"
}
