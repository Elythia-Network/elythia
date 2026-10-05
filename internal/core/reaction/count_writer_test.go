package reaction

import (
	"context"
	"testing"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- DirectWriter ---

func TestDirectWriter_Increment(t *testing.T) {
	noteRepo := testutil.NewMockNoteRepository()
	noteRepo.Notes["n1"] = &model.Note{ID: "n1", Reactions: []byte(`{}`)}
	w := NewDirectWriter(noteRepo)
	err := w.Increment("n1", "👍", 1)
	require.NoError(t, err)
}

func TestDirectWriter_Flush(t *testing.T) {
	noteRepo := testutil.NewMockNoteRepository()
	w := NewDirectWriter(noteRepo)
	assert.NoError(t, w.Flush(context.Background()))
}

func TestDirectWriter_GetBuffered(t *testing.T) {
	noteRepo := testutil.NewMockNoteRepository()
	w := NewDirectWriter(noteRepo)
	got, err := w.GetBuffered(context.Background(), "n1")
	assert.NoError(t, err)
	assert.Nil(t, got)
}

// --- MergeReactions ---

func TestMergeReactions_Empty(t *testing.T) {
	result := MergeReactions([]byte(`{"👍":3}`), nil)
	assert.JSONEq(t, `{"👍":3}`, string(result))
}

func TestMergeReactions_AddNew(t *testing.T) {
	result := MergeReactions([]byte(`{"👍":3}`), map[string]int64{"❤": 2})
	assert.JSONEq(t, `{"👍":3,"❤":2}`, string(result))
}

func TestMergeReactions_IncrementExisting(t *testing.T) {
	result := MergeReactions([]byte(`{"👍":3}`), map[string]int64{"👍": 2})
	assert.JSONEq(t, `{"👍":5}`, string(result))
}

func TestMergeReactions_RemoveZero(t *testing.T) {
	result := MergeReactions([]byte(`{"👍":1}`), map[string]int64{"👍": -1})
	assert.JSONEq(t, `{}`, string(result))
}

func TestMergeReactions_NilBase(t *testing.T) {
	result := MergeReactions(nil, map[string]int64{"❤": 5})
	assert.JSONEq(t, `{"❤":5}`, string(result))
}

func TestMergeReactions_InvalidJSON(t *testing.T) {
	result := MergeReactions([]byte(`invalid`), map[string]int64{"❤": 1})
	assert.JSONEq(t, `{"❤":1}`, string(result))
}

// --- BufferedWriter (integration test with testcontainers Redis) ---

func TestBufferedWriter_IncrementAndFlush(t *testing.T) {
	ctx := context.Background()
	tr, err := testutil.SetupRedis(ctx)
	if err != nil {
		t.Skip("Redis unavailable:", err)
	}
	defer tr.Teardown(ctx)

	noteRepo := testutil.NewMockNoteRepository()
	noteRepo.Notes["n1"] = &model.Note{ID: "n1", Reactions: []byte(`{"👍":1}`)}

	w := NewBufferedWriter(tr.Client, noteRepo)

	// Increment 2 回
	require.NoError(t, w.Increment("n1", "👍", 1))
	require.NoError(t, w.Increment("n1", "❤", 3))

	// GetBuffered で未 flush 分を確認
	buf, err := w.GetBuffered(ctx, "n1")
	require.NoError(t, err)
	assert.Equal(t, int64(1), buf["👍"])
	assert.Equal(t, int64(3), buf["❤"])

	// Flush → DB に反映
	require.NoError(t, w.Flush(ctx))

	// flush 後は Redis から消える
	buf2, err := w.GetBuffered(ctx, "n1")
	require.NoError(t, err)
	assert.Nil(t, buf2)
}

func TestBufferedWriter_FlushEmpty(t *testing.T) {
	ctx := context.Background()
	tr, err := testutil.SetupRedis(ctx)
	if err != nil {
		t.Skip("Redis unavailable:", err)
	}
	defer tr.Teardown(ctx)

	noteRepo := testutil.NewMockNoteRepository()
	w := NewBufferedWriter(tr.Client, noteRepo)
	require.NoError(t, w.Flush(ctx))
}

func TestBufferedWriter_GetBuffered_NoKey(t *testing.T) {
	ctx := context.Background()
	tr, err := testutil.SetupRedis(ctx)
	if err != nil {
		t.Skip("Redis unavailable:", err)
	}
	defer tr.Teardown(ctx)

	noteRepo := testutil.NewMockNoteRepository()
	w := NewBufferedWriter(tr.Client, noteRepo)
	buf, err := w.GetBuffered(ctx, "nonexistent")
	require.NoError(t, err)
	assert.Nil(t, buf)
}

// 複数 note の buffered deltas を 1 round-trip でまとめて取れる (#647)。
// 入力に存在しない / 空の noteID が混ざっても結果に含めない。
func TestBufferedWriter_GetBufferedMany(t *testing.T) {
	ctx := context.Background()
	tr, err := testutil.SetupRedis(ctx)
	if err != nil {
		t.Skip("Redis unavailable:", err)
	}
	defer tr.Teardown(ctx)

	noteRepo := testutil.NewMockNoteRepository()
	w := NewBufferedWriter(tr.Client, noteRepo)

	require.NoError(t, w.Increment("n1", ":foo@host.example:", 1))
	require.NoError(t, w.Increment("n1", "👍", 2))
	require.NoError(t, w.Increment("n2", ":bar@.:", 1))

	got, err := w.GetBufferedMany(ctx, []string{"n1", "n2", "n_missing", "", "n1"})
	require.NoError(t, err)

	require.Contains(t, got, "n1")
	assert.Equal(t, int64(1), got["n1"][":foo@host.example:"])
	assert.Equal(t, int64(2), got["n1"]["👍"])

	require.Contains(t, got, "n2")
	assert.Equal(t, int64(1), got["n2"][":bar@.:"])

	assert.NotContains(t, got, "n_missing", "存在しない note は結果に含めない")
	assert.NotContains(t, got, "", "空 noteID は無視")
}

func TestBufferedWriter_GetBufferedMany_EmptyInput(t *testing.T) {
	ctx := context.Background()
	tr, err := testutil.SetupRedis(ctx)
	if err != nil {
		t.Skip("Redis unavailable:", err)
	}
	defer tr.Teardown(ctx)

	noteRepo := testutil.NewMockNoteRepository()
	w := NewBufferedWriter(tr.Client, noteRepo)

	got, err := w.GetBufferedMany(ctx, nil)
	require.NoError(t, err)
	assert.Nil(t, got)

	got, err = w.GetBufferedMany(ctx, []string{})
	require.NoError(t, err)
	assert.Nil(t, got)
}

// directWriter は buffering 機能を持たないので GetBufferedMany は常に nil。
func TestDirectWriter_GetBufferedMany(t *testing.T) {
	noteRepo := testutil.NewMockNoteRepository()
	w := NewDirectWriter(noteRepo)
	got, err := w.GetBufferedMany(context.Background(), []string{"n1", "n2"})
	require.NoError(t, err)
	assert.Nil(t, got)
}
