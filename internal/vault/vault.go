// Package vault reads configuration from the QC Vault (HashiCorp Vault /
// OpenBao HTTP API): list keys and read KV secrets, v1 or v2. It only reads:
// there is no write, delete or token creation here.
package vault

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"sort"
	"strings"
	"time"
)

// SecretToken is where the operator's Vault token is stored.
const SecretToken = "vault/token"

var (
	ErrNoCredentials = errors.New("chưa đăng nhập Vault QC: `taskpilot auth vault` (tab Status, phím v)")
	ErrUnauthorized  = errors.New("Vault từ chối token (hết hạn hoặc bị thu hồi): đăng nhập lại bằng `taskpilot auth vault`")
	ErrForbidden     = errors.New("Vault: token không có quyền đọc đường dẫn này")
	ErrNotFound      = errors.New("Vault: không có đường dẫn này")
)

// Client talks to one Vault.
type Client struct {
	BaseURL string
	Token   string
	HTTP    *http.Client

	mounts map[string]Mount // cached
}

// Mount is a secrets engine mount.
type Mount struct {
	Path    string // "kv/" (with the trailing slash)
	Type    string // kv, …
	Version string // "2" for KV v2
}

func (c *Client) http() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 30 * time.Second}
}

func (c *Client) do(ctx context.Context, method, p string, out any) error {
	if c.Token == "" {
		return ErrNoCredentials
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.BaseURL, "/")+"/v1/"+strings.TrimLeft(p, "/"), nil)
	if err != nil {
		return err
	}
	req.Header.Set("X-Vault-Token", c.Token)
	req.Header.Set("X-Vault-Request", "true")
	resp, err := c.http().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNoContent:
		return nil
	case http.StatusUnauthorized:
		return ErrUnauthorized
	case http.StatusForbidden:
		var e struct {
			Errors []string `json:"errors"`
		}
		_ = json.Unmarshal(body, &e)
		if len(e.Errors) > 0 && strings.Contains(strings.Join(e.Errors, " "), "invalid token") {
			return ErrUnauthorized
		}
		return ErrForbidden
	case http.StatusNotFound:
		return ErrNotFound
	default:
		return fmt.Errorf("vault %s %s: HTTP %d: %s", method, p, resp.StatusCode, strings.TrimSpace(string(body[:min(len(body), 300)])))
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(body, out)
}

// TokenInfo describes the stored token.
type TokenInfo struct {
	DisplayName string
	Policies    []string
	Expires     *time.Time
}

// LookupSelf checks the token and tells who it is and until when.
func (c *Client) LookupSelf(ctx context.Context) (*TokenInfo, error) {
	var out struct {
		Data struct {
			DisplayName string   `json:"display_name"`
			Policies    []string `json:"policies"`
			ExpireTime  string   `json:"expire_time"`
		} `json:"data"`
	}
	if err := c.do(ctx, http.MethodGet, "auth/token/lookup-self", &out); err != nil {
		return nil, err
	}
	ti := &TokenInfo{DisplayName: out.Data.DisplayName, Policies: out.Data.Policies}
	if t, err := time.Parse(time.RFC3339Nano, out.Data.ExpireTime); err == nil {
		ti.Expires = &t
	}
	return ti, nil
}

// Mounts lists the secret engines the token can see.
func (c *Client) Mounts(ctx context.Context) (map[string]Mount, error) {
	if c.mounts != nil {
		return c.mounts, nil
	}
	var out struct {
		Data struct {
			Secret map[string]struct {
				Type    string            `json:"type"`
				Options map[string]string `json:"options"`
			} `json:"secret"`
		} `json:"data"`
	}
	if err := c.do(ctx, http.MethodGet, "sys/internal/ui/mounts", &out); err != nil {
		return nil, err
	}
	c.mounts = map[string]Mount{}
	for p, m := range out.Data.Secret {
		if m.Type == "cubbyhole" || m.Type == "system" || m.Type == "identity" {
			continue
		}
		c.mounts[p] = Mount{Path: p, Type: m.Type, Version: m.Options["version"]}
	}
	return c.mounts, nil
}

// Clean normalizes a logical path: no leading slash, no "..", no data/ or
// metadata/ segment (KV v2 paths are given as kv/app/config).
func Clean(p string) (string, error) {
	p = strings.TrimSpace(p)
	if strings.Contains(p, "..") {
		return "", errors.New("đường dẫn không được chứa ..")
	}
	trailing := strings.HasSuffix(p, "/")
	p = strings.Trim(path.Clean("/"+p), "/")
	if p == "." {
		p = ""
	}
	if trailing && p != "" {
		p += "/"
	}
	return p, nil
}

// split finds the mount of a logical path.
func (c *Client) split(ctx context.Context, p string) (Mount, string, error) {
	ms, err := c.Mounts(ctx)
	if err != nil {
		return Mount{}, "", err
	}
	best := ""
	for mp := range ms {
		if strings.HasPrefix(p+"/", mp) && len(mp) > len(best) {
			best = mp
		}
	}
	if best == "" {
		return Mount{}, "", fmt.Errorf("không có secret engine nào chứa %q (xem `taskpilot vault ls`)", p)
	}
	return ms[best], strings.TrimPrefix(strings.TrimPrefix(p, strings.TrimSuffix(best, "/")), "/"), nil
}

// List returns the keys under a path ("" lists the mounts). Folders end in /.
func (c *Client) List(ctx context.Context, p string) ([]string, error) {
	p, err := Clean(p)
	if err != nil {
		return nil, err
	}
	if p == "" {
		ms, err := c.Mounts(ctx)
		if err != nil {
			return nil, err
		}
		var out []string
		for mp := range ms {
			out = append(out, mp)
		}
		sort.Strings(out)
		return out, nil
	}
	m, sub, err := c.split(ctx, strings.TrimSuffix(p, "/"))
	if err != nil {
		return nil, err
	}
	api := strings.TrimSuffix(m.Path, "/") + "/" + sub
	if m.Version == "2" {
		api = strings.TrimSuffix(m.Path, "/") + "/metadata/" + sub
	}
	var out struct {
		Data struct {
			Keys []string `json:"keys"`
		} `json:"data"`
	}
	if err := c.do(ctx, "LIST", strings.TrimSuffix(api, "/")+"/", &out); err != nil {
		return nil, err
	}
	sort.Strings(out.Data.Keys)
	return out.Data.Keys, nil
}

// Read returns the key/values of a secret.
func (c *Client) Read(ctx context.Context, p string) (map[string]any, error) {
	p, err := Clean(p)
	if err != nil {
		return nil, err
	}
	if p == "" || strings.HasSuffix(p, "/") {
		return nil, errors.New("đây là thư mục: dùng list")
	}
	m, sub, err := c.split(ctx, p)
	if err != nil {
		return nil, err
	}
	if m.Version == "2" {
		var out struct {
			Data struct {
				Data map[string]any `json:"data"`
			} `json:"data"`
		}
		if err := c.do(ctx, http.MethodGet, strings.TrimSuffix(m.Path, "/")+"/data/"+sub, &out); err != nil {
			return nil, err
		}
		return out.Data.Data, nil
	}
	var out struct {
		Data map[string]any `json:"data"`
	}
	if err := c.do(ctx, http.MethodGet, strings.TrimSuffix(m.Path, "/")+"/"+sub, &out); err != nil {
		return nil, err
	}
	return out.Data, nil
}

// Allowed reports whether p lies under one of the prefixes (a project's
// vault_paths). Prefixes are logical paths ("kv/payment/").
func Allowed(p string, prefixes []string) bool {
	p, err := Clean(p)
	if err != nil {
		return false
	}
	for _, pre := range prefixes {
		pre, _ = Clean(pre)
		pre = strings.TrimSuffix(pre, "/")
		if pre != "" && (p == pre || strings.HasPrefix(p, pre+"/")) {
			return true
		}
	}
	return false
}

// Format renders key/values sorted by key, one per line.
func Format(kv map[string]any) string {
	keys := make([]string, 0, len(kv))
	for k := range kv {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		v := kv[k]
		s, ok := v.(string)
		if !ok {
			j, _ := json.Marshal(v)
			s = string(j)
		}
		fmt.Fprintf(&b, "%s=%s\n", k, s)
	}
	return b.String()
}
