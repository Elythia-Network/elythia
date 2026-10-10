package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"time"

	"github.com/elythia-network/elythia/internal/backup"
)

// VerifyState is what verify.json says about a generation.
type VerifyState string

// Verify states of a generation.
const (
	// VerifyNone means verify.json does not exist.
	VerifyNone VerifyState = "none"
	// VerifyOK means verify.json exists and reports OK.
	VerifyOK VerifyState = "ok"
	// VerifyFailed means verify.json reports a failure.
	VerifyFailed VerifyState = "failed"
	// VerifyUnreadable means verify.json exists but could not be parsed.
	VerifyUnreadable VerifyState = "unreadable"
)

// maxVerifyFileSize bounds how much of verify.json is read.
const maxVerifyFileSize = 1 << 20

// Generation is one generation directory found in the storage.
type Generation struct {
	ID   string
	Time time.Time
	// Complete reports whether meta.json exists. A generation without it is
	// still being written or was abandoned.
	Complete bool
	Verify   VerifyState
	Keys     []string
	Size     int64
}

// Usable reports whether the generation counts as a backup for retention
// and delay checks. requireVerified is the schedule's Verify option.
//
// 検証を毎回回す設定では、検証に通ったものだけを数える。回さない設定では、
// 揃っていて検証に落ちていないものを数える (手で確かめて落ちたものは数えない)。
func (g Generation) Usable(requireVerified bool) bool {
	if !g.Complete {
		return false
	}
	if requireVerified {
		return g.Verify == VerifyOK
	}
	return g.Verify == VerifyOK || g.Verify == VerifyNone
}

// Scan lists the generations in st, oldest first.
func Scan(ctx context.Context, st backup.Storage) ([]Generation, error) {
	objs, err := st.List(ctx, backup.GenerationPrefix())
	if err != nil {
		return nil, fmt.Errorf("list generations: %w", err)
	}
	byID := map[string]*Generation{}
	for _, o := range objs {
		id := backup.GenerationIDFromKey(o.Key)
		if id == "" {
			continue
		}
		g, ok := byID[id]
		if !ok {
			t, _ := backup.IDTime(id)
			g = &Generation{ID: id, Time: t, Verify: VerifyNone}
			byID[id] = g
		}
		g.Keys = append(g.Keys, o.Key)
		g.Size += o.Size
		switch o.Key {
		case backup.Key(id, backup.MetaFile):
			g.Complete = true
		case backup.Key(id, backup.VerifyFile):
			g.Verify = VerifyUnreadable
		}
	}
	gens := make([]Generation, 0, len(byID))
	for _, g := range byID {
		if g.Verify == VerifyUnreadable {
			state, err := readVerifyState(ctx, st, g.ID)
			if err != nil {
				return nil, err
			}
			g.Verify = state
		}
		gens = append(gens, *g)
	}
	// ID は UTC の時刻を固定幅で書いたものなので、文字列の順が時刻の順になる。
	sort.Slice(gens, func(i, j int) bool { return gens[i].ID < gens[j].ID })
	return gens, nil
}

func readVerifyState(ctx context.Context, st backup.Storage, id string) (VerifyState, error) {
	rc, err := st.Get(ctx, backup.Key(id, backup.VerifyFile))
	if errors.Is(err, backup.ErrNotFound) {
		// List の後に消された。
		return VerifyNone, nil
	}
	if err != nil {
		return "", fmt.Errorf("read %s: %w", backup.Key(id, backup.VerifyFile), err)
	}
	defer rc.Close()
	var res backup.VerifyResult
	if err := json.NewDecoder(io.LimitReader(rc, maxVerifyFileSize)).Decode(&res); err != nil {
		return VerifyUnreadable, nil
	}
	if res.OK {
		return VerifyOK, nil
	}
	return VerifyFailed, nil
}

// Latest returns the newest generation in gens (oldest first) that satisfies
// ok, or nil.
func Latest(gens []Generation, ok func(Generation) bool) *Generation {
	for i := len(gens) - 1; i >= 0; i-- {
		if ok(gens[i]) {
			return &gens[i]
		}
	}
	return nil
}

// PlanPrune returns the generations to delete, oldest first.
//
// It keeps the newest keep usable generations and everything newer than the
// oldest of them. Everything older than that (usable or not, complete or
// not) is deleted. Nothing is deleted while there are fewer than keep usable
// generations, or when keep is 0.
//
// 古い世代は、使える新しい世代が keep 個そろってから消す。壊れたバックアップしか
// 残らない状態を作らないため。検証に落ちた世代や meta.json の無い半端な世代も、
// keep 個の窓より新しい間は残す: 落ちた理由を調べられるようにするためと、半端な
// 世代は今まさに書いている途中かもしれないため。窓より古くなったら、使える世代が
// keep 個新しくある以上、残す理由が無いので消す (残すと容量の料金が増え続ける)。
func PlanPrune(gens []Generation, keep int, requireVerified bool) []Generation {
	if keep <= 0 {
		return nil
	}
	seen := 0
	cut := -1
	for i := len(gens) - 1; i >= 0; i-- {
		if gens[i].Usable(requireVerified) {
			seen++
			if seen == keep {
				cut = i
				break
			}
		}
	}
	if cut <= 0 {
		return nil
	}
	return append([]Generation(nil), gens[:cut]...)
}

// DeleteGeneration removes every object of g. meta.json goes first so that a
// generation interrupted half-way is seen as incomplete, never as a complete
// generation whose dump is missing.
func DeleteGeneration(ctx context.Context, st backup.Storage, g Generation) error {
	meta := backup.Key(g.ID, backup.MetaFile)
	keys := make([]string, 0, len(g.Keys))
	for _, k := range g.Keys {
		if k == meta {
			if err := st.Delete(ctx, k); err != nil {
				return fmt.Errorf("delete %s: %w", k, err)
			}
			continue
		}
		keys = append(keys, k)
	}
	for _, k := range keys {
		if err := st.Delete(ctx, k); err != nil {
			return fmt.Errorf("delete %s: %w", k, err)
		}
	}
	return nil
}
