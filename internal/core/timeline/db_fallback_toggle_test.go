package timeline

import (
	"context"
	"errors"
	"fmt"
	"strings"
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
	// limits records the n of every timeline query, in call order.
	limits []int
	// hydrates counts FindManyByIDsWithUser calls (one per page resolve).
	hydrates int
}

func (s *spyNoteRepo) FindManyByIDsWithUser(ids []string) ([]*model.Note, error) {
	s.hydrates++
	return s.NoteRepository.FindManyByIDsWithUser(ids)
}

func (s *spyNoteRepo) ListHomeTimeline(userID string, limit int, sinceID, untilID string, f model.TimelineDBFilter) ([]*model.Note, error) {
	s.homeCalls++
	s.limits = append(s.limits, limit)
	return s.NoteRepository.ListHomeTimeline(userID, limit, sinceID, untilID, f)
}

func (s *spyNoteRepo) ListLocalTimeline(limit int, sinceID, untilID string, f model.TimelineDBFilter) ([]*model.Note, error) {
	s.localCalls++
	s.limits = append(s.limits, limit)
	return s.NoteRepository.ListLocalTimeline(limit, sinceID, untilID, f)
}

func (s *spyNoteRepo) ListGlobalTimeline(limit int, sinceID, untilID string, f model.TimelineDBFilter) ([]*model.Note, error) {
	s.globalCalls++
	s.limits = append(s.limits, limit)
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

// --- 6. allowPartial で 1 ページ分が全て消えたとき (#3448) ---

// partialRead reads one descending page of a timeline with the given filter.
type partialRead func(svc *Service, limit int, filter TimelineFilter) ([]*model.Note, error)

// partialReads covers every fan-out timeline. global も含める — gate の有無は
// 違うが、0 件で終端を返してはいけないのは同じ。
var partialReads = map[string]partialRead{
	"home": func(svc *Service, limit int, f TimelineFilter) ([]*model.Note, error) {
		return svc.HomeTimeline(context.Background(), dbFallbackViewer, "", "", limit, f)
	},
	"local": func(svc *Service, limit int, f TimelineFilter) ([]*model.Note, error) {
		return svc.LocalTimeline(context.Background(), dbFallbackViewer, "", "", limit, f)
	},
	"hybrid": func(svc *Service, limit int, f TimelineFilter) ([]*model.Note, error) {
		return svc.HybridTimeline(context.Background(), dbFallbackViewer, "", "", limit, f)
	},
	"global": func(svc *Service, limit int, f TimelineFilter) ([]*model.Note, error) {
		return svc.GlobalTimeline(context.Background(), dbFallbackViewer, "", "", limit, f)
	},
}

// pushDangling fans out n ids that resolve to nothing (TTL の切れたリレー由来の
// note を模す)。新しい順に返す。
func pushDangling(t *testing.T, fanout *FanoutTimelineService, n int) []string {
	t.Helper()
	ids := make([]string, n)
	now := time.Now()
	for i := n - 1; i >= 0; i-- {
		ids[i] = idGen.Generate(now.Add(-time.Duration(i) * time.Second))
		pushAll(t, fanout, ids[i])
	}
	return ids
}

// olderDBNotes builds n notes older than anything pushDangling makes, newest first.
func olderDBNotes(n int) []*model.Note {
	notes := make([]*model.Note, n)
	base := time.Now().Add(-time.Hour)
	for i := range notes {
		notes[i] = dbFallbackNote(idGen.Generate(base.Add(-time.Duration(i) * time.Minute)))
	}
	return notes
}

func noteIDs(notes []*model.Note) []string {
	out := make([]string, 0, len(notes))
	for _, n := range notes {
		out = append(out, n.ID)
	}
	return out
}

// 1 ページ分の ID が全て解決できないとき、allowPartial でも空を返さず DB へ
// 倒れること (#3448)。同梱 frontend は常に allowPartial: true を送り、空のページを
// 「終端」と判断して以後読まないので、古い note が残っていてもそこで止まっていた。
//
// upstream は Redis を読み切って 0 件なら DB へ倒す
// (FanoutTimelineEndpointService の `ps.allowPartial ? redisTimeline.length !== 0 : ...`)。
func TestTimelines_AllowPartialEmptyPageFallsBackToDB(t *testing.T) {
	for name, read := range partialReads {
		for _, allowPartial := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/allowPartial=%v", name, allowPartial), func(t *testing.T) {
				older := olderDBNotes(5)
				svc, fanout, spy := newDBFallbackFixture(t, older...)
				pushDangling(t, fanout, 20)

				got, err := read(svc, 20, TimelineFilter{AllowPartial: allowPartial})
				require.NoError(t, err)
				assert.Equal(t, noteIDs(older), noteIDs(got),
					"Redis の 1 ページ分が全て消えても、DB にある古い note を返すこと")
				assert.Positive(t, spy.dbCalls())
			})
		}
	}
}

// 解決できた note が全て filter で落ちたときも同じ (mute などで 1 ページ分が消える)。
// 境界は filter 前の resolved から取るので、落ちた note を DB から引き直さない。
func TestTimelines_AllowPartialFilteredPageFallsBackToDB(t *testing.T) {
	for name, read := range partialReads {
		t.Run(name, func(t *testing.T) {
			older := olderDBNotes(3)
			muted := make([]*model.Note, 0, 20)
			now := time.Now()
			for i := 19; i >= 0; i-- {
				n := &model.Note{ID: idGen.Generate(now.Add(-time.Duration(i) * time.Second)), UserID: "muted", Visibility: model.NoteVisibilityPublic}
				muted = append(muted, n)
			}
			svc, fanout, _ := newDBFallbackFixture(t, append(append([]*model.Note{}, older...), muted...)...)
			for _, n := range muted {
				pushAll(t, fanout, n.ID)
			}
			got, err := read(svc, 20, TimelineFilter{AllowPartial: true, MutedUserIDs: []string{"muted"}})
			require.NoError(t, err)
			assert.Equal(t, noteIDs(older), noteIDs(got),
				"mute で 1 ページ分が消えても、その先の note を返すこと")
		})
	}
}

// 1 件でも解決できたら、allowPartial はその部分ページを DB を引かずに返す
// (従来どおり)。#3448 で変えたのは 0 件のときだけ。
func TestTimelines_AllowPartialKeepsPartialPage(t *testing.T) {
	for name, read := range partialReads {
		t.Run(name, func(t *testing.T) {
			older := olderDBNotes(5)
			newest := dbFallbackNote(idGen.Generate(time.Now().Add(time.Second)))
			svc, fanout, spy := newDBFallbackFixture(t, append([]*model.Note{newest}, older...)...)
			pushDangling(t, fanout, 19)
			pushAll(t, fanout, newest.ID)

			got, err := read(svc, 20, TimelineFilter{AllowPartial: true})
			require.NoError(t, err)
			assert.Equal(t, []string{newest.ID}, noteIDs(got))
			assert.Zero(t, spy.dbCalls(), "部分ページなら DB を引かない")
		})
	}
}

// fallback を切っていて Redis も読み切ったなら、upstream と同じく空を返す
// (`dbFallback` を空配列に差し替えた形)。DB は引かない。
func TestTimelines_AllowPartialEmptyPageWithoutDBFallback(t *testing.T) {
	for name, read := range partialReads {
		if name == "global" {
			continue // global は gate しない (TestService_GlobalTimeline_NotGatedByDbFallback)
		}
		t.Run(name, func(t *testing.T) {
			svc, fanout, spy := newDBFallbackFixture(t, olderDBNotes(5)...)
			svc.SetDBFallbackToggle(&fakeDBFallbackToggle{enabled: false})
			pushDangling(t, fanout, 20)

			got, err := read(svc, 20, TimelineFilter{AllowPartial: true})
			require.NoError(t, err)
			assert.Empty(t, got)
			assert.NotNil(t, got, "空でも non-nil slice であること")
			assert.Zero(t, spy.dbCalls(), "fallback 無効なら DB を引かない")
		})
	}
}

// fallback を切っていても、Redis にまだ古い ID があるなら読み進める。Redis が
// 唯一の取得元なので、そこで空を返すと frontend が止まる。upstream も
// Redis の ID を読み切るまで getAndFilterFromDb を繰り返す。
func TestTimelines_ScanOlderRedisWithoutDBFallback(t *testing.T) {
	for name, read := range partialReads {
		if name == "global" {
			continue
		}
		for _, allowPartial := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/allowPartial=%v", name, allowPartial), func(t *testing.T) {
				// Redis: 解決できる古い 3 件 + 解決できない新しい 20 件。
				// 2 回目の読み取り (要求 60 件) で 3 件に届き、読み切る。
				older := olderDBNotes(3)
				svc, fanout, spy := newDBFallbackFixture(t, older...)
				svc.SetDBFallbackToggle(&fakeDBFallbackToggle{enabled: false})
				for i := len(older) - 1; i >= 0; i-- {
					pushAll(t, fanout, older[i].ID)
				}
				pushDangling(t, fanout, 20)

				got, err := read(svc, 20, TimelineFilter{AllowPartial: allowPartial})
				require.NoError(t, err)
				assert.Equal(t, noteIDs(older), noteIDs(got))
				assert.Zero(t, spy.dbCalls(), "fallback 無効なら DB を引かない (hydrate は走る)")
			})
		}
	}

	// 何回も読み進める形。limit 2 で、解決できない 20 件の奥に 3 件ある。
	// allowPartial なら 1 件でも見つかった回で止まり、limit で切る。
	t.Run("several rounds, capped at limit", func(t *testing.T) {
		older := olderDBNotes(3)
		svc, fanout, _ := newDBFallbackFixture(t, older...)
		svc.SetDBFallbackToggle(&fakeDBFallbackToggle{enabled: false})
		for i := len(older) - 1; i >= 0; i-- {
			pushAll(t, fanout, older[i].ID)
		}
		pushDangling(t, fanout, 20)

		got, err := partialReads["home"](svc, 2, TimelineFilter{AllowPartial: true})
		require.NoError(t, err)
		assert.Equal(t, noteIDs(older[:2]), noteIDs(got))
	})
}

// scanItem is one entry of a fan-out list fixture for the scan tests.
type scanItem struct {
	kind byte // 'm' = muted (resolves, filtered out), 'd' = dangling, 'g' = good
	note *model.Note
	id   string
}

// buildScanList creates the given kinds newest first, stores the resolvable
// ones and returns them in that order.
func buildScanList(kinds string) []scanItem {
	items := make([]scanItem, len(kinds))
	now := time.Now()
	for i := range kinds {
		id := idGen.Generate(now.Add(-time.Duration(i) * time.Second))
		items[i] = scanItem{kind: kinds[i], id: id}
		switch kinds[i] {
		case 'm':
			items[i].note = &model.Note{ID: id, UserID: "muted", Visibility: model.NoteVisibilityPublic}
		case 'g':
			items[i].note = dbFallbackNote(id)
		}
	}
	return items
}

// fallback を切って Redis を読み進めるとき、**prune されない** ID (mute で落ちる
// note) をまたいで進むこと。宙吊りの ID だけで組むと、prune が list から消すので
// 読み進める位置を間違えても次のリクエストでは辻褄が合ってしまう。
//
// 重複が無く、降順で、期待した古い note が返ること。読み進めた先の宙吊りの ID も
// prune されること。
func TestTimelines_ScanOlderAdvancesThroughFilteredIDs(t *testing.T) {
	// 新しい順。limit 2 / allowPartial=false で、1 ページ目 (2 件) は全て mute、
	// 2 回目 (6 件) は mute と宙吊り、3 回目 (6 件) で g が 1 件、4 回目 (3 件) で
	// 2 件目の g が見つかる。
	const kinds = "mm" + "mmmmdd" + "gmmmmm" + "gmg" + "g"
	cases := map[string]struct {
		read func(*Service) ([]*model.Note, error)
		// list picks the list an item is pushed to.
		list func(i int, it scanItem) Name
	}{
		"home": {
			read: func(svc *Service) ([]*model.Note, error) {
				return svc.HomeTimeline(context.Background(), dbFallbackViewer, "", "", 2, TimelineFilter{MutedUserIDs: []string{"muted"}})
			},
			list: func(int, scanItem) Name { return HomeTimelineName(dbFallbackViewer.ID) },
		},
		// 複数の list から合流させる形。mute は home、宙吊りは local、g は交互に置く。
		"hybrid": {
			read: func(svc *Service) ([]*model.Note, error) {
				return svc.HybridTimeline(context.Background(), dbFallbackViewer, "", "", 2, TimelineFilter{MutedUserIDs: []string{"muted"}})
			},
			list: func(i int, it scanItem) Name {
				switch {
				case it.kind == 'm':
					return HomeTimelineName(dbFallbackViewer.ID)
				case it.kind == 'd' || i%2 == 0:
					return LocalTimeline
				default:
					return LocalTimelineWithReplyToName(dbFallbackViewer.ID)
				}
			},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			items := buildScanList(kinds)
			var stored []*model.Note
			var good, dangling []string
			for _, it := range items {
				switch it.kind {
				case 'g':
					good = append(good, it.id)
					stored = append(stored, it.note)
				case 'm':
					stored = append(stored, it.note)
				case 'd':
					dangling = append(dangling, it.id)
				}
			}
			svc, fanout, spy := newDBFallbackFixture(t, stored...)
			svc.SetDBFallbackToggle(&fakeDBFallbackToggle{enabled: false})
			lists := map[Name]struct{}{}
			for i := len(items) - 1; i >= 0; i-- {
				l := tc.list(i, items[i])
				lists[l] = struct{}{}
				require.NoError(t, fanout.Push(context.Background(), l, items[i].id, MaxTimelineLength))
			}

			got, err := tc.read(svc)
			require.NoError(t, err)
			assert.Equal(t, good[:2], noteIDs(got), "重複せず、降順で、mute をまたいだ先の note を返すこと")
			assert.Zero(t, spy.dbCalls())

			for l := range lists {
				left, err := fanout.Get(context.Background(), l, "", "", 0)
				require.NoError(t, err)
				for _, d := range dangling {
					assert.NotContains(t, left, d, "読み進めた先の宙吊りの ID も prune すること")
				}
			}
		})
	}
}

// DB で継ぎ足すときは、足りない件数だけを頼む (upstream の
// `ps.dbFallback(dbUntil, dbSince, remainingToRead)`)。
func TestTimelines_DBTopUpAsksOnlyForMissing(t *testing.T) {
	inRedis := idGen.Generate(time.Now())
	svc, fanout, spy := newDBFallbackFixture(t, dbFallbackNote(inRedis), olderDBNotes(3)[0])
	pushAll(t, fanout, inRedis)

	_, err := partialReads["home"](svc, 10, TimelineFilter{})
	require.NoError(t, err)
	assert.Equal(t, []int{9}, spy.limits)
}

// limit が 0 以下なら既定の 20 件で読む。0 のまま進むと 1 ページ目が空になり、
// fallback を切っていれば空を返して frontend が止まる。
func TestReadFanout_NonPositiveLimitUsesDefault(t *testing.T) {
	notes := make([]*model.Note, 0, 25)
	for i := 0; i < 25; i++ {
		notes = append(notes, dbFallbackNote(idGen.Generate(time.Now().Add(-time.Duration(i)*time.Second))))
	}
	svc, _, spy := newDBFallbackFixture(t, notes...)
	svc.SetDBFallbackToggle(&fakeDBFallbackToggle{enabled: false})
	r := fanoutRead{
		keys:  []Name{HomeTimelineName(dbFallbackViewer.ID)},
		ids:   func() ([]string, error) { return noteIDs(notes), nil },
		db:    func(string, string, int) ([]*model.Note, error) { return nil, errors.New("unexpected db") },
		gated: true,
	}
	for _, limit := range []int{0, -1} {
		got, err := svc.readFanout(context.Background(), r, dbFallbackViewer.ID, "", "", limit, TimelineFilter{AllowPartial: true})
		require.NoError(t, err)
		assert.Len(t, got, defaultTimelineLimit)
	}
	assert.Zero(t, spy.dbCalls())
}

// PageFilter (handler 側にしか無い判定) も、0 件かどうかを決める前に通すこと。
// 落ちた後で 0 件なら DB へ倒れ、filter の error はそのまま返る。
func TestTimelines_PageFilterRunsBeforeEmptyCheck(t *testing.T) {
	dropAll := func([]*model.Note) ([]*model.Note, error) { return []*model.Note{}, nil }
	for name, read := range partialReads {
		t.Run(name, func(t *testing.T) {
			older := olderDBNotes(3)
			inRedis := dbFallbackNote(idGen.Generate(time.Now()))
			svc, fanout, _ := newDBFallbackFixture(t, append([]*model.Note{inRedis}, older...)...)
			pushAll(t, fanout, inRedis.ID)

			got, err := read(svc, 20, TimelineFilter{AllowPartial: true, PageFilter: dropAll})
			require.NoError(t, err)
			assert.Equal(t, noteIDs(older), noteIDs(got))

			boom := errors.New("lookup failed")
			_, err = read(svc, 20, TimelineFilter{AllowPartial: true, PageFilter: func([]*model.Note) ([]*model.Note, error) { return nil, boom }})
			assert.ErrorIs(t, err, boom)
		})
	}
}

// 読み進める 1 回の量は limit を下回らない。upstream の式のままだと、残り 1 件で
// 3 件ずつしか読まず、mute で落ちる ID が長く続くと窓の長さに比例して回数が
// 増える (1 回ごとに hydrate などの DB 問い合わせが走る)。
func TestTimelines_ScanOlderReadsAtLeastLimitPerRound(t *testing.T) {
	// 新しい順に g×9 と、mute で落ちる 150 件。limit 10 / allowPartial=false で、
	// 1 ページ目は 10 件 (g×9 + m)、残り 149 件の m を読み進めて読み切る。
	kinds := strings.Repeat("g", 9) + strings.Repeat("m", 150)
	items := buildScanList(kinds)
	var stored []*model.Note
	for _, it := range items {
		stored = append(stored, it.note)
	}
	svc, fanout, spy := newDBFallbackFixture(t, stored...)
	svc.SetDBFallbackToggle(&fakeDBFallbackToggle{enabled: false})
	for i := len(items) - 1; i >= 0; i-- {
		require.NoError(t, fanout.Push(context.Background(), HomeTimelineName(dbFallbackViewer.ID), items[i].id, MaxTimelineLength))
	}

	got, err := svc.HomeTimeline(context.Background(), dbFallbackViewer, "", "", 10, TimelineFilter{MutedUserIDs: []string{"muted"}})
	require.NoError(t, err)
	assert.Len(t, got, 9)
	// 1 ページ目 + ceil(149 / 10) 回。下限が無いと 1 + ceil(149 / 3) = 51 回。
	assert.Equal(t, 1+15, spy.hydrates)
}
