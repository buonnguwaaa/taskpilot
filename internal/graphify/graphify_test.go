package graphify

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/buonnguwaaa/taskpilot/internal/config"
)

func write(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for p, c := range files {
		full := filepath.Join(root, p)
		os.MkdirAll(filepath.Dir(full), 0o755)
		if err := os.WriteFile(full, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

const genGRPC = `package generate

import (
	context "context"
	grpc "google.golang.org/grpc"
)

const (
	GenerateService_GenerateWorkOrderV2_FullMethodName = "/generate.GenerateService/GenerateWorkOrderV2"
)

type GenerateServiceClient interface {
	GenerateWorkOrderV2(ctx context.Context, in *GenerateWorkOrderRequest, opts ...grpc.CallOption) (*GenerateWorkOrderResponse, error)
}

type GenerateServiceServer interface {
	GenerateWorkOrderV2(context.Context, *GenerateWorkOrderRequest) (*GenerateWorkOrderResponse, error)
}

func RegisterGenerateServiceServer(s grpc.ServiceRegistrar, srv GenerateServiceServer) {}
func NewGenerateServiceClient(cc grpc.ClientConnInterface) GenerateServiceClient { return nil }
`

// Two repos: the worker calls GenerateWorkOrderV2 through a client embedded
// in its own struct; the report service registers the server and handles it.
func fixture(t *testing.T) (worker, report string) {
	root := t.TempDir()
	worker, report = filepath.Join(root, "worker"), filepath.Join(root, "report")
	write(t, worker, map[string]string{
		"go.mod":                                 "module bom-worker\n",
		"proto/bom/generate/generate_grpc.pb.go": genGRPC,
		"client/generate.go": `package client

import "bom-worker/proto/bom/generate"

type GenerateClient struct {
	generate.GenerateServiceClient
}
`,
		"usecase/gen.go": `package usecase

import (
	"context"

	"bom-worker/proto/bom/generate"
)

type taskUsecase struct{ generateClient generateAPI }

type generateAPI interface{ GenerateWorkOrderV2(context.Context, *generate.GenerateWorkOrderRequest) error }

func (t *taskUsecase) genWO(ctx context.Context, id int64) error {
	_, err := t.generateClient.GenerateWorkOrderV2(ctx, &generate.GenerateWorkOrderRequest{
		PlanProductLineId: id,
	})
	return err
}
`,
	})
	write(t, report, map[string]string{
		"go.mod":                                 "module bom-report\n",
		"proto/bom/generate/generate_grpc.pb.go": genGRPC,
		"server/server.go": `package server

import pb "bom-report/proto/bom/generate"

func Run(s any, h pb.GenerateServiceServer) {
	pb.RegisterGenerateServiceServer(nil, h)
}
`,
		"service/handle.go": `package service

import (
	"context"

	pb "bom-report/proto/bom/generate"
)

type generateService struct{}

func (g *generateService) GenerateWorkOrderV2(ctx context.Context, req *pb.GenerateWorkOrderRequest) (*pb.GenerateWorkOrderResponse, error) {
	return nil, nil
}
`,
	})
	return worker, report
}

func TestGRPCLinksAcrossRepos(t *testing.T) {
	worker, report := fixture(t)
	w, err := ScanRepo("bom-worker", worker)
	if err != nil {
		t.Fatal(err)
	}
	r, err := ScanRepo("bom-report", report)
	if err != nil {
		t.Fatal(err)
	}
	if len(w.Calls) != 1 || len(w.Serves) != 0 || len(r.Serves) != 1 || !r.served["/generate.GenerateService/GenerateWorkOrderV2"] {
		t.Fatalf("worker calls=%+v serves=%+v; report serves=%+v", w.Calls, w.Serves, r.Serves)
	}
	ls := Links(map[string]*RepoRPC{"bom-worker": w, "bom-report": r})
	if len(ls) != 1 {
		t.Fatalf("links = %+v", ls)
	}
	l := ls[0]
	c, p := l.Consumers[0], l.Providers[0]
	if l.Method != "/generate.GenerateService/GenerateWorkOrderV2" || c.Repo != "bom-worker" || c.File != "usecase/gen.go" ||
		c.Line != 14 || c.Func != "(taskUsecase).genWO" || c.FuncLine != 13 || p.Repo != "bom-report" || p.File != "service/handle.go" || p.Line != 11 {
		t.Fatalf("link = %+v", l)
	}
	if out := FormatLinks(ls, "worker", 10); !strings.Contains(out, "called by bom-worker usecase/gen.go:14") || !strings.Contains(out, "served by bom-report service/handle.go:11") {
		t.Fatalf("format:\n%s", out)
	}
	if out := FormatLinks(ls, "nothing", 10); !strings.Contains(out, "no gRPC link") {
		t.Fatal(out)
	}
}

func TestInjectLinks(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "merged.json")
	g := map[string]any{"nodes": []any{
		map[string]any{"id": "bom-worker::usecase_genwo", "repo": "bom-worker", "source_file": "usecase/gen.go", "source_location": "L14", "_callable": true},
		map[string]any{"id": "bom-worker::usecase_gen", "repo": "bom-worker", "source_file": "usecase/gen.go", "source_location": "L1"},
		map[string]any{"id": "bom-report::service_handle", "repo": "bom-report", "source_file": "service/handle.go", "source_location": "L11", "_callable": true},
	}, "links": []any{
		map[string]any{"source": "bom-worker::usecase_gen", "target": "bom-worker::usecase_genwo", "relation": "contains", "source_file": "usecase/gen.go"},
	}}
	b, _ := json.Marshal(g)
	os.WriteFile(in, b, 0o644)
	links := []Link{{Method: "/generate.GenerateService/GenerateWorkOrderV2",
		Consumers: []Site{{Repo: "bom-worker", File: "usecase/gen.go", Line: 15, FuncLine: 14}},
		Providers: []Site{{Repo: "bom-report", File: "service/handle.go", Line: 11, FuncLine: 11}}}}
	out := filepath.Join(dir, "graph.json")
	n, e, added, err := injectLinks(in, out, links, true, []string{"bom-report", "bom-worker"})
	if err != nil || n != 3 || e != 2 || added != 1 {
		t.Fatalf("n=%d e=%d added=%d err=%v", n, e, added, err)
	}
	var got struct {
		Nodes []map[string]any `json:"nodes"`
		Links []map[string]any `json:"links"`
	}
	b, _ = os.ReadFile(out)
	json.Unmarshal(b, &got)
	edge := got.Links[1]
	if edge["relation"] != "calls_grpc" || edge["source"] != "bom-worker::usecase_genwo" || edge["target"] != "bom-report::service_handle" ||
		edge["source_file"] != "bom-worker/usecase/gen.go" || edge["context"] != "/generate.GenerateService/GenerateWorkOrderV2" {
		t.Fatalf("edge = %+v", edge)
	}
	if got.Nodes[2]["source_file"] != "bom-report/service/handle.go" || got.Links[0]["source_file"] != "bom-worker/usecase/gen.go" {
		t.Fatalf("paths carry the repo: %+v %+v", got.Nodes[2], got.Links[0])
	}
}

func run(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v: %v\n%s", args, err, out)
	}
}

// A repo without release/prod is indexed from its base on the prod line, and
// the snapshot is the remote branch, whatever the local checkout is on.
func TestFirstBranchAndExport(t *testing.T) {
	root := t.TempDir()
	origin, clone := filepath.Join(root, "origin"), filepath.Join(root, "clone")
	os.MkdirAll(origin, 0o755)
	run(t, origin, "git", "init", "-q", "-b", "main")
	write(t, origin, map[string]string{"a.go": "package a\n"})
	run(t, origin, "git", "add", ".")
	run(t, origin, "git", "commit", "-qm", "a")
	run(t, root, "git", "clone", "-q", origin, clone)
	run(t, clone, "git", "checkout", "-qb", "feature")
	write(t, clone, map[string]string{"local.go": "package a\n"})
	write(t, origin, map[string]string{"b.go": "package a\n"})
	run(t, origin, "git", "add", ".")
	run(t, origin, "git", "commit", "-qm", "b")

	repo := config.Repo{Path: clone, Base: "main"}
	ctx := context.Background()
	if _, _, err := firstBranch(ctx, repo, "release/qc"); err == nil || !strings.Contains(err.Error(), "no branch release/qc") {
		t.Fatalf("missing branch: %v", err)
	}
	commit, branch, err := firstBranch(ctx, repo, "release/prod,$base")
	if err != nil || branch != "main" || len(commit) != 40 {
		t.Fatalf("commit=%q branch=%q err=%v", commit, branch, err)
	}
	dst := filepath.Join(root, "snap")
	if err := exportTree(ctx, clone, commit, dst); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dst, "b.go")); err != nil {
		t.Fatal("the snapshot has the fetched remote commit")
	}
	if _, err := os.Stat(filepath.Join(dst, "local.go")); err == nil {
		t.Fatal("the operator's local files are not in the snapshot")
	}
}

func TestReadSourceStaysInTheRepo(t *testing.T) {
	g := &config.Graphify{Dir: t.TempDir()}
	write(t, SrcDir(g.Dir, "bom", "qc", "bom-api"), map[string]string{"x/a.go": "l1\nl2\nl3\n"})
	out, err := ReadSource(g, "bom", "qc", "bom-api", "x/a.go", 2, 3)
	if err != nil || !strings.Contains(out, "2\tl2") || strings.Contains(out, "l1") {
		t.Fatalf("%q %v", out, err)
	}
	if _, err := ReadSource(g, "bom", "qc", "bom-api", "../../../../etc/passwd", 0, 0); err == nil {
		t.Fatal("paths outside the repo are refused")
	}
	if _, err := ReadSource(g, "bom", "qc", "../x", "a.go", 0, 0); err == nil {
		t.Fatal("bad repo name refused")
	}
}

func TestDedupeRepo(t *testing.T) {
	in := "  payment-fiserv/payment-fiserv/internal/a.go\n- .X() [calls] bom-report/application/b.go:L9 a/b/c"
	want := "  payment-fiserv/internal/a.go\n- .X() [calls] bom-report/application/b.go:L9 a/b/c"
	if got := dedupeRepo(in); got != want {
		t.Fatalf("%q", got)
	}
}
