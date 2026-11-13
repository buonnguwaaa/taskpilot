package vault

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func fakeVault(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Vault-Token") != "tok" {
			w.WriteHeader(403)
			io.WriteString(w, `{"errors":["permission denied","invalid token"]}`)
			return
		}
		switch {
		case r.URL.Path == "/v1/auth/token/lookup-self":
			io.WriteString(w, `{"data":{"display_name":"oidc-nhannhk","policies":["default","dev-read"],"expire_time":"2026-11-12T10:00:00Z"}}`)
		case r.URL.Path == "/v1/sys/internal/ui/mounts":
			io.WriteString(w, `{"data":{"secret":{"kv/":{"type":"kv","options":{"version":"2"}},"old/":{"type":"kv","options":{}},"cubbyhole/":{"type":"cubbyhole"}}}}`)
		case r.Method == "LIST" && r.URL.Path == "/v1/kv/metadata/payment/":
			io.WriteString(w, `{"data":{"keys":["payment-vnpay","common/"]}}`)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/kv/data/payment/payment-vnpay":
			io.WriteString(w, `{"data":{"data":{"VNPAY_URL":"https://sandbox","DB_PASSWORD":"s3cr3t","PORT":3904},"metadata":{}}}`)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/old/app":
			io.WriteString(w, `{"data":{"A":"1"}}`)
		case r.URL.Path == "/v1/kv/data/payment/forbidden":
			w.WriteHeader(403)
			io.WriteString(w, `{"errors":["1 error occurred:\n\t* permission denied\n\n"]}`)
		default:
			w.WriteHeader(404)
			io.WriteString(w, `{"errors":[]}`)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestClient(t *testing.T) {
	srv := fakeVault(t)
	c := &Client{BaseURL: srv.URL, Token: "tok"}
	ctx := context.Background()

	info, err := c.LookupSelf(ctx)
	if err != nil || info.DisplayName != "oidc-nhannhk" || info.Expires == nil || len(info.Policies) != 2 {
		t.Fatalf("lookup = %+v %v", info, err)
	}
	if roots, err := c.List(ctx, ""); err != nil || strings.Join(roots, ",") != "kv/,old/" {
		t.Fatalf("mounts = %v %v", roots, err)
	}
	if keys, err := c.List(ctx, "kv/payment/"); err != nil || strings.Join(keys, ",") != "common/,payment-vnpay" {
		t.Fatalf("list v2 = %v %v", keys, err)
	}
	kv, err := c.Read(ctx, "/kv/payment/payment-vnpay")
	if err != nil || kv["VNPAY_URL"] != "https://sandbox" {
		t.Fatalf("read v2 = %v %v", kv, err)
	}
	if got := Format(kv); got != "DB_PASSWORD=s3cr3t\nPORT=3904\nVNPAY_URL=https://sandbox\n" {
		t.Fatalf("format = %q", got)
	}
	if kv, err := c.Read(ctx, "old/app"); err != nil || kv["A"] != "1" {
		t.Fatalf("read v1 = %v %v", kv, err)
	}
	if _, err := c.Read(ctx, "kv/payment/forbidden"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("forbidden: %v", err)
	}
	if _, err := c.Read(ctx, "kv/payment/../../sys"); err == nil {
		t.Fatal(".. must be refused")
	}
	if _, err := (&Client{BaseURL: srv.URL, Token: "bad"}).LookupSelf(ctx); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("bad token: %v", err)
	}
	if _, err := (&Client{BaseURL: srv.URL}).LookupSelf(ctx); !errors.Is(err, ErrNoCredentials) {
		t.Fatalf("no token: %v", err)
	}
}

func TestAllowed(t *testing.T) {
	roots := []string{"kv/payment/", "old/payment"}
	for p, want := range map[string]bool{
		"kv/payment/payment-vnpay": true,
		"kv/payment":               true,
		"/kv/payment/x/y":          true,
		"kv/paymentx/a":            false,
		"kv/bom/a":                 false,
		"old/payment/a":            true,
		"kv/payment/../bom/a":      false,
		"":                         false,
	} {
		if got := Allowed(p, roots); got != want {
			t.Errorf("Allowed(%q) = %v, want %v", p, got, want)
		}
	}
}
