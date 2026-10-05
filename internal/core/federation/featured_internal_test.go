package federation

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/activitypub"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
)

// AP の `type` は文字列とその配列のどちらでも来る。配列で来たときに読めないと、
// Note が Note と判定されず取り込みが丸ごと空振りする。
func TestSingleAPType(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{name: "string", raw: `"Note"`, want: "Note"},
		{name: "array takes the first entry", raw: `["Note", "Article"]`, want: "Note"},
		{name: "empty array", raw: `[]`, want: ""},
		{name: "absent", raw: ``, want: ""},
		{name: "unexpected shape", raw: `{"foo": 1}`, want: ""},
		{name: "array of non-strings", raw: `[1, 2]`, want: ""},
		// **先頭だけを見る (upstream `getApType` と同じ)。** 走査方式にすると
		// `[42, "Note"]` が "Note" を返すので、head 方式との差はこの 2 件目で
		// 必ず表面化する (1 件目だけだと走査方式でも同じ結果になり判別
		// できない)。
		{name: "array with trailing non-string", raw: `["Note", 42]`, want: "Note"},
		{name: "array with leading non-string", raw: `[42, "Note"]`, want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, singleAPType(json.RawMessage(tt.raw)))
		})
	}
}

// コレクションの要素は URI 文字列か、埋め込まれたオブジェクトのどちらか。
func TestFeaturedItemRef(t *testing.T) {
	tests := []struct {
		name     string
		raw      string
		wantURI  string
		wantType string
	}{
		{
			name:    "bare URI has no type",
			raw:     `"https://remote.example/notes/1"`,
			wantURI: "https://remote.example/notes/1",
		},
		{
			name:     "inlined object",
			raw:      `{"id": "https://remote.example/notes/1", "type": "Note"}`,
			wantURI:  "https://remote.example/notes/1",
			wantType: "Note",
		},
		{
			name:    "object without type",
			raw:     `{"id": "https://remote.example/notes/1"}`,
			wantURI: "https://remote.example/notes/1",
		},
		{name: "number", raw: `42`},
		{name: "malformed", raw: `{`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			uri, apType := featuredItemRef(json.RawMessage(tt.raw))
			assert.Equal(t, tt.wantURI, uri)
			assert.Equal(t, tt.wantType, apType)
		})
	}
}

// in-flight guard に当たっても、既に取り込み済みの投稿は既存行で拾うこと
// (#2684 review HIGH-2 / MED-1)。
//
// **ReplaceByUser は delete-then-insert なので、ここで落とすと集合ごと
// 書き直して生きているピンが消える。** 台帳の区間は ingestNoteWithCreated
// 全体 (著者の取得・featured コレクションの取得・入れ子の note 取得を含む)
// なので**秒のオーダーで開いている**。singleflight 本体全体を覆っていた頃と
// 比べて外れたのは、この note 自身の HTTP fetch の分。
//
// **取得 URI と document id の両方で引くこと。** 行は document id で保存
// されるので、別名 URL では取得 URI だけでは必ず空振りする。窓が重なるのは
// まさにその別名のときなので、片方だけだと fallback が意味を持たない。
//
// 台帳を直接触るのは、この競合を fetch や DB の遅延で再現しようとすると
// 本質的に flaky になるため。見たいのは「鍵が載っている状態で既存行を
// 拾うか」だけなので、その状態を直接作る。
func TestResolveFeaturedNotes_UsesExistingRowWhenTheKeyIsInFlight(t *testing.T) {
	const actorURI = "https://remote.example/users/iris"
	cases := []struct {
		name     string
		listed   string // featured collection に載る URI (= 取得 URI)
		storedAs string // note 行の URI (= document id)
	}{
		{"same uri", "https://remote.example/notes/i1", "https://remote.example/notes/i1"},
		{"alias uri", "https://remote.example/@iris/i1", "https://remote.example/notes/i1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			userRepo := testutil.NewMockUserRepository()
			noteRepo := testutil.NewMockNoteRepository()
			urls := activitypub.NewURLBuilder("https://example.com")
			idGen, err := id.NewGenerator("aidx")
			require.NoError(t, err)
			// fetch してはいけない経路なので、fetcher は必ず失敗する。
			r := NewResolver(userRepo, noteRepo, urls, emptyDocFetcher{}, idGen)

			host := "remote.example"
			uri := actorURI
			user := &model.User{ID: "9irisuser00000000000", Username: "iris", Host: &host, URI: &uri}
			require.NoError(t, userRepo.Create(user))
			nURI := tc.storedAs
			require.NoError(t, noteRepo.Create(&model.Note{ID: "9irisnote00000000000", UserID: user.ID, URI: &nURI}))

			items := []json.RawMessage{json.RawMessage(`"` + tc.listed + `"`)}

			// 台帳に載っている状態でも既存行を拾うこと。**ここで skip すると
			// ReplaceByUser が集合ごと書き直して生きたピンを消す。**
			// resolveNoteOnce が fetch 後に鍵へ document id を紐づけた状態。
			chain := (&resolveChain{}).with(noteGroupKey(tc.listed, false, false), tc.storedAs)
			assert.Equal(t, []string{"9irisnote00000000000"}, r.resolveFeaturedNotes(user, items, chain),
				"in-flight でも既存行があれば拾うこと (skip すると生きたピンが消える)")
		})
	}
}

// emptyDocFetcher never serves a document, so any code path that tries to
// fetch fails visibly instead of silently succeeding.
type emptyDocFetcher struct{}

func (emptyDocFetcher) FetchObject(string) ([]byte, error) {
	return nil, errors.New("fetch must not happen in this test")
}

// fixtureDocFetcher serves canned documents by URI.
type fixtureDocFetcher struct{ docs map[string]string }

func (d fixtureDocFetcher) FetchObject(uri string) ([]byte, error) {
	if body, ok := d.docs[uri]; ok {
		return []byte(body), nil
	}
	return nil, errors.New("no fixture for " + uri)
}

func featuredActorDoc(host, name, featured string) string {
	base := "https://" + host + "/users/" + name
	line := ""
	if featured != "" {
		line = `"featured": "` + featured + `",`
	}
	return `{"@context":"https://www.w3.org/ns/activitystreams","id":"` + base +
		`","type":"Person","preferredUsername":"` + name + `","inbox":"` + base + `/inbox",` + line +
		`"publicKey":{"id":"` + base + `#main-key","owner":"` + base +
		`","publicKeyPem":"-----BEGIN PUBLIC KEY-----\nFAKE\n-----END PUBLIC KEY-----"}}`
}

func plainNoteDoc(noteID, authorURI string) string {
	return `{"@context":"https://www.w3.org/ns/activitystreams","id":"` + noteID +
		`","type":"Note","attributedTo":"` + authorURI +
		`","content":"<p>hi</p>","to":["https://www.w3.org/ns/activitystreams#Public"]}`
}

// inbox 直送の in-flight guard に当たっても、既に行があれば拾うこと (#2686)。
//
// **ReplaceByUser は delete-then-insert なので、ここで落とすと集合ごと
// 書き直して生きているピンが消える。** ingesting の窓では通常まだ行が無い
// (Store は Create より前) が、第三の経路が先に作る競合はありうる。消えるのは
// 黙って起きる不可逆な損失なので、singleflight 鍵側と同じく確実に潰す。
//
// 台帳を直接触るのは、この競合を fetch や DB の遅延で再現しようとすると
// 本質的に flaky になるため。
func TestResolveFeaturedNotes_UsesExistingRowWhenIngesting(t *testing.T) {
	const (
		actorURI = "https://remote.example/users/leo"
		noteURI  = "https://remote.example/notes/l1"
	)
	userRepo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	// fetch してはいけない経路なので、fetcher は必ず失敗する。
	r := NewResolver(userRepo, noteRepo, urls, emptyDocFetcher{}, idGen)

	host := "remote.example"
	uri := actorURI
	user := &model.User{ID: "9leouser000000000000", Username: "leo", Host: &host, URI: &uri}
	require.NoError(t, userRepo.Create(user))
	nURI := noteURI
	require.NoError(t, noteRepo.Create(&model.Note{ID: "9leonote000000000000", UserID: user.ID, URI: &nURI}))

	items := []json.RawMessage{json.RawMessage(`"` + noteURI + `"`)}

	// document id 側の鍵だけを持つチェーン (inbox 直送で立つ形)。
	chain := (&resolveChain{}).with(noteURI, "")
	assert.Equal(t, []string{"9leonote000000000000"}, r.resolveFeaturedNotes(user, items, chain),
		"ingest 中でも既存行があれば拾うこと (skip すると生きたピンが消える)")
}

// **別のチェーンが同じ note を解決中でも待たない。** featured の取り込みは
// best-effort なので、待つと actor の鍵を握ったまま件数分の上限を積み上げ、
// その待ちが循環に見えたときに本命の note の解決が代わりに弾かれる
// (#2685 review HIGH-2 / MEDIUM-2)。
//
// **落とす前に既存行を引く。** ReplaceByUser は delete-then-insert なので、
// ここで落とすと集合ごと書き直して生きているピンが消える
// (#2685 review MEDIUM-1)。
func TestResolveFeaturedNotes_DoesNotWaitForAnotherChain(t *testing.T) {
	const actorURI = "https://remote.example/users/wren"
	const noteURI = "https://remote.example/notes/w1"

	// 上限が効いてしまう回帰が入ったとき、5 分ハングせず落ちるように縮める。
	prev := resolveJoinTimeout
	resolveJoinTimeout = 3 * time.Second
	defer func() { resolveJoinTimeout = prev }()

	cases := []struct {
		name      string
		storeNote bool
		wantPins  []string
	}{
		{"既存行があれば拾う", true, []string{"9wrennote0000000000"}},
		{"既存行が無ければ落とす", false, []string{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			userRepo := testutil.NewMockUserRepository()
			noteRepo := testutil.NewMockNoteRepository()
			urls := activitypub.NewURLBuilder("https://example.com")
			idGen, err := id.NewGenerator("aidx")
			require.NoError(t, err)
			// 待たない経路なので fetch は起きてはいけない。
			r := NewResolver(userRepo, noteRepo, urls, emptyDocFetcher{}, idGen)

			host := "remote.example"
			uri := actorURI
			user := &model.User{ID: "9wrenuser0000000000", Username: "wren", Host: &host, URI: &uri}
			require.NoError(t, userRepo.Create(user))
			if tc.storeNote {
				nURI := noteURI
				require.NoError(t, noteRepo.Create(&model.Note{
					ID: "9wrennote0000000000", UserID: user.ID, URI: &nURI,
				}))
			}

			// 別のチェーンが note の鍵を握ったまま返らない状態を作る。
			key := noteGroupKey(noteURI, false, false)
			held := make(chan struct{})
			blocker := make(chan struct{})
			leaderDone := make(chan struct{})
			go func() {
				defer close(leaderDone)
				_, _ = r.joinResolve(&r.resolveNoteGroup, chainWithID(9999), noteWaitKey(key), key,
					func() (any, error) {
						close(held)
						<-blocker
						return nil, nil
					})
			}()
			<-held
			defer func() {
				close(blocker)
				<-leaderDone
			}()

			items := []json.RawMessage{json.RawMessage(`"` + noteURI + `"`)}
			done := make(chan []string, 1)
			go func() {
				done <- r.resolveFeaturedNotes(user, items, (&resolveChain{}).with("unrelated", "unrelated"))
			}()
			select {
			case got := <-done:
				assert.Equal(t, tc.wantPins, got)
			case <-time.After(2 * time.Second):
				t.Fatal("featured の解決が別チェーンの in-flight を待っている")
			}
		})
	}
}

// **待たないのは枝ごと。** 相乗りする瞬間だけ待たない形にすると、自分が先頭に
// なったときに内側 (取り込む投稿の著者 actor の解決) で待ってしまい、その間
// actor の鍵を握り続ける (#2685 review round 4)。
func TestResolveFeaturedNotes_DoesNotWaitInNestedResolves(t *testing.T) {
	const actorURI = "https://remote.example/users/f1"
	const noteURI = "https://remote.example/notes/f1n"

	prev := resolveJoinTimeout
	resolveJoinTimeout = 1200 * time.Millisecond
	defer func() { resolveJoinTimeout = prev }()

	userRepo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	r := NewResolver(userRepo, noteRepo, urls,
		fixtureDocFetcher{docs: map[string]string{noteURI: plainNoteDoc(noteURI, actorURI)}}, idGen)

	host := "remote.example"
	uri := actorURI
	user := &model.User{ID: "9f1user00000000000000", Username: "f1", Host: &host, URI: &uri}
	require.NoError(t, userRepo.Create(user))

	// 著者 actor の鍵 (featured の内側なので skipFeatured=true) を別チェーンが
	// 握ったまま返らない状態にする。
	akey := actorGroupKey(actorURI, false, true)
	held := make(chan struct{})
	blocker := make(chan struct{})
	leaderDone := make(chan struct{})
	go func() {
		defer close(leaderDone)
		_, _ = r.joinResolve(&r.resolveActorGroup, chainWithID(7777), actorWaitKey(akey), akey,
			func() (any, error) {
				close(held)
				<-blocker
				return nil, nil
			})
	}()
	<-held
	defer func() { close(blocker); <-leaderDone }()

	items := []json.RawMessage{json.RawMessage(`"` + noteURI + `"`)}
	done := make(chan time.Duration, 1)
	go func() {
		start := time.Now()
		_ = r.resolveFeaturedNotes(user, items, (&resolveChain{}).with("unrelated", "unrelated"))
		done <- time.Since(start)
	}()
	select {
	case elapsed := <-done:
		assert.Less(t, elapsed, 500*time.Millisecond,
			"featured の取り込みが内側の actor 解決で待っている (実測 %s)", elapsed)
	case <-time.After(5 * time.Second):
		t.Fatal("featured の取り込みが内側の actor 解決でブロックしている")
	}
}
