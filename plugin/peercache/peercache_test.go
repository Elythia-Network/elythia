package peercache_test

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/plugin"
	"github.com/elythia-network/elythia/plugin/peercache"
)

// testSchema は **`plugin_` で始めない。** pluginstore.ListSchemas が
// `plugin_%` で引くので、その接頭辞を使うと実在のプラグインの schema と
// 見分けが付かなくなる (テストが途中で死んで残ったときに紛らわしい)。
const testSchema = "peercache_test"

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// testDB opens the plugin schema this package owns.
//
// **専用 schema を使う。** 他のパッケージのテストと同じ DB を共有するので、
// テーブルを作り直す側が相手の前提を壊さないようにする (#2450 と同じ理由)。
func testDB(t *testing.T) *sql.DB {
	t.Helper()
	base := fmt.Sprintf("host=%s port=%s user=%s password=%s dbname=%s sslmode=disable",
		envOr("TEST_DB_HOST", "localhost"), envOr("TEST_DB_PORT", "5432"),
		envOr("TEST_DB_USER", "mk"), envOr("TEST_DB_PASS", "mk"),
		envOr("TEST_DB_NAME", "misskey_test"))

	admin, err := sql.Open("pgx", base)
	require.NoError(t, err)
	defer admin.Close() //nolint:errcheck // 使い捨て
	if err := admin.Ping(); err != nil {
		t.Skipf("PostgreSQL に繋がりません: %v", err)
	}
	_, err = admin.Exec(`DROP SCHEMA IF EXISTS ` + testSchema + ` CASCADE`)
	require.NoError(t, err)
	_, err = admin.Exec(`CREATE SCHEMA ` + testSchema)
	require.NoError(t, err)

	db, err := sql.Open("pgx", base+" search_path="+testSchema)
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = db.Close()
		if a, err := sql.Open("pgx", base); err == nil {
			_, _ = a.Exec(`DROP SCHEMA IF EXISTS ` + testSchema + ` CASCADE`)
			_ = a.Close()
		}
	})

	for _, m := range peercache.Migrations(1) {
		_, err := db.Exec(m.SQL)
		require.NoErrorf(t, err, "migration %d", m.Version)
	}
	return db
}

// fakeContext is the slice of plugin.Context the cache uses.
// fakeContext queues Go callbacks instead of running them.
//
// **同期に走らせない。** 走らせてしまうと、実装が ctx.Go をやめて描画のスレッドで
// nodeinfo を待つようになっても (最大 10 秒) テストが緑のままになる。
type fakeContext struct {
	plugin.Context
	peer   *fakePeer
	mu     sync.Mutex
	queued []func()
}

func (c *fakeContext) Peer() plugin.Peer    { return c.peer }
func (c *fakeContext) Logger() *slog.Logger { return slog.Default() }
func (c *fakeContext) Go(fn func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.queued = append(c.queued, fn)
}

// drainConcurrently runs the queued callbacks in parallel, the way ctx.Go does.
func (c *fakeContext) drainConcurrently() {
	c.mu.Lock()
	fns := c.queued
	c.queued = nil
	c.mu.Unlock()
	var wg sync.WaitGroup
	for _, fn := range fns {
		wg.Add(1)
		go func(f func()) { defer wg.Done(); f() }(fn)
	}
	wg.Wait()
}

// drain runs everything Go queued so far and reports how many ran.
func (c *fakeContext) drain() int {
	c.mu.Lock()
	fns := c.queued
	c.queued = nil
	c.mu.Unlock()
	for _, fn := range fns {
		fn()
	}
	return len(fns)
}

func newFakeContext(p *fakePeer) *fakeContext { return &fakeContext{peer: p} }

type sent struct {
	host    string
	payload any
}

type fakePeer struct {
	plugin.Peer
	mu       sync.Mutex
	has      map[string]bool
	sends    []sent
	next     int
	err      error
	hasErr   bool
	hasDelay time.Duration
	hasCalls int
}

func (p *fakePeer) Has(context.Context, string) (bool, error) {
	p.mu.Lock()
	p.hasCalls++
	p.mu.Unlock()
	if p.hasDelay > 0 {
		time.Sleep(p.hasDelay)
	}
	if p.hasErr {
		return false, nil
	}
	return true, nil
}

func (p *fakePeer) Send(_ context.Context, host string, payload any) (string, error) {
	if p.has != nil && !p.has[host] {
		return "", fmt.Errorf("持っていません")
	}
	if p.err != nil {
		return "", p.err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.next++
	p.sends = append(p.sends, sent{host: host, payload: payload})
	return fmt.Sprintf("id%d", p.next), nil
}

func newCache(t *testing.T, db *sql.DB, p *fakePeer, opts ...func(*peercache.Options)) (*peercache.Cache, *fakeContext) {
	t.Helper()
	fc := newFakeContext(p)
	o := peercache.Options{
		Context: fc,
		DB:      db,
		Request: func(key string) any { return map[string]string{"key": key} },
	}
	for _, fn := range opts {
		fn(&o)
	}
	c, err := peercache.New(o)
	require.NoError(t, err)
	return c, fc
}

// **初回は空で返り、取り寄せは裏で走る。** 描画を待たせないのがこの型の目的。
func TestCache_FirstLookupIsEmptyAndAsks(t *testing.T) {
	db := testDB(t)
	p := &fakePeer{}
	c, fc := newCache(t, db, p)
	ctx := context.Background()

	got, err := c.Lookup(ctx, "other.example", "alice")
	fc.drain()
	require.NoError(t, err)
	assert.Nil(t, got, "初回は空")
	require.Len(t, p.sends, 1, "取り寄せを出す")
	assert.Equal(t, "other.example", p.sends[0].host)
	assert.Equal(t, map[string]string{"key": "alice"}, p.sends[0].payload)

	// 応答が届いたら次から出る。
	require.NoError(t, c.Store(ctx, "id1", map[string]int{"score": 7}, true))
	got, err = c.Lookup(ctx, "other.example", "alice")
	fc.drain()
	require.NoError(t, err)
	assert.JSONEq(t, `{"score":7}`, string(got))
	assert.Len(t, p.sends, 1, "期限内なら問い合わせ直さない")
}

// **空振りも覚える。** 覚えないと、そのプラグインを使っていない利用者の
// プロフィールを開くたびに相手へ問い合わせることになる。
func TestCache_RemembersNegative(t *testing.T) {
	db := testDB(t)
	p := &fakePeer{}
	c, fc := newCache(t, db, p)
	ctx := context.Background()

	_, err := c.Lookup(ctx, "other.example", "bob")
	fc.drain()
	require.NoError(t, err)
	require.Len(t, p.sends, 1)
	require.NoError(t, c.Store(ctx, "id1", nil, false))

	for i := 0; i < 3; i++ {
		got, err := c.Lookup(ctx, "other.example", "bob")
		fc.drain()
		require.NoError(t, err)
		assert.Nil(t, got)
	}
	assert.Len(t, p.sends, 1, "否定 TTL の間は問い合わせ直さない")
}

// **否定は肯定より短く覚える。** 同じ TTL にすると、相手が後から使い始めても
// 長いあいだ気付けない。TTL の使い分けそのものを見る。
func TestCache_NegativeTTLIsSeparate(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	short := func(o *peercache.Options) {
		o.TTL = time.Hour
		o.NegativeTTL = time.Nanosecond
	}

	t.Run("negative expires on its own TTL", func(t *testing.T) {
		p := &fakePeer{}
		c, fc := newCache(t, db, p, short)
		_, err := c.Lookup(ctx, "neg.example", "bob")
		fc.drain()
		require.NoError(t, err)
		require.NoError(t, c.Store(ctx, "id1", nil, false))
		_, err = db.Exec(`DELETE FROM peer_cache_ask`)
		require.NoError(t, err)

		_, err = c.Lookup(ctx, "neg.example", "bob")
		fc.drain()
		require.NoError(t, err)
		assert.Len(t, p.sends, 2, "否定 TTL が切れたら取り直す")
	})

	t.Run("positive keeps the long TTL", func(t *testing.T) {
		p := &fakePeer{}
		c, fc := newCache(t, db, p, short)
		_, err := c.Lookup(ctx, "pos.example", "bob")
		fc.drain()
		require.NoError(t, err)
		require.NoError(t, c.Store(ctx, "id1", map[string]int{"score": 1}, true))
		_, err = db.Exec(`DELETE FROM peer_cache_ask`)
		require.NoError(t, err)

		got, err := c.Lookup(ctx, "pos.example", "bob")
		fc.drain()
		require.NoError(t, err)
		assert.JSONEq(t, `{"score":1}`, string(got))
		assert.Len(t, p.sends, 1, "肯定 TTL の間は取り直さない")
	})
}

// 期限切れでも**古いものは返す**。取り直しは裏で進む。
func TestCache_StaleValueIsStillReturned(t *testing.T) {
	db := testDB(t)
	p := &fakePeer{}
	c, fc := newCache(t, db, p, func(o *peercache.Options) { o.TTL = time.Nanosecond })
	ctx := context.Background()

	_, err := c.Lookup(ctx, "other.example", "carol")
	fc.drain()
	require.NoError(t, err)
	require.NoError(t, c.Store(ctx, "id1", map[string]int{"score": 1}, true))

	// askInterval の抑止に掛からないよう印を消しておく。
	_, err = db.Exec(`DELETE FROM peer_cache_ask`)
	require.NoError(t, err)

	got, err := c.Lookup(ctx, "other.example", "carol")
	fc.drain()
	require.NoError(t, err)
	assert.JSONEq(t, `{"score":1}`, string(got), "期限切れでも古いものを返す")
	assert.Len(t, p.sends, 2, "裏で取り直す")
}

// **同じ相手に群がらない。** 否定 TTL は応答が届いてから効くので、届く前に
// 大勢が同じプロフィールを開くと問い合わせが重なる。
func TestCache_DoesNotPileUpAsks(t *testing.T) {
	db := testDB(t)
	p := &fakePeer{}
	c, fc := newCache(t, db, p)
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		_, err := c.Lookup(ctx, "other.example", "dave")
		fc.drain()
		require.NoError(t, err)
	}
	assert.Len(t, p.sends, 1, "応答待ちの間は 1 回だけ")
}

// 同じ交換の応答は 2 回来ることがある (送信がキューに載るため)。2 回目は
// 知らない id として静かに捨てる。
func TestCache_StoreIsIdempotent(t *testing.T) {
	db := testDB(t)
	p := &fakePeer{}
	c, fc := newCache(t, db, p)
	ctx := context.Background()

	_, err := c.Lookup(ctx, "other.example", "erin")
	fc.drain()
	require.NoError(t, err)
	require.NoError(t, c.Store(ctx, "id1", map[string]int{"score": 3}, true))
	require.NoError(t, c.Store(ctx, "id1", map[string]int{"score": 99}, true), "2 回目もエラーにしない")

	got, err := c.Lookup(ctx, "other.example", "erin")
	fc.drain()
	require.NoError(t, err)
	assert.JSONEq(t, `{"score":3}`, string(got), "最初の応答が残る")

	// 知らない id も静かに捨てる。
	require.NoError(t, c.Store(ctx, "unknown", map[string]int{"score": 0}, true))
}

func TestCache_Sweep(t *testing.T) {
	db := testDB(t)
	c, _ := newCache(t, db, &fakePeer{})
	ctx := context.Background()

	_, err := db.Exec(`
		INSERT INTO peer_cache (host, key, payload, fetched_at, expires_at)
		VALUES ('old.example', 'x', 'null', now() - interval '8 days', now());
		INSERT INTO peer_cache_pending (id, host, key, created_at)
		VALUES ('stale', 'old.example', 'x', now() - interval '2 days');
	`)
	require.NoError(t, err)

	require.NoError(t, c.Sweep(ctx))
	var snapshots, pendings int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM peer_cache`).Scan(&snapshots))
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM peer_cache_pending`).Scan(&pendings))
	assert.Equal(t, 0, snapshots)
	assert.Equal(t, 0, pendings)
}

func TestNew_RequiresOptions(t *testing.T) {
	db := testDB(t)
	ok := peercache.Options{Context: newFakeContext(&fakePeer{}), DB: db, Request: func(string) any { return nil }}

	for _, tt := range []struct {
		name  string
		mutin func(*peercache.Options)
	}{
		{"no context", func(o *peercache.Options) { o.Context = nil }},
		{"no db", func(o *peercache.Options) { o.DB = nil }},
		{"no request", func(o *peercache.Options) { o.Request = nil }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			o := ok
			tt.mutin(&o)
			_, err := peercache.New(o)
			assert.Error(t, err)
		})
	}

	c, err := peercache.New(ok)
	require.NoError(t, err)
	assert.NotNil(t, c)
}

// 応答の JSON が null なら、found が true でも否定として覚える
// (相手が「持っている」と答えつつ中身が無い場合)。
func TestCache_NullPayloadIsNegative(t *testing.T) {
	db := testDB(t)
	p := &fakePeer{}
	c, fc := newCache(t, db, p)
	ctx := context.Background()

	_, err := c.Lookup(ctx, "other.example", "frank")
	fc.drain()
	require.NoError(t, err)
	require.NoError(t, c.Store(ctx, "id1", nil, true))

	var payload []byte
	require.NoError(t, db.QueryRow(
		`SELECT payload FROM peer_cache WHERE host='other.example' AND key='frank'`).Scan(&payload))
	assert.JSONEq(t, `null`, string(payload))

	got, err := c.Lookup(ctx, "other.example", "frank")
	fc.drain()
	require.NoError(t, err)
	assert.Nil(t, got)

	// **否定 TTL で覚えること。** 肯定 TTL で覚えると、中身が無い相手を
	// 長いあいだ問い合わせ直さなくなる。
	var seconds float64
	require.NoError(t, db.QueryRow(`
		SELECT extract(epoch from (expires_at - fetched_at)) FROM peer_cache
		WHERE host='other.example' AND key='frank'`).Scan(&seconds))
	assert.InDelta(t, peercache.DefaultNegativeTTL.Seconds(), seconds, 1)
}

// 相手が同じプラグインを持っていなければ、静かに諦める (普通のこと)。
func TestCache_SkipsHostWithoutPlugin(t *testing.T) {
	db := testDB(t)
	p := &fakePeer{hasErr: true}
	c, fc := newCache(t, db, p)

	got, err := c.Lookup(context.Background(), "other.example", "alice")
	fc.drain()
	require.NoError(t, err)
	assert.Nil(t, got)
	assert.Empty(t, p.sends, "問い合わせを出さない")

	var pendings int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM peer_cache_pending`).Scan(&pendings))
	assert.Equal(t, 0, pendings, "記録も残さない")
}

// Send が失敗しても Lookup はエラーにしない (描画は続く)。
func TestCache_SendFailureIsNotFatal(t *testing.T) {
	db := testDB(t)
	p := &fakePeer{err: assertErr{}}
	c, fc := newCache(t, db, p)

	got, err := c.Lookup(context.Background(), "other.example", "alice")
	fc.drain()
	require.NoError(t, err)
	assert.Nil(t, got)

	var pendings int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM peer_cache_pending`).Scan(&pendings))
	assert.Equal(t, 0, pendings, "出せなかった問い合わせは記録しない")
}

// DB が壊れているときはエラーを返す (黙って空を出さない)。
func TestCache_ReportsDBErrors(t *testing.T) {
	db := testDB(t)
	c, fc := newCache(t, db, &fakePeer{})
	ctx := context.Background()

	_, err := db.Exec(`DROP TABLE peer_cache`)
	require.NoError(t, err)
	_, err = c.Lookup(ctx, "other.example", "alice")
	fc.drain()
	assert.Error(t, err, "読めないことを隠さない")

	_, err = db.Exec(`DROP TABLE peer_cache_pending`)
	require.NoError(t, err)
	assert.Error(t, c.Store(ctx, "id1", nil, false))
	assert.Error(t, c.Sweep(ctx))
}

// JSON にできない payload はエラーになる。
func TestCache_StoreRejectsUnmarshalablePayload(t *testing.T) {
	db := testDB(t)
	p := &fakePeer{}
	c, fc := newCache(t, db, p)
	ctx := context.Background()

	_, err := c.Lookup(ctx, "other.example", "alice")
	fc.drain()
	require.NoError(t, err)
	err = c.Store(ctx, "id1", make(chan int), true)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "JSON")
}

type assertErr struct{}

func (assertErr) Error() string { return "だめ" }

// **同時に開いても問い合わせは 1 回。** 判定と記録を分けていた頃は、その間に
// Has (最大 10 秒) と Send が入るので view の数だけ並んだ (実測で 20 件中 20 件)。
func TestCache_ClaimIsAtomicUnderConcurrency(t *testing.T) {
	db := testDB(t)
	// **Has を遅らせる。** 遅延ゼロだと check-then-act でも 1 件に収まってしまい、
	// 直した所を検査できない。
	p := &fakePeer{hasDelay: 30 * time.Millisecond}
	c, fc := newCache(t, db, p)

	const viewers = 20
	var wg sync.WaitGroup
	for i := 0; i < viewers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := c.Lookup(context.Background(), "other.example", "alice")
			assert.NoError(t, err)
		}()
	}
	wg.Wait()

	// 積まれた取り寄せを並行に走らせる (本番は ctx.Go が goroutine を起こす)。
	fc.drainConcurrently()

	assert.Len(t, p.sends, 1, "同時に %d 件開いても問い合わせは 1 回", viewers)
}

// **失敗しても印は残る。** 相手が落ちていて毎回失敗する場合に、view のたびに
// 外向きのリクエストが飛ばないこと。
func TestCache_FailedAskIsStillSuppressed(t *testing.T) {
	db := testDB(t)
	p := &fakePeer{err: assertErr{}}
	c, fc := newCache(t, db, p)
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		_, err := c.Lookup(ctx, "dead.example", "alice")
		require.NoError(t, err)
		fc.drain()
	}
	assert.Empty(t, p.sends)
	// **外向きの試行が 1 回で止まること。** 印を残さない実装だと、失敗のたびに
	// 抑止が効かず 5 回とも nodeinfo を引きに行く (最大 10 秒 x 5)。
	assert.Equal(t, 1, p.hasCalls, "落ちている相手には繰り返し接続しない")

	var asks int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM peer_cache_ask`).Scan(&asks))
	assert.Equal(t, 1, asks, "印は 1 つ")
}

// **取り寄せは描画を待たせない。** ctx.Go をやめて同期に呼ぶと、nodeinfo が
// キャッシュに無いとき最大 10 秒画面が固まる。
func TestCache_LookupDoesNotBlockOnFetch(t *testing.T) {
	db := testDB(t)
	p := &fakePeer{}
	c, fc := newCache(t, db, p)

	_, err := c.Lookup(context.Background(), "other.example", "alice")
	require.NoError(t, err)
	assert.Empty(t, p.sends, "Lookup が返る時点ではまだ問い合わせていない")

	assert.Equal(t, 1, fc.drain(), "取り寄せは ctx.Go に積まれている")
	assert.Len(t, p.sends, 1)
}

// **Sweep は消してよい行だけ消す。** 範囲が広がると、キャッシュ全消しと、
// 応答待ち pending の全消し (届いた応答が相関を失う) になる。
func TestCache_SweepKeepsFreshRows(t *testing.T) {
	db := testDB(t)
	c, _ := newCache(t, db, &fakePeer{})

	_, err := db.Exec(`
		INSERT INTO peer_cache (host, key, payload, fetched_at, expires_at) VALUES
			('old.example', 'x', 'null', now() - interval '8 days', now()),
			('new.example', 'x', 'null', now(), now() + interval '1 hour');
		INSERT INTO peer_cache_pending (id, host, key, created_at) VALUES
			('stale', 'old.example', 'x', now() - interval '2 days'),
			('fresh', 'new.example', 'x', now());
		INSERT INTO peer_cache_ask (host, key, asked_at) VALUES
			('old.example', 'x', now() - interval '2 days'),
			('new.example', 'x', now());
	`)
	require.NoError(t, err)

	require.NoError(t, c.Sweep(context.Background()))

	for _, tt := range []struct{ table, want string }{
		{"peer_cache", "new.example"},
		{"peer_cache_pending", "new.example"},
		{"peer_cache_ask", "new.example"},
	} {
		rows, err := db.Query(`SELECT host FROM ` + tt.table)
		require.NoError(t, err)
		var hosts []string
		for rows.Next() {
			var h string
			require.NoError(t, rows.Scan(&h))
			hosts = append(hosts, h)
		}
		require.NoError(t, rows.Err())
		require.NoError(t, rows.Close())
		assert.Equalf(t, []string{tt.want}, hosts, "%s は新しい行を残す", tt.table)
	}
}

// **取り直しが反映されること。** upsert が DO NOTHING に退行すると、期限切れの
// 値を永久に返し続ける。
func TestCache_StoreRefreshesExistingEntry(t *testing.T) {
	db := testDB(t)
	p := &fakePeer{}
	c, fc := newCache(t, db, p, func(o *peercache.Options) { o.TTL = time.Nanosecond })
	ctx := context.Background()

	_, err := c.Lookup(ctx, "other.example", "gina")
	require.NoError(t, err)
	fc.drain()
	require.NoError(t, c.Store(ctx, "id1", map[string]int{"score": 1}, true))

	// 期限切れなので取り直しが走る。
	_, err = db.Exec(`DELETE FROM peer_cache_ask`)
	require.NoError(t, err)
	got, err := c.Lookup(ctx, "other.example", "gina")
	require.NoError(t, err)
	fc.drain()
	assert.JSONEq(t, `{"score":1}`, string(got), "古いものを返す")
	require.Len(t, p.sends, 2)

	require.NoError(t, c.Store(ctx, "id2", map[string]int{"score": 2}, true))
	got, err = c.Lookup(ctx, "other.example", "gina")
	require.NoError(t, err)
	fc.drain()
	assert.JSONEq(t, `{"score":2}`, string(got), "新しい値で置き換わる")
}

// 知らない id にゴミ行を書かない。
func TestCache_StoreIgnoresUnknownID(t *testing.T) {
	db := testDB(t)
	c, _ := newCache(t, db, &fakePeer{})

	require.NoError(t, c.Store(context.Background(), "unknown", map[string]int{"a": 1}, true))
	var rows int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM peer_cache`).Scan(&rows))
	assert.Equal(t, 0, rows)
}

// 既定の TTL は肯定と否定で別の値。
func TestDefaultTTLs(t *testing.T) {
	assert.Equal(t, 30*time.Minute, peercache.DefaultTTL)
	assert.Equal(t, 10*time.Minute, peercache.DefaultNegativeTTL)
	assert.Greater(t, peercache.DefaultTTL, peercache.DefaultNegativeTTL,
		"否定は肯定より短く覚える (相手が使い始めたときに気付けるように)")
}

// **消費するのは常に 1 つ、version は from ちょうど。** ここが増えると、後ろに
// 自前の migration を置いたプラグインが version の重複で起動しなくなる。
func TestMigrations_ConsumesExactlyOneVersion(t *testing.T) {
	for _, from := range []int{1, 7, 42} {
		got := peercache.Migrations(from)
		require.Len(t, got, 1, "from=%d", from)
		assert.Equal(t, from, got[0].Version)
		assert.Contains(t, got[0].SQL, "peer_cache")
		assert.Contains(t, got[0].SQL, "peer_cache_pending")
		assert.Contains(t, got[0].SQL, "peer_cache_ask")
	}
}
