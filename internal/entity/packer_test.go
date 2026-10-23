package entity

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"

	"github.com/elythia-network/elythia/internal/model"
)

// isolateDefaultPacker clears the default Packer for the test and restores it
// afterwards.
func isolateDefaultPacker(t *testing.T) {
	t.Helper()
	prev := defaultPacker.Load()
	defaultPacker.Store(nil)
	t.Cleanup(func() { defaultPacker.Store(prev) })
}

func packerTestUser() *model.User {
	return &model.User{ID: "u1", Username: "alice", Host: sp(remoteHost), AvatarURL: sp("https://" + remoteHost + "/a.png")}
}

// **明示的に渡した Packer は、既定の Packer ではなく自分の依存を使う (#3788)。**
//
// (5-6) 以降で呼び出し側へ Packer を渡したとき、既定の Packer が空でも、渡した
// 側の配線が効かなければならない。逆に既定の Packer の配線が漏れ込むと、
// 世代ごとに組み立てた依存が混ざる。
func TestPacker_UsesItsOwnDependencies(t *testing.T) {
	isolateDefaultPacker(t)
	p := NewPacker(PackerDeps{
		CanChat:  &stubCanChatLookup{results: map[string]bool{"u1": false}},
		Silenced: silencedByID{"u1": true},
		MediaURL: internalCtx(),
	})

	got := p.PackUserDetailed(packerTestUser(), nil)
	if got.CanChat {
		t.Error("canChat must come from the Packer's lookup")
	}
	if !got.IsSilenced {
		t.Error("isSilenced must come from the Packer's lookup")
	}
	if !strings.HasPrefix(got.AvatarURL, testInternalProxy+"/avatar.webp?") {
		t.Errorf("avatarUrl must be proxied by the Packer's media context: %s", got.AvatarURL)
	}

	// 既定の Packer は空のままなので、パッケージ関数は未配線の値を返す。
	plain := PackUserDetailed(packerTestUser(), nil)
	if !plain.CanChat || plain.IsSilenced || plain.AvatarURL != "https://"+remoteHost+"/a.png" {
		t.Errorf("the default Packer must not see the explicit Packer's deps: %+v", plain)
	}
}

// パッケージ関数は Set* で配線した既定の Packer へ委ね、同じ依存の Packer と
// 同じ結果を返す。
func TestPacker_PackageFuncsDelegateToTheDefault(t *testing.T) {
	isolateDefaultPacker(t)
	canChat := &stubCanChatLookup{results: map[string]bool{"u1": false}}
	SetCanChatLookup(canChat)
	SetMediaURLContext(internalCtx())

	viaFunc := PackUserDetailed(packerTestUser(), nil)
	viaPacker := NewPacker(PackerDeps{CanChat: canChat, MediaURL: internalCtx()}).PackUserDetailed(packerTestUser(), nil)
	if viaFunc.CanChat != viaPacker.CanChat || viaFunc.AvatarURL != viaPacker.AvatarURL {
		t.Errorf("package func and Packer differ: %+v vs %+v", viaFunc, viaPacker)
	}
	if viaFunc.CanChat || !strings.HasPrefix(viaFunc.AvatarURL, testInternalProxy) {
		t.Errorf("package func must use the wired default Packer: %+v", viaFunc)
	}
}

// Set* は既定の Packer の写しを作って 1 つの依存だけを差し替え、他の依存を残す。
func TestSetters_KeepTheOtherDependencies(t *testing.T) {
	isolateDefaultPacker(t)
	SetCanChatLookup(&stubCanChatLookup{results: map[string]bool{"u1": false}})
	SetSilencedLookup(silencedByID{"u1": true})
	SetMediaURLContext(internalCtx())

	got := PackUserDetailed(packerTestUser(), nil)
	if got.CanChat || !got.IsSilenced || !strings.HasPrefix(got.AvatarURL, testInternalProxy) {
		t.Errorf("a later Set* dropped an earlier dependency: %+v", got)
	}

	SetCanChatLookup(nil)
	got = PackUserDetailed(packerTestUser(), nil)
	if !got.CanChat || !got.IsSilenced {
		t.Errorf("Set*(nil) must clear only its own dependency: %+v", got)
	}
}

// nil の *Packer は、すべての依存が未配線のときと同じに動く (落ちない)。
func TestPacker_NilBehavesAsUnwired(t *testing.T) {
	var p *Packer
	got := p.PackUserDetailed(packerTestUser(), nil)
	if !got.CanChat || got.IsSilenced || got.AvatarURL != "https://"+remoteHost+"/a.png" {
		t.Errorf("nil Packer must behave as unwired: %+v", got)
	}
	if lite := p.PackUserLite(packerTestUser()); lite.AvatarURL != "https://"+remoteHost+"/a.png" {
		t.Errorf("nil Packer must leave the avatar raw: %s", lite.AvatarURL)
	}
}

type silencedByID map[string]bool

func (s silencedByID) IsSilenced(userID string) bool { return s[userID] }

// **既定の Packer を読むのは、委ねる薄い関数と packer.go だけ (#3788)。**
//
// Packer のメソッドの中で currentPacker() を呼ぶか、委ねる薄い関数 (修飾子の
// 無い PackUserLite(...) など) を呼ぶと、明示的に渡した Packer の依存ではなく
// 既定の Packer の依存を使ってしまう。テストは代表の経路しか見ないので、呼び出しの
// 場所でまとめて縛る。
//
// ファイルはディスクから列挙する (git ls-files ではない)。縛りたいのはこの
// パッケージとしてコンパイルされるファイルで、それはディスク上のファイルだから。
func TestCurrentPackerIsOnlyReadByTheDelegatingFuncs(t *testing.T) {
	wrappers := map[string]bool{}
	wf, err := parser.ParseFile(token.NewFileSet(), "packer_default_funcs.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range wf.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Recv == nil {
			wrappers[fd.Name.Name] = true
		}
	}
	// 抽出が空振りしていないこと (薄い関数は 27 個)。
	if len(wrappers) < 27 {
		t.Fatalf("found only %d delegating funcs; the scan is broken", len(wrappers))
	}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		af, err := parser.ParseFile(token.NewFileSet(), f, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(af, func(n ast.Node) bool {
			ce, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			id, ok := ce.Fun.(*ast.Ident)
			if !ok {
				return true
			}
			switch {
			case id.Name == "currentPacker":
				seen++
				if f != "packer.go" && f != "packer_default_funcs.go" {
					t.Errorf("%s calls currentPacker(); a Packer method must use its receiver", f)
				}
			case wrappers[id.Name] && f != "packer_default_funcs.go":
				t.Errorf("%s calls the delegating func %s; call the method on the Packer instead", f, id.Name)
			}
			return true
		})
	}
	if seen < 27 {
		t.Errorf("found only %d currentPacker() calls; the scan is broken", seen)
	}
}
