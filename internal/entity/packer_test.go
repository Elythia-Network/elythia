package entity

import (
	"encoding/json"
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
	p := NewPacker(internalCtx(), PackerDeps{
		CanChat:  &stubCanChatLookup{results: map[string]bool{"u1": false}},
		Silenced: silencedByID{"u1": true},
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
	plain := currentPacker().PackUserDetailed(packerTestUser(), nil)
	if !plain.CanChat || plain.IsSilenced || plain.AvatarURL != "https://"+remoteHost+"/a.png" {
		t.Errorf("the default Packer must not see the explicit Packer's deps: %+v", plain)
	}
}

// Set* は既定の Packer を書き換え、同じ依存で作った Packer と同じ結果になる。
// 委ねる薄い関数は #3798 で無くなったので、既定の Packer を直接読んで比べる。
func TestSetters_WriteTheDefaultPacker(t *testing.T) {
	isolateDefaultPacker(t)
	SetMediaURLContext(internalCtx())

	raw := "https://" + remoteHost + "/a.png"
	viaDefault := currentPacker().ProxyMediaURL(raw)
	viaPacker := NewPacker(internalCtx(), PackerDeps{}).ProxyMediaURL(raw)
	if viaDefault != viaPacker {
		t.Errorf("default Packer and Packer differ: %q vs %q", viaDefault, viaPacker)
	}
	if !strings.HasPrefix(viaDefault, testInternalProxy) {
		t.Errorf("Set* must write the default Packer: %q", viaDefault)
	}
}

// Set* は既定の Packer の写しを作って 1 つの依存だけを差し替え、他の依存を残す。
func TestSetters_KeepTheOtherDependencies(t *testing.T) {
	isolateDefaultPacker(t)
	SetCanChatLookup(&stubCanChatLookup{results: map[string]bool{"u1": false}})
	SetSilencedLookup(silencedByID{"u1": true})
	SetMediaURLContext(internalCtx())

	got := currentPacker().PackUserDetailed(packerTestUser(), nil)
	if got.CanChat || !got.IsSilenced || !strings.HasPrefix(got.AvatarURL, testInternalProxy) {
		t.Errorf("a later Set* dropped an earlier dependency: %+v", got)
	}

	SetCanChatLookup(nil)
	got = currentPacker().PackUserDetailed(packerTestUser(), nil)
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

// **NoteFieldResolver は、渡された Packer で添付ファイルを包む (#3790)。**
//
// 以前は既定の Packer を読んでいたので、明示した Packer で投稿を包んでも、
// 埋め込んだ投稿のファイルの URL だけ既定の Packer の mediaURL になった。既定の
// Packer を空にして、渡した Packer の proxy が本体・renote・reply のすべての
// ファイルに付くことを、深い経路と浅い経路の両方で確かめる。
func TestNoteFieldResolver_PacksFilesWithItsPacker(t *testing.T) {
	isolateDefaultPacker(t)
	remoteFile := func(id string) *model.DriveFile {
		return &model.DriveFile{
			ID: id, Type: "image/png", URL: "https://" + remoteHost + "/" + id + ".png",
			URI: sp("https://" + remoteHost + "/" + id + ".png"), UserHost: sp(remoteHost), IsLink: true,
		}
	}
	files := &stubDriveFileLookup{files: map[string]*model.DriveFile{
		"f1": remoteFile("f1"), "f2": remoteFile("f2"), "f3": remoteFile("f3"),
	}}
	pk := NewPacker(internalCtx(), PackerDeps{})
	r := NewNoteFieldResolver(pk, files, nil, nil, nil, nil, makeIDGen(t))
	notes := func() []NoteEntity {
		return []NoteEntity{{
			ID:      "n1",
			FileIDs: model.StringArray{"f1"},
			Renote:  &NoteEntity{ID: "n2", FileIDs: model.StringArray{"f2"}},
			Reply:   &NoteEntity{ID: "n3", FileIDs: model.StringArray{"f3"}},
		}}
	}
	check := func(t *testing.T, n []NoteEntity) {
		t.Helper()
		for _, e := range []*NoteEntity{&n[0], n[0].Renote, n[0].Reply} {
			if len(e.Files) != 1 {
				t.Fatalf("%s: files = %d", e.ID, len(e.Files))
			}
			b, err := json.Marshal(e.Files[0])
			if err != nil {
				t.Fatal(err)
			}
			var got struct {
				URL string `json:"url"`
			}
			if err := json.Unmarshal(b, &got); err != nil {
				t.Fatal(err)
			}
			if !strings.HasPrefix(got.URL, testInternalProxy) {
				t.Errorf("%s: the file url must go through the resolver's Packer: %s", e.ID, got.URL)
			}
		}
	}
	t.Run("deep", func(t *testing.T) {
		n := notes()
		r.ResolveFiles(n)
		check(t, n)
	})
	t.Run("shallow", func(t *testing.T) {
		n := notes()
		r.ResolveFilesShallow(n)
		check(t, n)
	})
}

// HasMediaURL は mediaURL を持つ Packer だけ true を返す。Packer を構築子で
// 受ける部品 (webpush の 2 つと userpack) の HasEntityPacker がこれを使い、
// criticalWiring が入れ忘れを見る。
func TestPacker_HasMediaURL(t *testing.T) {
	var nilPacker *Packer
	if nilPacker.HasMediaURL() || NewPacker(nil, PackerDeps{}).HasMediaURL() {
		t.Error("a Packer without mediaURL must report false")
	}
	if !NewPacker(internalCtx(), PackerDeps{}).HasMediaURL() {
		t.Error("a Packer with mediaURL must report true")
	}
}

// **NewPacker は第 1 引数の mediaURL をそのまま持つ (#3834)。**
//
// mediaURL は PackerDeps の欄から構築子の必須の引数へ移した。構築子が引数を
// 捨てたり別の値を持ったりすると、渡した側の配線が効かずにメディアの URL が生の
// まま出る (閲覧者の IP がリモートへ漏れる側)。同じ値を持つことを見る。
func TestNewPacker_KeepsMediaURL(t *testing.T) {
	ctx := internalCtx()
	silenced := silencedByID{"u1": true}
	p := NewPacker(ctx, PackerDeps{Silenced: silenced})
	if p.mediaURL != ctx {
		t.Errorf("NewPacker must keep the mediaURL it was given: got %p, want %p", p.mediaURL, ctx)
	}
	if p.silenced == nil {
		t.Error("NewPacker must keep the PackerDeps fields alongside mediaURL")
	}
	if NewPacker(nil, PackerDeps{}).mediaURL != nil {
		t.Error("NewPacker(nil, ...) must leave mediaURL nil")
	}
}

type silencedByID map[string]bool

func (s silencedByID) IsSilenced(userID string) bool { return s[userID] }

// **本番のコードは既定の Packer を持たず、読まない (#3800)。**
//
// 既定の Packer と Set* は、このパッケージのテスト用の補助 (default_packer_test.go)
// にだけある。本番のファイルが currentPacker() を呼ぶか定義すると、組み立てで
// 渡した Packer ではなくプロセス共有の状態を読む形に戻る。テストは代表の経路しか
// 見ないので、呼び出しと定義の場所でまとめて縛る。
//
// ファイルはディスクから列挙する (git ls-files ではない)。縛りたいのはこの
// パッケージとしてコンパイルされるファイルで、それはディスク上のファイルだから。
func TestProductionDoesNotUseTheDefaultPacker(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	helperDefined := false
	for _, f := range files {
		af, err := parser.ParseFile(token.NewFileSet(), f, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		isTest := strings.HasSuffix(f, "_test.go")
		ast.Inspect(af, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.FuncDecl:
				if n.Recv == nil && n.Name.Name == "currentPacker" {
					if isTest {
						helperDefined = true
					} else {
						t.Errorf("%s defines currentPacker; the default Packer is test-only", f)
					}
				}
			case *ast.CallExpr:
				if id, ok := n.Fun.(*ast.Ident); ok && id.Name == "currentPacker" && !isTest {
					t.Errorf("%s calls currentPacker(); take the Packer the assembly passes instead", f)
				}
			}
			return true
		})
	}
	// 走査が空振りしていないこと (テスト用の補助の定義は見つかるはず)。
	if !helperDefined {
		t.Error("currentPacker is not defined in any _test.go file; the scan is broken")
	}
}
