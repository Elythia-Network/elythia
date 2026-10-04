package apierr_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// TestInvalidParamIDLint fails when a non-test file under internal/api
// builds an INVALID_PARAM error (apierr.Error / apierr.ErrorWithKind with
// the code "INVALID_PARAM") whose id is neither upstream's ajv id
// (UUIDInvalidParam, 3d81ceae) nor its cast id (UUIDInvalidParamCast,
// 0b5f1631).
//
// 本家の INVALID_PARAM は endpoint-base.ts (ajv) と ApiCallService.ts (型の
// 変換) が投げる 2 つの id しか無い。handler が独自の id を発番していた
// (auth/session/show などの ed1d7571-…、#3330) ため、id で分岐するクライアントが
// 本家と違う扱いになっていた。
//
// allowed は本家に対応する endpoint が無い mk-go 独自の endpoint と、本家の
// endpoint が自分の meta.errors で INVALID_PARAM を定義している箇所だけ。
// 値は「その id がどの経路で出るか」と件数。
func TestInvalidParamIDLint(t *testing.T) {
	allowed := map[string]struct {
		count int
		route string
	}{
		// 本家の users/lists/list.ts の meta.errors.noSuchUser が
		// code INVALID_PARAM / id ab36de0e-… を持つ (本家の定義そのもの)。
		"userlists/handler.go": {1, "users/lists/list で userId の利用者がいないとき"},
		// 以下は mk-go 独自の endpoint (本家に対応物が無い)。
		"admin/federation_rules.go": {6, "admin/federation/rules/* の ruleId 欠落・規則の検証失敗"},
		"admin/gone_instance.go":    {1, "admin/federation/clean-gone-instance の host 欠落"},
		"admin/remote_check.go":     {1, "admin/federation/check-host の host 欠落"},
		"bubblegame/versus.go":      {1, "bubble-game/versus/* の引数の検証失敗"},
		// 本家の reset-password は存在しない・期限切れの token を素の Error
		// (500 INTERNAL_ERROR) で落とす。mk-go は 400 で返しており、ajv の
		// 失敗ではないので独自の id のまま残す (docs/divergence.md)。
		"resetpassword/handler.go": {3, "reset-password の token が無い・期限切れ"},
	}

	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	ok := map[string]bool{
		"3d81ceae-475f-4600-b2a8-2bc116157532": true,
		"0b5f1631-7c1a-41a6-b399-cce335f34d85": true,
		"UUIDInvalidParam":                     true,
		"UUIDInvalidParamCast":                 true,
	}
	// 抽出が空振りしていないことの下限。#3330 で本家の id に直した site と、
	// 本家の id を組む apierr の helper を名指しで要求する。ここに載る関数が
	// 本家の id の INVALID_PARAM を組まなくなる (helper の改名・抽出の破損) と
	// 落ちる。
	required := []string{
		"apierr/errors.go:InvalidParam",
		"apierr/errors.go:InvalidParamCast",
		"auth/handler.go:SessionShow",
		"i/handler_2fa.go:TwoFADone",
		"i/handler_extra.go:ClaimAchievement",
		"bubblegame/handler.go:Ranking",
		"chat/handler.go:RoomsCreate",
		"fetchexternal/handler.go:Fetch",
		"admin/emoji.go:EmojiImportZip",
	}

	// CLAUDE.md Section 4: ディスクではなく git ls-files で列挙する。
	cmd := exec.Command("git", "ls-files", "--", "*.go")
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git ls-files: %v", err)
	}
	violations := map[string][]string{}
	found := map[string]bool{}
	fset := token.NewFileSet()
	for _, rel := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if rel == "" || strings.HasSuffix(rel, "_test.go") {
			continue
		}
		f, perr := parser.ParseFile(fset, filepath.Join(root, rel), nil, 0)
		if perr != nil {
			t.Fatal(perr)
		}
		rel = filepath.ToSlash(rel)
		inApierr := f.Name.Name == "apierr"
		// 関数の外 (パッケージ変数) で組むものも違反として拾う。
		ast.Inspect(f, func(n ast.Node) bool {
			call, isCall := n.(*ast.CallExpr)
			if !isCall || !isInvalidParamCtor(call, inApierr) {
				return true
			}
			if id := idText(call.Args[2]); !ok[id] {
				violations[rel] = append(violations[rel], rel+":"+strconv.Itoa(fset.Position(call.Pos()).Line)+" id="+id)
			}
			return true
		})
		for _, decl := range f.Decls {
			fn, isFunc := decl.(*ast.FuncDecl)
			if !isFunc || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, isCall := n.(*ast.CallExpr)
				if !isCall {
					return true
				}
				if isInvalidParamCtor(call, inApierr) && ok[idText(call.Args[2])] || isInvalidParamHelper(call.Fun, inApierr) {
					found[rel+":"+fn.Name.Name] = true
				}
				return true
			})
		}
	}
	for _, site := range required {
		if !found[site] {
			t.Errorf("%s no longer builds an INVALID_PARAM with upstream's id (or the extraction is broken)", site)
		}
	}

	var unexpected []string
	for path, sites := range violations {
		a, isAllowed := allowed[path]
		if !isAllowed || len(sites) != a.count {
			unexpected = append(unexpected, path+" ("+strconv.Itoa(len(sites))+"):\n      "+strings.Join(sites, "\n      "))
		}
	}
	sort.Strings(unexpected)
	if len(unexpected) > 0 {
		t.Errorf("INVALID_PARAM with an id other than upstream's 3d81ceae / 0b5f1631:\n  - %s\n\nUse apierr.JSONInvalidParam / InvalidParamClient (ajv) or InvalidParamCast.",
			strings.Join(unexpected, "\n  - "))
	}
	stale := make([]string, 0)
	for path := range allowed {
		if _, found := violations[path]; !found {
			stale = append(stale, path)
		}
	}
	sort.Strings(stale)
	for _, path := range stale {
		t.Errorf("allowed entry %q has no custom INVALID_PARAM id anymore; remove it", path)
	}
}

// isInvalidParamCtor reports whether call is apierr.Error /
// apierr.ErrorWithKind (or the bare names inside package apierr) with the
// code "INVALID_PARAM".
func isInvalidParamCtor(call *ast.CallExpr, inApierr bool) bool {
	if len(call.Args) < 3 || !isAPIErrFunc(call.Fun, inApierr, "Error", "ErrorWithKind") {
		return false
	}
	lit, isLit := call.Args[0].(*ast.BasicLit)
	return isLit && lit.Value == `"INVALID_PARAM"`
}

// isInvalidParamHelper reports whether fun is one of the apierr helpers
// that build INVALID_PARAM with upstream's ids.
func isInvalidParamHelper(fun ast.Expr, inApierr bool) bool {
	return isAPIErrFunc(fun, inApierr, "InvalidParam", "InvalidParamClient", "InvalidParamCast", "JSONInvalidParam", "JSONInvalidParamClient")
}

// isAPIErrFunc reports whether fun names one of names in package apierr
// (apierr.X from outside, the bare X inside).
func isAPIErrFunc(fun ast.Expr, inApierr bool, names ...string) bool {
	var name string
	switch f := fun.(type) {
	case *ast.SelectorExpr:
		pkg, isIdent := f.X.(*ast.Ident)
		if !isIdent || pkg.Name != "apierr" {
			return false
		}
		name = f.Sel.Name
	case *ast.Ident:
		if !inApierr {
			return false
		}
		name = f.Name
	default:
		return false
	}
	for _, n := range names {
		if name == n {
			return true
		}
	}
	return false
}

// idText renders the id argument: the unquoted literal, or the constant's
// name for an identifier / selector.
func idText(e ast.Expr) string {
	switch v := e.(type) {
	case *ast.BasicLit:
		s, err := strconv.Unquote(v.Value)
		if err != nil {
			return v.Value
		}
		return s
	case *ast.Ident:
		return v.Name
	case *ast.SelectorExpr:
		return v.Sel.Name
	}
	return "<expr>"
}

// TestRetiredCustomIDIsGone fails when a non-test file under internal/api
// still carries ed1d7571-a3ac-4370-899c-0dbe5e230cc8 as a string literal, in
// any response shape (apierr.Error or a bare {error: {id}} body).
//
// この id は mk-go が発番した独自のもので、本家のどの応答にも無い。
// TestInvalidParamIDLint は apierr.Error / ErrorWithKind で組む INVALID_PARAM
// しか見ないので、signin の errBody のような `{error: {id}}` だけの本文で返して
// いた箇所 (signin-flow / signin-with-passkey、#3330) は拾えなかった。
func TestRetiredCustomIDIsGone(t *testing.T) {
	const retired = "ed1d7571-a3ac-4370-899c-0dbe5e230cc8"
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	// CLAUDE.md Section 4: ディスクではなく git ls-files で列挙する。
	cmd := exec.Command("git", "ls-files", "--", "*.go")
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git ls-files: %v", err)
	}
	scanned := map[string]bool{}
	var hits []string
	fset := token.NewFileSet()
	for _, rel := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if rel == "" || strings.HasSuffix(rel, "_test.go") {
			continue
		}
		f, perr := parser.ParseFile(fset, filepath.Join(root, rel), nil, 0)
		if perr != nil {
			t.Fatal(perr)
		}
		rel = filepath.ToSlash(rel)
		scanned[rel] = true
		ast.Inspect(f, func(n ast.Node) bool {
			lit, isLit := n.(*ast.BasicLit)
			if !isLit || lit.Kind != token.STRING {
				return true
			}
			if s, uerr := strconv.Unquote(lit.Value); uerr == nil && strings.Contains(s, retired) {
				hits = append(hits, rel+":"+strconv.Itoa(fset.Position(lit.Pos()).Line))
			}
			return true
		})
	}
	// 抽出が空振りしていないことの下限。#3330 で id を外した file を名指しで
	// 要求する。
	for _, want := range []string{"signin/handler.go", "signin/passkey.go", "auth/handler.go"} {
		if !scanned[want] {
			t.Errorf("%s was not scanned (the extraction is broken)", want)
		}
	}
	sort.Strings(hits)
	if len(hits) > 0 {
		t.Errorf("mk-go's own id %s is still returned (upstream has no such id):\n  - %s", retired, strings.Join(hits, "\n  - "))
	}
}
