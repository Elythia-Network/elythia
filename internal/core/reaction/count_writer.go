package reaction

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

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
	// SCAN でバッファキーを探す
	var cursor uint64
	var flushed int
	for {
		keys, next, err := w.rdb.Scan(ctx, cursor, bufferKeyPrefix+"*", 100).Result()
		if err != nil {
			return fmt.Errorf("reaction flush: scan: %w", err)
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
	return nil
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
