package backup

import (
	"context"
	"fmt"
)

// DeleteGeneration removes every object of generation id and returns the
// number of bytes freed. It returns ErrNotFound when the generation has no
// objects.
//
// meta.json goes first. A generation without meta.json is incomplete
// (ListGenerations / Generation.Complete), so a deletion that stops half-way
// never leaves something that looks like a usable generation whose dump is
// gone. The freed count includes what was deleted before a failure.
//
// 管理画面の削除 (#3462) と、daemon の世代の整理 (#3460) が同じ順序で消すよう、
// ここに1つだけ置く。
func DeleteGeneration(ctx context.Context, st Storage, id string) (int64, error) {
	if !ValidID(id) {
		return 0, fmt.Errorf("backup: invalid generation id %q", id)
	}
	objs, err := st.List(ctx, Key(id, ""))
	if err != nil {
		return 0, err
	}
	if len(objs) == 0 {
		return 0, ErrNotFound
	}
	metaKey := Key(id, MetaFile)
	var freed int64
	for _, o := range objs {
		if o.Key != metaKey {
			continue
		}
		if err := st.Delete(ctx, o.Key); err != nil {
			return 0, fmt.Errorf("backup: delete %s: %w", o.Key, err)
		}
		freed += o.Size
	}
	for _, o := range objs {
		if o.Key == metaKey {
			continue
		}
		if err := st.Delete(ctx, o.Key); err != nil {
			return freed, fmt.Errorf("backup: delete %s: %w", o.Key, err)
		}
		freed += o.Size
	}
	return freed, nil
}
