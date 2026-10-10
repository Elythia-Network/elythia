package backup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
)

// maxMetaBytes bounds how much of meta.json / verify.json is read. Row counts
// of a few thousand tables fit easily.
const maxMetaBytes = 16 << 20

// Generation is one generation found in the storage.
type Generation struct {
	ID string
	// Meta is nil when the generation has no meta.json (it was interrupted
	// or is still being written) or when MetaError is set.
	Meta *Meta
	// MetaError is why meta.json could not be read, such as an unknown
	// format version.
	MetaError error
	// Verify is nil until `backup verify` has written verify.json.
	Verify *VerifyResult
	// Size is the total size of the objects of the generation.
	Size    int64
	Objects []ObjectInfo
}

// Complete reports whether the generation has a readable meta.json.
func (g Generation) Complete() bool { return g.Meta != nil }

// ListGenerations returns the generations in the storage, oldest first.
func ListGenerations(ctx context.Context, st Storage) ([]Generation, error) {
	objs, err := st.List(ctx, GenerationPrefix())
	if err != nil {
		return nil, err
	}
	byID := map[string]*Generation{}
	for _, o := range objs {
		id := GenerationIDFromKey(o.Key)
		if id == "" {
			continue
		}
		g := byID[id]
		if g == nil {
			g = &Generation{ID: id}
			byID[id] = g
		}
		g.Size += o.Size
		g.Objects = append(g.Objects, o)
	}
	out := make([]Generation, 0, len(byID))
	for _, g := range byID {
		for _, o := range g.Objects {
			switch o.Key {
			case Key(g.ID, MetaFile):
				g.Meta, g.MetaError = ReadMeta(ctx, st, g.ID)
			case Key(g.ID, VerifyFile):
				// verify.json が読めなくても世代そのものは一覧に出す。
				g.Verify, _ = ReadVerify(ctx, st, g.ID)
			}
		}
		out = append(out, *g)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// ReadMeta reads and decodes meta.json of generation id. It rejects format
// versions it does not know.
func ReadMeta(ctx context.Context, st Storage, id string) (*Meta, error) {
	var m Meta
	if err := readJSON(ctx, st, id, MetaFile, &m); err != nil {
		return nil, err
	}
	if m.FormatVersion != MetaFormatVersion {
		return nil, fmt.Errorf("backup: %s has formatVersion %d, this elythia reads %d", Key(id, MetaFile), m.FormatVersion, MetaFormatVersion)
	}
	if m.ID != id {
		return nil, fmt.Errorf("backup: %s is for generation %q", Key(id, MetaFile), m.ID)
	}
	return &m, nil
}

// ReadVerify reads verify.json of generation id.
func ReadVerify(ctx context.Context, st Storage, id string) (*VerifyResult, error) {
	var v VerifyResult
	if err := readJSON(ctx, st, id, VerifyFile, &v); err != nil {
		return nil, err
	}
	return &v, nil
}

func readJSON(ctx context.Context, st Storage, id, name string, v any) error {
	if !ValidID(id) {
		return fmt.Errorf("backup: invalid generation id %q", id)
	}
	r, err := st.Get(ctx, Key(id, name))
	if err != nil {
		return err
	}
	defer r.Close()
	body, err := io.ReadAll(io.LimitReader(r, maxMetaBytes+1))
	if err != nil {
		return fmt.Errorf("backup: read %s: %w", Key(id, name), err)
	}
	if len(body) > maxMetaBytes {
		return errors.New("backup: " + Key(id, name) + " is too large")
	}
	if err := json.Unmarshal(body, v); err != nil {
		return fmt.Errorf("backup: decode %s: %w", Key(id, name), err)
	}
	return nil
}
