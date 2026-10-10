package backup

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// recordingStorage records the order of deletions and can fail some of them.
type recordingStorage struct {
	Storage
	deleted []string
	failOn  map[string]error
	listErr error
}

func (r *recordingStorage) Delete(ctx context.Context, key string) error {
	if err := r.failOn[key]; err != nil {
		return err
	}
	r.deleted = append(r.deleted, key)
	return r.Storage.Delete(ctx, key)
}

func (r *recordingStorage) List(ctx context.Context, prefix string) ([]ObjectInfo, error) {
	if r.listErr != nil {
		return nil, r.listErr
	}
	return r.Storage.List(ctx, prefix)
}

func putGenerationFiles(t *testing.T, st Storage, id string, files map[string]string) {
	t.Helper()
	for name, body := range files {
		require.NoError(t, st.Put(context.Background(), Key(id, name), strings.NewReader(body)))
	}
}

func TestDeleteGeneration(t *testing.T) {
	const (
		id    = "20261001T040000Z"
		other = "20261002T040000Z"
	)
	ctx := context.Background()
	newStorage := func(t *testing.T) *recordingStorage {
		t.Helper()
		dir, err := NewDirStorage(markedDir(t))
		require.NoError(t, err)
		// 名前の順では dump.pgc が meta.json より先に来るので、順序を決めているのが
		// DeleteGeneration だと分かる。
		putGenerationFiles(t, dir, id, map[string]string{DumpFile: "dump-bytes", MetaFile: "{meta}", VerifyFile: "{v}"})
		putGenerationFiles(t, dir, other, map[string]string{DumpFile: "x", MetaFile: "{}"})
		return &recordingStorage{Storage: dir}
	}

	t.Run("meta.json first, every object, others stay", func(t *testing.T) {
		st := newStorage(t)
		freed, err := DeleteGeneration(ctx, st, id)
		require.NoError(t, err)
		assert.Equal(t, int64(len("dump-bytes")+len("{meta}")+len("{v}")), freed)
		require.Len(t, st.deleted, 3)
		assert.Equal(t, Key(id, MetaFile), st.deleted[0])
		left, err := st.List(ctx, Key(id, ""))
		require.NoError(t, err)
		assert.Empty(t, left)
		gens, err := ListGenerations(ctx, st)
		require.NoError(t, err)
		require.Len(t, gens, 1)
		assert.Equal(t, other, gens[0].ID)

		_, err = DeleteGeneration(ctx, st, id)
		assert.ErrorIs(t, err, ErrNotFound)
	})
	t.Run("meta.json delete fails", func(t *testing.T) {
		st := newStorage(t)
		st.failOn = map[string]error{Key(id, MetaFile): errors.New("denied")}
		freed, err := DeleteGeneration(ctx, st, id)
		assert.ErrorContains(t, err, "denied")
		assert.Zero(t, freed)
		assert.Empty(t, st.deleted, "nothing else may go while meta.json stays")
	})
	t.Run("dump delete fails after meta.json", func(t *testing.T) {
		st := newStorage(t)
		st.failOn = map[string]error{Key(id, DumpFile): errors.New("denied")}
		freed, err := DeleteGeneration(ctx, st, id)
		assert.ErrorContains(t, err, "denied")
		assert.Equal(t, int64(len("{meta}")), freed)
		gens, err := ListGenerations(ctx, st)
		require.NoError(t, err)
		require.Len(t, gens, 2)
		assert.False(t, gens[0].Complete(), "what is left must look incomplete")
	})
	t.Run("invalid id and list error", func(t *testing.T) {
		st := newStorage(t)
		_, err := DeleteGeneration(ctx, st, "../x")
		assert.ErrorContains(t, err, "invalid generation id")
		st.listErr = errors.New("boom")
		_, err = DeleteGeneration(ctx, st, id)
		assert.ErrorContains(t, err, "boom")
		assert.Empty(t, st.deleted)
	})
}
