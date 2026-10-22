package entitycompat

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// Sync part kinds a field of a struct (or a package-level var) can hold. Each one is a detection branch
// of ScanCacheTypes, and the cache-check gate names an existing type for each
// of them (a lower bound), so a branch that stops matching fails the gate.
const (
	SyncPartMutex        = "sync.Mutex"
	SyncPartRWMutex      = "sync.RWMutex"
	SyncPartMap          = "sync.Map"
	SyncPartOnce         = "sync.Once"
	SyncPartAtomic       = "atomic"
	SyncPartSingleflight = "singleflight.Group"
	SyncPartLRU          = "lru.Cache"
	SyncPartExpirable    = "expirable.LRU"
	// SyncPartScannedType is a package-level var whose type is a struct type
	// of the same package that holds sync parts itself.
	SyncPartScannedType = "scanned type"
)

// syncPartTypes maps an import path and a type name to the sync part kind. A
// type of sync/atomic counts whatever its name (atomic.Int64, atomic.Pointer[T],
// atomic.Value ...).
var syncPartTypes = map[string]map[string]string{
	"sync": {
		"Mutex": SyncPartMutex, "RWMutex": SyncPartRWMutex, "Map": SyncPartMap, "Once": SyncPartOnce,
	},
	"golang.org/x/sync/singleflight":               {"Group": SyncPartSingleflight},
	"github.com/hashicorp/golang-lru/v2":           {"Cache": SyncPartLRU},
	"github.com/hashicorp/golang-lru/v2/expirable": {"LRU": SyncPartExpirable},
}

// importNames are the package names of the import paths whose last element is
// not the package name.
var importNames = map[string]string{
	"github.com/hashicorp/golang-lru/v2": "lru",
}

// CacheScanExcludedDirs are the directories (and everything under them) that
// ScanCacheTypes does not read, with the reason. どれも、サーバーの世代(#3666)の
// 部品から使われない。
var CacheScanExcludedDirs = map[string]string{
	// テストの補助。本番のコードからimportされない。
	"internal/testutil": "test helpers, not imported by production code",
	"plugin/plugintest": "test helpers for plugins, not imported by production code",
	// バックアップのdaemonと、そこから戻す処理。importするのはinternal/cli/backupだけで、
	// サーバーの世代は使わない。internal/backup自体はbackupadmin経由でサーバーが使うので読む。
	"internal/backup/daemon":    "the backup daemon; only internal/cli/backup imports it",
	"internal/backup/restoreop": "the restore run by the backup daemon; only internal/cli/backup imports it",
	// サブコマンドの処理。世代を組み立てて動かす側で、世代の部品ではない。
	"internal/cli": "subcommands; they build and run generations, and are no part of one",
}

// cacheScanFixtureDir is the directory whose subdirectories ending in
// "fixture" hold the inputs of the entitycompat gates.
const cacheScanFixtureDir = "internal/entitycompat/"

// CacheType is a named struct type or a package-level var of the scanned
// sources that the cache-check gate classifies: a struct type whose fields
// hold sync parts, a struct type that declares DropLocal, a struct type with
// a map field reached from a DropLocal type, or a package-level var whose type
// holds sync parts.
type CacheType struct {
	// Key is "<package dir>.<name>", e.g. "internal/core/captcha.Service".
	Key string
	// Var reports whether the entry is a package-level var.
	Var bool
	// Pos is "<file>:<line>" of the declaration.
	Pos string
	// Parts are the sync part kinds the fields (or the var) hold.
	Parts map[string]bool
	// Fingerprint is the sorted names of all the fields, embedded ones by
	// their type name and those of an unnamed struct field as "outer.inner".
	// For a var, it is the fields of its unnamed struct type and the other
	// vars of the same var block as "var:<name>". どのフィールドを足しても
	// 変わるので、型を分類し直すまで落ちる(mutexで守る値をポインタで足す形も)。
	Fingerprint []string

	fields []scannedField
}

type scannedField struct {
	name  string
	isMap bool
	// parts found directly in the field type.
	parts map[string]bool
	// refs are the keys of the named types of the same package the field
	// type uses (through pointers, slices, maps and type arguments).
	refs []string
}

// CacheTypeScan is the result of ScanCacheTypes.
type CacheTypeScan struct {
	// Types are the entries to classify (see CacheType), by key.
	Types map[string]*CacheType
	// DropLocal is the set of keys of the named types (of any kind) that
	// declare a DropLocal method.
	DropLocal map[string]bool
	// OwnedBy maps the key of a named type without DropLocal to a type of the
	// same package that declares DropLocal and reaches it through field types
	// of that package.
	OwnedBy map[string]string
	// Files is the number of files read.
	Files int
}

// ScanCacheTypes reads every tracked non-test .go file under internal and
// plugin of the repository at root (except CacheScanExcludedDirs and the
// fixtures) and returns the entries the cache-check gate classifies (see
// CacheType) and the types that declare DropLocal.
//
// **go/typesを使わず、構文木だけで判定する。** 型検査にはimport先のexport
// dataが要り、route_resolve_test.goのcollectRoutesに書いたとおり、CIのtestの
// buildの後でも約18秒かかる。フィールドの型は、importの名前と同じパッケージの
// 型名から引く。
func ScanCacheTypes(root string) (*CacheTypeScan, error) {
	paths, err := cacheScanFiles(root)
	if err != nil {
		return nil, err
	}
	scan := &CacheTypeScan{
		Types: map[string]*CacheType{}, DropLocal: map[string]bool{}, OwnedBy: map[string]string{}, Files: len(paths),
	}
	structs := map[string]*CacheType{}
	fset := token.NewFileSet()
	// declaredは走査したパッケージの名前付きの型(struct以外も)。フィールドの型が
	// 走査した型かどうかを引くのに使う。
	declared := map[string]bool{}
	type pending struct {
		file *ast.File
		pkg  string
	}
	var files []pending
	for _, rel := range paths {
		src, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", rel, err)
		}
		f, err := parser.ParseFile(fset, rel, src, parser.SkipObjectResolution)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", rel, err)
		}
		pkg := path.Dir(rel)
		files = append(files, pending{file: f, pkg: pkg})
		for _, decl := range f.Decls {
			switch d := decl.(type) {
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					if ts, ok := spec.(*ast.TypeSpec); ok {
						declared[pkg+"."+ts.Name.Name] = true
					}
				}
			case *ast.FuncDecl:
				if d.Recv != nil && d.Name.Name == "DropLocal" && len(d.Recv.List) == 1 {
					if name := baseTypeName(d.Recv.List[0].Type); name != "" {
						scan.DropLocal[pkg+"."+name] = true
					}
				}
			}
		}
	}

	for _, pf := range files {
		imports := fileImports(pf.file)
		for _, decl := range pf.file.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.TYPE {
				continue
			}
			for _, spec := range gd.Specs {
				ts := spec.(*ast.TypeSpec)
				st, ok := ts.Type.(*ast.StructType)
				if !ok {
					continue
				}
				ct := &CacheType{
					Key:   pf.pkg + "." + ts.Name.Name,
					Pos:   fmt.Sprintf("%s:%d", fset.Position(ts.Pos()).Filename, fset.Position(ts.Pos()).Line),
					Parts: map[string]bool{},
				}
				collectFields(st, "", imports, pf.pkg, declared, &ct.fields)
				structs[ct.Key] = ct
			}
		}
	}

	for _, ct := range structs {
		for _, f := range ct.fields {
			for p := range f.parts {
				ct.Parts[p] = true
			}
		}
	}

	// DropLocalを持つ型から、同じパッケージのフィールドの型を辿る。途中の型は
	// 同期の部品を持たなくてよい(role.Service → policyProvider → policyProviderRuntime)。
	// 辿って届いても、持ち主のDropLocalが捨てるとは限らないので、ゲートの表で
	// 人が分類する。
	owners := make([]string, 0, len(scan.DropLocal))
	for key := range scan.DropLocal {
		owners = append(owners, key)
	}
	sort.Strings(owners)
	for _, owner := range owners {
		queue := []string{owner}
		seen := map[string]bool{owner: true}
		for len(queue) > 0 {
			ct := structs[queue[0]]
			queue = queue[1:]
			if ct == nil {
				continue
			}
			for _, f := range ct.fields {
				for _, r := range f.refs {
					if seen[r] {
						continue
					}
					seen[r] = true
					queue = append(queue, r)
					if _, ok := scan.OwnedBy[r]; !ok && !scan.DropLocal[r] {
						scan.OwnedBy[r] = owner
					}
				}
			}
		}
	}

	for key, ct := range structs {
		hasMap := false
		for _, f := range ct.fields {
			hasMap = hasMap || f.isMap
		}
		if len(ct.Parts) == 0 && !scan.DropLocal[key] && (scan.OwnedBy[key] == "" || !hasMap) {
			continue
		}
		for _, f := range ct.fields {
			ct.Fingerprint = append(ct.Fingerprint, f.name)
		}
		sort.Strings(ct.Fingerprint)
		scan.Types[key] = ct
	}

	for _, pf := range files {
		imports := fileImports(pf.file)
		for _, decl := range pf.file.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.VAR {
				continue
			}
			for _, ct := range scanVars(gd, fset, imports, pf.pkg, declared, structs) {
				scan.Types[ct.Key] = ct
			}
		}
	}
	return scan, nil
}

// cacheScanFiles lists the tracked non-test .go files ScanCacheTypes reads,
// relative to root.
//
// ディスクではなくgit ls-filesで列挙する(CLAUDE.md Section 4)。addし忘れた
// ファイルが手元でだけ数えられ、CIで初めて落ちることを防ぐ。
func cacheScanFiles(root string) ([]string, error) {
	out, err := exec.Command("git", "-C", root, "ls-files", "--", "internal", "plugin").Output()
	if err != nil {
		return nil, fmt.Errorf("git ls-files: %w", err)
	}
	var paths []string
	for _, rel := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if !strings.HasSuffix(rel, ".go") || strings.HasSuffix(rel, "_test.go") || cacheScanExclusion(rel) != "" {
			continue
		}
		paths = append(paths, rel)
	}
	if len(paths) == 0 {
		return nil, fmt.Errorf("git ls-files listed no .go file under internal and plugin of %s", root)
	}
	return paths, nil
}

// cacheScanExclusion returns why ScanCacheTypes does not read rel: a key of
// CacheScanExcludedDirs, "testdata" (a file under a testdata directory) or
// "fixture" (a file under internal/entitycompat/<name>fixture), or "" when it
// reads rel. testdataとfixtureは検査の入力に置いた例で、本番のビルドに入らない。
func cacheScanExclusion(rel string) string {
	for dir := range CacheScanExcludedDirs {
		if strings.HasPrefix(rel, dir+"/") {
			return dir
		}
	}
	if slices.Contains(strings.Split(path.Dir(rel), "/"), "testdata") {
		return "testdata"
	}
	if sub, ok := strings.CutPrefix(rel, cacheScanFixtureDir); ok {
		if dir, _, ok := strings.Cut(sub, "/"); ok && strings.HasSuffix(dir, "fixture") {
			return "fixture"
		}
	}
	return ""
}

// fileImports maps the names a file refers to its imports by to their paths.
func fileImports(f *ast.File) map[string]string {
	out := map[string]string{}
	for _, imp := range f.Imports {
		p, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			continue
		}
		name := path.Base(p)
		if n, ok := importNames[p]; ok {
			name = n
		}
		if imp.Name != nil {
			name = imp.Name.Name
		}
		out[name] = p
	}
	return out
}

// collectFields appends the fields of st. 名前の無いstructのフィールドの中は、
// 外のフィールド名を前に付けて同じ型のフィールドとして数える。
func collectFields(st *ast.StructType, prefix string, imports map[string]string, pkg string, declared map[string]bool, out *[]scannedField) {
	for _, field := range st.Fields.List {
		var names []string
		for _, n := range field.Names {
			names = append(names, prefix+n.Name)
		}
		if len(names) == 0 {
			// 埋め込みは型の名前がフィールドの名前になる。
			names = []string{prefix + baseTypeName(field.Type)}
		}
		if inner, ok := unwrapType(field.Type).(*ast.StructType); ok && len(inner.Fields.List) > 0 {
			for _, n := range names {
				collectFields(inner, n+".", imports, pkg, declared, out)
			}
			continue
		}
		parts, refs := typeParts(field.Type, imports, pkg, declared)
		_, isMap := unwrapType(field.Type).(*ast.MapType)
		for _, n := range names {
			*out = append(*out, scannedField{name: n, isMap: isMap, parts: parts, refs: refs})
		}
	}
}

// typeParts returns the sync parts a type expression holds anywhere in it
// (pointers, slices, maps, type arguments and the fields of unnamed structs),
// and the named types of the package pkg it refers to.
func typeParts(e ast.Expr, imports map[string]string, pkg string, declared map[string]bool) (map[string]bool, []string) {
	parts := map[string]bool{}
	var refs []string
	ast.Inspect(e, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.SelectorExpr:
			id, ok := x.X.(*ast.Ident)
			if !ok {
				return true
			}
			p, ok := imports[id.Name]
			if !ok {
				return true
			}
			if p == "sync/atomic" {
				parts[SyncPartAtomic] = true
			} else if kind, ok := syncPartTypes[p][x.Sel.Name]; ok {
				parts[kind] = true
			}
			return false
		case *ast.Ident:
			if declared[pkg+"."+x.Name] {
				refs = append(refs, pkg+"."+x.Name)
			}
		}
		return true
	})
	return parts, refs
}

// scanVars returns the package-level vars of gd whose type holds sync parts.
//
// 型は宣言の型から引き、無ければ値の形(T{}・&T{}・new(T)・make(T))から引く。
// sync.OnceValueなどの呼び出しはsync.Onceとして数える。
// 関数の戻り値で作るvar(var c = newCache())は型が分からないので数えない。
// 同じvarのブロックの他の名前も指紋に入れる。パッケージのmutexと、それが守る
// mapを並べて書く形で、mapを足すと落ちるようにするため。
func scanVars(gd *ast.GenDecl, fset *token.FileSet, imports map[string]string, pkg string, declared map[string]bool, structs map[string]*CacheType) []*CacheType {
	var names []string
	for _, spec := range gd.Specs {
		for _, n := range spec.(*ast.ValueSpec).Names {
			names = append(names, n.Name)
		}
	}
	var out []*CacheType
	for _, spec := range gd.Specs {
		vs := spec.(*ast.ValueSpec)
		for i, n := range vs.Names {
			if n.Name == "_" {
				continue
			}
			typ := vs.Type
			once := false
			if typ == nil && i < len(vs.Values) {
				typ = valueType(vs.Values[i])
				once = isOnceCall(vs.Values[i], imports)
			}
			if typ == nil && !once {
				continue
			}
			parts, refs := map[string]bool{}, []string(nil)
			if typ != nil {
				parts, refs = typeParts(typ, imports, pkg, declared)
			}
			if once {
				parts[SyncPartOnce] = true
			}
			for _, r := range refs {
				if ct := structs[r]; ct != nil && len(ct.Parts) > 0 {
					parts[SyncPartScannedType] = true
				}
			}
			if len(parts) == 0 {
				continue
			}
			ct := &CacheType{
				Key:   pkg + "." + n.Name,
				Var:   true,
				Pos:   fmt.Sprintf("%s:%d", fset.Position(n.Pos()).Filename, fset.Position(n.Pos()).Line),
				Parts: parts,
			}
			if st, ok := unwrapType(typ).(*ast.StructType); ok {
				var fields []scannedField
				collectFields(st, "", imports, pkg, declared, &fields)
				for _, f := range fields {
					ct.Fingerprint = append(ct.Fingerprint, f.name)
				}
			}
			for _, other := range names {
				if other != n.Name && other != "_" {
					ct.Fingerprint = append(ct.Fingerprint, "var:"+other)
				}
			}
			sort.Strings(ct.Fingerprint)
			out = append(out, ct)
		}
	}
	return out
}

// onceFuncs are the functions of sync that wrap a function so it runs once and
// keep what it returned.
var onceFuncs = []string{"OnceFunc", "OnceValue", "OnceValues"}

// isOnceCall reports whether e is a call of sync.OnceFunc, sync.OnceValue or
// sync.OnceValues. 戻り値は関数の型で、値の形から型を引けないが、最初の呼び出しの
// 結果をプロセスの間ずっと持つので、sync.Onceと同じに数える。
func isOnceCall(e ast.Expr, imports map[string]string) bool {
	call, ok := e.(*ast.CallExpr)
	if !ok {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	id, ok := sel.X.(*ast.Ident)
	return ok && imports[id.Name] == "sync" && slices.Contains(onceFuncs, sel.Sel.Name)
}

// valueType returns the type of a value expression when its form tells it.
func valueType(e ast.Expr) ast.Expr {
	switch x := e.(type) {
	case *ast.CompositeLit:
		return x.Type
	case *ast.UnaryExpr:
		if x.Op == token.AND {
			return valueType(x.X)
		}
	case *ast.CallExpr:
		if id, ok := x.Fun.(*ast.Ident); ok && (id.Name == "new" || id.Name == "make") && len(x.Args) > 0 {
			return x.Args[0]
		}
	}
	return nil
}

// unwrapType strips pointers.
func unwrapType(e ast.Expr) ast.Expr {
	for {
		x, ok := e.(*ast.StarExpr)
		if !ok {
			return e
		}
		e = x.X
	}
}

// baseTypeName returns the name of a named type expression (a receiver or an
// embedded field), without pointers, packages and type arguments.
func baseTypeName(e ast.Expr) string {
	switch x := unwrapType(e).(type) {
	case *ast.Ident:
		return x.Name
	case *ast.SelectorExpr:
		return x.Sel.Name
	case *ast.IndexExpr:
		return baseTypeName(x.X)
	case *ast.IndexListExpr:
		return baseTypeName(x.X)
	}
	return ""
}
