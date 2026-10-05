package timeline

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeDBFallbackToggle is a DBFallbackToggleProvider stub.
type fakeDBFallbackToggle struct {
	enabled bool
}

func (f *fakeDBFallbackToggle) FanoutTimelineDBFallbackEnabled() bool { return f.enabled }

// spyNoteRepo counts the timeline queries that reach the database. embed して
// いるので、数えない残りのメソッドはそのまま mock に委譲される。
//
// **件数の比較では足りない。** off のときに「DB を引いたが結果が空だった」のか
// 「引かなかった」のかは、返り値からは区別できない。
type spyNoteRepo struct {
	repository.NoteRepository
	homeCalls, localCalls, globalCalls int
}

func (s *spyNoteRepo) ListHomeTimeline(userID string, limit int, sinceID, untilID string, f model.TimelineDBFilter) ([]*model.Note, error) {
	s.homeCalls++
	return s.NoteRepository.ListHomeTimeline(userID, limit, sinceID, untilID, f)
}

func (s *spyNoteRepo) ListLocalTimeline(limit int, sinceID, untilID string, f model.TimelineDBFilter) ([]*model.Note, error) {
	s.localCalls++
	return s.NoteRepository.ListLocalTimeline(limit, sinceID, untilID, f)
}

func (s *spyNoteRepo) ListGlobalTimeline(limit int, sinceID, untilID string, f model.TimelineDBFilter) ([]*model.Note, error) {
	s.globalCalls++
	return s.NoteRepository.ListGlobalTimeline(limit, sinceID, untilID, f)
}

func (s *spyNoteRepo) dbCalls() int { return s.homeCalls + s.localCalls + s.globalCalls }

// dbFallbackViewer is the fixture viewer. home / hybrid は認証必須。
var dbFallbackViewer = &model.User{ID: "viewer"}

type timelineRead func(*Service, string, string, int) ([]*model.Note, error)

func readGlobal(svc *Service, untilID, sinceID string, limit int) ([]*model.Note, error) {
	return svc.GlobalTimeline(context.Background(), dbFallbackViewer, untilID, sinceID, limit, TimelineFilter{})
}

// gatedReads are the timelines that `enableFanoutTimelineDbFallback` covers.
// **global は入らない** — upstream の global-timeline は fanout を通らないので
// このつまみの対象外 (#2762)。gate の有無は TestService_GlobalTimeline_* が固定する。
var gatedReads = map[string]timelineRead{
	"home": func(svc *Service, untilID, sinceID string, limit int) ([]*model.Note, error) {
		return svc.HomeTimeline(context.Background(), dbFallbackViewer, untilID, sinceID, limit, TimelineFilter{})
	},
	"local": func(svc *Service, untilID, sinceID string, limit int) ([]*model.Note, error) {
		return svc.LocalTimeline(context.Background(), dbFallbackViewer, untilID, sinceID, limit, TimelineFilter{})
	},
	"hybrid": func(svc *Service, untilID, sinceID string, limit int) ([]*model.Note, error) {
		return svc.HybridTimeline(context.Background(), dbFallbackViewer, untilID, sinceID, limit, TimelineFilter{})
	},
}

// allReads includes global, for the cases that must hold everywhere.
var allReads = func() map[string]timelineRead {
	m := map[string]timelineRead{"global": readGlobal}
	for k, v := range gatedReads {
		m[k] = v
	}
	return m
}()

// newDBFallbackFixture builds a Service whose database holds the given notes.
// Redis は空から始まる。
func newDBFallbackFixture(t *testing.T, notes ...*model.Note) (*Service, *FanoutTimelineService, *spyNoteRepo) {
	t.Helper()
	fanout := newTestService(t)
	repo := testutil.NewMockNoteRepository()
	for _, n := range notes {
		require.NoError(t, repo.Create(n))
	}
	spy := &spyNoteRepo{NoteRepository: repo}
	svc := NewService(fanout, spy, testutil.NewMockFollowingRepository())
	return svc, fanout, spy
}

func dbFallbackNote(id string) *model.Note {
	return &model.Note{ID: id, UserID: dbFallbackViewer.ID, Visibility: model.NoteVisibilityPublic}
}

// pushAll fans the note out to every list the four timelines read.
func pushAll(t *testing.T, fanout *FanoutTimelineService, noteID string) {
	t.Helper()
	names := []Name{
		HomeTimelineName(dbFallbackViewer.ID),
		LocalTimeline,
		GlobalTimeline,
		LocalTimelineWithReplyToName(dbFallbackViewer.ID),
	}
	for _, n := range names {
		require.NoError(t, fanout.Push(context.Background(), n, noteID, MaxTimelineLength))
	}
}

// --- 1. Redis が空 ---

// Redis が空なら shouldFallbackToDB が真になり、全ページが DB から返る。
// fallback を切ると DB を一度も引かず空を返す。
func TestService_DbFallbackDisabled_EmptyRedisReturnsNothing(t *testing.T) {
	for name, read := range gatedReads {
		t.Run(name, func(t *testing.T) {
			svc, _, spy := newDBFallbackFixture(t, dbFallbackNote(idGen.Generate(time.Now())))

			svc.SetDBFallbackToggle(&fakeDBFallbackToggle{enabled: true})
			got, err := read(svc, "", "", 10)
			require.NoError(t, err)
			assert.Len(t, got, 1, "fallback 有効なら DB から返る")
			require.Positive(t, spy.dbCalls(), "前提: DB を引いている")

			before := spy.dbCalls()
			svc.SetDBFallbackToggle(&fakeDBFallbackToggle{enabled: false})
			got, err = read(svc, "", "", 10)
			require.NoError(t, err)
			assert.Empty(t, got, "fallback 無効なら DB へ倒れない")
			assert.Equal(t, before, spy.dbCalls(), "DB を一度も引かないこと")

			// handler は nil slice を JSON にすると `[]` ではなく `null` を返す。
			assert.NotNil(t, got, "空でも non-nil slice であること")
		})
	}
}

// --- 2. sinceId 付きページング (#2720 で必ず DB へ倒れる経路) ---

// sinceId を含むページングは Redis に十分な ID があっても DB へ倒れる
// (shouldFallbackToDB が常に真、#2720)。fallback を切るとこの経路が空になる。
// **issue #2762 が止めたかったのはこの負荷。**
func TestService_DbFallbackDisabled_SinceIdPagingReturnsNothing(t *testing.T) {
	for name, read := range gatedReads {
		t.Run(name, func(t *testing.T) {
			old := idGen.Generate(time.Now().Add(-time.Minute))
			recent := idGen.Generate(time.Now())
			svc, fanout, spy := newDBFallbackFixture(t, dbFallbackNote(old), dbFallbackNote(recent))
			pushAll(t, fanout, old)
			pushAll(t, fanout, recent)

			svc.SetDBFallbackToggle(&fakeDBFallbackToggle{enabled: true})
			got, err := read(svc, "", old, 10)
			require.NoError(t, err)
			assert.NotEmpty(t, got, "fallback 有効なら DB が処理する")
			require.Positive(t, spy.dbCalls(), "前提: sinceId 付きは DB へ倒れる")

			before := spy.dbCalls()
			svc.SetDBFallbackToggle(&fakeDBFallbackToggle{enabled: false})
			got, err = read(svc, "", old, 10)
			require.NoError(t, err)
			assert.Empty(t, got, "fallback 無効なら sinceId 付きは空になる")
			assert.Equal(t, before, spy.dbCalls(), "DB を一度も引かないこと")
		})
	}
}

// --- 3. 継ぎ足し (Redis が limit に足りない) ---

// Redis から取れた分が limit に満たないとき、upstream は残りを DB から継ぎ足す。
// fallback を切ると **Redis の持ち分だけ**を返す (件数は揃わない)。
func TestService_DbFallbackDisabled_DoesNotTopUp(t *testing.T) {
	for name, read := range gatedReads {
		t.Run(name, func(t *testing.T) {
			inRedis := idGen.Generate(time.Now())
			older := idGen.Generate(time.Now().Add(-time.Minute))
			svc, fanout, spy := newDBFallbackFixture(t, dbFallbackNote(inRedis), dbFallbackNote(older))
			// Redis には新しい方だけを積む。limit 10 に対して 1 件しか無いので
			// 継ぎ足しが走る。
			pushAll(t, fanout, inRedis)

			svc.SetDBFallbackToggle(&fakeDBFallbackToggle{enabled: true})
			got, err := read(svc, "", "", 10)
			require.NoError(t, err)
			assert.Len(t, got, 2, "fallback 有効なら足りない分を DB で埋める")
			require.Positive(t, spy.dbCalls(), "前提: 継ぎ足しで DB を引いている")

			before := spy.dbCalls()
			svc.SetDBFallbackToggle(&fakeDBFallbackToggle{enabled: false})
			got, err = read(svc, "", "", 10)
			require.NoError(t, err)
			require.Len(t, got, 1, "fallback 無効なら Redis の持ち分だけ")
			assert.Equal(t, inRedis, got[0].ID)
			// spy が数えるのは timeline の fallback クエリだけ。hydrate
			// (`FindManyByIDsWithUser`) は Redis の 1 件を引くために走る。
			assert.Equal(t, before, spy.dbCalls(), "fallback クエリを投げないこと (hydrate は走る)")
		})
	}
}

// --- 4. enableFanoutTimeline が off のときは別扱い ---

// **FTT 全停止と DB fallback 停止は別のつまみ。** upstream は
// `if (!enableFanoutTimeline) return getFromDb()` を endpoint 側に持っており、
// useDbFallback は見ない。両方 off のときに DB まで止めると、タイムラインが
// 常に空になる。
func TestService_FanoutOffStillQueriesDbEvenWhenFallbackDisabled(t *testing.T) {
	for name, read := range allReads {
		t.Run(name, func(t *testing.T) {
			svc, _, spy := newDBFallbackFixture(t, dbFallbackNote(idGen.Generate(time.Now())))
			svc.SetFanoutToggle(&fakeFanoutToggle{enabled: false})
			svc.SetDBFallbackToggle(&fakeDBFallbackToggle{enabled: false})

			got, err := read(svc, "", "", 10)
			require.NoError(t, err)
			assert.Len(t, got, 1, "FTT off は DB 直行 (fallback の設定に関係しない)")
			assert.Positive(t, spy.dbCalls())
		})
	}
}

// --- 4b. global は対象外 ---

// **global は gate しない。** upstream の `global-timeline` は
// `FanoutTimelineEndpointService` を通らず常に SQL を引くので、このつまみの
// 対象外になっている。mk-go が GTL を fanout 経路にしているのは性能上の拡張で、
// つまみの意味論を変える理由にはならない。
//
// gate すると、同梱 frontend のウィザードが group / open で
// `enableFanoutTimelineDbFallback: false` を送るため、**誰も設定を触っていない
// インスタンスで GTL が Redis 窓を超えて遡れなくなる**。
func TestService_GlobalTimeline_NotGatedByDbFallback(t *testing.T) {
	t.Run("Redis が空でも DB へ倒れる", func(t *testing.T) {
		svc, _, spy := newDBFallbackFixture(t, dbFallbackNote(idGen.Generate(time.Now())))
		svc.SetDBFallbackToggle(&fakeDBFallbackToggle{enabled: false})

		got, err := readGlobal(svc, "", "", 10)
		require.NoError(t, err)
		assert.Len(t, got, 1, "global は fallback 無効でも DB から返す")
		assert.Positive(t, spy.dbCalls())
	})

	t.Run("件数が足りなければ継ぎ足す", func(t *testing.T) {
		// Redis に 1 件だけ積み、limit 10 で読む。gate 対象の 3 経路なら
		// fallback off でこの継ぎ足しが止まるが、global は止まらない。
		inRedis := idGen.Generate(time.Now())
		older := idGen.Generate(time.Now().Add(-time.Minute))
		svc, fanout, spy := newDBFallbackFixture(t, dbFallbackNote(inRedis), dbFallbackNote(older))
		require.NoError(t, fanout.Push(context.Background(), GlobalTimeline, inRedis, MaxTimelineLength))
		svc.SetDBFallbackToggle(&fakeDBFallbackToggle{enabled: false})

		got, err := readGlobal(svc, "", "", 10)
		require.NoError(t, err)
		assert.Len(t, got, 2, "global は fallback 無効でも足りない分を DB で埋める")
		assert.Positive(t, spy.dbCalls())
	})

	t.Run("窓を超えた遡りが続く", func(t *testing.T) {
		// Redis には最新の 1 件だけを積む (= 窓が浅い状態)。その窓より古い
		// untilId を要求すると Redis からは何も返らないので、DB へ倒れないと
		// 遡れない。gate すると空になり、無限スクロールが窓で行き止まりになる。
		oldest := idGen.Generate(time.Now().Add(-2 * time.Minute))
		older := idGen.Generate(time.Now().Add(-time.Minute))
		newer := idGen.Generate(time.Now())
		svc, fanout, spy := newDBFallbackFixture(t,
			dbFallbackNote(oldest), dbFallbackNote(older), dbFallbackNote(newer))
		require.NoError(t, fanout.Push(context.Background(), GlobalTimeline, newer, MaxTimelineLength))
		svc.SetDBFallbackToggle(&fakeDBFallbackToggle{enabled: false})

		got, err := readGlobal(svc, older, "", 10)
		require.NoError(t, err)
		require.Len(t, got, 1, "窓の外へ遡れること")
		assert.Equal(t, oldest, got[0].ID)
		assert.Positive(t, spy.dbCalls())
	})
}

// --- 5. 既定 / production adapter ---

func TestService_DbFallbackDefaultsToTrue(t *testing.T) {
	assert.True(t, (&Service{}).dbFallbackEnabled(), "provider 未配線なら有効扱い")
}

func TestNewMetaDbFallbackToggle_ReadsFromMeta(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		repo := &stubMetaRepo{meta: &model.Meta{EnableFanoutTimelineDBFallback: enabled}}
		assert.Equal(t, enabled, NewMetaDBFallbackToggle(repo).FanoutTimelineDBFallbackEnabled())
	}
}

func TestNewMetaDbFallbackToggle_FailsOpen(t *testing.T) {
	// meta が読めないときは有効側に倒す (既定値が true のため)。無効側に倒すと
	// 一時的な DB エラーでタイムラインが Redis の持ち分だけに縮む。
	assert.True(t, NewMetaDBFallbackToggle(&stubMetaRepo{err: errors.New("db down")}).FanoutTimelineDBFallbackEnabled())
	assert.True(t, NewMetaDBFallbackToggle(&stubMetaRepo{meta: nil}).FanoutTimelineDBFallbackEnabled())
	assert.True(t, NewMetaDBFallbackToggle(nil).FanoutTimelineDBFallbackEnabled())

	var p *metaRepoCacheLimits
	assert.True(t, p.FanoutTimelineDBFallbackEnabled())
}

// enableFanoutTimeline と enableFanoutTimelineDbFallback は**別の列**。
// 片方の値がもう片方に漏れていないことを固定する。
func TestMetaToggles_AreIndependent(t *testing.T) {
	repo := &stubMetaRepo{meta: &model.Meta{EnableFanoutTimeline: true, EnableFanoutTimelineDBFallback: false}}
	assert.True(t, NewMetaFanoutToggle(repo).FanoutTimelineEnabled())
	assert.False(t, NewMetaDBFallbackToggle(repo).FanoutTimelineDBFallbackEnabled())

	repo = &stubMetaRepo{meta: &model.Meta{EnableFanoutTimeline: false, EnableFanoutTimelineDBFallback: true}}
	assert.False(t, NewMetaFanoutToggle(repo).FanoutTimelineEnabled())
	assert.True(t, NewMetaDBFallbackToggle(repo).FanoutTimelineDBFallbackEnabled())
}

// --- 6. production の配線 ---

// 配線が抜けたら落ちること。#2762 の本体は「列も admin 公開もあるのに読み取り
// 側へ配線されていない」だった。router が個別 setter を呼ぶ形のままだと同じ
// 抜けを再発させてもテストで捕まらない (internal/server は CI のカバレッジ
// 対象外) ので、配線は WireMetaToggles に閉じ込めてここで固定する。
//
// **3 つの setter を個別に確かめる。** どの provider も未配線なら fail-open で
// true を返すので、「有効なときに期待どおり動く」ことを見ても配線の有無は
// 区別できない。各 setter が効いている状態でしか成立しない条件を選ぶ。

// svc.SetDBFallbackToggle: FTT は on のまま fallback だけ off。
// 両方 off にすると FTT 側の DB 直行と区別が付かない。
func TestWireMetaToggles_WiresReadDbFallback(t *testing.T) {
	repo := &stubMetaRepo{meta: &model.Meta{EnableFanoutTimeline: true, EnableFanoutTimelineDBFallback: false}}
	svc, _, spy := newDBFallbackFixture(t, dbFallbackNote(idGen.Generate(time.Now())))
	WireMetaToggles(NewFanoutHook(svc.fanout, testutil.NewMockFollowingRepository()), svc, repo)

	got, err := svc.HomeTimeline(context.Background(), dbFallbackViewer, "", "", 10, TimelineFilter{})
	require.NoError(t, err)
	assert.Empty(t, got, "dbFallback の配線が効いていれば DB へ倒れない")
	assert.Zero(t, spy.dbCalls(), "DB を一度も引かないこと")
}

// svc.SetFanoutToggle: FTT off + fallback off。配線されていれば FTT off の
// DB 直行が優先されて note が返る。未配線なら FTT on 扱いで Redis を読み、
// 空 + fallback off で何も返らない。
func TestWireMetaToggles_WiresReadFanoutToggle(t *testing.T) {
	repo := &stubMetaRepo{meta: &model.Meta{EnableFanoutTimeline: false, EnableFanoutTimelineDBFallback: false}}
	svc, _, spy := newDBFallbackFixture(t, dbFallbackNote(idGen.Generate(time.Now())))
	WireMetaToggles(NewFanoutHook(svc.fanout, testutil.NewMockFollowingRepository()), svc, repo)

	got, err := svc.HomeTimeline(context.Background(), dbFallbackViewer, "", "", 10, TimelineFilter{})
	require.NoError(t, err)
	assert.Len(t, got, 1, "FTT off の配線が効いていれば DB 直行になる")
	assert.Positive(t, spy.dbCalls())
}

// hook.SetFanoutToggle: FTT off なら push しない。
func TestWireMetaToggles_WiresPushFanoutToggle(t *testing.T) {
	ctx := context.Background()
	repo := &stubMetaRepo{meta: &model.Meta{EnableFanoutTimeline: false, EnableFanoutTimelineDBFallback: true}}
	svc, fanout, _ := newDBFallbackFixture(t)
	hook := NewFanoutHook(fanout, testutil.NewMockFollowingRepository())
	WireMetaToggles(hook, svc, repo)

	author := &model.User{ID: "wire_author"}
	noteID := idGen.Generate(time.Now())
	hook.OnNoteCreated(&model.Note{ID: noteID, UserID: author.ID, Visibility: model.NoteVisibilityPublic}, author)

	ids, err := fanout.Get(ctx, GlobalTimeline, "", "", 10)
	require.NoError(t, err)
	assert.Empty(t, ids, "hook 側の配線が効いていれば push されない")
}

// nil を渡しても panic しない (片側だけ配線したい呼び出しに備える)。
func TestWireMetaToggles_NilArgs(t *testing.T) {
	repo := &stubMetaRepo{meta: &model.Meta{}}
	assert.NotPanics(t, func() { WireMetaToggles(nil, nil, repo) })
}
