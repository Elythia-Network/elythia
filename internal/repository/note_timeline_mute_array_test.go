package repository

import (
	"fmt"
	"sort"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
)

// muteArrayFixture is the shared setup of the #3987 tests: a viewer, authors
// muted in various ways, and notes touching each of them through the author,
// the renote target and the reply target.
type muteArrayFixture struct {
	repo     NoteRepository
	rc       *RelationCache
	mutings  MutingRepository
	renotes  RenoteMutingRepository
	viewer   string
	active   string // muted without expiry
	future   string // muted until one hour later
	expired  string // muted until one hour ago
	soon     string // muted until ten minutes later
	renoteMu string // renote-muted
	ok       string // not muted

	soonExpiresAt time.Time
	notes         map[string]string // label -> note id
}

const muteArrayIDPrefix = "m3987"

func newMuteArrayFixture(t *testing.T) *muteArrayFixture {
	t.Helper()
	// 期限の判定をキャッシュ側の時刻で確かめるため、TTL では捨てない長さにする。
	rc := NewRelationCacheWithTTL(testDB, time.Hour)
	f := &muteArrayFixture{
		repo:    NewNoteRepository(testDB),
		rc:      rc,
		mutings: NewMutingRepositoryWithRelationCache(testDB, rc),
		renotes: NewRenoteMutingRepositoryWithRelationCache(testDB, rc),
		notes:   map[string]string{},
	}
	user := func(label string) string {
		// user.token は char(16) で、insertTestUser は "tok_" + id を入れるので短くする。
		id := "m7u" + label
		insertTestUser(t, id, muteArrayIDPrefix+label)
		t.Cleanup(func() { cleanupUser(t, id) })
		return id
	}
	f.viewer = user("viewer")
	f.active = user("active")
	f.future = user("future")
	f.expired = user("expired")
	f.soon = user("soon")
	f.renoteMu = user("renotemu")
	f.ok = user("ok")

	// ホームの経路でも全員の投稿が候補に入るよう、閲覧者は全員をフォローする。
	for _, followee := range []string{f.active, f.future, f.expired, f.soon, f.renoteMu, f.ok} {
		id := muteArrayIDPrefix + "f_" + followee
		require.NoError(t, testDB.Create(&model.Following{ID: id, FollowerID: f.viewer, FolloweeID: followee}).Error)
		t.Cleanup(func() { testDB.Exec(`DELETE FROM "following" WHERE id = ?`, id) })
	}

	hourLater := time.Now().Add(time.Hour)
	hourAgo := time.Now().Add(-time.Hour)
	f.soonExpiresAt = time.Now().Add(10 * time.Minute)
	f.mute(t, "active", f.active, nil)
	f.mute(t, "future", f.future, &hourLater)
	f.mute(t, "expired", f.expired, &hourAgo)
	f.mute(t, "soon", f.soon, &f.soonExpiresAt)
	rm := &model.RenoteMuting{ID: muteArrayIDPrefix + "r_renotemu", MuterID: f.viewer, MuteeID: f.renoteMu}
	require.NoError(t, f.renotes.Create(rm))
	t.Cleanup(func() { cleanupRenoteMuting(t, rm.ID) })

	text := func(s string) *string { return &s }
	seq := 0
	note := func(label, author string, mod func(n *model.Note)) string {
		seq++
		id := fmt.Sprintf("%sn_%02d_%s", muteArrayIDPrefix, seq, label)
		n := &model.Note{
			ID: id, UserID: author, Text: text(label),
			Visibility: model.NoteVisibilityPublic,
			Reactions:  datatypes.JSON([]byte("{}")),
		}
		if mod != nil {
			mod(n)
		}
		require.NoError(t, f.repo.Create(n))
		t.Cleanup(func() { cleanupNote(t, id) })
		f.notes[label] = id
		return id
	}
	renoteOf := func(targetID, targetUser string) func(n *model.Note) {
		return func(n *model.Note) {
			n.Text = nil
			n.RenoteID = &targetID
			n.RenoteUserID = &targetUser
		}
	}
	replyTo := func(targetID, targetUser string) func(n *model.Note) {
		return func(n *model.Note) {
			n.ReplyID = &targetID
			n.ReplyUserID = &targetUser
		}
	}
	activeNote := note("active", f.active, nil)
	note("future", f.future, nil)
	note("expired", f.expired, nil)
	note("soon", f.soon, nil)
	okNote := note("ok", f.ok, nil)
	note("ok_renotes_active", f.ok, renoteOf(activeNote, f.active))
	note("ok_replies_active", f.ok, replyTo(activeNote, f.active))
	note("renotemu_plain", f.renoteMu, nil)
	note("renotemu_pure", f.renoteMu, renoteOf(okNote, f.ok))
	note("renotemu_quote", f.renoteMu, func(n *model.Note) {
		renoteOf(okNote, f.ok)(n)
		n.Text = text("quote")
	})
	return f
}

func (f *muteArrayFixture) mute(t *testing.T, label, mutee string, expiresAt *time.Time) {
	t.Helper()
	m := &model.Muting{ID: muteArrayIDPrefix + "m_" + label, MuterID: f.viewer, MuteeID: mutee, ExpiresAt: expiresAt}
	require.NoError(t, f.mutings.Create(m))
	t.Cleanup(func() { cleanupMuting(t, m.ID) })
}

// labels returns the labels of the fixture notes among rows, sorted.
func (f *muteArrayFixture) labels(rows []*model.Note) []string {
	byID := make(map[string]string, len(f.notes))
	for label, id := range f.notes {
		byID[id] = label
	}
	var out []string
	for _, n := range rows {
		if label, ok := byID[n.ID]; ok {
			out = append(out, label)
		}
	}
	sort.Strings(out)
	return out
}

// list runs the home, local and global timelines with filter and returns the
// fixture labels each of them kept.
func (f *muteArrayFixture) list(t *testing.T, filter model.TimelineDBFilter) map[string][]string {
	t.Helper()
	// 他のテストの行を混ぜないよう、ID の範囲で fixture のノートだけに絞る。
	since, until := muteArrayIDPrefix+"n_", muteArrayIDPrefix+"n_~"
	home, err := f.repo.ListHomeTimeline(f.viewer, 100, since, until, filter)
	require.NoError(t, err)
	local, err := f.repo.ListLocalTimeline(100, since, until, filter)
	require.NoError(t, err)
	global, err := f.repo.ListGlobalTimeline(100, since, until, filter)
	require.NoError(t, err)
	return map[string][]string{
		"home":   f.labels(home),
		"local":  f.labels(local),
		"global": f.labels(global),
	}
}

// subqueryFilter is the filter the timelines used before #3987.
func (f *muteArrayFixture) subqueryFilter() model.TimelineDBFilter {
	return model.TimelineDBFilter{ViewerID: f.viewer, UseMutingSubquery: true, UseRenoteMutingSubquery: true}
}

// cachedFilter is the filter the handler builds from the relation cache.
func (f *muteArrayFixture) cachedFilter(t *testing.T) model.TimelineDBFilter {
	t.Helper()
	muted, err := f.rc.MutedUserIDs(f.viewer)
	require.NoError(t, err)
	renoteMuted, err := f.rc.RenoteMutedUserIDs(f.viewer)
	require.NoError(t, err)
	filter := f.subqueryFilter()
	filter.MutedUserIDs, filter.MutedUserIDsComplete = muted, true
	filter.RenoteMutedUserIDs, filter.RenoteMutedUserIDsComplete = renoteMuted, true
	return filter
}

// assertSameAsSubquery checks that the cached lists keep exactly the notes the
// subquery keeps, and that both keep want.
func (f *muteArrayFixture) assertSameAsSubquery(t *testing.T, want []string) {
	t.Helper()
	sort.Strings(want)
	bySubquery := f.list(t, f.subqueryFilter())
	byList := f.list(t, f.cachedFilter(t))
	for _, tl := range []string{"home", "local", "global"} {
		assert.Equal(t, want, bySubquery[tl], "%s timeline by subquery", tl)
		assert.Equal(t, bySubquery[tl], byList[tl], "%s timeline: the cached list must keep the same notes as the subquery", tl)
	}
}

// TestTimelineMuteArray_SameNotesAsSubquery pins that the timeline SQL keeps
// the same notes whether the mutes come from the relation cache as an array or
// from the muting / renote_muting subqueries (#3987), including expiry.
func TestTimelineMuteArray_SameNotesAsSubquery(t *testing.T) {
	f := newMuteArrayFixture(t)

	// 期限前 (active / future / soon) は投稿者・リノート先・返信先のどれでも落ち、
	// 期限切れ (expired) は残る。リノートミュートは pure renote だけを落とす。
	beforeExpiry := []string{"expired", "ok", "renotemu_plain", "renotemu_quote"}
	f.assertSameAsSubquery(t, beforeExpiry)

	// 一覧をキャッシュに載せたまま soon の期限を越えたことにする。実時間は
	// 待たず、DB の行の期限を過去へずらし (キャッシュは捨てない)、キャッシュの
	// 時計を 30 分進める。キャッシュは読み直さず、一覧を返すときに自分の時計で
	// 期限を判定する。SQL は NOW() で判定する。future (1 時間後) はどちらでも残る。
	require.NoError(t, testDB.Exec(`UPDATE "muting" SET "expiresAt" = NOW() - interval '1 second' WHERE id = ?`,
		muteArrayIDPrefix+"m_soon").Error)
	advanced := time.Now().Add(30 * time.Minute)
	f.rc.now = func() time.Time { return advanced }
	f.assertSameAsSubquery(t, append([]string{"soon"}, beforeExpiry...))
}

// TestTimelineMuteArray_FollowsMuteWrites pins that the cached list follows a
// mute added or removed through the repository, so the array keeps matching
// the subquery right after the write (#3987).
func TestTimelineMuteArray_FollowsMuteWrites(t *testing.T) {
	f := newMuteArrayFixture(t)
	base := []string{"expired", "ok", "renotemu_plain", "renotemu_quote"}
	// キャッシュに載せてから書き込む。
	f.assertSameAsSubquery(t, base)

	f.mute(t, "ok", f.ok, nil)
	// ok を投稿者・リノート先にもつノートは全部落ちる。
	f.assertSameAsSubquery(t, []string{"expired", "renotemu_plain"})

	require.NoError(t, f.mutings.Delete(&model.Muting{ID: muteArrayIDPrefix + "m_ok"}))
	f.assertSameAsSubquery(t, base)

	require.NoError(t, f.renotes.Delete(&model.RenoteMuting{ID: muteArrayIDPrefix + "r_renotemu"}))
	f.assertSameAsSubquery(t, append([]string{"renotemu_pure"}, base...))
}

// TestTimelineMuteArray_ChoosesShape pins when the list replaces the subquery
// (#3987): only when the caller read it completely and it is at most
// maxTimelineMuteArrayIDs long. A list that was not read, or a long one, keeps
// the subquery, so a mute is never dropped silently.
func TestTimelineMuteArray_ChoosesShape(t *testing.T) {
	f := newMuteArrayFixture(t)
	withMutes := []string{"expired", "ok", "renotemu_plain", "renotemu_quote"}
	// 一覧が使われたかを見分けるため、DB の行と食い違う一覧を渡す。
	noMutes := []string{"active", "expired", "future", "ok", "ok_renotes_active", "ok_replies_active",
		"renotemu_plain", "renotemu_pure", "renotemu_quote", "soon"}

	bogus := func(n int) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = fmt.Sprintf("%sx_%03d", muteArrayIDPrefix, i)
		}
		return out
	}
	tests := []struct {
		name   string
		filter func(model.TimelineDBFilter) model.TimelineDBFilter
		want   []string
	}{
		{
			name: "complete empty lists drop both conditions",
			filter: func(fl model.TimelineDBFilter) model.TimelineDBFilter {
				fl.MutedUserIDsComplete, fl.RenoteMutedUserIDsComplete = true, true
				return fl
			},
			want: noMutes,
		},
		{
			name: "incomplete lists keep the subqueries",
			filter: func(fl model.TimelineDBFilter) model.TimelineDBFilter {
				return fl
			},
			want: withMutes,
		},
		{
			name: "complete list at the limit is used",
			filter: func(fl model.TimelineDBFilter) model.TimelineDBFilter {
				fl.MutedUserIDs = bogus(maxTimelineMuteArrayIDs)
				fl.RenoteMutedUserIDs = bogus(maxTimelineMuteArrayIDs)
				fl.MutedUserIDsComplete, fl.RenoteMutedUserIDsComplete = true, true
				return fl
			},
			want: noMutes,
		},
		{
			name: "complete list over the limit keeps the subqueries",
			filter: func(fl model.TimelineDBFilter) model.TimelineDBFilter {
				fl.MutedUserIDs = bogus(maxTimelineMuteArrayIDs + 1)
				fl.RenoteMutedUserIDs = bogus(maxTimelineMuteArrayIDs + 1)
				fl.MutedUserIDsComplete, fl.RenoteMutedUserIDsComplete = true, true
				return fl
			},
			want: withMutes,
		},
		{
			name: "list without the subquery request is applied as is",
			filter: func(fl model.TimelineDBFilter) model.TimelineDBFilter {
				fl.UseMutingSubquery, fl.UseRenoteMutingSubquery = false, false
				fl.MutedUserIDs = []string{f.active, f.future}
				fl.RenoteMutedUserIDs = []string{f.renoteMu}
				return fl
			},
			want: []string{"expired", "ok", "renotemu_plain", "renotemu_quote", "soon"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := f.list(t, tt.filter(f.subqueryFilter()))
			want := append([]string(nil), tt.want...)
			sort.Strings(want)
			for _, tl := range []string{"home", "local", "global"} {
				assert.Equal(t, want, got[tl], "%s timeline", tl)
			}
		})
	}
}

func TestUseTimelineMuteSubquery(t *testing.T) {
	tests := []struct {
		name      string
		requested bool
		viewer    string
		complete  bool
		n         int
		want      bool
	}{
		{"not requested", false, "v", false, 0, false},
		{"anonymous", true, "", false, 0, false},
		{"incomplete list", true, "v", false, 0, true},
		{"complete empty list", true, "v", true, 0, false},
		{"complete list at the limit", true, "v", true, maxTimelineMuteArrayIDs, false},
		{"complete list over the limit", true, "v", true, maxTimelineMuteArrayIDs + 1, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, useTimelineMuteSubquery(tt.requested, tt.viewer, tt.complete, tt.n))
		})
	}
}
