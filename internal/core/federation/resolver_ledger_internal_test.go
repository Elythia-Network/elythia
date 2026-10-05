package federation

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/activitypub"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
)

// gatedDocFetcher blocks the first fetch of gatedURI until release is closed,
// so a test can hold one goroutine inside the HTTP window on purpose.
type gatedDocFetcher struct {
	docs     map[string]string
	gatedURI string
	reached  chan struct{}
	release  chan struct{}
	once     sync.Once
}

func (f *gatedDocFetcher) FetchObject(uri string) ([]byte, error) {
	if uri == f.gatedURI {
		f.once.Do(func() { close(f.reached) })
		<-f.release
	}
	if b, ok := f.docs[uri]; ok {
		return []byte(b), nil
	}
	return nil, errors.New("no fixture for " + uri)
}

// quoteNoteDoc builds a Note document quoting another URI.
func quoteNoteDoc(noteID, authorURI, quoted string) string {
	return `{"@context":"https://www.w3.org/ns/activitystreams","id":"` + noteID +
		`","type":"Note","attributedTo":"` + authorURI +
		`","_misskey_quote":"` + quoted +
		`","content":"<p>quoting</p>","to":["https://www.w3.org/ns/activitystreams#Public"]}`
}

// in-flight 台帳を載せる区間が ingest だけに絞られていること (#2684)。
//
// **区間を singleflight 本体全体に広げてはいけない。** 広げると、別 worker が
// 同じ引用先を解決している最中 (= leader が HTTP fetch で止まっている間) に
// 引用元を取り込んだとき、待てば引けるものを待たずに諦めて
// **renoteId を落としたまま保存する**。しかも resolveNoteOnce も
// ingestNoteWithCreated も FindByURI で早期 return するので、その note が
// 取り込み直されることは無く**恒久的に失われる** (#2684 review R2 HIGH-1)。
//
// 判定は「待たずに返ったかどうか」で行う。leader を解放してから結果だけを
// 見ると、G2 が guard に到達する前に解放が走った場合に**変異を見逃す**
// (解放後は普通に解決できてしまうため)。
func TestResolveNoteOnce_LedgerDoesNotCoverTheFetchWindow(t *testing.T) {
	const (
		actorURI = "https://remote.example/users/olive"
		target   = "https://remote.example/notes/o1"
		quoting  = "https://remote.example/notes/o2"
	)
	fetcher := &gatedDocFetcher{
		docs: map[string]string{
			actorURI: featuredActorDoc("remote.example", "olive", ""),
			target:   plainNoteDoc(target, actorURI),
		},
		gatedURI: target,
		reached:  make(chan struct{}),
		release:  make(chan struct{}),
	}
	userRepo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	r := NewResolver(userRepo, &syncNoteRepo{NoteRepository: noteRepo}, urls, fetcher, idGen)

	// **どの経路で抜けても leader を解放する。** 解放し損ねると leader が
	// gate で止まったまま singleflight の entry を握り続け、テストバイナリの
	// 残り全体に goroutine が居座る。
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(fetcher.release) }) }
	t.Cleanup(release)

	// G1: 引用先を解決中。HTTP fetch の中で止める。
	leader := make(chan error, 1)
	go func() {
		_, e := r.ResolveNote(target)
		leader <- e
	}()
	select {
	case <-fetcher.reached:
	case <-time.After(10 * time.Second):
		t.Fatal("leader が fetch に到達しない")
	}

	// G2: 同じ引用先を指す note を、別経路 (inbox 直送) で取り込む。
	type ingested struct {
		renoteID *string
		err      error
	}
	done := make(chan ingested, 1)
	started := make(chan struct{})
	go func() {
		close(started)
		n, _, e := r.IngestNoteWithCreated([]byte(quoteNoteDoc(quoting, actorURI, target)), actorURI)
		if n == nil {
			done <- ingested{nil, e}
			return
		}
		done <- ingested{n.RenoteID, e}
	}()
	<-started

	// **leader を解放する前に返ってきたら、台帳が fetch の窓まで覆っている。**
	select {
	case got := <-done:
		release()
		<-leader
		t.Fatalf("leader の fetch 完了を待たずに返った = 台帳の区間が広すぎる (renoteID=%v err=%v)", got.renoteID, got.err)
	case <-time.After(500 * time.Millisecond):
	}

	release()
	var got ingested
	select {
	case got = <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("引用元の取り込みが返らない")
	}
	require.NoError(t, <-leader)
	require.NoError(t, got.err)
	assert.NotNil(t, got.renoteID, "待てば引けるのに renoteId を落としている")
}

// ledgerSink is a minimal EphemeralSink that only answers URI lookups, which is
// all the in-flight fallback needs.
type ledgerSink struct {
	byURI map[string]*model.Note
}

func (ledgerSink) Enabled() bool                                           { return true }
func (ledgerSink) PutNote(context.Context, *model.Note, *model.User) error { return nil }
func (ledgerSink) UserIDByURI(context.Context, string) (string, error)     { return "", nil }
func (ledgerSink) GetUser(context.Context, string) (*model.User, error)    { return nil, nil }
func (ledgerSink) DropNote(context.Context, string, string) error          { return nil }

func (s ledgerSink) NoteIDByURI(_ context.Context, uri string) (string, error) {
	if n, ok := s.byURI[uri]; ok {
		return n.ID, nil
	}
	return "", nil
}

func (s ledgerSink) GetNote(_ context.Context, id string) (*model.Note, error) {
	for _, n := range s.byURI {
		if n.ID == id {
			return n, nil
		}
	}
	return nil, nil
}

// in-flight guard に当たった quote も、既に取り込み済みなら既存行で繋ぐこと
// (#2684)。
//
// **document id でも引くこと。** 行は document id で保存されるので、別名 URL
// では取得 URI だけでは空振りする。featured 側と同じ理由だが、quote 側は
// 落ちたときに**回復しない** (renoteId が nil のまま保存され、その note は
// FindByURI の早期 return で二度と取り込み直されない) ぶん重い。
//
// 台帳を直接触るのは、この競合を fetch や DB の遅延で再現しようとすると
// 本質的に flaky になるため。
func TestResolveQuoteURI_UsesExistingRowWhenTheKeyIsInFlight(t *testing.T) {
	const (
		listed   = "https://remote.example/@pat/p1"
		storedAs = "https://remote.example/notes/p1"
	)
	noteRepo := testutil.NewMockNoteRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	r := NewResolver(testutil.NewMockUserRepository(), noteRepo, urls, emptyDocFetcher{}, idGen)

	nURI := storedAs
	require.NoError(t, noteRepo.Create(&model.Note{ID: "9patnote000000000000", UserID: "9patuser000000000000", URI: &nURI}))

	// 台帳に無ければ通常経路。取得 URI では引けないので fetch に行き、
	// fetcher が失敗するので nil。
	require.Nil(t, r.resolveQuoteURI(listed, 0, false, nil), "前提: 取得 URI だけでは引けない")

	chain := (&resolveChain{}).with(noteGroupKey(listed, false, false), storedAs)
	got := r.resolveQuoteURI(listed, 0, false, chain)
	require.NotNil(t, got, "in-flight でも既存行があれば引用を繋ぐこと")
	assert.Equal(t, "9patnote000000000000", got.ID)
}

// ephemeral 側も同じ (#2684)。2.5 の逆引きは取得 URI しか見ていないので、
// 別名 URL では document id で引き直さないと拾えない。
func TestResolveQuoteURI_UsesEphemeralRowWhenTheKeyIsInFlight(t *testing.T) {
	const (
		listed   = "https://remote.example/@quinn/q1"
		storedAs = "https://remote.example/notes/q1"
	)
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	r := NewResolver(testutil.NewMockUserRepository(), testutil.NewMockNoteRepository(), urls, emptyDocFetcher{}, idGen)
	r.SetEphemeralSink(ledgerSink{byURI: map[string]*model.Note{
		storedAs: {ID: "9quinnnote0000000000", UserID: "9quinnuser0000000000"},
	}})

	require.Nil(t, r.resolveQuoteURI(listed, 0, true, nil), "前提: 取得 URI だけでは引けない")

	chain := (&resolveChain{}).with(noteGroupKey(listed, false, true), storedAs)
	got := r.resolveQuoteURI(listed, 0, true, chain)
	require.NotNil(t, got, "in-flight でも ephemeral に在れば引用を繋ぐこと")
	assert.Equal(t, "9quinnnote0000000000", got.ID)
}

// 別 goroutine が同じ引用先を取り込んでいる最中でも、引用元は待って引けること
// (#2685)。
//
// **台帳がプロセス全体で 1 つだった頃はここが壊れていた。** 「自分の祖先が
// 握っている」(待つと自分を待つので解けない) と「無関係な goroutine が握って
// いる」(待つのが正しい) を区別できず、後者も諦めていたので、**renoteId を
// 落としたまま保存**していた。しかも resolveNoteOnce も ingestNoteWithCreated も
// FindByURI で早期 return するので、その note が取り込み直されることは無く
// 恒久的に失われる。
//
// leader を HTTP fetch の中で止めたまま引用元を取り込ませ、**待たずに返って
// いないこと**を確かめる。結果だけ見ると、待機側が guard に到達する前に解放が
// 走った場合に変異を見逃す。
func TestResolveChain_ConcurrentIngestOfSameTargetStillLinksQuote(t *testing.T) {
	const (
		actorURI = "https://remote.example/users/nina"
		target   = "https://remote.example/notes/n1"
		quoting  = "https://remote.example/notes/n2"
	)
	fetcher := &gatedDocFetcher{
		docs: map[string]string{
			actorURI: featuredActorDoc("remote.example", "nina", ""),
			target:   plainNoteDoc(target, actorURI),
		},
		gatedURI: target,
		reached:  make(chan struct{}),
		release:  make(chan struct{}),
	}
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	r := NewResolver(testutil.NewMockUserRepository(), &syncNoteRepo{NoteRepository: testutil.NewMockNoteRepository()}, urls, fetcher, idGen)

	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(fetcher.release) }) }
	t.Cleanup(release)

	// G1: 引用先を **ingest 経路で** 取り込み中。fetch の中で止める。
	leader := make(chan error, 1)
	go func() {
		_, e := r.ResolveNote(target)
		leader <- e
	}()
	select {
	case <-fetcher.reached:
	case <-time.After(10 * time.Second):
		t.Fatal("leader が fetch に到達しない")
	}

	// G2: **別のチェーン**から、同じ引用先を指す note を取り込む。
	type ingested struct {
		renoteID *string
		err      error
	}
	done := make(chan ingested, 1)
	started := make(chan struct{})
	go func() {
		close(started)
		n, _, e := r.IngestNoteWithCreated([]byte(quoteNoteDoc(quoting, actorURI, target)), actorURI)
		if n == nil {
			done <- ingested{nil, e}
			return
		}
		done <- ingested{n.RenoteID, e}
	}()
	<-started

	select {
	case got := <-done:
		release()
		<-leader
		t.Fatalf("別チェーンなのに待たずに返った = 台帳がチェーンに閉じていない (renoteID=%v err=%v)",
			got.renoteID, got.err)
	case <-time.After(500 * time.Millisecond):
	}

	release()
	var got ingested
	select {
	case got = <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("引用元の取り込みが返らない")
	}
	require.NoError(t, <-leader)
	require.NoError(t, got.err)
	assert.NotNil(t, got.renoteID, "別 goroutine の取り込み中でも待てば引けるのに renoteId を落としている")
}
