package repository

import (
	"sync"
	"time"

	"github.com/elythia-network/elythia/internal/model"
)

// defaultAntennaCacheTTL is the backstop for a missed invalidation.
//
// **無効化漏れを「永久に古い」にしないための保険**であって、これを当てにして
// 無効化を省くためのものではない。upstream の AntennaService は TTL 無しの
// pubsub 無効化だけだが、無効化を 1 経路でも落とすと「antenna を編集しても
// 反映されない」がデバッグ困難な形で残る。CachedMetaRepository (#1740) と同じ
// 組み合わせにしてある。
const defaultAntennaCacheTTL = time.Minute

// CachedAntennaRepository wraps an AntennaRepository with an in-process cache
// of ListAllActive (#2752).
//
// **ListAllActive は inbound note 1 件ごとに引かれる** (antenna.Service の
// OnNoteCreated)。#2743 でこの経路に連合の inbound 全量が乗ったので、note
// あたり 1 クエリが常時流れる。upstream は同じ位置でプロセス内キャッシュを持ち
// (AntennaService.getAntennas)、`antennaCreated` / `antennaUpdated` /
// `antennaDeleted` の内部イベントで無効化する。
//
// **無効化の網羅は interface が担保する。** 書き込みメソッドはこの wrapper が
// すべて実装しているので、AntennaRepository に新しい書き込みを足すと wrapper
// 側もコンパイルエラーで気付ける。
//
// **ただし repository を経由しない書き込みは既に 1 つある。** `antenna.userId`
// は `ON DELETE CASCADE` (migration 000007) なので、アカウントの hard delete
// (`HardDeleteUser`) は antenna 行を消すがここを通らない。TS が作った DB では
// `userListId` にも upstream の CASCADE が残るため、user list の削除でも同じ
// 経路が開く (mk-go 生まれの DB にはこの FK が無い)。どちらも TTL が保険。
//
// 返す slice と要素は**呼び出しごとの複製**。キャッシュ本体を渡すと、
// antenna.Service の move 追随 (`a.Users = next`) のような呼び出し側の書き換えが
// 他 goroutine の読み取りと競合する。
type CachedAntennaRepository struct {
	inner AntennaRepository
	ttl   time.Duration

	mu   sync.RWMutex
	rows []*model.Antenna
	at   time.Time

	// onChange は書き込み成功時に呼ばれる post-update hook。cross-worker
	// invalidation のため、自プロセスの invalidate に加えて他 worker へ
	// antennaUpdated を publish するのに使う。nil 安全。
	onChange func()
}

// **interface 充足をこのファイルで固定する。** 「新しい書き込みを足すと
// コンパイルエラーで気付ける」という上の主張は、wrapper が interface を満たす
// ことに依存している。その根拠が router の配線 (別パッケージ) にしか無いと、
// 配線を変えた瞬間に主張が崩れる。
var _ AntennaRepository = (*CachedAntennaRepository)(nil)

// NewCachedAntennaRepository wraps inner with the default TTL.
func NewCachedAntennaRepository(inner AntennaRepository) *CachedAntennaRepository {
	return &CachedAntennaRepository{inner: inner, ttl: defaultAntennaCacheTTL}
}

// NewCachedAntennaRepositoryWithTTL wraps inner with a custom TTL (tests).
func NewCachedAntennaRepositoryWithTTL(inner AntennaRepository, ttl time.Duration) *CachedAntennaRepository {
	return &CachedAntennaRepository{inner: inner, ttl: ttl}
}

// SetInvalidationHook registers a callback fired after a successful write, in
// addition to the local invalidation. nil-safe.
func (c *CachedAntennaRepository) SetInvalidationHook(fn func()) {
	c.onChange = fn
}

// Invalidate drops the cached rows. Exported so a cross-worker subscriber can
// drop this process's cache on a remote antennaUpdated event.
func (c *CachedAntennaRepository) Invalidate() {
	c.mu.Lock()
	c.rows = nil
	c.mu.Unlock()
}

// ListAllActive returns the active antennas, serving them from cache.
func (c *CachedAntennaRepository) ListAllActive() ([]*model.Antenna, error) {
	c.mu.RLock()
	if c.rows != nil && time.Since(c.at) < c.ttl {
		out := copyAntennas(c.rows)
		c.mu.RUnlock()
		return out, nil
	}
	c.mu.RUnlock()

	c.mu.Lock()
	defer c.mu.Unlock()
	// ダブルチェック: RUnlock〜Lock の間に別 goroutine が詰めていることがある。
	if c.rows != nil && time.Since(c.at) < c.ttl {
		return copyAntennas(c.rows), nil
	}
	rows, err := c.inner.ListAllActive()
	if err != nil {
		return nil, err
	}
	// **空の結果もキャッシュする。** active antenna が 0 件のインスタンスは
	// 珍しくなく (実測でそうだった)、nil を「未キャッシュ」と区別しないと
	// そこだけ毎 note クエリが残る。
	if rows == nil {
		rows = []*model.Antenna{}
	}
	c.rows = rows
	c.at = time.Now()
	return copyAntennas(rows), nil
}

// copyAntennas returns a shallow copy of each row.
//
// **shallow なので slice の backing array は共有する。** 現在の唯一の書き換え
// 元 (`OnMoveAccount` の `a.Users = append(append(model.StringArray{}, a.Users...), x)`)
// は新しい配列を作ってから差し替えるので安全。
//
// **`a.Users = append(a.Users, x)` と書くと安全でない** — 容量が余っていれば
// backing array を in-place で書くので、キャッシュ側にも見える。「slice ごと
// 差し替えれば安全」ではなく「**新しい配列を作れば**安全」。それを守れない
// 呼び出し元を足すなら deep copy が要る。
func copyAntennas(rows []*model.Antenna) []*model.Antenna {
	out := make([]*model.Antenna, 0, len(rows))
	for _, a := range rows {
		// nil 要素は現状のどの実装からも来ない (等価変異で、外してもテストは
		// 落ちない)。来たときに panic するより落とすほうがましという判断で
		// 残している。
		if a == nil {
			continue
		}
		cp := *a
		out = append(out, &cp)
	}
	return out
}

// --- 書き込み: すべて invalidate する ---

func (c *CachedAntennaRepository) Create(a *model.Antenna) error {
	return c.afterWrite(c.inner.Create(a))
}

// UpdateFields writes through and invalidates, **except for a lastUsedAt-only
// bump**.
//
// `antennas/notes` は読むたびに lastUsedAt を bump する。ここで invalidate すると
// antenna タイムラインを開くたびに全 worker の cache が落ち、cache が最も要る
// 構成でこそ効かなくなる。lastUsedAt は ListAllActive の消費者
// (OnNoteCreated の matchNote / OnMoveAccount) が読まないので、cache 上で古い
// ままでも判定は変わらない。deactivate の cron は DB を直接見る。
//
// **ListAllActive の戻り値から lastUsedAt を読む消費者を足すなら、この最適化を
// 外すこと。** 最大 TTL ぶん古い値が見える。
func (c *CachedAntennaRepository) UpdateFields(antennaID string, fields map[string]any) error {
	err := c.inner.UpdateFields(antennaID, fields)
	if err != nil || isLastUsedAtOnly(fields) {
		return err
	}
	return c.afterWrite(nil)
}

// isLastUsedAtOnly reports whether fields touches nothing but lastUsedAt.
func isLastUsedAtOnly(fields map[string]any) bool {
	if len(fields) != 1 {
		return false
	}
	_, ok := fields["lastUsedAt"]
	return ok
}

func (c *CachedAntennaRepository) Delete(a *model.Antenna) error {
	return c.afterWrite(c.inner.Delete(a))
}

func (c *CachedAntennaRepository) DeactivateUnusedSince(cutoff time.Time) (int64, error) {
	n, err := c.inner.DeactivateUnusedSince(cutoff)
	return n, c.afterWrite(err)
}

func (c *CachedAntennaRepository) afterWrite(err error) error {
	if err != nil {
		return err
	}
	c.Invalidate()
	if c.onChange != nil {
		c.onChange()
	}
	return nil
}

// --- 読み取り: そのまま委譲 ---

func (c *CachedAntennaRepository) FindByID(id string) (*model.Antenna, error) {
	if !storable(id) {
		return nil, ErrNotFound
	}
	return c.inner.FindByID(id)
}

func (c *CachedAntennaRepository) ListByUser(userID string) ([]*model.Antenna, error) {
	return c.inner.ListByUser(userID)
}

func (c *CachedAntennaRepository) CountByUser(userID string) (int64, error) {
	return c.inner.CountByUser(userID)
}
