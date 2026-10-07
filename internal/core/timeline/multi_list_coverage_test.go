package timeline

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
)

// #3449: 複数の list を混ぜる timeline (social / ログイン中の local) で、list ごとの
// 深さの差のせいで、遡ったときに浅い list の期間の note が抜けていた。

// replyAwareRepo makes the mock's local / home queries honour the parts of
// the SQL that decide which notes the multi-list fallback must reproduce:
// local returns public notes only, and both apply the reply exclusion with
// its #3449 exceptions (KeepRepliesToViewer / KeepHomeFanoutReplies).
//
// MockNoteRepository はどちらのクエリでも public / home の全 note を返し、
// filter を見ない。そのままだと「DB が list の中身を再現しているか」を検証
// できないので、ここで絞る。フォロー関係 (home の対象者、`withReplies` 付きの
// フォロー) は再現しない — SQL そのものは internal/repository のテスト
// (TestNoteRepository_ReplyExclusionKeepsMultiListFanout) が実 DB で固定する。
type replyAwareRepo struct {
	*spyNoteRepo
}

func keepReply(n *model.Note, f model.TimelineDBFilter) bool {
	if !f.ExcludeRepliesToOthers && (f.WithReplies == nil || *f.WithReplies) {
		return true
	}
	if n.ReplyID == nil || (n.ReplyUserID != nil && *n.ReplyUserID == n.UserID) {
		return true
	}
	if f.ViewerID == "" {
		return false
	}
	if f.KeepRepliesToViewer && n.ReplyUserID != nil && *n.ReplyUserID == f.ViewerID {
		return true
	}
	return f.KeepHomeFanoutReplies && (n.UserID == f.ViewerID || slices.Contains([]string(n.Mentions), f.ViewerID))
}

func filterNotes(notes []*model.Note, limit int, keep func(*model.Note) bool) []*model.Note {
	out := make([]*model.Note, 0, len(notes))
	for _, n := range notes {
		if keep(n) {
			out = append(out, n)
		}
	}
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

func (r *replyAwareRepo) ListHomeTimeline(userID string, limit int, sinceID, untilID string, f model.TimelineDBFilter) ([]*model.Note, error) {
	all, err := r.spyNoteRepo.ListHomeTimeline(userID, 0, sinceID, untilID, f)
	if err != nil {
		return nil, err
	}
	return filterNotes(all, limit, func(n *model.Note) bool { return keepReply(n, f) }), nil
}

func (r *replyAwareRepo) ListLocalTimeline(limit int, sinceID, untilID string, f model.TimelineDBFilter) ([]*model.Note, error) {
	all, err := r.spyNoteRepo.ListLocalTimeline(0, sinceID, untilID, f)
	if err != nil {
		return nil, err
	}
	return filterNotes(all, limit, func(n *model.Note) bool {
		return n.Visibility == model.NoteVisibilityPublic && n.UserHost == nil && keepReply(n, f)
	}), nil
}

// pageRead reads one page of a multi-list timeline.
type pageRead func(svc *Service, untilID, sinceID string, limit int, allowPartial bool) ([]*model.Note, error)

var multiListReads = map[string]pageRead{
	"hybrid": func(svc *Service, untilID, sinceID string, limit int, allowPartial bool) ([]*model.Note, error) {
		// handler はフォロー一覧を必ず読む (読めないときの扱いは
		// TestHybridDBFallback_ReplyExceptions)。
		return svc.HybridTimeline(context.Background(), dbFallbackViewer, untilID, sinceID, limit, TimelineFilter{AllowPartial: allowPartial, FollowingIDs: map[string]struct{}{}})
	},
	"local": func(svc *Service, untilID, sinceID string, limit int, allowPartial bool) ([]*model.Note, error) {
		return svc.LocalTimeline(context.Background(), dbFallbackViewer, untilID, sinceID, limit, TimelineFilter{AllowPartial: allowPartial})
	},
}

// shallowListName is the second list each timeline merges with localTimeline,
// the one the fixture keeps shallow.
var shallowListName = map[string]Name{
	"hybrid": HomeTimelineName(dbFallbackViewer.ID),
	"local":  LocalTimelineWithReplyToName(dbFallbackViewer.ID),
}

// walkPages pages back from the newest note until a page comes back empty and
// returns every id in the order served.
func walkPages(t *testing.T, svc *Service, read pageRead, limit int, allowPartial bool) []string {
	t.Helper()
	var out []string
	until := ""
	for range 100 {
		page, err := read(svc, until, "", limit, allowPartial)
		require.NoError(t, err)
		if len(page) == 0 {
			return out
		}
		out = append(out, noteIDs(page)...)
		until = page[len(page)-1].ID
	}
	t.Fatal("paging did not terminate")
	return nil
}

func publicNote(at time.Time, userID string) *model.Note {
	return &model.Note{ID: idGen.Generate(at), UserID: userID, Visibility: model.NoteVisibilityPublic}
}

// replyToViewer is a local public reply addressed to the viewer, the kind of
// note `localTimelineWithReplyTo:<viewer>` holds.
func replyToViewer(at time.Time) *model.Note {
	n := publicNote(at, "other")
	parent, viewer := "parent-of-"+n.ID, dbFallbackViewer.ID
	n.ReplyID, n.ReplyUserID = &parent, &viewer
	return n
}

// depthFixture is localTimeline holding 30 deep notes, a second list holding
// only one newer note, and one note in the middle of that period that the
// second list should hold but lives only in the database (the #3449
// reproduction).
//
//   - hybrid: 2 本目はホームの list。DB にだけあるのは自分の公開範囲 home の投稿
//     (ローカルに流れない投稿)
//   - local: 2 本目は自分宛ての返信の list。DB にだけあるのは自分宛ての返信
type depthFixture struct {
	svc     *Service
	fanout  *FanoutTimelineService
	spy     *spyNoteRepo
	deep    []*model.Note // newest first, all in localTimeline
	shallow *model.Note   // the only note of the second list
	dbOnly  *model.Note
}

func newDepthFixture(t *testing.T, name string, extra ...*model.Note) depthFixture {
	t.Helper()
	now := time.Now()
	deep := make([]*model.Note, 30)
	for i := range deep {
		deep[i] = publicNote(now.Add(-time.Duration(i)*time.Minute-time.Second), "other")
	}
	var shallow, dbOnly *model.Note
	if name == "hybrid" {
		shallow = publicNote(now, dbFallbackViewer.ID)
		dbOnly = &model.Note{ID: idGen.Generate(now.Add(-15*time.Minute - 30*time.Second)), UserID: dbFallbackViewer.ID, Visibility: model.NoteVisibilityHome}
	} else {
		shallow = replyToViewer(now)
		dbOnly = replyToViewer(now.Add(-15*time.Minute - 30*time.Second))
	}
	all := append(append([]*model.Note{shallow, dbOnly}, deep...), extra...)
	fanout := newTestService(t)
	repo := testutil.NewMockNoteRepository()
	for _, n := range all {
		require.NoError(t, repo.Create(n))
	}
	spy := &spyNoteRepo{NoteRepository: repo}
	svc := NewService(fanout, &replyAwareRepo{spy}, testutil.NewMockFollowingRepository())
	ctx := context.Background()
	for i := len(deep) - 1; i >= 0; i-- {
		require.NoError(t, fanout.Push(ctx, LocalTimeline, deep[i].ID, MaxTimelineLength))
	}
	if name == "hybrid" {
		// 自分の公開投稿は localTimeline にも積まれる。
		require.NoError(t, fanout.Push(ctx, LocalTimeline, shallow.ID, MaxTimelineLength))
	}
	require.NoError(t, fanout.Push(ctx, shallowListName[name], shallow.ID, MaxTimelineLength))
	return depthFixture{svc: svc, fanout: fanout, spy: spy, deep: deep, shallow: shallow, dbOnly: dbOnly}
}

func (f depthFixture) allIDsDesc(extra ...*model.Note) []string {
	ids := append(append(noteIDs(f.deep), f.shallow.ID, f.dbOnly.ID), noteIDs(extra)...)
	sort.Sort(sort.Reverse(sort.StringSlice(ids)))
	return ids
}

// 再現: 浅い list の最古より古い範囲を、深い list だけで埋めない。そこは DB が
// 返すので、どの list にも無い note (dbOnly) も抜けない。
func TestMultiListTimelines_ShallowListDoesNotSkip(t *testing.T) {
	for name, read := range multiListReads {
		for _, allowPartial := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/allowPartial=%v", name, allowPartial), func(t *testing.T) {
				f := newDepthFixture(t, name)
				got := walkPages(t, f.svc, read, 10, allowPartial)
				assert.Equal(t, f.allIDsDesc(), got,
					"浅い list の期間にある、DB にだけある note も、抜けず重ならずに返ること")
				assert.Positive(t, f.spy.dbCalls())
			})
		}
	}
}

// Redis の list にある自分宛ての返信が cutoff より古くなっても、DB から返ること
// (DB fallback が list の中身を再現する、KeepRepliesToViewer)。cutoff を入れた
// だけでは、以前は Redis から出ていた返信がそこから先だけ消える。
func TestMultiListTimelines_ReplyToViewerBeyondCutoffComesFromDB(t *testing.T) {
	for name, read := range multiListReads {
		for _, allowPartial := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/allowPartial=%v", name, allowPartial), func(t *testing.T) {
				// 自分宛ての返信の list は深く、localTimeline は浅い。
				oldReply := replyToViewer(time.Now().Add(-40 * time.Minute))
				f := newDepthFixture(t, name, oldReply)
				ctx := context.Background()
				replyTo := LocalTimelineWithReplyToName(dbFallbackViewer.ID)
				require.NoError(t, f.fanout.Purge(ctx, replyTo))
				for _, n := range []*model.Note{oldReply, f.dbOnly} {
					if n.ReplyUserID != nil {
						require.NoError(t, f.fanout.Push(ctx, replyTo, n.ID, MaxTimelineLength))
					}
				}
				if name == "local" {
					require.NoError(t, f.fanout.Push(ctx, replyTo, f.shallow.ID, MaxTimelineLength))
				}
				got := walkPages(t, f.svc, read, 10, allowPartial)
				assert.Contains(t, got, oldReply.ID, "list にあった自分宛ての返信が、cutoff より古くなっても出ること")
				assert.Equal(t, f.allIDsDesc(oldReply), got)
			})
		}
	}
}

// social では、ホームの list にある自分の他人宛ての返信も、cutoff より古く
// なったら DB の home 側が返すこと (KeepHomeFanoutReplies)。local 側は他人宛ての
// 返信を返さないので、home 側が無ければ消える。
func TestHybridTimeline_OwnReplyBeyondCutoffComesFromDB(t *testing.T) {
	now := time.Now()
	ownReply := publicNote(now.Add(-20*time.Minute-30*time.Second), dbFallbackViewer.ID)
	parent, other := "parent-of-"+ownReply.ID, "other"
	ownReply.ReplyID, ownReply.ReplyUserID = &parent, &other
	f := newDepthFixture(t, "hybrid", ownReply)
	ctx := context.Background()
	// localTimeline を 10 件の深さにし、ホームの list に古い自分の返信を足す。
	require.NoError(t, f.fanout.Purge(ctx, LocalTimeline))
	for i := 9; i >= 0; i-- {
		require.NoError(t, f.fanout.Push(ctx, LocalTimeline, f.deep[i].ID, MaxTimelineLength))
	}
	require.NoError(t, f.fanout.Push(ctx, LocalTimeline, f.shallow.ID, MaxTimelineLength))
	home := HomeTimelineName(dbFallbackViewer.ID)
	require.NoError(t, f.fanout.Purge(ctx, home))
	for _, id := range []string{ownReply.ID, f.shallow.ID} {
		require.NoError(t, f.fanout.Push(ctx, home, id, MaxTimelineLength))
	}

	got := walkPages(t, f.svc, multiListReads["hybrid"], 10, true)
	assert.Contains(t, got, ownReply.ID)
	assert.Equal(t, f.allIDsDesc(ownReply), got)
}

// filterCapturingRepo records the filter of every home / local query.
type filterCapturingRepo struct {
	*testutil.MockNoteRepository
	home, local []model.TimelineDBFilter
}

func (r *filterCapturingRepo) ListHomeTimeline(userID string, limit int, sinceID, untilID string, f model.TimelineDBFilter) ([]*model.Note, error) {
	r.home = append(r.home, f)
	return r.MockNoteRepository.ListHomeTimeline(userID, limit, sinceID, untilID, f)
}

func (r *filterCapturingRepo) ListLocalTimeline(limit int, sinceID, untilID string, f model.TimelineDBFilter) ([]*model.Note, error) {
	r.local = append(r.local, f)
	return r.MockNoteRepository.ListLocalTimeline(limit, sinceID, untilID, f)
}

// social の DB fallback が付ける返信の例外。home 側の例外 (KeepHomeFanoutReplies) は
// フォロー一覧が読めたとき (FollowingIDs が非 nil = followers 限定の返信の gate が
// SQL でも掛かるとき) だけ付ける。handler の loadFollowingIDs は失敗すると nil を
// 返すので、そのときは付けない (fail closed)。
func TestHybridDBFallback_ReplyExceptions(t *testing.T) {
	cases := []struct {
		name         string
		followingIDs map[string]struct{}
		withReplies  *bool
		wantHome     bool
		wantToViewer bool
	}{
		{name: "followings loaded", followingIDs: map[string]struct{}{}, wantHome: true, wantToViewer: true},
		{name: "followings failed to load", followingIDs: nil, wantHome: false, wantToViewer: true},
		{name: "withReplies needs no exception", followingIDs: map[string]struct{}{}, withReplies: func() *bool { b := true; return &b }(), wantHome: true, wantToViewer: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := &filterCapturingRepo{MockNoteRepository: testutil.NewMockNoteRepository()}
			svc := NewService(newTestService(t), repo, testutil.NewMockFollowingRepository())
			_, err := svc.HybridTimeline(context.Background(), dbFallbackViewer, "", "", 10, TimelineFilter{FollowingIDs: tc.followingIDs, WithReplies: tc.withReplies})
			require.NoError(t, err)
			require.Len(t, repo.home, 1)
			require.Len(t, repo.local, 1)
			assert.Equal(t, tc.wantHome, repo.home[0].KeepHomeFanoutReplies)
			assert.Equal(t, tc.followingIDs != nil, repo.home[0].HideFollowersOnlyReplyFromNonFollowee)
			assert.False(t, repo.local[0].KeepHomeFanoutReplies, "local 側には付けない")
			assert.Equal(t, tc.wantToViewer, repo.home[0].KeepRepliesToViewer)
			assert.Equal(t, tc.wantToViewer, repo.local[0].KeepRepliesToViewer)
		})
	}
}

// list はあるが、窓 (untilId より古い範囲) には何も無い: その list は窓の範囲を
// 持っていないので、Redis の深い list があっても DB から返す。
func TestMultiListTimelines_ListEmptyInWindowFallsBackToDB(t *testing.T) {
	for name, read := range multiListReads {
		t.Run(name, func(t *testing.T) {
			f := newDepthFixture(t, name)
			got, err := read(f.svc, f.shallow.ID, "", 20, true)
			require.NoError(t, err)
			assert.Equal(t, f.allIDsDesc()[1:21], noteIDs(got))
			assert.Contains(t, noteIDs(got), f.dbOnly.ID)
			assert.Positive(t, f.spy.dbCalls())
		})
	}
}

// 1 ページ目のうち、浅い list の範囲だけを Redis から返し、残りを DB で継ぎ足す。
// 継ぎ足しの境界は Redis から返した最古 (= cutoff 以降) なので、間が空かない。
// cutoff で切れて足りないときは allowPartial でも継ぎ足す (短いページを返さない)。
func TestMultiListTimelines_TopsUpBeyondCutoff(t *testing.T) {
	for name, read := range multiListReads {
		for _, allowPartial := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/allowPartial=%v", name, allowPartial), func(t *testing.T) {
				f := newDepthFixture(t, name)
				got, err := read(f.svc, "", "", 20, allowPartial)
				require.NoError(t, err)
				assert.Equal(t, f.allIDsDesc()[:20], noteIDs(got))
				assert.Positive(t, f.spy.dbCalls())
			})
		}
	}
}

// cutoff で ID が落ちても、残りで 1 ページが埋まるなら allowPartial は従来どおり
// 部分ページを返す (mute などで落ちた分を DB で埋めない)。
func TestMultiListTimelines_AllowPartialWithinCoverageKeepsPartialPage(t *testing.T) {
	f := newDepthFixture(t, "hybrid")
	ctx := context.Background()
	// ホームの list を 10 件の深さにする。cutoff は deep[9]、cutoff 以降の ID は 11 件。
	home := HomeTimelineName(dbFallbackViewer.ID)
	require.NoError(t, f.fanout.Purge(ctx, home))
	for i := 9; i >= 0; i-- {
		require.NoError(t, f.fanout.Push(ctx, home, f.deep[i].ID, MaxTimelineLength))
	}
	require.NoError(t, f.fanout.Push(ctx, home, f.shallow.ID, MaxTimelineLength))

	got, err := f.svc.HybridTimeline(ctx, dbFallbackViewer, "", "", 10, TimelineFilter{AllowPartial: true, MutedUserIDs: []string{"other"}})
	require.NoError(t, err)
	assert.Equal(t, []string{f.shallow.ID}, noteIDs(got))
	assert.Zero(t, f.spy.dbCalls(), "cutoff で切れていないページは、部分ページのまま返す")
}

// 深さが揃っているなら従来どおり Redis だけで返す (DB を引かない)。
func TestMultiListTimelines_EqualDepthStaysOnRedis(t *testing.T) {
	for name, read := range multiListReads {
		t.Run(name, func(t *testing.T) {
			f := newDepthFixture(t, name)
			// 3 分より古い ID は末尾より新しいときしか積まれないので、古い順に積み直す。
			ctx := context.Background()
			require.NoError(t, f.fanout.Purge(ctx, shallowListName[name]))
			for i := len(f.deep) - 1; i >= 0; i-- {
				require.NoError(t, f.fanout.Push(ctx, shallowListName[name], f.deep[i].ID, MaxTimelineLength))
			}
			require.NoError(t, f.fanout.Push(ctx, shallowListName[name], f.shallow.ID, MaxTimelineLength))
			first, err := read(f.svc, "", "", 10, false)
			require.NoError(t, err)
			second, err := read(f.svc, first[len(first)-1].ID, "", 10, false)
			require.NoError(t, err)
			want := append([]string{f.shallow.ID}, noteIDs(f.deep[:19])...)
			assert.Equal(t, want, append(noteIDs(first), noteIDs(second)...))
			assert.Zero(t, f.spy.dbCalls(), "全ての list が持っている範囲では DB を引かない")
		})
	}
}

// key の無い list は cutoff に数えない。返信を受けたことの無い利用者の
// `localTimelineWithReplyTo:<viewer>` は常に無いので、数えるとログイン中の
// social / local がほぼ全て DB へ行く (本家 #13495 との違い)。
func TestMultiListTimelines_MissingListIsNotACutoff(t *testing.T) {
	for name, read := range multiListReads {
		t.Run(name, func(t *testing.T) {
			f := newDepthFixture(t, name)
			require.NoError(t, f.fanout.Purge(context.Background(), shallowListName[name]))
			// hybrid の localTimelineWithReplyTo:<viewer> は最初から無い。
			got, err := read(f.svc, f.deep[5].ID, "", 10, false)
			require.NoError(t, err)
			assert.Equal(t, noteIDs(f.deep[6:16]), noteIDs(got))
			assert.Zero(t, f.spy.dbCalls(), "key の無い list のせいで DB へ倒さない")
		})
	}
}

// 遅れて届いた古い ID は先頭に LPUSH される。それが list の最小値になっても、
// list の深さはそこまで延びない (それより新しく、先に push されて押し出された
// note がありうる)。cutoff は位置としての末尾から取る。
func TestMultiListTimelines_LateIDAtHeadDoesNotDeepenCoverage(t *testing.T) {
	now := time.Now()
	tail := publicNote(now.Add(-5*time.Minute-30*time.Second), dbFallbackViewer.ID)
	late := publicNote(now.Add(-10*time.Minute-30*time.Second), dbFallbackViewer.ID)
	// tail より前に push され、LTRIM で押し出された (= ホームの list に無い) 投稿。
	trimmed := &model.Note{ID: idGen.Generate(now.Add(-7*time.Minute - 30*time.Second)), UserID: dbFallbackViewer.ID, Visibility: model.NoteVisibilityHome}
	f := newDepthFixture(t, "hybrid", tail, late, trimmed)
	ctx := context.Background()
	home := HomeTimelineName(dbFallbackViewer.ID)
	require.NoError(t, f.fanout.Purge(ctx, home))
	// push の順 (古い順): tail → shallow → late。late は猶予の経路で先頭に入る。
	key := f.fanout.key(home)
	for _, id := range []string{tail.ID, f.shallow.ID, late.ID} {
		require.NoError(t, testRedis.Client.LPush(ctx, key, id).Err())
	}
	for _, n := range []*model.Note{tail, late} {
		require.NoError(t, f.fanout.Push(ctx, LocalTimeline, n.ID, MaxTimelineLength))
	}

	got := walkPages(t, f.svc, multiListReads["hybrid"], 10, true)
	assert.Contains(t, got, trimmed.ID, "末尾より新しい、押し出された投稿も DB から出ること")
	assert.Equal(t, f.allIDsDesc(tail, late, trimmed), got)
}

// fallback を切っているときは cutoff をかけない (本家 #13495 も `useDbFallback`
// のときだけかける)。捨てても取り戻す先が無く、深い list の note まで見えなくなる。
func TestMultiListTimelines_NoCutoffWithoutDBFallback(t *testing.T) {
	for name, read := range multiListReads {
		for _, allowPartial := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/allowPartial=%v", name, allowPartial), func(t *testing.T) {
				f := newDepthFixture(t, name)
				f.svc.SetDBFallbackToggle(&fakeDBFallbackToggle{enabled: false})
				got := walkPages(t, f.svc, read, 10, allowPartial)
				assert.Equal(t, append([]string{f.shallow.ID}, noteIDs(f.deep)...), got, "Redis の持ち分は全て返す")
				assert.Zero(t, f.spy.dbCalls())
			})
		}
	}
}

// 昇順 (sinceId 単独) は従来どおり DB が処理する。cutoff で先頭を削っても、
// sinceId との間を飛ばさない。
func TestMultiListTimelines_AscendingUnchanged(t *testing.T) {
	for name, read := range multiListReads {
		t.Run(name, func(t *testing.T) {
			f := newDepthFixture(t, name)
			since := f.deep[25].ID
			got, err := read(f.svc, "", since, 10, false)
			require.NoError(t, err)
			want := f.allIDsDesc()
			sort.Strings(want)
			start := sort.SearchStrings(want, since) + 1
			assert.Equal(t, want[start:start+10], noteIDs(got))
			assert.Contains(t, noteIDs(got), f.dbOnly.ID)
		})
	}
}

func TestCoverageCutoff(t *testing.T) {
	cases := []struct {
		name  string
		tails []string
		want  string
	}{
		{name: "no lists", tails: nil, want: ""},
		{name: "single list has no cutoff", tails: []string{"b"}, want: ""},
		{name: "newest tail wins", tails: []string{"a", "c", "b"}, want: "c"},
		{name: "missing list is ignored", tails: []string{"a", "", "b"}, want: "b"},
		{name: "all missing", tails: []string{"", ""}, want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, coverageCutoff(tc.tails))
		})
	}
}

func TestWithinCoverage(t *testing.T) {
	assert.Equal(t, []string{"d", "c"}, withinCoverage([]string{"d", "c", "b", "a"}, "c"))
	assert.Equal(t, []string{"c", "d"}, withinCoverage([]string{"a", "b", "c", "d"}, "c"), "昇順も入力順を保つ")
	ids := []string{"b", "a"}
	assert.Equal(t, ids, withinCoverage(ids, ""))
	assert.Empty(t, withinCoverage([]string{"b", "a"}, "c"))
}

// tail は窓に関係なく list 全体の、位置としての末尾 (最も前に push された要素)。
// key が無い list は空文字列。
func TestFanoutTimelineService_GetMultiWithTails(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	now := time.Now()
	older := idGen.Generate(now.Add(-time.Minute))
	newer := idGen.Generate(now)
	// 猶予内の古い ID は先頭に入るので、最小値 (older) は末尾に無い。
	require.NoError(t, svc.Push(ctx, LocalTimeline, newer, 100))
	require.NoError(t, svc.Push(ctx, LocalTimeline, older, 100))

	lists, tails, err := svc.GetMultiWithTails(ctx, []Name{LocalTimeline, GlobalTimeline}, older, "", 0)
	require.NoError(t, err)
	assert.Equal(t, [][]string{{}, {}}, lists, "窓 (untilId より古い範囲) には何も無い")
	assert.Equal(t, []string{newer, ""}, tails)

	lists, tails, err = svc.GetMultiWithTails(ctx, nil, "", "", 0)
	require.NoError(t, err)
	assert.Nil(t, lists)
	assert.Nil(t, tails)
}

func TestFanoutTimelineService_GetMultiWithTailsError(t *testing.T) {
	svc := newTestService(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err := svc.GetMultiWithTails(ctx, []Name{LocalTimeline}, "", "", 0)
	assert.Error(t, err)
}
