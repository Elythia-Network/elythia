package processors

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/elythia-network/elythia/internal/repository"

	"github.com/elythia-network/elythia/internal/queue/driver"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCleanRemoteNotes_Disabled(t *testing.T) {
	noteRepo := testutil.NewMockNoteRepository()
	p := NewCleanRemoteNotesProcessor(noteRepo, func() CleanRemoteNotesConfig { return CleanRemoteNotesConfig{Enabled: false} })
	err := p.Handle(context.Background(), driver.RawTask{TypeName: "test"})
	require.NoError(t, err)
}

func TestCleanRemoteNotes_Enabled(t *testing.T) {
	noteRepo := testutil.NewMockNoteRepository()
	p := NewCleanRemoteNotesProcessor(noteRepo, func() CleanRemoteNotesConfig {
		return CleanRemoteNotesConfig{
			Enabled:              true,
			ExpiryDays:           90,
			MaxProcessingMinutes: 1,
		}
	})
	err := p.Handle(context.Background(), driver.RawTask{TypeName: "test"})
	require.NoError(t, err)
}

func TestCleanRemoteNotes_DefaultValues(t *testing.T) {
	noteRepo := testutil.NewMockNoteRepository()
	p := NewCleanRemoteNotesProcessor(noteRepo, func() CleanRemoteNotesConfig {
		return CleanRemoteNotesConfig{
			Enabled:              true,
			ExpiryDays:           0,
			MaxProcessingMinutes: 0,
		}
	})
	err := p.Handle(context.Background(), driver.RawTask{TypeName: "test"})
	require.NoError(t, err)
}

func TestCleanRemoteNotes_CancelledContext(t *testing.T) {
	noteRepo := testutil.NewMockNoteRepository()
	p := NewCleanRemoteNotesProcessor(noteRepo, func() CleanRemoteNotesConfig {
		return CleanRemoteNotesConfig{
			Enabled:              true,
			ExpiryDays:           90,
			MaxProcessingMinutes: 60,
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := p.Handle(ctx, driver.RawTask{TypeName: "test"})
	assert.NoError(t, err)
}

// **実行中に無効化したら止まること。**
//
// リモートノートの自動削除は不可逆なので、管理画面の停止スイッチが唯一の
// 止め方になる。起動時に固定していると、プロセスを再起動するまで走り続けた。
func TestCleanRemoteNotes_StopsWhenDisabledDuringRun(t *testing.T) {
	repo := &countingNoteRepo{batch: 100}
	enabled := true
	p := NewCleanRemoteNotesProcessor(repo, func() CleanRemoteNotesConfig {
		return CleanRemoteNotesConfig{Enabled: enabled, ExpiryDays: 90, MaxProcessingMinutes: 60}
	})
	// 1 バッチ目の直後に運営者が止める。
	repo.onDelete = func(calls int) {
		if calls >= 1 {
			enabled = false
		}
	}
	require.NoError(t, p.Handle(context.Background(), driver.RawTask{}))
	require.Equal(t, 1, repo.calls,
		"無効化した後はバッチを回さないこと (実測 %d 回)", repo.calls)
}

// countingNoteRepo は DeleteExpiredRemoteNotes の呼び出し回数を数える。
type countingNoteRepo struct {
	repository.NoteRepository
	calls    int
	batch    int64
	onDelete func(calls int)
}

func (r *countingNoteRepo) DeleteExpiredRemoteNotesAfter(_, batchSize int, _ string) (int64, int, string, error) {
	n, err := r.DeleteExpiredRemoteNotes(0, 0)
	// 常に batchSize ちょうど見たことにして「まだ残っている」と見せる。
	return n, batchSize, "cursor", err
}

func (r *countingNoteRepo) DeleteExpiredRemoteNotes(int, int) (int64, error) {
	r.calls++
	if r.onDelete != nil {
		r.onDelete(r.calls)
	}
	// 常に batchSize ちょうど返して「まだ残っている」と見せる。
	return r.batch, nil
}

// pagingNoteRepo walks a fixed list of root ids like the real cursor query.
type pagingNoteRepo struct {
	repository.NoteRepository
	roots  []string
	afters []string // DeleteExpiredRemoteNotesAfter が受け取った cursor
	onCall func()
}

func (r *pagingNoteRepo) DeleteExpiredRemoteNotesAfter(_, batchSize int, after string) (int64, int, string, error) {
	r.afters = append(r.afters, after)
	if r.onCall != nil {
		r.onCall()
	}
	var page []string
	for _, id := range r.roots {
		if id > after && len(page) < batchSize {
			page = append(page, id)
		}
	}
	if len(page) == 0 {
		return 0, 0, "", nil
	}
	return 0, len(page), page[len(page)-1], nil
}

type memCursorStore struct {
	value   string
	sets    []string
	dels    int
	failGet bool
}

func (s *memCursorStore) Get(context.Context) (string, error) {
	if s.failGet {
		return "", errors.New("redis down")
	}
	return s.value, nil
}
func (s *memCursorStore) Set(_ context.Context, v string) error {
	s.value = v
	s.sets = append(s.sets, v)
	return nil
}
func (s *memCursorStore) Del(context.Context) error { s.value = ""; s.dels++; return nil }

// **走査位置をジョブをまたいで引き継ぐ (upstream 2026.9.1 #17957)。** 打ち切られた
// 次の実行は、保存した位置から続ける。末尾まで来たらカーソルを消し、次回は先頭から。
func TestCleanRemoteNotes_CursorSurvivesAcrossRuns(t *testing.T) {
	roots := make([]string, 0, 250)
	for i := range 250 {
		roots = append(roots, fmt.Sprintf("r%03d", i))
	}
	repo := &pagingNoteRepo{roots: roots}
	store := &memCursorStore{}
	enabled := true
	p := NewCleanRemoteNotesProcessor(repo, func() CleanRemoteNotesConfig {
		return CleanRemoteNotesConfig{Enabled: enabled, ExpiryDays: 90, MaxProcessingMinutes: 60}
	})
	p.SetCursorStore(store)

	// 1 回目: 1 バッチ目の直後に止まる (時間切れや運営者の停止と同じ)。
	repo.onCall = func() {
		if len(repo.afters) >= 1 {
			enabled = false
		}
	}
	require.NoError(t, p.Handle(context.Background(), driver.RawTask{}))
	require.Equal(t, []string{""}, repo.afters)
	require.Equal(t, "r099", store.value, "1 バッチ分の前進が保存される")

	// 2 回目: 保存した位置から始め、末尾まで進んだらカーソルを消す。
	enabled = true
	repo.onCall = nil
	repo.afters = nil
	require.NoError(t, p.Handle(context.Background(), driver.RawTask{}))
	assert.Equal(t, []string{"r099", "r199"}, repo.afters, "r199 の後ろは 50 件なので 1 回で末尾に達する")
	assert.Equal(t, 1, store.dels, "末尾に達したら消す")
	assert.Empty(t, store.value)

	// Redis が読めなければ先頭から (不整合は起きない)。
	store.value, store.failGet = "r199", true
	repo.afters = nil
	require.NoError(t, p.Handle(context.Background(), driver.RawTask{}))
	assert.Equal(t, "", repo.afters[0])
}
