package repository_test

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
)

// countingAntennaRepo counts ListAllActive round-trips so the cache can be
// observed. 書き込みは呼び出し回数だけ記録する。
type countingAntennaRepo struct {
	mu       sync.Mutex
	rows     []*model.Antenna
	listCall int
	listErr  error
	byID     map[string]*model.Antenna
	byUser   map[string][]*model.Antenna
}

func (r *countingAntennaRepo) ListAllActive() ([]*model.Antenna, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.listCall++
	if r.listErr != nil {
		return nil, r.listErr
	}
	// **gorm と同じく 0 件では nil を返す。** `var rows []*T; db.Find(&rows)` は
	// 行が無いと nil のままなので、fake が非 nil の空 slice を返すと
	// 「空をキャッシュするか」の分岐がテストで踏まれない (実際に空振りした)。
	if len(r.rows) == 0 {
		return nil, nil
	}
	out := make([]*model.Antenna, 0, len(r.rows))
	for _, a := range r.rows {
		cp := *a
		out = append(out, &cp)
	}
	return out, nil
}

func (r *countingAntennaRepo) Create(*model.Antenna) error                { return nil }
func (r *countingAntennaRepo) FindByID(id string) (*model.Antenna, error) { return r.byID[id], nil }
func (r *countingAntennaRepo) UpdateFields(string, map[string]any) error  { return nil }
func (r *countingAntennaRepo) Delete(*model.Antenna) error                { return nil }
func (r *countingAntennaRepo) ListByUser(userID string) ([]*model.Antenna, error) {
	return r.byUser[userID], nil
}
func (r *countingAntennaRepo) CountByUser(userID string) (int64, error) {
	return int64(len(r.byUser[userID])), nil
}
func (r *countingAntennaRepo) DeactivateUnusedSince(time.Time) (int64, error) { return 0, nil }

func newCountingRepo(rows ...*model.Antenna) *countingAntennaRepo {
	return &countingAntennaRepo{rows: rows}
}

// #2752: ListAllActive は inbound note 1 件ごとに引かれる。キャッシュが効いて
// いないと連合の流量ぶんだけ DB を叩く。
func TestCachedAntennaRepository_ServesFromCache(t *testing.T) {
	inner := newCountingRepo(&model.Antenna{ID: "a1", IsActive: true})
	c := repository.NewCachedAntennaRepository(inner)

	for i := 0; i < 5; i++ {
		rows, err := c.ListAllActive()
		require.NoError(t, err)
		require.Len(t, rows, 1)
	}
	assert.Equal(t, 1, inner.listCall, "2 回目以降はキャッシュから返す")
}

// **active が 0 件でもキャッシュする。** nil を「未キャッシュ」と区別しないと、
// active antenna が無いインスタンス (実測でそうだった) だけ毎 note クエリが残る。
func TestCachedAntennaRepository_CachesEmptyResult(t *testing.T) {
	inner := newCountingRepo()
	c := repository.NewCachedAntennaRepository(inner)

	for i := 0; i < 3; i++ {
		rows, err := c.ListAllActive()
		require.NoError(t, err)
		assert.Empty(t, rows)
	}
	assert.Equal(t, 1, inner.listCall, "空の結果もキャッシュする")
}

// **書き込みは必ず invalidate する。** 無効化漏れは「antenna を編集しても
// 反映されない」というデバッグしにくい壊れ方になる。
func TestCachedAntennaRepository_WritesInvalidate(t *testing.T) {
	cases := []struct {
		name  string
		write func(c *repository.CachedAntennaRepository) error
	}{
		{"Create", func(c *repository.CachedAntennaRepository) error {
			return c.Create(&model.Antenna{ID: "a2"})
		}},
		{"UpdateFields", func(c *repository.CachedAntennaRepository) error {
			return c.UpdateFields("a1", map[string]any{"isActive": true})
		}},
		{"Delete", func(c *repository.CachedAntennaRepository) error {
			return c.Delete(&model.Antenna{ID: "a1"})
		}},
		{"DeactivateUnusedSince", func(c *repository.CachedAntennaRepository) error {
			_, err := c.DeactivateUnusedSince(time.Now())
			return err
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			inner := newCountingRepo(&model.Antenna{ID: "a1", IsActive: true})
			c := repository.NewCachedAntennaRepository(inner)
			_, err := c.ListAllActive()
			require.NoError(t, err)
			require.Equal(t, 1, inner.listCall)

			require.NoError(t, tc.write(c))
			_, err = c.ListAllActive()
			require.NoError(t, err)
			assert.Equal(t, 2, inner.listCall, "書き込み後は DB を引き直す")
		})
	}
}

// 書き込みが失敗したら invalidate も hook も走らせない (無駄な引き直しを避ける)。
func TestCachedAntennaRepository_FailedWriteKeepsCache(t *testing.T) {
	inner := &failingAntennaRepo{countingAntennaRepo: newCountingRepo(&model.Antenna{ID: "a1", IsActive: true})}
	c := repository.NewCachedAntennaRepository(inner)
	hookCalls := 0
	c.SetInvalidationHook(func() { hookCalls++ })

	_, err := c.ListAllActive()
	require.NoError(t, err)
	require.Error(t, c.Create(&model.Antenna{ID: "a2"}))

	_, err = c.ListAllActive()
	require.NoError(t, err)
	assert.Equal(t, 1, inner.listCall, "失敗した書き込みでは invalidate しない")
	assert.Zero(t, hookCalls, "失敗した書き込みでは hook も呼ばない")
}

type failingAntennaRepo struct{ *countingAntennaRepo }

func (r *failingAntennaRepo) Create(*model.Antenna) error { return assertErr("boom") }

type assertErr string

func (e assertErr) Error() string { return string(e) }

// cross-worker からの Invalidate でキャッシュが落ちること。
func TestCachedAntennaRepository_InvalidateDropsCache(t *testing.T) {
	inner := newCountingRepo(&model.Antenna{ID: "a1", IsActive: true})
	c := repository.NewCachedAntennaRepository(inner)
	_, err := c.ListAllActive()
	require.NoError(t, err)

	c.Invalidate()
	_, err = c.ListAllActive()
	require.NoError(t, err)
	assert.Equal(t, 2, inner.listCall)
}

// 書き込み時に hook (= cross-worker publish) が呼ばれること。
func TestCachedAntennaRepository_HookFiresOnWrite(t *testing.T) {
	inner := newCountingRepo()
	c := repository.NewCachedAntennaRepository(inner)
	calls := 0
	c.SetInvalidationHook(func() { calls++ })

	require.NoError(t, c.Create(&model.Antenna{ID: "a1"}))
	require.NoError(t, c.UpdateFields("a1", map[string]any{"name": "x"}))
	require.NoError(t, c.Delete(&model.Antenna{ID: "a1"}))
	_, err := c.DeactivateUnusedSince(time.Now())
	require.NoError(t, err)

	assert.Equal(t, 4, calls, "全ての書き込みで cross-worker 通知を出す")
}

// TTL は無効化漏れの保険。切れたら引き直す。
func TestCachedAntennaRepository_TTLExpiry(t *testing.T) {
	inner := newCountingRepo(&model.Antenna{ID: "a1", IsActive: true})
	c := repository.NewCachedAntennaRepositoryWithTTL(inner, 20*time.Millisecond)

	_, err := c.ListAllActive()
	require.NoError(t, err)
	time.Sleep(40 * time.Millisecond)
	_, err = c.ListAllActive()
	require.NoError(t, err)
	assert.Equal(t, 2, inner.listCall)
}

// **返すのは複製。** 呼び出し側 (antenna の move 追随) は `a.Users = next` の
// ように戻り値を書き換えるので、キャッシュ本体を渡すと他 goroutine の読み取りと
// 競合する。
func TestCachedAntennaRepository_ReturnsCopies(t *testing.T) {
	inner := newCountingRepo(&model.Antenna{ID: "a1", IsActive: true, Users: model.StringArray{"@a@b"}})
	c := repository.NewCachedAntennaRepository(inner)

	first, err := c.ListAllActive()
	require.NoError(t, err)
	require.Len(t, first, 1)
	first[0].Users = model.StringArray{"@mutated@x"}
	first[0].Name = "mutated"

	second, err := c.ListAllActive()
	require.NoError(t, err)
	require.Len(t, second, 1)
	assert.Equal(t, model.StringArray{"@a@b"}, second[0].Users, "キャッシュが書き換わらないこと")
	assert.Empty(t, second[0].Name)
	assert.NotSame(t, first[0], second[0], "呼び出しごとに別インスタンス")
}

// エラーはキャッシュしない (transient な失敗で以後ずっと空になると困る)。
func TestCachedAntennaRepository_ErrorNotCached(t *testing.T) {
	inner := newCountingRepo(&model.Antenna{ID: "a1", IsActive: true})
	inner.listErr = assertErr("db down")
	c := repository.NewCachedAntennaRepository(inner)

	_, err := c.ListAllActive()
	require.Error(t, err)

	inner.listErr = nil
	rows, err := c.ListAllActive()
	require.NoError(t, err)
	assert.Len(t, rows, 1)
	assert.Equal(t, 2, inner.listCall)
}

// **lastUsedAt だけの bump は invalidate しない** (#2752)。`antennas/notes` は
// 読むたびにこれを書くので、落とすと antenna が使われている構成でこそ cache が
// 効かなくなる。upstream も `needPublishEvent = !antenna.isActive` の条件付きで
// しか antennaUpdated を publish しない。
func TestCachedAntennaRepository_LastUsedAtBumpKeepsCache(t *testing.T) {
	inner := newCountingRepo(&model.Antenna{ID: "a1", IsActive: true})
	c := repository.NewCachedAntennaRepository(inner)
	hookCalls := 0
	c.SetInvalidationHook(func() { hookCalls++ })

	_, err := c.ListAllActive()
	require.NoError(t, err)
	require.NoError(t, c.UpdateFields("a1", map[string]any{"lastUsedAt": time.Now()}))

	_, err = c.ListAllActive()
	require.NoError(t, err)
	assert.Equal(t, 1, inner.listCall, "lastUsedAt だけの更新では引き直さない")
	assert.Zero(t, hookCalls, "cross-worker 通知も出さない")

	// 同じ map に isActive が混ざれば invalidate する (再活性化の経路)。
	require.NoError(t, c.UpdateFields("a1", map[string]any{"lastUsedAt": time.Now(), "isActive": true}))
	_, err = c.ListAllActive()
	require.NoError(t, err)
	assert.Equal(t, 2, inner.listCall, "isActive が混ざれば引き直す")
	assert.Equal(t, 1, hookCalls)
}

// pass-through の 3 メソッドが inner に届くこと。FindByID は所有権 gate の
// 入口 (Show / update / delete / WS antenna channel) なので、落とすと広く壊れる。
func TestCachedAntennaRepository_PassThroughReads(t *testing.T) {
	inner := newCountingRepo(&model.Antenna{ID: "a1", IsActive: true})
	inner.byID = map[string]*model.Antenna{"a1": {ID: "a1", UserID: "owner"}}
	inner.byUser = map[string][]*model.Antenna{"owner": {{ID: "a1"}}}
	c := repository.NewCachedAntennaRepository(inner)

	got, err := c.FindByID("a1")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "owner", got.UserID)

	list, err := c.ListByUser("owner")
	require.NoError(t, err)
	assert.Len(t, list, 1)

	n, err := c.CountByUser("owner")
	require.NoError(t, err)
	assert.EqualValues(t, 1, n)
}

// **並行アクセスで race が無いこと。** copyAntennas の存在理由 (呼び出し側が
// 戻り値を書き換える) は並行してこそ問題になるので、逐次のテストだけでは
// -race が仕事をしない。
func TestCachedAntennaRepository_ConcurrentUse(t *testing.T) {
	inner := newCountingRepo(&model.Antenna{ID: "a1", IsActive: true, Users: model.StringArray{"@a@b"}})
	c := repository.NewCachedAntennaRepository(inner)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				rows, err := c.ListAllActive()
				if err != nil {
					continue
				}
				for _, a := range rows {
					// OnMoveAccount と同じ形の書き換え。
					a.Users = append(model.StringArray{}, a.Users...)
					a.Users = append(a.Users, "@moved@x")
				}
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for j := 0; j < 50; j++ {
			c.Invalidate()
		}
	}()
	wg.Wait()

	rows, err := c.ListAllActive()
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, model.StringArray{"@a@b"}, rows[0].Users, "キャッシュが汚染されないこと")
}
