package hasakiwork

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os/exec"
	"strings"
	"time"

	"github.com/coder/websocket"
)

// Chrome drives a dedicated Chrome profile over the DevTools protocol so the
// operator can log in through SSO and taskpilot can read the session cookie.
// The profile is separate from the operator's everyday Chrome: Chrome refuses
// remote debugging on the default profile, and keeping it separate means the
// debug port never exposes the operator's own browsing session.
type Chrome struct {
	// Exe is the Chrome binary (on WSL, the Windows chrome.exe path under /mnt/c).
	Exe string
	// ProfileDir is passed to --user-data-dir (a Windows path when Exe is Windows Chrome).
	ProfileDir string
	Port       int
	HTTP       *http.Client
}

// CDPCookie is a cookie as returned by Storage.getCookies.
type CDPCookie struct {
	Name     string  `json:"name"`
	Value    string  `json:"value"`
	Domain   string  `json:"domain"`
	Expires  float64 `json:"expires"`
	HTTPOnly bool    `json:"httpOnly"`
}

func (c *Chrome) base() string { return fmt.Sprintf("http://127.0.0.1:%d", c.Port) }

// Running reports whether a Chrome with this debug port is up.
func (c *Chrome) Running(ctx context.Context) bool {
	_, err := c.browserWS(ctx)
	return err == nil
}

// Launch starts Chrome on url with the dedicated profile and debug port.
func (c *Chrome) Launch(ctx context.Context, url string) error {
	if c.Running(ctx) {
		return nil
	}
	cmd := exec.Command(c.Exe,
		"--user-data-dir="+c.ProfileDir,
		fmt.Sprintf("--remote-debugging-port=%d", c.Port),
		"--no-first-run", "--no-default-browser-check", url)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start chrome: %w", err)
	}
	go cmd.Wait() // reap; the Windows launcher may exit immediately
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if c.Running(ctx) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	return errors.New("chrome did not open its debug port within 30s")
}

func (c *Chrome) browserWS(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base()+"/json/version", nil)
	if err != nil {
		return "", err
	}
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 3 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var v struct {
		WS string `json:"webSocketDebuggerUrl"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		return "", err
	}
	if v.WS == "" {
		return "", errors.New("no browser websocket")
	}
	return v.WS, nil
}

// call sends one browser-level CDP command and returns its result.
func (c *Chrome) call(ctx context.Context, method string, params any, out any) error {
	wsURL, err := c.browserWS(ctx)
	if err != nil {
		return err
	}
	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		return err
	}
	defer conn.CloseNow()
	conn.SetReadLimit(32 << 20)
	if params == nil {
		params = struct{}{}
	}
	msg, _ := json.Marshal(map[string]any{"id": 1, "method": method, "params": params})
	if err := conn.Write(ctx, websocket.MessageText, msg); err != nil {
		return err
	}
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return err
		}
		var resp struct {
			ID     int             `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(data, &resp) != nil || resp.ID != 1 {
			continue // an event
		}
		if resp.Error != nil {
			return fmt.Errorf("%s: %s", method, resp.Error.Message)
		}
		if out != nil {
			return json.Unmarshal(resp.Result, out)
		}
		return nil
	}
}

// Cookies returns the cookies that would be sent to host.
func (c *Chrome) Cookies(ctx context.Context, host string) ([]CDPCookie, error) {
	var res struct {
		Cookies []CDPCookie `json:"cookies"`
	}
	if err := c.call(ctx, "Storage.getCookies", nil, &res); err != nil {
		return nil, err
	}
	var out []CDPCookie
	for _, ck := range res.Cookies {
		d := strings.TrimPrefix(ck.Domain, ".")
		if host == d || (strings.HasPrefix(ck.Domain, ".") && strings.HasSuffix(host, "."+d)) {
			out = append(out, ck)
		}
	}
	return out, nil
}

// Close shuts the browser down, closing the debug port.
func (c *Chrome) Close(ctx context.Context) error {
	err := c.call(ctx, "Browser.close", nil, nil)
	if err != nil && strings.Contains(err.Error(), "EOF") {
		return nil // the browser closed the socket while exiting
	}
	return err
}

// OpenTab opens url in a new tab of the running browser.
func (c *Chrome) OpenTab(ctx context.Context, url string) error {
	return c.call(ctx, "Target.createTarget", map[string]any{"url": url}, nil)
}

// Open launches the browser on url, or opens url in a new tab if it already runs.
func (c *Chrome) Open(ctx context.Context, url string) error {
	if c.Running(ctx) {
		return c.OpenTab(ctx, url)
	}
	return c.Launch(ctx, url)
}

// WaitForCookies polls the cookies of host until ready accepts them.
func (c *Chrome) WaitForCookies(ctx context.Context, host string, ready func([]CDPCookie) bool) ([]CDPCookie, error) {
	for {
		if cookies, err := c.Cookies(ctx, host); err == nil && ready(cookies) {
			return cookies, nil
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("not logged in yet (did you finish logging in?): %w", ctx.Err())
		case <-time.After(2 * time.Second):
		}
	}
}

// SessionCookieName is the httpOnly cookie that carries the Hasaki Work JWT.
const SessionCookieName = "wshr-token"

// WaitForSession polls until the session cookie exists, returning a Cookie
// header value for host.
func (c *Chrome) WaitForSession(ctx context.Context, host string) (string, []CDPCookie, error) {
	for {
		cookies, err := c.Cookies(ctx, host)
		if err == nil {
			for _, ck := range cookies {
				if ck.Name == SessionCookieName && ck.Value != "" {
					return CookieHeader(cookies), cookies, nil
				}
			}
		}
		select {
		case <-ctx.Done():
			return "", nil, fmt.Errorf("no %s cookie yet (did you finish logging in?): %w", SessionCookieName, ctx.Err())
		case <-time.After(2 * time.Second):
		}
	}
}

// CookieHeader renders cookies as a Cookie header value.
func CookieHeader(cookies []CDPCookie) string {
	parts := make([]string, 0, len(cookies))
	for _, ck := range cookies {
		parts = append(parts, ck.Name+"="+ck.Value)
	}
	return strings.Join(parts, "; ")
}
