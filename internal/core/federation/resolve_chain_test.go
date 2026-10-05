package federation

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
)

// resolveChain は不変で、枝ごとに独立していること (#2685)。
//
// **兄弟の枝に鍵が漏れないことが要点。** featured が 2 件目のピンを解決すると
// きに 1 件目の鍵が残っていると、無関係な note を「解決中」と誤判定して
// 取りこぼす。可変な台帳を defer で外す方式だと外し忘れで同じことが起きる。
func TestResolveChain(t *testing.T) {
	t.Run("nil は空", func(t *testing.T) {
		var c *resolveChain
		_, ok := c.lookup("a")
		assert.False(t, ok)
		assert.NotNil(t, c.with("a", "A"), "nil からでも足せること")
	})

	t.Run("足した鍵と document id を引けること", func(t *testing.T) {
		c := (&resolveChain{}).with("k", "doc")
		got, ok := c.lookup("k")
		assert.True(t, ok)
		assert.Equal(t, "doc", got)
	})

	t.Run("id 未確定は空文字で入る", func(t *testing.T) {
		c := (&resolveChain{}).with("k", "")
		got, ok := c.lookup("k")
		assert.True(t, ok)
		assert.Empty(t, got, "fetch 前は id が分からない")
	})

	t.Run("同じ鍵は上書きできる", func(t *testing.T) {
		// resolveNoteDepth が鍵だけで入り、resolveNoteOnce が fetch 後に
		// id を確定させる流れ。
		c := (&resolveChain{}).with("k", "").with("k", "doc")
		got, _ := c.lookup("k")
		assert.Equal(t, "doc", got)
	})

	t.Run("元のチェーンを変更しない", func(t *testing.T) {
		base := (&resolveChain{}).with("a", "A")
		branch := base.with("b", "B")

		_, ok := base.lookup("b")
		assert.False(t, ok, "枝で足した鍵が親に漏れないこと")
		_, ok = branch.lookup("a")
		assert.True(t, ok, "親の鍵は枝にも見えること")

		// 兄弟同士も独立していること。
		sibling := base.with("c", "C")
		_, ok = sibling.lookup("b")
		assert.False(t, ok, "兄弟の鍵が見えないこと")
		_, ok = branch.lookup("c")
		assert.False(t, ok)
	})

	t.Run("空の鍵は無視する", func(t *testing.T) {
		base := (&resolveChain{}).with("a", "A")
		assert.Same(t, base, base.with("", "x"), "同じチェーンを返すこと")
	})
}

// syncNoteRepo serialises the note repo so `-race` can run the concurrent
// resolve tests.
//
// **本番の repo は GORM/PostgreSQL なので競合しない。** 競合するのは
// testutil の mock (素の map) の側で、#2685 で待って進むようになったことで
// 2 goroutine が実際に同時に触るようになり表面化した。mock 全体に mutex を
// 足すと `FindByIDWithRelations` → `FindByID` のような再入で自己デッドロック
// するため、ここでは並行テストが使う分だけ包む。
type syncNoteRepo struct {
	repository.NoteRepository
	mu sync.Mutex
}

func (s *syncNoteRepo) Create(n *model.Note) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.NoteRepository.Create(n)
}

func (s *syncNoteRepo) FindByURI(uri string) (*model.Note, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.NoteRepository.FindByURI(uri)
}

func (s *syncNoteRepo) FindByID(id string) (*model.Note, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.NoteRepository.FindByID(id)
}

func (s *syncNoteRepo) IncrementCount(id, col string, d int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.NoteRepository.IncrementCount(id, col, d)
}

// fetch 後に台帳へ載せるのは **document id**。取得 URI を載せると、別名 URL で
// 解決したときに入れ子の解決が既存行を引けず、renoteId 欠落やピンの消失に戻る
// (#2684 review MED-1 / #2685 review MEDIUM-2)。
func TestChainAfterProbe(t *testing.T) {
	const (
		alias = "https://remote.example/@iris/i1"
		docID = "https://remote.example/notes/i1"
	)
	got := chainAfterProbe(nil, alias, false, false, docID)

	v, ok := got.lookup(noteGroupKey(alias, false, false))
	require.True(t, ok, "取得 URI の鍵で引けること")
	assert.Equal(t, docID, v, "値は document id であること (取得 URI ではない)")

	v, ok = got.lookup(docID)
	require.True(t, ok, "document id でも引けること")
	assert.Equal(t, docID, v)

	t.Run("鍵の名前空間を引き継ぐ", func(t *testing.T) {
		got := chainAfterProbe(nil, alias, true, true, docID)
		_, ok := got.lookup(noteGroupKey(alias, true, true))
		assert.True(t, ok)
		_, ok = got.lookup(noteGroupKey(alias, false, false))
		assert.False(t, ok, "cross-host / ephemeral の別を潰さないこと")
	})

	t.Run("既にある鍵を潰さない", func(t *testing.T) {
		prev := (&resolveChain{}).with("other", "other")
		got := chainAfterProbe(prev, alias, false, false, docID)
		_, ok := got.lookup("other")
		assert.True(t, ok)
	})
}

// 木の識別子は 1 度だけ採番し、以降は引き継ぐこと。
func TestEnsureTree(t *testing.T) {
	nilChain := (*resolveChain)(nil).ensureTree()
	require.NotNil(t, nilChain)
	require.NotZero(t, nilChain.treeID())
	require.NotNil(t, nilChain.budget, "木の根で待ちの予算を作ること")

	again := nilChain.ensureTree()
	assert.Same(t, nilChain, again, "採番済みならそのまま返すこと")

	// with を通していない裸の chain (id 未採番) でも、載っている鍵は残すこと。
	bare := &resolveChain{ids: map[string]string{"k": "v"}}
	got := bare.ensureTree()
	assert.NotZero(t, got.treeID())
	v, ok := got.lookup("k")
	assert.True(t, ok)
	assert.Equal(t, "v", v)
	assert.Zero(t, bare.id, "レシーバは変更しないこと")

	t.Run("予算は枝で共有する", func(t *testing.T) {
		root := (*resolveChain)(nil).with("a", "")
		branch := root.with("b", "")
		assert.Equal(t, root.treeID(), branch.treeID())
		assert.Same(t, root.budget, branch.budget, "枝ごとに予算を作り直さないこと")

		branch.chargeWait(time.Second)
		assert.Equal(t, resolveJoinTimeout-time.Second, root.waitBudget(),
			"枝が使った分は木全体から引くこと")
	})

	t.Run("予算は待ちにだけ減る", func(t *testing.T) {
		// **経過時刻ではなく費やした待ち時間で持つこと。** 期限 (時刻) で持つと
		// fetch のような待ち以外の作業でも減り、上限より長くかかる解決の途中で
		// 正当な待ちに出会うと 1ms も待たずに諦める (#2685 review round 4)。
		root := (*resolveChain)(nil).ensureTree()
		time.Sleep(20 * time.Millisecond)
		assert.Equal(t, resolveJoinTimeout, root.waitBudget(),
			"待っていない時間で予算を減らさないこと")
	})

	t.Run("待ちの予算", func(t *testing.T) {
		assert.Equal(t, resolveJoinTimeout, (*resolveChain)(nil).waitBudget(),
			"木の外から来た呼び出しは 1 回分を使う")
		spent := (*resolveChain)(nil).ensureTree()
		spent.chargeWait(resolveJoinTimeout)
		assert.LessOrEqual(t, spent.waitBudget(), time.Duration(0), "使い切ったら待たないこと")
	})

	t.Run("best-effort の印は枝に引き継ぐ", func(t *testing.T) {
		root := (*resolveChain)(nil).with("a", "")
		require.True(t, root.mayWait())
		be := root.asBestEffort()
		assert.False(t, be.mayWait())
		assert.False(t, be.with("b", "").mayWait(), "枝でも待たないこと")
		assert.True(t, root.mayWait(), "元のチェーンは変えないこと")
		assert.Equal(t, root.treeID(), be.treeID())
		assert.Same(t, root.budget, be.budget)
		assert.Same(t, be, be.asBestEffort(), "既に印が付いていればそのまま返すこと")
	})
}

// ingestedDocID は「祖先が**自分の document として**載せた entry」だけを拾うこと
// (#2695)。チェーンには 3 種類の entry が載るので、鍵と値の一致で選り分ける。
//
//   - `with(id, id)`: 祖先の取り込み中 (これだけが対象)
//   - `with(鍵, "")`: resolveNoteDepth の fetch 前の前置き。正規形ではその鍵が
//     document id と一致するので、値を見ないと自分の前置きを祖先と取り違え、
//     あらゆる正規の解決が諦めに落ちる
//   - `with(取得 URI, document id)`: chainAfterProbe。**値は空ではない**ので、
//     「値が空でない」で判定すると、featured の item の document id がたまたま
//     祖先の取得 URI と一致したときに誤爆して**別の note の行**を返す
//     (#2710 review round 3 MEDIUM-1)
func TestResolveChain_IngestedDocID(t *testing.T) {
	cases := []struct {
		name  string
		chain *resolveChain
		docID string
		want  string
		ok    bool
	}{
		{"祖先が取り込み中", (&resolveChain{}).with("https://h/notes/x", "https://h/notes/x"), "https://h/notes/x", "https://h/notes/x", true},
		{"自分の前置き (値が空)", (&resolveChain{}).with("https://h/notes/x", ""), "https://h/notes/x", "", false},
		// 取得 URI 側の entry。値は非空だが鍵と一致しないので対象外。
		// ここを true にすると、別 note の id を「祖先が取り込み中」と誤認する。
		{"祖先の取得 URI に一致 (別 note)", (&resolveChain{}).with("https://h/@u/x", "https://h/notes/x"), "https://h/@u/x", "", false},
		// 同じ chainAfterProbe が入れるもう 1 つの entry。こちらは鍵 == 値。
		{"祖先の document id に一致", (&resolveChain{}).with("https://h/@u/x", "https://h/notes/x").with("https://h/notes/x", "https://h/notes/x"), "https://h/notes/x", "https://h/notes/x", true},
		{"載っていない", (&resolveChain{}).with("https://h/notes/y", "https://h/notes/y"), "https://h/notes/x", "", false},
		{"docID が空", (&resolveChain{}).with("", ""), "", "", false},
		{"チェーンが nil", nil, "https://h/notes/x", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := tc.chain.ingestedDocID(tc.docID)
			assert.Equal(t, tc.ok, ok)
			assert.Equal(t, tc.want, got)
		})
	}
}
