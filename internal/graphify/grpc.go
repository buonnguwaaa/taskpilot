package graphify

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// gRPC links between the repos of a project, read from the Go sources of a
// line: each repo carries the generated code of the protos it uses
// (*_grpc.pb.go), so the full method name ("/pkg.Service/Method") is the key
// that joins a call in one repo to the handler in another.
//
//	provider: a non-generated file calls Register<Service>Server, and a method
//	          named <Method> takes *<Request> of that proto package (the handler)
//	consumer: a call <expr>.<Method>(ctx, &pkg.<Request>{…}) (or on an expression
//	          that looks like a client) in a file importing that proto package
//
// graphify does not see these calls: the client is usually embedded in a
// struct of the repo, so its call graph stops at the selector.

// RPC is one method of a gRPC service, from the generated code.
type RPC struct {
	FullName string // /pkg.Service/Method
	Service  string // Service
	Method   string
	Request  string // request message type name
	pkgDir   string // the generated package's directory, relative to the repo
}

// Site is a place in a repo's source.
type Site struct {
	Repo string `json:"repo"`
	File string `json:"file"`           // relative to the repo
	Line int    `json:"line"`           // of the call, or of the handler
	Func string `json:"func,omitempty"` // enclosing function / the handler
	// FuncLine is the line of the enclosing function's declaration (graph node).
	FuncLine int `json:"func_line,omitempty"`
}

// Link is a gRPC method called from one repo and served by another.
type Link struct {
	Method    string `json:"method"` // /pkg.Service/Method
	Consumers []Site `json:"consumers"`
	Providers []Site `json:"providers"`
}

// RepoRPC is what one repo serves and calls.
type RepoRPC struct {
	Serves []Site // handlers, Func = method name
	Calls  []Site // call sites

	siteMethod map[string]string // siteKey → full method name
	served     map[string]bool   // full method names this repo registers a server for
	rpcs       map[string]RPC    // full name → rpc
}

var fullNameRe = regexp.MustCompile(`^"(/[\w.]+/\w+)"$`)

// ScanRepo reads the gRPC services a repo serves and the calls it makes.
func ScanRepo(repo, dir string) (*RepoRPC, error) {
	module := goModule(dir)
	fset := token.NewFileSet()
	type parsed struct {
		rel string
		f   *ast.File
	}
	var gen, src []parsed
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			switch d.Name() {
			case "vendor", "node_modules", ".git", "testdata":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		rel, _ := filepath.Rel(dir, p)
		isGen := strings.HasSuffix(p, ".pb.go")
		if isGen && !strings.HasSuffix(p, "_grpc.pb.go") {
			return nil // messages only
		}
		f, err := parser.ParseFile(fset, p, nil, parser.SkipObjectResolution)
		if err != nil {
			return nil // a file that does not parse is skipped
		}
		if isGen {
			gen = append(gen, parsed{rel, f})
		} else {
			src = append(src, parsed{rel, f})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	out := &RepoRPC{siteMethod: map[string]string{}, served: map[string]bool{}, rpcs: map[string]RPC{}}
	// Generated code: full names and request types per client method.
	byPkg := map[string]map[string]RPC{} // import path → method name → rpc (several services may share a name)
	for _, g := range gen {
		pkgDir := filepath.ToSlash(filepath.Dir(g.rel))
		names := map[string]string{} // Service_Method → full name
		for _, decl := range g.f.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.CONST {
				continue
			}
			for _, spec := range gd.Specs {
				vs := spec.(*ast.ValueSpec)
				for i, n := range vs.Names {
					if !strings.HasSuffix(n.Name, "_FullMethodName") || i >= len(vs.Values) {
						continue
					}
					if bl, ok := vs.Values[i].(*ast.BasicLit); ok {
						if m := fullNameRe.FindStringSubmatch(bl.Value); m != nil {
							names[strings.TrimSuffix(n.Name, "_FullMethodName")] = m[1]
						}
					}
				}
			}
		}
		for _, decl := range g.f.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.TYPE {
				continue
			}
			for _, spec := range gd.Specs {
				ts := spec.(*ast.TypeSpec)
				it, ok := ts.Type.(*ast.InterfaceType)
				if !ok || !strings.HasSuffix(ts.Name.Name, "Client") {
					continue
				}
				svc := strings.TrimSuffix(ts.Name.Name, "Client")
				for _, m := range it.Methods.List {
					ft, ok := m.Type.(*ast.FuncType)
					if !ok || len(m.Names) == 0 || ft.Params == nil || len(ft.Params.List) < 2 {
						continue
					}
					full := names[svc+"_"+m.Names[0].Name]
					if full == "" {
						full = "/" + g.f.Name.Name + "." + svc + "/" + m.Names[0].Name
					}
					req := typeName(ft.Params.List[1].Type)
					rpc := RPC{FullName: full, Service: svc, Method: m.Names[0].Name, Request: req, pkgDir: pkgDir}
					imp := importPath(module, pkgDir)
					if byPkg[imp] == nil {
						byPkg[imp] = map[string]RPC{}
					}
					byPkg[imp][rpc.Method+"|"+req] = rpc
					out.rpcs[full] = rpc
				}
			}
		}
	}
	if len(byPkg) == 0 {
		return out, nil
	}
	for _, s := range src {
		aliases := map[string]string{} // local name → import path of a proto package
		for _, im := range s.f.Imports {
			p, _ := strconv.Unquote(im.Path.Value)
			if byPkg[p] == nil {
				continue
			}
			name := filepath.Base(p)
			if im.Name != nil {
				name = im.Name.Name
			}
			aliases[name] = p
		}
		if len(aliases) == 0 {
			continue
		}
		// Servers registered in this file.
		ast.Inspect(s.f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || !strings.HasPrefix(sel.Sel.Name, "Register") || !strings.HasSuffix(sel.Sel.Name, "Server") {
				return true
			}
			x, ok := sel.X.(*ast.Ident)
			if !ok || aliases[x.Name] == "" {
				return true
			}
			svc := strings.TrimSuffix(strings.TrimPrefix(sel.Sel.Name, "Register"), "Server")
			for _, rpc := range byPkg[aliases[x.Name]] {
				if rpc.Service == svc {
					out.served[rpc.FullName] = true
				}
			}
			return true
		})
		for _, decl := range s.f.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			fnLine := fset.Position(fd.Pos()).Line
			// A handler: a method taking *alias.Request.
			if fd.Recv != nil && fd.Type.Params != nil {
				for _, prm := range fd.Type.Params.List {
					if alias, req, ok := selType(prm.Type); ok && aliases[alias] != "" {
						if rpc, ok := byPkg[aliases[alias]][fd.Name.Name+"|"+req]; ok {
							site := Site{Repo: repo, File: filepath.ToSlash(s.rel), Line: fnLine, Func: fd.Name.Name, FuncLine: fnLine}
							out.Serves = append(out.Serves, site)
							out.siteMethod[siteKey(site, true)] = rpc.FullName
						}
					}
				}
			}
			// Calls of a client method.
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				rpc, ok := matchCall(sel, call.Args, aliases, byPkg)
				if !ok {
					return true
				}
				site := Site{Repo: repo, File: filepath.ToSlash(s.rel), Line: fset.Position(call.Pos()).Line,
					Func: funcName(fd), FuncLine: fnLine}
				out.Calls = append(out.Calls, site)
				out.siteMethod[siteKey(site, false)] = rpc.FullName
				return true
			})
		}
	}
	return out, nil
}

// matchCall reports whether sel(args) calls a client method: the request
// argument is &alias.Request{…} / alias.Request{…}, or the receiver looks
// like a client and the method name is unique among the imported services.
func matchCall(sel *ast.SelectorExpr, args []ast.Expr, aliases map[string]string, byPkg map[string]map[string]RPC) (RPC, bool) {
	name := sel.Sel.Name
	if len(args) >= 2 {
		arg := args[1]
		if u, ok := arg.(*ast.UnaryExpr); ok && u.Op == token.AND {
			arg = u.X
		}
		if cl, ok := arg.(*ast.CompositeLit); ok {
			if alias, req, ok := selType(cl.Type); ok && aliases[alias] != "" {
				rpc, ok := byPkg[aliases[alias]][name+"|"+req]
				return rpc, ok
			}
		}
	}
	if !looksLikeClient(sel.X) {
		return RPC{}, false
	}
	var found []RPC
	for _, p := range aliases {
		for _, rpc := range byPkg[p] {
			if rpc.Method == name {
				found = append(found, rpc)
			}
		}
	}
	if len(found) == 1 && len(args) >= 2 {
		return found[0], true
	}
	return RPC{}, false
}

func looksLikeClient(x ast.Expr) bool {
	var last string
	switch v := x.(type) {
	case *ast.Ident:
		last = v.Name
	case *ast.SelectorExpr:
		last = v.Sel.Name
	default:
		return false
	}
	return strings.Contains(strings.ToLower(last), "client")
}

// selType splits *alias.Name / alias.Name.
func selType(e ast.Expr) (alias, name string, ok bool) {
	if st, isStar := e.(*ast.StarExpr); isStar {
		e = st.X
	}
	sel, isSel := e.(*ast.SelectorExpr)
	if !isSel {
		return "", "", false
	}
	id, isID := sel.X.(*ast.Ident)
	if !isID {
		return "", "", false
	}
	return id.Name, sel.Sel.Name, true
}

func typeName(e ast.Expr) string {
	if st, ok := e.(*ast.StarExpr); ok {
		e = st.X
	}
	switch v := e.(type) {
	case *ast.Ident:
		return v.Name
	case *ast.SelectorExpr:
		return v.Sel.Name
	}
	return ""
}

func funcName(fd *ast.FuncDecl) string {
	if fd.Recv != nil && len(fd.Recv.List) > 0 {
		return "(" + strings.TrimPrefix(typeName(fd.Recv.List[0].Type), "*") + ")." + fd.Name.Name
	}
	return fd.Name.Name
}

func goModule(dir string) string {
	b, err := os.ReadFile(filepath.Join(dir, "go.mod"))
	if err != nil {
		return ""
	}
	for _, l := range strings.Split(string(b), "\n") {
		if f := strings.Fields(l); len(f) >= 2 && f[0] == "module" {
			return f[1]
		}
	}
	return ""
}

func importPath(module, pkgDir string) string {
	if pkgDir == "." {
		return module
	}
	if module == "" {
		return pkgDir
	}
	return module + "/" + pkgDir
}

// methodOf returns the full method name of a site.
func (r *RepoRPC) methodOf(s Site, served bool) string {
	return r.siteMethod[siteKey(s, served)]
}

func siteKey(s Site, served bool) string {
	return strconv.FormatBool(served) + "|" + s.File + "|" + strconv.Itoa(s.Line)
}

// Links joins the repos of a line: methods called in one repo and served by
// another, sorted by method.
func Links(repos map[string]*RepoRPC) []Link {
	byMethod := map[string]*Link{}
	get := func(m string) *Link {
		if byMethod[m] == nil {
			byMethod[m] = &Link{Method: m}
		}
		return byMethod[m]
	}
	for _, r := range repos {
		for _, s := range r.Serves {
			m := r.methodOf(s, true)
			if m != "" && r.served[m] {
				get(m).Providers = append(get(m).Providers, s)
			}
		}
	}
	for _, r := range repos {
		for _, s := range r.Calls {
			if m := r.methodOf(s, false); m != "" {
				get(m).Consumers = append(get(m).Consumers, s)
			}
		}
	}
	var out []Link
	for _, l := range byMethod {
		if len(l.Consumers) == 0 || len(l.Providers) == 0 {
			continue
		}
		cross := false
		for _, c := range l.Consumers {
			for _, p := range l.Providers {
				cross = cross || c.Repo != p.Repo
			}
		}
		if !cross {
			continue
		}
		sortSites(l.Consumers)
		sortSites(l.Providers)
		out = append(out, *l)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Method < out[j].Method })
	return out
}

func sortSites(s []Site) {
	sort.Slice(s, func(i, j int) bool {
		if s[i].Repo != s[j].Repo {
			return s[i].Repo < s[j].Repo
		}
		if s[i].File != s[j].File {
			return s[i].File < s[j].File
		}
		return s[i].Line < s[j].Line
	})
}
