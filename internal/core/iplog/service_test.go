package iplog

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type observation struct {
	userID string
	ip     string
	at     time.Time
}

type fakeObserver struct {
	mu   sync.Mutex
	got  []observation
	err  error
	fail int // 先頭 fail 回だけ失敗する
}

func (f *fakeObserver) Observe(userID, ip string, at time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail > 0 {
		f.fail--
		return errors.New("db down")
	}
	if f.err != nil {
		return f.err
	}
	f.got = append(f.got, observation{userID, ip, at})
	return nil
}

func (f *fakeObserver) calls() []observation {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]observation(nil), f.got...)
}

type fakeMeta struct {
	mu      sync.Mutex
	enabled bool
	err     error
}

// **エラーと一緒に値も返す。** 実装によっては古いキャッシュを添えて error を返す
// ので、`m == nil` だけを見ていると「読めなかったのに記録する」経路が残る。
func (f *fakeMeta) Fetch() (*model.Meta, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return &model.Meta{EnableIPLogging: f.enabled}, f.err
}

func (f *fakeMeta) set(v bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.enabled = v
}

// newTestService wires a service whose clock and dispatch are deterministic.
func newTestService(t *testing.T, enabled bool) (*Service, *fakeObserver, *fakeMeta, *time.Time) {
	t.Helper()
	repo := &fakeObserver{}
	meta := &fakeMeta{enabled: enabled}
	clock := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	s := NewService(repo, meta, time.Hour)
	s.now = func() time.Time { return clock }
	// 書き込みを同期にする。goroutine のままだと観測が届く前にアサートしうる。
	s.dispatch = func(f func()) { f() }
	s.clearedAt = clock
	return s, repo, meta, &clock
}

func TestService_RecordsWhenEnabled(t *testing.T) {
	s, repo, _, clock := newTestService(t, true)

	s.Record("u1", "192.0.2.1")

	got := repo.calls()
	require.Len(t, got, 1)
	assert.Equal(t, observation{"u1", "192.0.2.1", *clock}, got[0])
}

// **無効なら 1 件も書かない。** 記録の可否は運用者の設定で、この機能のために
// 暗黙に有効化しない。
func TestService_SkipsWhenDisabled(t *testing.T) {
	s, repo, _, _ := newTestService(t, false)

	s.Record("u1", "192.0.2.1")

	assert.Empty(t, repo.calls())
}

// **切り替えが再起動なしで効く (#3107)。** 起動時に 1 度だけ meta を読む形だと、
// 管理画面で有効にしても記録が始まらない。
func TestService_HonoursToggleAtRuntime(t *testing.T) {
	s, repo, meta, _ := newTestService(t, false)

	s.Record("u1", "192.0.2.1")
	require.Empty(t, repo.calls(), "無効の間に書いている")

	meta.set(true)
	s.Record("u1", "192.0.2.1")
	assert.Len(t, repo.calls(), 1, "有効にしても記録が始まらない")

	// 無効へ戻したら止まる (有効化だけ見ると、無効化が効かない実装でも緑になる)。
	meta.set(false)
	s.Record("u1", "198.51.100.7")
	assert.Len(t, repo.calls(), 1, "無効へ戻しても記録が続いている")
}

// 記録するのは正規形。表記揺れのまま入れると同じ端末の観測が別の行に分かれる。
func TestService_NormalizesBeforeWriting(t *testing.T) {
	s, repo, _, _ := newTestService(t, true)

	s.Record("u1", "::ffff:192.0.2.1")

	got := repo.calls()
	require.Len(t, got, 1)
	assert.Equal(t, "192.0.2.1", got[0].ip)
}

// IPv4 と IPv4-mapped は同じ観測として畳まれ、2 回目は書かれない。
func TestService_MappedAndPlainAreTheSameObservation(t *testing.T) {
	s, repo, _, _ := newTestService(t, true)

	s.Record("u1", "192.0.2.1")
	s.Record("u1", "::ffff:192.0.2.1")

	assert.Len(t, repo.calls(), 1)
}

func TestService_SkipsUnparsableIP(t *testing.T) {
	s, repo, _, _ := newTestService(t, true)

	s.Record("u1", "not-an-ip")
	s.Record("u1", "")

	assert.Empty(t, repo.calls())
}

func TestService_SkipsEmptyUser(t *testing.T) {
	s, repo, _, _ := newTestService(t, true)

	s.Record("", "192.0.2.1")

	assert.Empty(t, repo.calls())
}

// meta が読めないときは書かない。読めないことを「有効」に倒すと、無効な構成で
// 記録が始まる。
//
// **値が返ってきていても error があれば書かない。** 古いキャッシュを添えて error を
// 返す実装があるので、`m == nil` だけを見ると素通りする (実測で変異が検出されなかった)。
func TestService_SkipsWhenMetaUnavailable(t *testing.T) {
	s, repo, meta, _ := newTestService(t, true)
	meta.err = errors.New("db down")

	s.Record("u1", "192.0.2.1")
	require.Empty(t, repo.calls())

	// meta が読めるようになったら再開する (error を恒久的な無効化に倒さない)。
	meta.err = nil
	s.Record("u1", "192.0.2.1")
	assert.Len(t, repo.calls(), 1)
}

// meta が nil で返ってきたときも書かない。
func TestService_SkipsWhenMetaIsNil(t *testing.T) {
	repo := &fakeObserver{}
	s := NewService(repo, nilMeta{}, time.Hour)
	s.dispatch = func(f func()) { f() }

	s.Record("u1", "192.0.2.1")

	assert.Empty(t, repo.calls())
}

type nilMeta struct{}

func (nilMeta) Fetch() (*model.Meta, error) { return nil, nil }

// 窓の中の 2 回目は書かない。書くとリクエストのたびに UPDATE が走る。
func TestService_DedupesWithinWindow(t *testing.T) {
	s, repo, _, clock := newTestService(t, true)

	s.Record("u1", "192.0.2.1")
	s.Record("u1", "192.0.2.1")
	*clock = clock.Add(59 * time.Minute)
	s.Record("u1", "192.0.2.1")

	assert.Len(t, repo.calls(), 1)

	// 別の IP / 別の利用者は抑制されない。
	s.Record("u1", "198.51.100.7")
	s.Record("u2", "192.0.2.1")
	assert.Len(t, repo.calls(), 3)
}

// 窓を越えたら書き直す。越えないと lastSeenAt が更新されず、保持の刈り取りで
// 現役の IP が消える。
func TestService_RecordsAgainAfterWindow(t *testing.T) {
	s, repo, _, clock := newTestService(t, true)

	s.Record("u1", "192.0.2.1")
	*clock = clock.Add(time.Hour)
	s.Record("u1", "192.0.2.1")

	got := repo.calls()
	require.Len(t, got, 2)
	assert.True(t, got[1].at.After(got[0].at))
}

// **書き込みに失敗したら抑制を解く。** 解かないと、DB が一瞬落ちただけで
// その (利用者, IP) の観測を窓ぶん丸ごと落とす。
func TestService_RetriesAfterWriteFailure(t *testing.T) {
	s, repo, _, _ := newTestService(t, true)
	repo.fail = 1

	s.Record("u1", "192.0.2.1")
	require.Empty(t, repo.calls())

	s.Record("u1", "192.0.2.1")
	assert.Len(t, repo.calls(), 1)
}

// 未配線でも落ちない。
func TestService_UnwiredIsNoOp(t *testing.T) {
	var nilService *Service
	assert.NotPanics(t, func() { nilService.Record("u1", "192.0.2.1") })
	assert.NotPanics(t, func() { NewService(nil, &fakeMeta{enabled: true}, 0).Record("u1", "192.0.2.1") })
	assert.NotPanics(t, func() { NewService(&fakeObserver{}, nil, 0).Record("u1", "192.0.2.1") })
}

func TestNewService_DefaultsWindow(t *testing.T) {
	assert.Equal(t, DedupeWindow, NewService(&fakeObserver{}, &fakeMeta{}, 0).window)
	assert.Equal(t, DedupeWindow, NewService(&fakeObserver{}, &fakeMeta{}, -time.Second).window)
	assert.Equal(t, time.Minute, NewService(&fakeObserver{}, &fakeMeta{}, time.Minute).window)
}

// 既定の dispatch は goroutine なので、リクエストが DB の応答を待たない。
//
// **「走ったこと」を見るだけでは空虚。** 同期実装でも当然走るので、それだと
// `go f()` を `f()` に変えた変異が素通りする (実測)。**呼び出し側が f の完了を
// 待たずに戻ること**を見る。
func TestService_DefaultDispatchIsAsynchronous(t *testing.T) {
	s := NewService(&fakeObserver{}, &fakeMeta{enabled: true}, time.Hour)

	started := make(chan struct{})
	release := make(chan struct{})
	returned := make(chan struct{})

	go func() {
		s.dispatch(func() {
			close(started)
			<-release
		})
		// 同期実装ならここへ来るのは close(release) の後になる。
		close(returned)
	}()

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("既定の dispatch が走らない")
	}
	select {
	case <-returned:
	case <-time.After(2 * time.Second):
		t.Fatal("dispatch が f の完了を待っている (同期になっている)")
	}
	close(release)
}

// **`Record` が dispatch を経由すること。** 他の全テストが dispatch を同期に
// 差し替えるので、`Record` が `s.dispatch(...)` を通らずに直接 `write` を呼ぶ形へ
// 変えても全部緑になる (実測)。既定の dispatch のまま、observer を止めて
// `Record` が先に戻ることを見る。
//
// これが崩れると、**認証済みリクエストのたびに `user_ip` の書き込みを待つ**という、
// この設計が避けたい状態になる。
func TestService_RecordDoesNotWaitForTheWrite(t *testing.T) {
	blocked := make(chan struct{})
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })

	repo := &blockingObserver{entered: blocked, release: release}
	s := NewService(repo, &fakeMeta{enabled: true}, time.Hour)

	returned := make(chan struct{})
	go func() {
		s.Record("u1", "192.0.2.1")
		close(returned)
	}()

	select {
	case <-blocked:
	case <-time.After(2 * time.Second):
		t.Fatal("書き込みが始まらない")
	}
	select {
	case <-returned:
	case <-time.After(2 * time.Second):
		t.Fatal("Record が書き込みの完了を待っている (dispatch を経由していない)")
	}
}

type blockingObserver struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (b *blockingObserver) Observe(string, string, time.Time) error {
	b.once.Do(func() { close(b.entered) })
	<-b.release
	return nil
}

// 並行に呼ばれても 1 回しか書かない (重複排除が競合で破れない)。
func TestService_ConcurrentRecordsWriteOnce(t *testing.T) {
	repo := &fakeObserver{}
	s := NewService(repo, &fakeMeta{enabled: true}, time.Hour)
	s.dispatch = func(f func()) { f() }

	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.Record("u1", "192.0.2.1")
		}()
	}
	wg.Wait()

	assert.Len(t, repo.calls(), 1)
}
