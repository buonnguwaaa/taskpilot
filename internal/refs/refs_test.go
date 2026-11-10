package refs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolve(t *testing.T) {
	for in, want := range map[string]string{
		`C:\Users\HASAKI\Downloads\Telegram Desktop\spec.pdf`: "/mnt/c/Users/HASAKI/Downloads/Telegram Desktop/spec.pdf",
		`D:/data/x.png`:     "/mnt/d/data/x.png",
		"file:///tmp/a.txt": "/tmp/a.txt",
	} {
		if got := Resolve(in); got != want {
			t.Errorf("Resolve(%q) = %q, want %q", in, got, want)
		}
	}
	h, _ := os.UserHomeDir()
	if got := Resolve("~/x.md"); got != filepath.Join(h, "x.md") {
		t.Errorf("~ not expanded: %s", got)
	}
}

func TestParse(t *testing.T) {
	dir := t.TempDir()
	spec := filepath.Join(dir, "spec v1.pdf")
	plain := filepath.Join(dir, "a.txt")
	os.WriteFile(spec, []byte("pdf"), 0o644)
	os.WriteFile(plain, []byte("hello"), 0o644)

	text := "đọc @\"" + spec + "\" và @" + plain + ". Hỏi @team, mail a@b.c, thiếu @" + filepath.Join(dir, "nope.pdf")
	toks := Parse(text)
	var got []string
	for _, tk := range toks {
		got = append(got, tk.Path)
	}
	want := []string{spec, plain, filepath.Join(dir, "nope.pdf")}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("Parse = %q\nwant %q", got, want)
	}
	if toks[1].Raw != "@"+plain {
		t.Errorf("trailing period must not be part of the reference: %q", toks[1].Raw)
	}
}

func TestAttach(t *testing.T) {
	src, dst := t.TempDir(), filepath.Join(t.TempDir(), "refs", "mock-1")
	a := filepath.Join(src, "spec.pdf")
	os.WriteFile(a, []byte("v1"), 0o644)
	other := filepath.Join(src, "sub")
	os.MkdirAll(other, 0o755)
	b := filepath.Join(other, "spec.pdf") // same name, other content
	os.WriteFile(b, []byte("v2"), 0o644)

	out, files, err := Attach("xem @"+a+" và @"+b+" và lại @"+a, dst)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 || filepath.Base(files[0].Copy) != "spec.pdf" || filepath.Base(files[1].Copy) != "spec-2.pdf" {
		t.Fatalf("files = %+v", files)
	}
	if c, _ := os.ReadFile(files[1].Copy); string(c) != "v2" {
		t.Fatal("the second file must keep its content")
	}
	if !strings.HasPrefix(out, "xem @"+a) || !strings.Contains(out, "Tệp đính kèm") || !strings.Contains(out, files[1].Copy) || !strings.Contains(out, "gốc: "+b) {
		t.Fatalf("text:\n%s", out)
	}
	// Attaching the same file again reuses the copy.
	if _, again, err := Attach("@"+a, dst); err != nil || again[0].Copy != files[0].Copy {
		t.Fatalf("reattach: %+v %v", again, err)
	}
	if out, files, err := Attach("không có tệp nào, hỏi @team", dst); err != nil || files != nil || out != "không có tệp nào, hỏi @team" {
		t.Fatal("text without references is unchanged")
	}
	if _, _, err := Attach("@"+filepath.Join(src, "missing.pdf"), dst); err == nil || !strings.Contains(err.Error(), "không tìm thấy") {
		t.Fatalf("missing file: %v", err)
	}
	if _, _, err := Attach("@"+other+"/", dst); err == nil || !strings.Contains(err.Error(), "thư mục") {
		t.Fatalf("directory: %v", err)
	}
}

func TestFormat(t *testing.T) {
	if Format("/a/b.pdf") != "@/a/b.pdf" || Format("/a b/c.pdf") != `@"/a b/c.pdf"` {
		t.Fatal("Format")
	}
}
