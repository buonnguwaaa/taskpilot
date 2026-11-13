package claudeai

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestFindLinks(t *testing.T) {
	text := "Prototype: [https://claude.ai/artifact/TKJn6npaM7t13GKEdVc48T?sk=cT4V#play-1](https://claude.ai/artifact/TKJn6npaM7t13GKEdVc48T?sk=cT4V#play-1).\n" +
		"xem thêm https://claude.ai/code/artifact/d51bf2e7-7bb1-4290-9df2-28679e55be14, và https://claude.ai/chat/abc"
	got := FindLinks(text)
	want := []string{"https://claude.ai/artifact/TKJn6npaM7t13GKEdVc48T?sk=cT4V#play-1", "https://claude.ai/code/artifact/d51bf2e7-7bb1-4290-9df2-28679e55be14"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("links = %q", got)
	}
	if _, err := Slug("https://evil.example/artifact/x"); err == nil {
		t.Fatal("other hosts must be refused")
	}
}

func TestPickAndLocalPath(t *testing.T) {
	type f = struct {
		Size int64 `json:"size"`
	}
	design := map[string]f{"index.html": {}, "SKILL.md": {}, "artifact-type/app.js": {}, "project/Main.dc.html": {}, "project/canvas.json": {}}
	if got := pick(design); !reflect.DeepEqual(got, []string{"project/Main.dc.html", "project/canvas.json"}) {
		t.Fatalf("design files = %v", got)
	}
	page := map[string]f{"index.html": {}, "app.js": {}, "_files.json": {}}
	if got := pick(page); !reflect.DeepEqual(got, []string{"app.js", "index.html"}) {
		t.Fatalf("page files = %v", got)
	}
	dir := t.TempDir()
	if p, ok := localPath(dir, "project/a.html"); !ok || p != filepath.Join(dir, "project", "a.html") {
		t.Fatalf("local = %s %v", p, ok)
	}
	for _, bad := range []string{"../x", "a/../../x", ""} {
		if p, ok := localPath(dir, bad); ok && !strings.HasPrefix(p, dir+string(filepath.Separator)) {
			t.Fatalf("%q escapes: %s", bad, p)
		} else if ok {
			t.Fatalf("%q accepted as %s", bad, p)
		}
	}
}
