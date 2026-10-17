package reaction

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/elythia-network/elythia/internal/repository"
	"github.com/redis/go-redis/v9"
)

// ReactionCountWriter abstracts the reaction count persistence strategy.
// direct は即時 DB 更新、buffered は Redis にバッファして定期 flush する。
type ReactionCountWriter interface {
	Increment(noteID, reaction string, delta int) error
	// Flush persists all buffered counts to DB. direct 実装では no-op。
	Flush(ctx context.Context) error
	// GetBuffered returns buffered (unflushed) reaction deltas for a note.
	// direct 実装では nil を返す。read merge 時に使用する。
	GetBuffered(ctx context.Context, noteID string) (map[string]int64, error)
	// GetBufferedMany returns buffered deltas keyed by noteID for the
	// given set in a single round-trip (Redis pipeline)。PackNotes 等の
	// batched read で N+1 を回避するために使う (#647)。direct 実装では
	// nil を返す。
	GetBufferedMany(ctx context.Context, noteIDs []string) (map[string]map[string]int64, error)
}

const bufferKeyPrefix = "reaction-buffer:"

// --- Direct (no buffering) ---

type directWriter struct {
	noteRepo repository.NoteRepository
}

// NewDirectWriter returns a writer that updates the DB immediately.
func NewDirectWriter(noteRepo repository.NoteRepository) ReactionCountWriter {
	return &directWriter{noteRepo: noteRepo}
}

func (w *directWriter) Increment(noteID, reaction string, delta int) error {
	return w.noteRepo.IncrementReaction(noteID, reaction, delta)
}

func (w *directWriter) Flush(_ context.Context) error { return nil }

func (w *directWriter) GetBuffered(_ context.Context, _ string) (map[string]int64, error) {
	return nil, nil
}

func (w *directWriter) GetBufferedMany(_ context.Context, _ []string) (map[string]map[string]int64, error) {
	return nil, nil
}

// --- Buffered (Redis → periodic DB flush) ---

type bufferedWriter struct {
	rdb      *redis.Client
	noteRepo repository.NoteRepository
}

// NewBufferedWriter returns a writer that buffers in Redis.
func NewBufferedWriter(rdb *redis.Client, noteRepo repository.NoteRepository) ReactionCountWriter {
	return &bufferedWriter{rdb: rdb, noteRepo: noteRepo}
}

func (w *bufferedWriter) Increment(noteID, reaction string, delta int) error {
	key := bufferKeyPrefix + noteID
	return w.rdb.HIncrBy(context.Background(), key, reaction, int64(delta)).Err()
}

// Flush reads all buffered keys, applies them to DB, and deletes the keys.
// Lua script で HGETALL + DEL をアトミックに実行し、flush 中の Increment
// ロスを防ぐ。
func (w *bufferedWriter) Flush(ctx context.Context) error {
	_, err := w.flushAll(ctx)
	return err
}

// flushAll is Flush that also reports how many keys it found.
func (w *bufferedWriter) flushAll(ctx context.Context) (int, error) {
	// SCAN でバッファキーを探す
	var cursor uint64
	var flushed int
	for {
		keys, next, err := w.rdb.Scan(ctx, cursor, bufferKeyPrefix+"*", 100).Result()
		if err != nil {
			return flushed, fmt.Errorf("reaction flush: scan: %w", err)
		}
		for _, key := range keys {
			if err := w.flushKey(ctx, key); err != nil {
				slog.Error("reaction flush: key failed", "key", key, "error", err)
				continue
			}
			flushed++
		}
		cursor = next
		if cursor == 0 {
			break
		}
	}
	if flushed > 0 {
		slog.Info("reaction flush: completed", "notes", flushed)
	}
	return flushed, nil
}

// flushKey atomically reads and deletes one buffer key, then applies
// the deltas to the DB.
func (w *bufferedWriter) flushKey(ctx context.Context, key string) error {
	// Lua: HGETALL + DEL をアトミックに実行
	script := redis.NewScript(`
		local data = redis.call('HGETALL', KEYS[1])
		if #data > 0 then
			redis.call('DEL', KEYS[1])
		end
		return data
	`)
	result, err := script.Run(ctx, w.rdb, []string{key}).StringSlice()
	if err != nil {
		if err == redis.Nil {
			return nil
		}
		return err
	}
	if len(result) == 0 {
		return nil
	}

	noteID := key[len(bufferKeyPrefix):]
	deltas := make(map[string]int64, len(result)/2)
	for i := 0; i < len(result)-1; i += 2 {
		var v int64
		if _, err := fmt.Sscanf(result[i+1], "%d", &v); err != nil {
			// 値は mk-go 自身が書いた整数なので、読めないのは破損。0 として
			// 記録すると「増分なし」と区別が付かないので飛ばす。
			continue
		}
		deltas[result[i]] = v
	}

	for reaction, delta := range deltas {
		if err := w.noteRepo.IncrementReaction(noteID, reaction, int(delta)); err != nil {
			slog.Error("reaction flush: DB increment failed", "note", noteID, "reaction", reaction, "error", err)
		}
	}
	return nil
}

func (w *bufferedWriter) GetBuffered(ctx context.Context, noteID string) (map[string]int64, error) {
	key := bufferKeyPrefix + noteID
	result, err := w.rdb.HGetAll(ctx, key).Result()
	if err != nil {
		return nil, err
	}
	if len(result) == 0 {
		return nil, nil
	}
	deltas := make(map[string]int64, len(result))
	for k, v := range result {
		var n int64
		if _, err := fmt.Sscanf(v, "%d", &n); err != nil {
			continue
		}
		deltas[k] = n
	}
	return deltas, nil
}

// GetBufferedMany batch-fetches buffered deltas for noteIDs via a single
// Redis pipeline of HGETALL calls. 空 / 不在の note は結果 map に含めない。
// noteIDs が空または nil なら nil を返す。
func (w *bufferedWriter) GetBufferedMany(ctx context.Context, noteIDs []string) (map[string]map[string]int64, error) {
	if len(noteIDs) == 0 {
		return nil, nil
	}
	pipe := w.rdb.Pipeline()
	cmds := make(map[string]*redis.MapStringStringCmd, len(noteIDs))
	for _, id := range noteIDs {
		if id == "" {
			continue
		}
		if _, dup := cmds[id]; dup {
			continue
		}
		cmds[id] = pipe.HGetAll(ctx, bufferKeyPrefix+id)
	}
	if _, err := pipe.Exec(ctx); err != nil && err != redis.Nil {
		return nil, fmt.Errorf("reaction GetBufferedMany: pipeline exec: %w", err)
	}
	out := make(map[string]map[string]int64, len(cmds))
	for id, cmd := range cmds {
		result, err := cmd.Result()
		if err != nil {
			// 個別 key error は warn 相当、batch を投げ捨てるほどでない。
			continue
		}
		if len(result) == 0 {
			continue
		}
		deltas := make(map[string]int64, len(result))
		for k, v := range result {
			var n int64
			if _, err := fmt.Sscanf(v, "%d", &n); err != nil {
				continue
			}
			deltas[k] = n
		}
		out[id] = deltas
	}
	return out, nil
}

// MergeReactions merges buffered deltas into a note's existing reactions
// JSON (map[string]float64 from JSON unmarshal). Returns the merged JSON.
func MergeReactions(reactionsJSON []byte, buffered map[string]int64) []byte {
	if len(buffered) == 0 {
		return reactionsJSON
	}
	var reactions map[string]float64
	if len(reactionsJSON) > 0 {
		_ = json.Unmarshal(reactionsJSON, &reactions)
	}
	if reactions == nil {
		reactions = make(map[string]float64)
	}
	for k, v := range buffered {
		reactions[k] += float64(v)
		if reactions[k] <= 0 {
			delete(reactions, k)
		}
	}
	out, _ := json.Marshal(reactions)
	return out
}

// --- Switchable (follows meta.enableReactionsBuffering at runtime) ---

// FlushInterval is how often the queue role enqueues a reaction flush.
const FlushInterval = 30 * time.Second

// BufferDrainWindow is how long a SwitchableWriter keeps merging and flushing
// the Redis buffer after buffering was turned off (or at startup with buffering
// off).
//
// **meta のキャッシュの期限より長くする。** 無効の通知を取りこぼしたプロセスは、
// meta のキャッシュが切れて tick が読み直すまで (最大 MetaCacheTTL +
// FlushInterval) 溜め続ける。窓がそれより先に閉じると、そのプロセスが溜めた分を
// 他のプロセスが移さずに DB へ書き、順序が崩れる。さらに flush が数回回る分の
// 余裕を足す。窓の内の flush がまだ残りを見つけたら、窓を延ばす (Flush)。
const BufferDrainWindow = repository.MetaCacheTTL + 3*FlushInterval

// SwitchableWriter chooses between the direct and the buffered writer on every
// call, so meta.enableReactionsBuffering can change without rebuilding the
// server (#3663).
//
// 切り替えの途中の扱い:
//   - 1 回の Increment は、そのときの設定に従ってどちらか一方へだけ書く。
//     DB と Redis の両方へは書かないので、二重に足すことはない
//   - 有効 → 無効: Redis に溜まった分は消さない。無効にしてから
//     BufferDrainWindow の間は、読みも溜まった分を足し、queue の役割の
//     ticker も flush を積み続ける (NeedsFlush)
//   - **窓の内で DB へ直接書くときは、先にその note の溜まった分を DB へ移す。**
//     DB の数は 0 未満にならない (IncrementReaction は 0 以下で key を消す)
//     ので、溜まった +1 より先に取り消しの -1 を DB へ書くと -1 が消え、後の
//     flush の +1 だけが残って数が 1 つ多いまま固まる。移してから書けば、
//     DB へ当たる順序が付けた順・取り消した順と同じになる。ただし周期の
//     flush が同じ key を Redis から消して DB へ足すまでの間 (1 回の UPDATE の
//     間) に取り消しが来ると、移す分がもう無く、順序は保証できない。複数の
//     プロセスにまたがって閉じる手段 (DB と Redis をまたぐロック) の重さに
//     見合わないので許している
//   - Flush は設定に関わらず Redis のバッファを DB へ移す (key ごとに
//     HGETALL + DEL を原子的に行うので、複数のプロセスが同時に Flush しても
//     二重に足さない)。無効のときの Flush が残りを見つけたら、その Flush を
//     実行したプロセスの窓を延ばす。
//     queue の遅れや、通知を取りこぼして溜め続けるプロセスがあっても、
//     残りが無くなるまで flush を積み続けるため
//   - 無効 → 有効: 以後の Increment が Redis へ行く。前に DB へ書いた分は
//     そのまま
//
// 本家は有効 → 無効の metaUpdated を受けたプロセスがすぐに bake する。
// ここではすぐには移さず、次の flush (最大 FlushInterval 後) か、その note への
// 次の書き込みで移す。その間も読みには溜まった分を足す。
type SwitchableWriter struct {
	direct    ReactionCountWriter
	buffered  *bufferedWriter
	buffering atomic.Bool
	// drainUntil は、溜まった分を読みに足し flush を積み続ける期限 (UnixNano)。
	drainUntil atomic.Int64
	now        func() time.Time
}

// NewSwitchableWriter returns a writer that starts in the given mode.
//
// 無効で始めるときも BufferDrainWindow の間は Redis のバッファを見る。前の
// プロセスや世代が有効のまま溜めた分を、無効にしてから flush する前に
// 作り直したときに取り残さないため。
func NewSwitchableWriter(rdb *redis.Client, noteRepo repository.NoteRepository, buffering bool) *SwitchableWriter {
	w := &SwitchableWriter{
		direct:   NewDirectWriter(noteRepo),
		buffered: &bufferedWriter{rdb: rdb, noteRepo: noteRepo},
		now:      time.Now,
	}
	w.buffering.Store(buffering)
	if !buffering {
		w.extendDrain()
	}
	return w
}

func (w *SwitchableWriter) extendDrain() {
	w.drainUntil.Store(w.now().Add(BufferDrainWindow).UnixNano())
}

// SetBuffering switches the mode. 有効 → 無効のときだけ、溜まった分を移す期限を
// 延ばす。同じ値を何度渡してもよい (metaUpdated は自プロセスの更新でも届き、
// queue の ticker も毎回読み直す)。
func (w *SwitchableWriter) SetBuffering(on bool) {
	if was := w.buffering.Swap(on); was && !on {
		w.extendDrain()
	}
}

// Buffering reports whether increments currently go to Redis.
func (w *SwitchableWriter) Buffering() bool {
	return w.buffering.Load()
}

// NeedsFlush reports whether the Redis buffer may hold counts: buffering is on,
// or it was turned off less than BufferDrainWindow ago (extended while flushes
// keep finding counts).
func (w *SwitchableWriter) NeedsFlush() bool {
	return w.buffering.Load() || w.now().UnixNano() < w.drainUntil.Load()
}

// Increment writes to the writer of the current mode only. While draining, it
// first moves the note's buffered counts to the DB so that the DB sees the
// increments in order.
func (w *SwitchableWriter) Increment(noteID, reaction string, delta int) error {
	if w.buffering.Load() {
		return w.buffered.Increment(noteID, reaction, delta)
	}
	if w.NeedsFlush() {
		if err := w.buffered.flushKey(context.Background(), bufferKeyPrefix+noteID); err != nil {
			// 移せなくても書き込みは止めない (リアクション自体は成立している)。
			// 残った分は後の flush が移す。
			slog.Warn("reaction: moving the buffered counts before a direct write failed", "note", noteID, "err", err)
		}
	}
	return w.direct.Increment(noteID, reaction, delta)
}

// Flush moves the Redis buffer to the DB whatever the current mode is.
//
// 無効でも移す。設定を見て飛ばすと、有効の間に溜まった分が Redis に残り続ける
// (#1780)。無効のときに残りを見つけたら窓を延ばす。
func (w *SwitchableWriter) Flush(ctx context.Context) error {
	n, err := w.buffered.flushAll(ctx)
	// 延ばすのは残りを見つけたときだけ。SCAN の失敗では延ばさない (次の tick が
	// 試し直す)。延ばすと、Redis が落ちている間ずっと窓が閉じず、無効なのに
	// 書き込みと読みのたびに落ちた Redis を引く。
	if !w.buffering.Load() && n > 0 {
		w.extendDrain()
	}
	return err
}

// SetClock replaces the clock used for the drain window. Intended for tests.
func (w *SwitchableWriter) SetClock(now func() time.Time) {
	w.now = now
}

// GetBuffered returns the unflushed counts while the buffer may hold some.
func (w *SwitchableWriter) GetBuffered(ctx context.Context, noteID string) (map[string]int64, error) {
	if !w.NeedsFlush() {
		// 無効のまま期限を過ぎたら Redis を引かない (読むたびの往復を増やさない)。
		return nil, nil
	}
	return w.buffered.GetBuffered(ctx, noteID)
}

// GetBufferedMany is the batched GetBuffered.
func (w *SwitchableWriter) GetBufferedMany(ctx context.Context, noteIDs []string) (map[string]map[string]int64, error) {
	if !w.NeedsFlush() {
		return nil, nil
	}
	return w.buffered.GetBufferedMany(ctx, noteIDs)
}
