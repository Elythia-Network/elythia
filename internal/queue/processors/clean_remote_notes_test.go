package processors

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

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

// captureJobLog returns a context whose job log lines are appended to *lines.
func captureJobLog(lines *[]string) context.Context {
	return driver.WithJobLogger(context.Background(), func(_ context.Context, line string) error {
		*lines = append(*lines, line)
		return nil
	})
}

// **実行の記録をジョブのログに書くこと (#3551)。** upstream は同じ場面で
// job.log に書き、管理画面の Load logs で読める。
func TestCleanRemoteNotes_WritesJobLog(t *testing.T) {
	roots := make([]string, 0, 150)
	for i := range 150 {
		roots = append(roots, fmt.Sprintf("r%03d", i))
	}
	p := NewCleanRemoteNotesProcessor(&pagingNoteRepo{roots: roots}, func() CleanRemoteNotesConfig {
		return CleanRemoteNotesConfig{Enabled: true, ExpiryDays: 90, MaxProcessingMinutes: 60}
	})
	// 期限より古いカーソル (aidx の ID は時刻順で、"0" は最も古い)。
	p.SetCursorStore(&memCursorStore{value: "0000000000"})

	var lines []string
	require.NoError(t, p.Handle(captureJobLog(&lines), driver.RawTask{}))
	require.Len(t, lines, 4)
	assert.Equal(t, "Resuming from the cursor left by a previous run: 0000000000", lines[0])
	assert.Regexp(t, `^Deleted 0 notes; \d+ms$`, lines[1])
	assert.Regexp(t, `^Deleted 0 notes; \d+ms$`, lines[2])
	assert.Equal(t, "No more notes to clean. The next run will start from the beginning.", lines[3])
}

// **時間切れで止めたときは、そのことと止めた位置を書くこと (#3551)。**
func TestCleanRemoteNotes_WritesJobLogOnTimeout(t *testing.T) {
	repo := &countingNoteRepo{batch: 7}
	p := NewCleanRemoteNotesProcessor(repo, func() CleanRemoteNotesConfig {
		return CleanRemoteNotesConfig{Enabled: true, ExpiryDays: 90, MaxProcessingMinutes: 1}
	})
	base := time.Now()
	clock := base
	p.now = func() time.Time { return clock }
	// 1 バッチ目の直後に時間を進め、上限を超えさせる。
	repo.onDelete = func(int) { clock = base.Add(2 * time.Minute) }

	var lines []string
	require.NoError(t, p.Handle(captureJobLog(&lines), driver.RawTask{}))
	assert.Equal(t, 1, repo.calls)
	require.Len(t, lines, 2)
	assert.Regexp(t, `^Deleted 7 notes; \d+ms$`, lines[0])
	assert.Equal(t, "Reached maximum duration of 60000ms, stopping... (last cursor: cursor)", lines[1])
}

// **期限より新しいカーソルでは「再開した」と書かないこと (#3551)。** repository は
// そのカーソルを捨てて先頭から読み直すので、書くと記録が嘘になる。
func TestCleanRemoteNotes_JobLogSkipsResumeForDiscardedCursor(t *testing.T) {
	p := NewCleanRemoteNotesProcessor(&pagingNoteRepo{}, func() CleanRemoteNotesConfig {
		return CleanRemoteNotesConfig{Enabled: true, ExpiryDays: 90, MaxProcessingMinutes: 60}
	})
	p.SetCursorStore(&memCursorStore{value: "zzzzzzzzzz"})

	var lines []string
	require.NoError(t, p.Handle(captureJobLog(&lines), driver.RawTask{}))
	// 消せるノートが無いので、空のバッチの「Deleted 0 notes」も書かない。
	assert.Equal(t, []string{"No more notes to clean. The next run will start from the beginning."}, lines)
}

// **無効化や取り消しで止めたときは「時間切れ」と書かないこと (#3551)。**
// 時計がたまたま期限を過ぎていても、止めた理由は時間切れではない。
func TestCleanRemoteNotes_JobLogTimeoutOnlyWhenTimedOut(t *testing.T) {
	for _, tc := range []string{"disabled", "canceled"} {
		t.Run(tc, func(t *testing.T) {
			repo := &countingNoteRepo{batch: 7}
			enabled := true
			p := NewCleanRemoteNotesProcessor(repo, func() CleanRemoteNotesConfig {
				return CleanRemoteNotesConfig{Enabled: enabled, ExpiryDays: 90, MaxProcessingMinutes: 1}
			})
			base := time.Now()
			clock := base
			p.now = func() time.Time { return clock }
			var lines []string
			ctx, cancel := context.WithCancel(captureJobLog(&lines))
			defer cancel()
			repo.onDelete = func(int) {
				if tc == "disabled" {
					enabled = false
				} else {
					cancel()
				}
				// 止めた後に時計が期限を過ぎる。
				clock = base.Add(2 * time.Minute)
			}

			require.NoError(t, p.Handle(ctx, driver.RawTask{}))
			require.Len(t, lines, 1)
			assert.Regexp(t, `^Deleted 7 notes; \d+ms$`, lines[0])
		})
	}
}
