package federation

import (
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/activitypub"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
)

// 著者がピン留めしている投稿が inbox に直接 Create で届くと、取り込みが
// created=false を返して通知とチャートのフックが飛ぶ (#2686)。
//
// featured 経路の in-flight guard は #2684 で入れた resolvingNotes
// (singleflight の鍵) しか見ておらず、これは resolveNoteOnce でしか書かれない。
// **inbox 直送は resolveNoteOnce を通らない**ので guard が空振りし、同じ note を
// もう一度 fetch して内側の ingest が先に行を作る。外側の Create が UNIQUE に
// 当たり、dedup 経路で created=false になる。
func TestIngestNote_PinnedNoteViaInboxKeepsCreated(t *testing.T) {
	const (
		actorURI = "https://remote.example/users/kate"
		featured = "https://remote.example/users/kate/collections/featured"
		noteURI  = "https://remote.example/notes/k1"
	)
	userRepo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, err := id.NewGenerator("aidx")
	require.NoError(t, err)

	fetcher := &countingFetcher{docs: map[string]string{
		actorURI: featuredActorDoc("remote.example", "kate", featured),
		featured: `{"@context":"https://www.w3.org/ns/activitystreams","id":"` + featured +
			`","type":"OrderedCollection","orderedItems":["` + noteURI + `"]}`,
		noteURI: plainNoteDoc(noteURI, actorURI),
	}}
	r := NewResolver(userRepo, noteRepo, urls, fetcher, idGen)
	// **pinningRepo を配線しないと updateFeatured が即 return する。**
	// 配線を忘れると featured 経路そのものが走らず、テストが素通りする。
	r.SetPinningRepo(testutil.NewMockUserNotePiningRepository(), idGen)

	// inbox 直送 (handleCreate 相当)。resolveNoteOnce を通らない経路。
	note, created, err := r.IngestNoteWithCreated([]byte(plainNoteDoc(noteURI, actorURI)), actorURI)
	require.NoError(t, err)
	require.NotNil(t, note)

	// **created=false になると通知とチャートのフックが飛ぶ。**
	assert.True(t, created, "inbox 直送の取り込みが created=false になっている (通知が落ちる)")
	assert.Equal(t, 0, fetcher.count(noteURI), "既に手元にある note を再取得しないこと")
	assert.Len(t, noteRepo.Notes, 1, "note を二重に作らないこと")
}

// countingFetcher serves fixtures and counts fetches per URI.
type countingFetcher struct {
	docs  map[string]string
	calls []string
}

func (f *countingFetcher) FetchObject(uri string) ([]byte, error) {
	f.calls = append(f.calls, uri)
	if b, ok := f.docs[uri]; ok {
		return []byte(b), nil
	}
	return nil, assert.AnError
}

func (f *countingFetcher) count(uri string) int {
	n := 0
	for _, c := range f.calls {
		if c == uri {
			n++
		}
	}
	return n
}

// ピン留めが**別名 URL** で載っていると、featured 側の in-flight 判定が空振りして
// 同じ note をもう一度取り込み、inbox 直送の created が false に落ちる (#2695)。
//
// #2686 で潰したのは取得 URI と document の id が一致する形だけ。featured が
// `/@user/x` を載せていて id が `/notes/x` のとき、チェーンに載っているのは id 側
// なので取得 URI では引けない。id は fetch しないと分からないので、判定は
// resolveNoteOnce の**取得後**にもう一度要る。
func TestIngestNote_PinnedNoteViaInboxKeepsCreated_AliasURI(t *testing.T) {
	const (
		actorURI = "https://remote.example/users/lena"
		featured = "https://remote.example/users/lena/collections/featured"
		noteURI  = "https://remote.example/notes/l1"
		aliasURI = "https://remote.example/@lena/l1"
	)
	userRepo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	pins := testutil.NewMockUserNotePiningRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, err := id.NewGenerator("aidx")
	require.NoError(t, err)

	fetcher := &countingFetcher{docs: map[string]string{
		actorURI: featuredActorDoc("remote.example", "lena", featured),
		featured: `{"@context":"https://www.w3.org/ns/activitystreams","id":"` + featured +
			`","type":"OrderedCollection","orderedItems":["` + aliasURI + `"]}`,
		// **別名で引いても document の id は canonical。** ここが両者を食い違わせる。
		aliasURI: plainNoteDoc(noteURI, actorURI),
	}}
	r := NewResolver(userRepo, noteRepo, urls, fetcher, idGen)
	// pinningRepo が無いと updateFeatured が即 return してテストが素通りする。
	r.SetPinningRepo(pins, idGen)

	note, created, err := r.IngestNoteWithCreated([]byte(plainNoteDoc(noteURI, actorURI)), actorURI)
	require.NoError(t, err)
	require.NotNil(t, note)

	assert.True(t, created,
		"別名 URL のピンで inbox 直送の取り込みが created=false になっている (通知が落ちる、#2695)")
	assert.Len(t, noteRepo.Notes, 1, "note を二重に作らないこと")
	// 別名を 1 度取得するのは避けられない (id はそれでしか分からない)。
	// 2 度目が出るなら判定が効いていない。
	assert.Equal(t, 1, fetcher.count(aliasURI), "別名の取得は 1 度だけ")
	assert.Equal(t, 0, fetcher.count(noteURI), "既に手元にある note を再取得しないこと")

	// **ピンの結末も固定する。** 正規形の対 (TestResolveNote_AuthorPinsTheNote-
	// BeingIngested) と同じく、この回は落ちて次の actor 更新で入る。ここが無いと
	// 回復が壊れても気付けない (#2710 review LOW-2)。
	pinned, err := pins.ListByUser(note.UserID)
	require.NoError(t, err)
	assert.Empty(t, pinned, "取り込み中だった自分のピンはこの回では入らない")

	require.NoError(t, userRepo.UpdateUser(note.UserID, map[string]any{"lastFetchedAt": (*time.Time)(nil)}))
	user, err := r.ResolveActor(actorURI)
	require.NoError(t, err)
	require.Equal(t, note.UserID, user.ID)
	pinned, err = pins.ListByUser(user.ID)
	require.NoError(t, err)
	require.Len(t, pinned, 1, "次回の actor 更新でピンが入ること")
	assert.Equal(t, note.ID, pinned[0].NoteID)
}

// newAliasEnv builds a resolver whose alias URI serves a document with a
// different (canonical) id.
func newAliasEnv(t *testing.T, actorURI, name, aliasURI, noteURI string) (*Resolver, *testutil.MockNoteRepository) {
	t.Helper()
	noteRepo := testutil.NewMockNoteRepository()
	idGen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	fetcher := &countingFetcher{docs: map[string]string{
		actorURI: featuredActorDoc("remote.example", name, ""),
		aliasURI: plainNoteDoc(noteURI, actorURI),
	}}
	r := NewResolver(testutil.NewMockUserRepository(), noteRepo,
		activitypub.NewURLBuilder("https://example.com"), fetcher, idGen)
	return r, noteRepo
}

// 取得後の判定は **featured の取り込みから入った呼び出しだけ**に効くこと (#2695)。
//
// featured のピンをこの回だけ落とすのは、次の actor 更新で拾い直せるから許せる。
// 同じことを引用経路でやると `renoteId` が nil のまま保存され、再取り込みは
// resolveNoteOnce も ingestNoteWithCreated も FindByURI で早期 return するので
// **恒久的に失われる** (#2684 review R2 HIGH-1 と同じ型)。
//
// 引用経路が本当に守られているかは、`resolveNoteOnce` を直接呼んでも分からない
// (入口の情報が引数にしか無い)。実経路での固定は
// TestIngestNote_PinnedNoteQuotingTheIngestedNoteKeepsRenoteID が行う。
func TestResolveNoteOnce_AncestorIngestingGateIsBestEffortOnly(t *testing.T) {
	const (
		actorURI = "https://remote.example/users/mira"
		aliasURI = "https://remote.example/@mira/m1"
		noteURI  = "https://remote.example/notes/m1"
	)
	// 祖先が noteURI を取り込み中のチェーン。値が入っているのが「祖先が載せた」印。
	ancestor := func() *resolveChain {
		return (&resolveChain{}).with(noteURI, noteURI).ensureTree()
	}

	// **best-effort な印を持つチェーンでも、入口が featured でなければ効かない。**
	// 印は枝ごと引き継がれるので、featured の内側で走る引用解決もこの形になる。
	t.Run("quote path ingests anyway", func(t *testing.T) {
		r, noteRepo := newAliasEnv(t, actorURI, "mira", aliasURI, noteURI)
		resolved, err := r.resolveNoteOnce(aliasURI, 0, false, false, false, ancestor().asBestEffort())
		require.NoError(t, err, "引用経路で諦めている (renoteId を恒久的に落とす)")
		require.NotNil(t, resolved)
		// **wrapper ではなく note を見る。** wrapper は err==nil なら必ず非 nil
		// なので、そこだけ見ると常に真になる (#2751 の型変更で空振り化した)。
		require.NotNil(t, resolved.note)
		assert.Len(t, noteRepo.Notes, 1)
	})

	t.Run("featured path gives up", func(t *testing.T) {
		r, noteRepo := newAliasEnv(t, actorURI, "mira", aliasURI, noteURI)
		note, err := r.resolveNoteOnce(aliasURI, 0, false, false, true, ancestor())
		require.ErrorIs(t, err, ErrNoteAncestorIngesting)
		assert.Nil(t, note)
		assert.Empty(t, noteRepo.Notes, "祖先の取り込みを横取りしないこと")
	})

	// 既に行があるなら諦めずにそれを返す。ReplaceByUser は delete-then-insert
	// なので、ここで落とすと**生きているピンが消える** (#2685 review MEDIUM-1)。
	t.Run("featured path returns an existing row", func(t *testing.T) {
		r, noteRepo := newAliasEnv(t, actorURI, "mira", aliasURI, noteURI)
		user, err := r.ResolveActor(actorURI)
		require.NoError(t, err)
		uri := noteURI
		require.NoError(t, noteRepo.Create(&model.Note{ID: "9mira0000000000000000", UserID: user.ID, URI: &uri}))

		resolved, err := r.resolveNoteOnce(aliasURI, 0, false, false, true, ancestor())
		require.NoError(t, err)
		require.NotNil(t, resolved)
		require.NotNil(t, resolved.note)
		assert.Equal(t, "9mira0000000000000000", resolved.note.ID)
		assert.False(t, resolved.created, "既存行を返した経路は created にしない (#2751)")
	})
}

// チェーン固有の判定を**相乗りしただけの追従側へ配らない**こと (#2695)。
//
// group は先頭の返り値を追従側にも渡す。best-effort な先頭が
// ErrNoteAncestorIngesting で降りたとき、それをそのまま配ると身に覚えのない
// 理由で別チェーンの解決が落ちる (引用経路なら renoteId を恒久的に失う)。
func TestJoinResolveOpt_JoinerRedoesAChainLocalGiveUp(t *testing.T) {
	r := &Resolver{}
	var g resolveGroup

	leaderIn := make(chan struct{})
	leaderGo := make(chan struct{})
	leaderChain := (&resolveChain{}).ensureTree().asBestEffort()
	go func() {
		_, _ = r.joinResolveOpt(&g, leaderChain, noteWaitKey("k"), "k", false, func() (any, error) {
			close(leaderIn)
			<-leaderGo
			return nil, ErrNoteAncestorIngesting
		})
	}()
	<-leaderIn

	joinerRan := 0
	joinerDone := make(chan struct{})
	var got any
	var gotErr error
	go func() {
		defer close(joinerDone)
		got, gotErr = r.joinResolveOpt(&g, (&resolveChain{}).ensureTree(), noteWaitKey("k"), "k", true,
			func() (any, error) {
				joinerRan++
				return "resolved", nil
			})
	}()
	// **sleep で待たない。** 追従側が 50ms 以内にスケジュールされないと先頭が先に
	// 返り、追従側は自分が先頭になるだけでやり直し経路を通らない。それでも全ての
	// assert は成立するので、**落ちる flaky ではなく黙って検査をやめる flaky**に
	// なる (#2710 review MEDIUM-3)。group に登録されたことを直接見る。
	// **期限を付ける。** 上限が無いと、登録に至らない壊れ方をしたときに
	// パッケージ全体の testing binary が timeout で panic し、他のテストの結果も
	// 巻き添えで失われる (#2710 review LOW-1)。
	deadline := time.After(5 * time.Second)
	for {
		g.mu.Lock()
		joined := g.m["k"] != nil && len(g.m["k"].waiters) == 1
		g.mu.Unlock()
		if joined {
			break
		}
		select {
		case <-deadline:
			t.Fatal("追従側が group に登録されない")
		default:
		}
		runtime.Gosched()
	}
	close(leaderGo)

	select {
	case <-joinerDone:
	case <-time.After(15 * time.Second):
		t.Fatal("追従側が返らない")
	}
	require.NoError(t, gotErr, "先頭のチェーン固有の判定を追従側が受け取っている (#2695)")
	assert.Equal(t, "resolved", got)
	assert.Equal(t, 1, joinerRan, "追従側は 1 度だけやり直すこと")
}

// featured のピンが「いま取り込んでいる投稿」を**別名 URL で引用**していても、
// その引用を落とさないこと (#2710 review HIGH-1)。
//
// best-effort の印は枝ごと引き継がれるので、featured の取り込みの**内側**で走る
// 引用解決も `chain.mayWait()==false` になる。取得後の判定を chain の印で行うと
// ここに効いてしまい、ピン P が `renoteId=nil` のまま保存される。再取り込みは
// FindByURI で早期 return するので**恒久的に失われる**。
//
// 入口が featured かどうかは resolveNoteDepthOpt の mayWait しか知らないので、
// 判定はそれを引数で受け取る。
//
// **別名 URL に限った不変条件。** 同じ形でもピンが取り込み中の投稿を**正規 URI で**
// 引用している場合は、`resolveQuoteURI` の in-flight 判定が当たって既存行を引けず、
// renoteId は nil のまま保存される (develop でも同じ既存挙動)。ここを一般則と
// 読まないこと (#2710 review MEDIUM-2)。
func TestIngestNote_PinnedNoteQuotingTheIngestedNoteUnderAnAliasKeepsRenoteID(t *testing.T) {
	const (
		actorURI = "https://remote.example/users/nova"
		featured = "https://remote.example/users/nova/collections/featured"
		noteN    = "https://remote.example/notes/n1"
		aliasN   = "https://remote.example/@nova/n1"
		noteP    = "https://remote.example/notes/p1"
	)
	noteRepo := testutil.NewMockNoteRepository()
	idGen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	fetcher := &countingFetcher{docs: map[string]string{
		actorURI: featuredActorDoc("remote.example", "nova", featured),
		featured: `{"@context":"https://www.w3.org/ns/activitystreams","id":"` + featured +
			`","type":"OrderedCollection","orderedItems":["` + noteP + `"]}`,
		noteP: quoteNoteDoc(noteP, actorURI, aliasN),
		// 引用先は N の別名。document の id は canonical なので、チェーンに載って
		// いる N の id と**取得後に**一致する。
		aliasN: plainNoteDoc(noteN, actorURI),
	}}
	r := NewResolver(testutil.NewMockUserRepository(), noteRepo,
		activitypub.NewURLBuilder("https://example.com"), fetcher, idGen)
	r.SetPinningRepo(testutil.NewMockUserNotePiningRepository(), idGen)

	_, _, err = r.IngestNoteWithCreated([]byte(plainNoteDoc(noteN, actorURI)), actorURI)
	require.NoError(t, err)

	pin, err := noteRepo.FindByURI(noteP)
	require.NoError(t, err)
	require.NotNil(t, pin, "ピンが取り込まれていない (前提が崩れている)")
	require.NotNil(t, pin.RenoteID,
		"ピンの引用先が落ちている: 取得後の判定が引用経路へ波及している (#2710 review HIGH-1)")
	assert.NotEmpty(t, *pin.RenoteID)
}

// noteInFlightInChain の**2 つ目の lookup** (document id 側) が効くこと。
//
// 既定の flag では `noteGroupKey(uri, false, false) == uri` なので 1 つ目と同じ鍵に
// なり、featured 経路 (`featured.go` の `(false, false)`) からは到達しない。
// **到達するのは ephemeral 経路**。祖先の載せ方が 2 通りある
// (#2710 review LOW-4 / round 3 MEDIUM-2 / round 4 LOW-6):
//
//   - `chainAfterProbe` が別名 URI から入った場合。`eph\x00<別名>` と
//     `<document id>` が載るので、引用解決が document id を引くと 1 つ目が外れる
//   - inbox 直送の ephemeral (`ingestNoteWithCreated`)。`with(id, id)` しか載せず
//     `eph\x00` 鍵を作らないので、内側の引用解決は**別名でなくても**常に外れる
//
// `allowCrossHost=true` で呼ぶ本番経路は無い (`noteInFlightInChain` の呼び出しは
// 2 箇所とも false 固定。ap/show の cross-host は根の 1 回だけで、入れ子の
// `resolveQuoteURI` は常に false)。写像としては引けるので契約だけ固定しておく。
func TestNoteInFlightInChain_FallsBackToTheDocumentID(t *testing.T) {
	const (
		alias = "https://remote.example/@ora/o1"
		docID = "https://remote.example/notes/o1"
	)
	r := &Resolver{}
	// ephemeral な祖先が別名から入ったあとのチェーン (chainAfterProbe と同じ形)。
	ephChain := chainAfterProbe((&resolveChain{}), alias, false, true, docID)
	// 非 ephemeral 版。こちらは 1 つ目の鍵が docID と一致するので 2 つ目は使わない。
	plainChain := chainAfterProbe((&resolveChain{}), alias, false, false, docID)

	cases := []struct {
		name           string
		chain          *resolveChain
		lookup         string
		allowCrossHost bool
		ephemeral      bool
	}{
		// 本番で 2 つ目の lookup に落ちる形。
		{"ephemeral chain, document id", ephChain, docID, false, true},
		// 本番には無いが写像としては引ける (cross-host 鍵は 1 つ目で必ず外れる)。
		{"cross-host key misses, uri hits", plainChain, alias, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, inflight := r.noteInFlightInChain(tc.chain, tc.lookup, tc.allowCrossHost, tc.ephemeral)
			require.True(t, inflight, "2 つ目の lookup が効いていない")
			assert.Equal(t, docID, got, "既存行を引くための id は document id")
		})
	}

	// 1 つ目の鍵で当たる形 (2 つ目に落ちない) も一応固定しておく。
	got, inflight := r.noteInFlightInChain(plainChain, docID, false, false)
	require.True(t, inflight)
	assert.Equal(t, docID, got)

	// 載っていない URI では当たらないこと (どちらの lookup も素通しにしない)。
	_, inflight = r.noteInFlightInChain(ephChain, "https://remote.example/notes/zz", false, true)
	assert.False(t, inflight)
}

// featured の item の document id が、祖先の**取得 URI** とたまたま一致するだけの
// 別 note でも、ピンを落とさないこと (#2710 review round 3 MEDIUM-1 の回帰)。
//
// `chainAfterProbe` はチェーンに 2 つ載せる: `取得 URI → document id` と
// `document id → document id`。前者は**値が非空**なので、「値が空でない」で祖先を
// 判定すると、この形が「祖先が取り込み中」に見えて誤爆する。既存行はまだ無い
// (祖先の Create は quote / author の解決より後) ので、そのままピンが落ちる。
//
// 述語の単体テストだけだと、そこを skip した瞬間に検出できなくなる。挙動側でも
// 固定しておく (#2710 review round 4 MEDIUM-3)。
func TestUpdateFeatured_PinWhoseIDEqualsAncestorFetchURI(t *testing.T) {
	const (
		actorURI  = "https://remote.example/users/zed"
		featured  = "https://remote.example/users/zed/collections/featured"
		outerURI  = "https://remote.example/@zed/x" // 祖先の取得 URI
		outerDoc  = "https://remote.example/notes/x"
		pinFetch  = "https://remote.example/pin/1"
		pinDocURI = outerURI // ピンの document id が祖先の取得 URI と一致する
	)
	noteRepo := testutil.NewMockNoteRepository()
	pins := testutil.NewMockUserNotePiningRepository()
	idGen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	fetcher := &countingFetcher{docs: map[string]string{
		actorURI: featuredActorDoc("remote.example", "zed", featured),
		featured: `{"@context":"https://www.w3.org/ns/activitystreams","id":"` + featured +
			`","type":"OrderedCollection","orderedItems":["` + pinFetch + `"]}`,
		outerURI: plainNoteDoc(outerDoc, actorURI),
		pinFetch: plainNoteDoc(pinDocURI, actorURI),
	}}
	r := NewResolver(testutil.NewMockUserRepository(), noteRepo,
		activitypub.NewURLBuilder("https://example.com"), fetcher, idGen)
	r.SetPinningRepo(pins, idGen)

	note, err := r.ResolveNote(outerURI)
	require.NoError(t, err)
	require.NotNil(t, note)

	pinned, err := pins.ListByUser(note.UserID)
	require.NoError(t, err)
	require.Len(t, pinned, 1,
		"祖先の取得 URI と id が一致するだけの別 note のピンが落ちている (#2710 review round 3 MEDIUM-1)")
	row, err := noteRepo.FindByID(pinned[0].NoteID)
	require.NoError(t, err)
	require.NotNil(t, row.URI)
	assert.Equal(t, pinDocURI, *row.URI)
}
