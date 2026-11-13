package hasakiwork

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
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
	ws, err := c.browserWS(ctx)
	if err != nil {
		return err
	}
	return TargetCall(ctx, Target{WS: ws}, method, params, out)
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

// Open launches the browser on url, or opens url in a new tab if it already runs.
func (c *Chrome) Open(ctx context.Context, url string) error {
	if c.Running(ctx) {
		_, err := c.OpenTabID(ctx, url)
		return err
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

// CookieHeader renders cookies as a Cookie header value.
func CookieHeader(cookies []CDPCookie) string {
	parts := make([]string, 0, len(cookies))
	for _, ck := range cookies {
		parts = append(parts, ck.Name+"="+ck.Value)
	}
	return strings.Join(parts, "; ")
}

// EvalInPage evaluates a JavaScript expression in the first open page on
// host and returns its string result (e.g. a value the site keeps in
// localStorage rather than in a cookie).
func (c *Chrome) EvalInPage(ctx context.Context, host, expr string) (string, error) {
	ts, err := c.Targets(ctx)
	if err != nil {
		return "", err
	}
	for _, t := range ts {
		if u, err := url.Parse(t.URL); err == nil && t.Type == "page" && t.WS != "" && u.Hostname() == host {
			var s string
			err := Eval(ctx, t, expr, &s)
			return s, err
		}
	}
	return "", fmt.Errorf("no open page on %s", host)
}

// WaitForEval polls EvalInPage until the result is not empty.
func (c *Chrome) WaitForEval(ctx context.Context, host, expr string) (string, error) {
	for {
		if v, err := c.EvalInPage(ctx, host, expr); err == nil && v != "" {
			return v, nil
		}
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("hết thời gian chờ đăng nhập trên %s", host)
		case <-time.After(2 * time.Second):
		}
	}
}

// Target is a page or frame of the running browser.
type Target struct {
	ID   string `json:"id"`
	Type string `json:"type"` // page, iframe, ...
	URL  string `json:"url"`
	WS   string `json:"webSocketDebuggerUrl"`
}

// Targets lists the pages and out-of-process frames of the running browser.
func (c *Chrome) Targets(ctx context.Context) ([]Target, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base()+"/json/list", nil)
	if err != nil {
		return nil, err
	}
	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var ts []Target
	return ts, json.NewDecoder(resp.Body).Decode(&ts)
}

// SetCookie stores a cookie in the profile (Storage.setCookies).
func (c *Chrome) SetCookie(ctx context.Context, name, value, domain string, httpOnly bool) error {
	return c.call(ctx, "Storage.setCookies", map[string]any{"cookies": []map[string]any{{
		"name": name, "value": value, "domain": domain, "path": "/",
		"secure": true, "httpOnly": httpOnly, "sameSite": "Lax",
	}}}, nil)
}

// CloseTarget closes a tab.
func (c *Chrome) CloseTarget(ctx context.Context, id string) error {
	return c.call(ctx, "Target.closeTarget", map[string]any{"targetId": id}, nil)
}

// TargetCall sends one CDP command to a target (page or frame) and decodes
// its result into out.
func TargetCall(ctx context.Context, t Target, method string, params any, out any) error {
	conn, _, err := websocket.Dial(ctx, t.WS, nil)
	if err != nil {
		return err
	}
	defer conn.CloseNow()
	conn.SetReadLimit(256 << 20)
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
			continue
		}
		if resp.Error != nil {
			return fmt.Errorf("%s: %s", method, resp.Error.Message)
		}
		if out == nil {
			return nil
		}
		return json.Unmarshal(resp.Result, out)
	}
}

// Eval evaluates expr in a target, awaiting a promise, and decodes the value
// it returns into out. A thrown exception is an error.
func Eval(ctx context.Context, t Target, expr string, out any) error {
	var r struct {
		Result struct {
			Value json.RawMessage `json:"value"`
		} `json:"result"`
		Exception *struct {
			Text      string `json:"text"`
			Exception struct {
				Description string `json:"description"`
			} `json:"exception"`
		} `json:"exceptionDetails"`
	}
	err := TargetCall(ctx, t, "Runtime.evaluate", map[string]any{"expression": expr, "returnByValue": true, "awaitPromise": true}, &r)
	if err != nil {
		return err
	}
	if r.Exception != nil {
		d := r.Exception.Exception.Description
		if d == "" {
			d = r.Exception.Text
		}
		return errors.New(d)
	}
	if out == nil || len(r.Result.Value) == 0 {
		return nil
	}
	return json.Unmarshal(r.Result.Value, out)
}

// Screenshot captures what a page shows, as PNG bytes.
func Screenshot(ctx context.Context, t Target) ([]byte, error) {
	var r struct {
		Data []byte `json:"data"` // base64 in JSON, decoded by encoding/json
	}
	if err := TargetCall(ctx, t, "Page.captureScreenshot", map[string]any{"format": "png"}, &r); err != nil {
		return nil, err
	}
	return r.Data, nil
}

// OpenTabID opens url in a new tab of the running browser and returns the
// tab's target id.
func (c *Chrome) OpenTabID(ctx context.Context, url string) (string, error) {
	var r struct {
		TargetID string `json:"targetId"`
	}
	err := c.call(ctx, "Target.createTarget", map[string]any{"url": url}, &r)
	return r.TargetID, err
}
