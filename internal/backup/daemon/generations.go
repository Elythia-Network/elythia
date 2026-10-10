package daemon

import (
	"context"
	"fmt"
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
	// VerifyUnreadable means verify.json exists but could not be read or
	// parsed.
	VerifyUnreadable VerifyState = "unreadable"
)

// Generation is one generation found in the storage, classified for
// retention and delay checks.
type Generation struct {
	ID   string
	Time time.Time
	// Complete reports whether meta.json is readable and the dump it names
	// exists (backup.Generation.Complete). A generation without it is still
	// being written, was abandoned or is broken.
	Complete bool
	Verify   VerifyState
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

// Scan lists the generations in st, oldest first. It reads them with
// backup.ListGenerations (#3458), the same listing `backup list` and the
// admin page use.
//
// meta.json や verify.json が読めなかった世代は、理由 (壊れている、保存先の一時的な
// 誤り) に関わらず「使えない世代」に倒す。使えない世代が増えても、PlanPrune の窓は
// 古い方へ広がるだけで、消す世代は増えない。読めないことを理由に新しい世代を消す
// ことは起きない。
func Scan(ctx context.Context, st backup.Storage) ([]Generation, error) {
	listed, err := backup.ListGenerations(ctx, st)
	if err != nil {
		return nil, fmt.Errorf("list generations: %w", err)
	}
	gens := make([]Generation, 0, len(listed))
	for _, l := range listed {
		t, _ := backup.IDTime(l.ID)
		g := Generation{ID: l.ID, Time: t, Complete: l.Complete(), Verify: VerifyNone, Size: l.Size}
		verifyKey := backup.Key(l.ID, backup.VerifyFile)
		for _, o := range l.Objects {
			if o.Key == verifyKey {
				g.Verify = VerifyUnreadable
			}
		}
		if l.Verify != nil {
			g.Verify = VerifyFailed
			if l.Verify.OK {
				g.Verify = VerifyOK
			}
		}
		gens = append(gens, g)
	}
	return gens, nil
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
