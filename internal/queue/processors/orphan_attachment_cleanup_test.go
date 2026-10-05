package processors_test

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/queue/driver"
	"github.com/elythia-network/elythia/internal/queue/processors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeAttachmentCleaner is an in-memory stand-in for the drive file repository.
// 候補は id 昇順で保持し、production と同じく keyset cursor で切る。
type fakeAttachmentCleaner struct {
	candidates []string
	deleted    []string
	listCalls  int
	cutoffs    []string
	afters     []string
	listErr    error
	deleteErr  error
}

func (f *fakeAttachmentCleaner) ListOrphanRemoteAttachmentCandidates(cutoffID, afterID string, limit int) ([]string, error) {
	f.listCalls++
	f.cutoffs = append(f.cutoffs, cutoffID)
	f.afters = append(f.afters, afterID)
	if f.listErr != nil {
		return nil, f.listErr
	}
	out := make([]string, 0, limit)
	for _, id := range f.candidates {
		if id <= afterID {
			continue
		}
		out = append(out, id)
		if len(out) == limit {
			break
		}
	}
	return out, nil
}

func (f *fakeAttachmentCleaner) DeleteByIDs(ids []string) (int64, error) {
	if f.deleteErr != nil {
		return 0, f.deleteErr
	}
	f.deleted = append(f.deleted, ids...)
	remain := f.candidates[:0]
	drop := map[string]bool{}
	for _, id := range ids {
		drop[id] = true
	}
	for _, id := range f.candidates {
		if !drop[id] {
			remain = append(remain, id)
		}
	}
	f.candidates = remain
	return int64(len(ids)), nil
}

// fakeEphemeral reports a fixed set of live file IDs.
type fakeEphemeral struct {
	live map[string]bool
	err  error
	seen []string
}

func (f *fakeEphemeral) LiveFileIDs(_ context.Context, ids []string) (map[string]bool, error) {
	f.seen = append(f.seen, ids...)
	if f.err != nil {
		return nil, f.err
	}
	out := map[string]bool{}
	for _, id := range ids {
		if f.live[id] {
			out[id] = true
		}
	}
	return out, nil
}

func attachmentCfg(enabled bool, ttl time.Duration) func() processors.OrphanAttachmentCleanerConfig {
	return func() processors.OrphanAttachmentCleanerConfig {
		return processors.OrphanAttachmentCleanerConfig{Enabled: enabled, EphemeralTTL: ttl}
	}
}

// cutoffRecorder captures the instant the processor asked to convert into an
// ID, so the grace period can be asserted without exporting internals.
func cutoffRecorder(got *time.Time, id string) processors.CutoffIDFunc {
	return func(t time.Time) string {
		*got = t
		return id
	}
}

func newAttachmentProcessor(repo processors.OrphanAttachmentCleaner, eph processors.EphemeralFileChecker, ttl time.Duration, asked *time.Time) *processors.OrphanAttachmentCleanupProcessor {
	return processors.NewOrphanAttachmentCleanupProcessor(repo, eph, attachmentCfg(true, ttl), cutoffRecorder(asked, "cut"))
}

// **これが #2722 の核心。** 生きている ephemeral note が参照している添付は、
// 行がどれだけ古くても消してはいけない。dedup (upsertAttachments の
// FindByURI) は年齢に関係なく既存行を再利用するので、8 日前の行が今日届いた
// ephemeral note に結び直される。DB には note 行が無いため、SQL 側の
// 「どの note からも参照されていない」述語では守れない。
func TestOrphanAttachmentCleanup_KeepsLiveEphemeralAttachments(t *testing.T) {
	repo := &fakeAttachmentCleaner{candidates: []string{"f1", "f2", "f3"}}
	eph := &fakeEphemeral{live: map[string]bool{"f2": true}}
	var asked time.Time

	require.NoError(t, newAttachmentProcessor(repo, eph, time.Hour, &asked).
		Handle(context.Background(), driver.RawTask{}))

	assert.Equal(t, []string{"f1", "f3"}, repo.deleted, "印のある f2 は残す")
	assert.ElementsMatch(t, []string{"f1", "f2", "f3"}, eph.seen, "候補は全件 Redis に問い合わせる")
}

// Redis を引けないときは 1 件も消さない。「印が無い = ゴミ」と倒すと、
// Redis が一時的に落ちている間の実行で表示中の添付を全部消す。
func TestOrphanAttachmentCleanup_EphemeralErrorAborts(t *testing.T) {
	repo := &fakeAttachmentCleaner{candidates: []string{"f1", "f2"}}
	eph := &fakeEphemeral{err: errors.New("redis down")}
	var asked time.Time

	err := newAttachmentProcessor(repo, eph, time.Hour, &asked).
		Handle(context.Background(), driver.RawTask{})
	assert.Error(t, err)
	assert.Empty(t, repo.deleted)
}

// ephemeral store が未配線なら何も消さない (その構成ではこの形の行も作られない)。
func TestOrphanAttachmentCleanup_NilEphemeralDeletesNothing(t *testing.T) {
	repo := &fakeAttachmentCleaner{candidates: []string{"f1", "f2"}}
	var asked time.Time

	require.NoError(t, newAttachmentProcessor(repo, nil, time.Hour, &asked).
		Handle(context.Background(), driver.RawTask{}))
	assert.Empty(t, repo.deleted)
}

// keyset cursor が「見た行」で進むこと。残す行で止めると同じ行を引き直して
// 実行が終わらない。
func TestOrphanAttachmentCleanup_CursorAdvancesPastKeptRows(t *testing.T) {
	ids := make([]string, 0, 450)
	live := map[string]bool{}
	for i := 0; i < 450; i++ {
		id := fmt.Sprintf("f%03d", i)
		ids = append(ids, id)
		// 全部残す行にする。cursor が進まなければ無限に同じ 200 件を引く。
		live[id] = true
	}
	repo := &fakeAttachmentCleaner{candidates: ids}
	eph := &fakeEphemeral{live: live}
	var asked time.Time

	require.NoError(t, newAttachmentProcessor(repo, eph, time.Hour, &asked).
		Handle(context.Background(), driver.RawTask{}))

	assert.Empty(t, repo.deleted)
	assert.Equal(t, 3, repo.listCalls, "450 件を 200 ずつ 3 回で読み切る")
	assert.Equal(t, []string{"", "f199", "f399"}, repo.afters, "cursor が見た行で進む")
}

// ctx がキャンセルされたら次のバッチに進まない。worker の停止中に走り続けると
// shutdown が長引く。
func TestOrphanAttachmentCleanup_StopsOnCanceledContext(t *testing.T) {
	repo := &fakeAttachmentCleaner{candidates: []string{"f1", "f2"}}
	var asked time.Time
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	require.NoError(t, newAttachmentProcessor(repo, &fakeEphemeral{}, time.Hour, &asked).Handle(ctx, driver.RawTask{}))
	assert.Zero(t, repo.listCalls, "キャンセル済みなら 1 バッチも読まない")
	assert.Empty(t, repo.deleted)
}

func TestOrphanAttachmentCleanup_DisabledIsNoop(t *testing.T) {
	repo := &fakeAttachmentCleaner{candidates: []string{"f1"}}
	var asked time.Time
	p := processors.NewOrphanAttachmentCleanupProcessor(repo, &fakeEphemeral{},
		attachmentCfg(false, time.Hour), cutoffRecorder(&asked, "cut"))

	require.NoError(t, p.Handle(context.Background(), driver.RawTask{}))
	assert.Zero(t, repo.listCalls)
	assert.True(t, asked.IsZero(), "無効なら cutoff も組み立てない")
}

// 猶予の下限。既定 TTL (60 分) では 7 日が効く。
func TestOrphanAttachmentCleanup_GraceFloor(t *testing.T) {
	repo := &fakeAttachmentCleaner{candidates: []string{"f1"}}
	var asked time.Time
	require.NoError(t, newAttachmentProcessor(repo, &fakeEphemeral{}, time.Hour, &asked).
		Handle(context.Background(), driver.RawTask{}))

	require.NotEmpty(t, repo.cutoffs)
	assert.Equal(t, "cut", repo.cutoffs[0])
	grace := time.Since(asked)
	assert.Greater(t, grace, processors.MinOrphanAttachmentGrace-time.Minute)
	assert.Less(t, grace, processors.MinOrphanAttachmentGrace+time.Minute)
}

// TTL を長く設定した構成では TTL に比例した猶予を使うこと。
//
// **期待値はリテラルで書く。** 定数を参照して `ttl * factor` と組み立てると
// 倍率を変えたときに期待値も同じだけ動く恒真式になり、倍率を落とす変異を
// 捕まえられない (実際に一度そう書いて素通しした)。
func TestOrphanAttachmentCleanup_GraceScalesWithTTL(t *testing.T) {
	ttl := 30 * 24 * time.Hour
	repo := &fakeAttachmentCleaner{candidates: []string{"f1"}}
	var asked time.Time
	require.NoError(t, newAttachmentProcessor(repo, &fakeEphemeral{}, ttl, &asked).
		Handle(context.Background(), driver.RawTask{}))

	grace := time.Since(asked)
	want := 240 * 24 * time.Hour // 30 日 * 8
	assert.Greater(t, grace, want-time.Minute)
	assert.Less(t, grace, want+time.Minute)
}

// cutoff が空なら 1 件も触らない。repository 側も空 cutoff を no-op にするが、
// 二重に止める (cutoff の組み立てが壊れたときに全件消えるのが最悪の壊れ方)。
func TestOrphanAttachmentCleanup_EmptyCutoffSkips(t *testing.T) {
	repo := &fakeAttachmentCleaner{candidates: []string{"f1"}}
	var asked time.Time
	p := processors.NewOrphanAttachmentCleanupProcessor(repo, &fakeEphemeral{},
		attachmentCfg(true, time.Hour), cutoffRecorder(&asked, ""))

	require.NoError(t, p.Handle(context.Background(), driver.RawTask{}))
	assert.Zero(t, repo.listCalls)
}

func TestOrphanAttachmentCleanup_PropagatesErrors(t *testing.T) {
	var asked time.Time
	listFail := &fakeAttachmentCleaner{candidates: []string{"f1"}, listErr: errors.New("boom")}
	assert.Error(t, newAttachmentProcessor(listFail, &fakeEphemeral{}, time.Hour, &asked).
		Handle(context.Background(), driver.RawTask{}))

	deleteFail := &fakeAttachmentCleaner{candidates: []string{"f1"}, deleteErr: errors.New("boom")}
	assert.Error(t, newAttachmentProcessor(deleteFail, &fakeEphemeral{}, time.Hour, &asked).
		Handle(context.Background(), driver.RawTask{}))
}

// nil レシーバ / nil 依存で panic しないこと (配線漏れの構成でも起動する)。
func TestOrphanAttachmentCleanup_NilSafe(t *testing.T) {
	var nilP *processors.OrphanAttachmentCleanupProcessor
	require.NoError(t, nilP.Handle(context.Background(), driver.RawTask{}))

	repo := &fakeAttachmentCleaner{candidates: []string{"f1"}}
	p := processors.NewOrphanAttachmentCleanupProcessor(repo, &fakeEphemeral{},
		attachmentCfg(true, time.Hour), nil)
	require.NoError(t, p.Handle(context.Background(), driver.RawTask{}))
	assert.Zero(t, repo.listCalls)
}

// 削除は候補の順序を保つ (デバッグ時にログと突き合わせられるように)。
func TestOrphanAttachmentCleanup_DeletesInCandidateOrder(t *testing.T) {
	repo := &fakeAttachmentCleaner{candidates: []string{"f1", "f2", "f3"}}
	var asked time.Time
	require.NoError(t, newAttachmentProcessor(repo, &fakeEphemeral{}, time.Hour, &asked).
		Handle(context.Background(), driver.RawTask{}))

	assert.True(t, sort.StringsAreSorted(repo.deleted))
}
