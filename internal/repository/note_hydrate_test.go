package repository

import (
	"context"
	"encoding/json"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// preloadNoteRelations is the GORM Preload chain the note reads used before
// #3611. It is kept as the reference the hydrated shape is compared against
// (and by BenchmarkFindManyByIDsWithUser).
func preloadNoteRelations(db *gorm.DB) *gorm.DB {
	return db.Preload("User").
		Preload("Renote").
		Preload("Renote.User").
		Preload("Renote.Poll").
		Preload("Renote.Renote").
		Preload("Renote.Renote.User").
		Preload("Renote.Renote.Poll").
		Preload("Renote.Reply").
		Preload("Renote.Reply.User").
		Preload("Reply").
		Preload("Reply.User").
		Preload("Reply.Poll").
		Preload("Poll")
}

// sqlCounter is a GORM logger that records every statement it traces.
type sqlCounter struct {
	logger.Interface
	mu   sync.Mutex
	sqls []string
}

func newSQLCounter() *sqlCounter {
	return &sqlCounter{Interface: logger.Discard}
}

func (c *sqlCounter) LogMode(logger.LogLevel) logger.Interface { return c }

func (c *sqlCounter) Trace(_ context.Context, _ time.Time, fc func() (string, int64), _ error) {
	sql, _ := fc()
	c.mu.Lock()
	c.sqls = append(c.sqls, sql)
	c.mu.Unlock()
}

func (c *sqlCounter) reset() {
	c.mu.Lock()
	c.sqls = nil
	c.mu.Unlock()
}

func (c *sqlCounter) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.sqls)
}

// packView renders the parts of a note the NoteEntity packer reads, following
// its embed rules: the poll only on detail notes, renote (detail) and reply
// (no detail) embeds down to depth 2, nothing below a reply embed.
func packView(t *testing.T, n *model.Note, depth int, detail bool) map[string]any {
	t.Helper()
	if n == nil {
		return nil
	}
	row := *n
	row.User, row.Poll, row.Renote, row.Reply = nil, nil, nil, nil
	raw, err := json.Marshal(row)
	require.NoError(t, err)
	var out map[string]any
	require.NoError(t, json.Unmarshal(raw, &out))
	if n.User != nil {
		raw, err := json.Marshal(n.User)
		require.NoError(t, err)
		var u map[string]any
		require.NoError(t, json.Unmarshal(raw, &u))
		out["user"] = u
	} else {
		out["user"] = nil
	}
	if detail {
		if n.Poll != nil {
			p := *n.Poll
			p.Note = nil
			raw, err := json.Marshal(p)
			require.NoError(t, err)
			var pm map[string]any
			require.NoError(t, json.Unmarshal(raw, &pm))
			out["poll"] = pm
		} else {
			out["poll"] = nil
		}
		if depth < 2 {
			out["renote"] = packView(t, n.Renote, depth+1, true)
			out["reply"] = packView(t, n.Reply, depth+1, false)
		}
	}
	return out
}

// hydrateFixture is a set of notes covering every embed the packer expands.
type hydrateFixture struct {
	plain, polled, replyTarget, quote, boost, reply, pollWindow string
	aliasReply, aliasBoost                                      string
	// channelQuote は、チャンネル・タグ・メンション・添付を持つ引用の引用。
	// それぞれで絞る一覧 (#3611 の review M-2) に 2 段の embed を踏ませる。
	channelQuote, channelID, tag, fileID, listID string
	all                                          []string
}

func seedHydrateFixture(t *testing.T) hydrateFixture {
	t.Helper()
	pollRepo := NewPollRepository(testDB)
	users := []*model.User{
		insertTestUser(t, "u_hy3611_1", "hy3611a"),
		insertTestUser(t, "u_hy3611_2", "hy3611b"),
		insertTestUser(t, "u_hy3611_3", "hy3611c"),
		insertTestUser(t, "u_hy3611_4", "hy3611d"),
	}
	for _, u := range users {
		id := u.ID
		t.Cleanup(func() { cleanupUser(t, id) })
	}
	const channelID = "ch_hy3611"
	require.NoError(t, NewChannelRepository(testDB).Create(newTestChannel(channelID, "hy3611", nil)))
	t.Cleanup(func() { cleanupChannel(t, channelID) })
	const listID = "ul_hy3611"
	listRepo := NewUserListRepository(testDB)
	require.NoError(t, listRepo.Create(&model.UserList{ID: listID, UserID: users[0].ID, Name: "hy3611"}))
	require.NoError(t, listRepo.AddMember(&model.UserListMembership{ID: "ulm_hy3611", UserListID: listID, UserID: users[3].ID}))
	t.Cleanup(func() {
		testDB.Exec(`DELETE FROM "user_list_membership" WHERE "userListId" = ?`, listID)
		testDB.Exec(`DELETE FROM "user_list" WHERE id = ?`, listID)
	})
	var created []string
	t.Cleanup(func() {
		for i := len(created) - 1; i >= 0; i-- {
			cleanupNote(t, created[i])
		}
	})
	text := func(s string) *string { return &s }
	mk := func(id string, user int, renote, reply *string, body *string, poll bool) string {
		n := &model.Note{
			ID: id, UserID: users[user].ID, Text: body, Visibility: model.NoteVisibilityPublic,
			RenoteID: renote, ReplyID: reply, HasPoll: poll,
			Reactions: datatypes.JSON([]byte(`{"👍":2}`)),
		}
		require.NoError(t, testDB.Create(n).Error)
		created = append(created, id)
		if poll {
			require.NoError(t, pollRepo.Create(&model.Poll{
				NoteID: id, Choices: model.StringArray{"a", "b"}, Votes: model.Int64Array{1, 2},
				NoteVisibility: model.NoteVisibilityPublic, UserID: users[user].ID,
			}))
		}
		return id
	}
	var f hydrateFixture
	// ID は降順に並べたとき新しいものが先頭に来るよう、zz で始めて他のテストの
	// ノートより後ろに並べる。
	f.polled = mk("zzhy3611_01", 0, nil, nil, text("polled"), true)
	f.plain = mk("zzhy3611_02", 1, nil, nil, text("plain"), false)
	f.replyTarget = mk("zzhy3611_03", 2, nil, &f.plain, text("reply target with poll"), true)
	// 引用: renote = polled (投票あり)、reply = replyTarget (投票あり)。
	f.quote = mk("zzhy3611_04", 1, &f.polled, &f.replyTarget, text("quote"), true)
	f.boost = mk("zzhy3611_05", 3, &f.quote, nil, nil, false)
	f.reply = mk("zzhy3611_06", 3, nil, &f.quote, text("reply to quote"), false)
	// 投票の行はあるが hasPoll が偽 (作成の途中の窓)。
	f.pollWindow = mk("zzhy3611_07", 2, nil, nil, text("window"), false)
	require.NoError(t, pollRepo.Create(&model.Poll{
		NoteID: f.pollWindow, Choices: model.StringArray{"x"}, Votes: model.Int64Array{0},
		NoteVisibility: model.NoteVisibilityPublic, UserID: users[2].ID,
	}))
	// 同じページで、返信先 (reply embed) とリノート先 (renote embed) が同じノート
	// (quote) になる組。hydrate では同じ値を共有する。
	f.aliasReply = mk("zzhy3611_08", 0, nil, &f.quote, text("alias reply"), false)
	f.aliasBoost = mk("zzhy3611_09", 2, &f.quote, nil, nil, false)
	f.channelID, f.tag, f.fileID, f.listID = channelID, "hy3611tag", "hy3611file", listID
	f.channelQuote = mk("zzhy3611_10", 1, &f.quote, nil, text("channel quote #hy3611tag"), false)
	require.NoError(t, testDB.Model(&model.Note{}).Where("id = ?", f.channelQuote).Updates(map[string]any{
		"channelId":    channelID,
		"tags":         model.StringArray{f.tag},
		"mentions":     model.StringArray{users[0].ID},
		"fileIds":      model.StringArray{f.fileID},
		"renoteCount":  3,
		"repliesCount": 2,
	}).Error)
	f.all = append([]string(nil), created...)
	return f
}

// assertMatchesPreload compares every note in got with the same note read
// through the reference Preload chain, as the packer would see them.
// pollWindowID is the one note whose poll the hydrate deliberately drops.
func assertMatchesPreload(t *testing.T, label string, got []*model.Note, pollWindowID string) {
	t.Helper()
	require.NotEmpty(t, got, label)
	ids := make([]string, 0, len(got))
	for _, n := range got {
		ids = append(ids, n.ID)
	}
	var ref []*model.Note
	require.NoError(t, preloadNoteRelations(testDB).Where("id IN ?", ids).Find(&ref).Error)
	refByID := make(map[string]*model.Note, len(ref))
	for _, n := range ref {
		refByID[n.ID] = n
	}
	for _, n := range got {
		want := packView(t, refByID[n.ID], 0, true)
		if n.ID == pollWindowID {
			// hasPoll が偽なら poll を付けない (本家の packer と同じ)。Preload は
			// 行があれば付けていた。
			want["poll"] = nil
		}
		assert.Equal(t, want, packView(t, n, 0, true), "%s: note %s", label, n.ID)
	}
}

// The hydrated reads must give the packer exactly what the Preload chain gave,
// on every read path that used to preload (#3611).
func TestNoteRepository_HydrateMatchesPreload(t *testing.T) {
	f := seedHydrateFixture(t)
	repo := NewNoteRepository(testDB)
	unl := repo.(*noteRepository)

	// フィクスチャが期待どおりの深さを踏んでいること (比較が空振りしない)。
	boost, err := repo.FindByIDWithRelations(f.boost)
	require.NoError(t, err)
	require.NotNil(t, boost.Renote)
	require.NotNil(t, boost.Renote.Renote, "depth-2 renote")
	require.NotNil(t, boost.Renote.Renote.Poll, "depth-2 renote poll")
	require.NotNil(t, boost.Renote.Renote.User, "depth-2 renote user")
	require.NotNil(t, boost.Renote.Reply, "depth-2 reply")
	require.NotNil(t, boost.Renote.Reply.User, "depth-2 reply user")
	require.NotNil(t, boost.Renote.Poll, "depth-1 renote poll")

	for _, id := range f.all {
		n, err := repo.FindByIDWithRelations(id)
		require.NoError(t, err)
		assertMatchesPreload(t, "FindByIDWithRelations", []*model.Note{n}, f.pollWindow)
		n, err = repo.(NotePrimaryReader).FindByIDWithRelationsOnPrimary(id)
		require.NoError(t, err)
		assertMatchesPreload(t, "FindByIDWithRelationsOnPrimary", []*model.Note{n}, f.pollWindow)
	}

	many, err := repo.FindManyByIDsWithUser(f.all)
	require.NoError(t, err)
	require.Len(t, many, len(f.all))
	assertMatchesPreload(t, "FindManyByIDsWithUser", many, f.pollWindow)

	for _, uid := range []string{"u_hy3611_1", "u_hy3611_2", "u_hy3611_3", "u_hy3611_4"} {
		got, err := repo.ListByUserID(uid, "", "", 20)
		require.NoError(t, err)
		assertMatchesPreload(t, "ListByUserID", got, f.pollWindow)
		got, err = repo.ListPublicByUserID(uid, "", "", 20)
		require.NoError(t, err)
		assertMatchesPreload(t, "ListPublicByUserID", got, f.pollWindow)
		got, err = repo.ListByUserIDFiltered(uid, uid, "", "", 20, false, true, true, false)
		require.NoError(t, err)
		assertMatchesPreload(t, "ListByUserIDFiltered", got, f.pollWindow)
	}
	got, err := repo.ListPublicNotesForFeed("u_hy3611_2", 20)
	require.NoError(t, err)
	assertMatchesPreload(t, "ListPublicNotesForFeed", got, f.pollWindow)

	got, err = repo.ListRenotesOf(f.quote, "", "", "", 20)
	require.NoError(t, err)
	assertMatchesPreload(t, "ListRenotesOf", got, f.pollWindow)
	got, err = repo.ListRepliesOf(f.quote, "", "", "", 20)
	require.NoError(t, err)
	assertMatchesPreload(t, "ListRepliesOf", got, f.pollWindow)
	got, err = repo.ListChildrenOf(f.polled, "", "", "", 20)
	require.NoError(t, err)
	assertMatchesPreload(t, "ListChildrenOf", got, f.pollWindow)

	got, err = repo.ListGlobalTimeline(20, "", "", model.TimelineDBFilter{})
	require.NoError(t, err)
	assertMatchesPreload(t, "ListGlobalTimeline", got, f.pollWindow)
	got, err = repo.ListLocalTimeline(20, "", "", model.TimelineDBFilter{})
	require.NoError(t, err)
	assertMatchesPreload(t, "ListLocalTimeline", got, f.pollWindow)
	got, err = repo.ListHomeTimeline("u_hy3611_4", 20, "", "", model.TimelineDBFilter{})
	require.NoError(t, err)
	assertMatchesPreload(t, "ListHomeTimeline", got, f.pollWindow)

	got, err = repo.SearchByFilter(model.NoteSearchFilter{Query: "quote", Limit: 20})
	require.NoError(t, err)
	assertMatchesPreload(t, "SearchByFilter", got, f.pollWindow)

	// 条件で絞る一覧も、どれも hydrate を通る (review M-2)。各経路が fixture の
	// ノートを実際に返すことも確かめ、比較を空振りさせない。
	for _, tc := range []struct {
		label string
		list  func() ([]*model.Note, error)
		want  string
	}{
		{"ListByChannelID", func() ([]*model.Note, error) { return repo.ListByChannelID(f.channelID, "", "", "", 20) }, f.channelQuote},
		{"ListLocalByChannelID", func() ([]*model.Note, error) { return repo.ListLocalByChannelID(f.channelID, "", "", "", 20) }, f.channelQuote},
		{"ListFeatured", func() ([]*model.Note, error) { return repo.ListFeatured(f.channelID, "", 20, 0) }, f.channelQuote},
		{"ListFeaturedByUser", func() ([]*model.Note, error) { return repo.ListFeaturedByUser("u_hy3611_2", "u_hy3611_2", "", 20) }, f.quote},
		{"ListMentions", func() ([]*model.Note, error) { return repo.ListMentions("u_hy3611_1", "", false, 20, "", "") }, f.channelQuote},
		{"SearchByTag", func() ([]*model.Note, error) {
			return repo.SearchByTag([][]string{{f.tag}}, "", 20, "", "", model.NoteSearchTagFilter{})
		}, f.channelQuote},
		{"ListByFileID", func() ([]*model.Note, error) { return repo.ListByFileID(f.fileID, "", "", 20) }, f.channelQuote},
		{"ListPublicNotes", func() ([]*model.Note, error) { return repo.ListPublicNotes(model.PublicNotesFilter{}, 20, "", "") }, f.channelQuote},
		{"ListByUserList", func() ([]*model.Note, error) {
			return repo.ListByUserList(f.listID, 20, "", "", model.TimelineDBFilter{})
		}, f.boost},
	} {
		got, err := tc.list()
		require.NoError(t, err, tc.label)
		found := false
		for _, n := range got {
			found = found || n.ID == tc.want
		}
		require.True(t, found, "%s: %s not returned", tc.label, tc.want)
		assertMatchesPreload(t, tc.label, got, f.pollWindow)
	}

	// Unhydrated + HydrateNotes (hybrid の DB fallback が使う組) も同じ形になる。
	raw, err := unl.ListLocalTimelineUnhydrated(20, "", "", model.TimelineDBFilter{})
	require.NoError(t, err)
	for _, n := range raw {
		require.Nil(t, n.User, "unhydrated rows carry no relations")
	}
	require.NoError(t, unl.HydrateNotes(raw))
	assertMatchesPreload(t, "ListLocalTimelineUnhydrated+HydrateNotes", raw, f.pollWindow)
	raw, err = unl.ListHomeTimelineUnhydrated("u_hy3611_4", 20, "", "", model.TimelineDBFilter{})
	require.NoError(t, err)
	require.NoError(t, unl.HydrateNotes(raw))
	assertMatchesPreload(t, "ListHomeTimelineUnhydrated+HydrateNotes", raw, f.pollWindow)

	now := time.Now()
	require.NoError(t, testDB.Create(&model.Role{ID: "role_hy3611", Name: "hy3611", UpdatedAt: now, LastUsedAt: now}).Error)
	t.Cleanup(func() {
		testDB.Exec(`DELETE FROM "role_assignment" WHERE "roleId" = ?`, "role_hy3611")
		testDB.Exec(`DELETE FROM "role" WHERE id = ?`, "role_hy3611")
	})
	require.NoError(t, testDB.Create(&model.RoleAssignment{ID: "ra_hy3611", RoleID: "role_hy3611", UserID: "u_hy3611_4"}).Error)
	byRole, err := NewRoleNotesQuery(testDB).ListByRole("role_hy3611", 20, "", "")
	require.NoError(t, err)
	require.Len(t, byRole, 2)
	assertMatchesPreload(t, "RoleNotesQuery.ListByRole", byRole, f.pollWindow)
}

// A note whose hasPoll is false gets no poll even when a poll row exists, and
// one whose hasPoll is true gets it (#3611, upstream packs the poll only when
// note.hasPoll).
func TestNoteRepository_HydratePollFollowsHasPoll(t *testing.T) {
	f := seedHydrateFixture(t)
	repo := NewNoteRepository(testDB)

	n, err := repo.FindByIDWithRelations(f.pollWindow)
	require.NoError(t, err)
	assert.Nil(t, n.Poll)
	n, err = repo.FindByIDWithRelations(f.polled)
	require.NoError(t, err)
	require.NotNil(t, n.Poll)
	assert.Equal(t, model.Int64Array{1, 2}, n.Poll.Votes)
}

// The single-note read issues one statement per kind it needs: a note without
// poll / renote / reply costs the note and its author only (#3611; the Preload
// chain also queried poll, i.e. 3).
func TestNoteRepository_FindByIDWithRelations_QueryCount(t *testing.T) {
	f := seedHydrateFixture(t)
	counter := newSQLCounter()
	repo := NewNoteRepository(testDB.Session(&gorm.Session{Logger: counter}))

	cases := []struct {
		name string
		id   string
		want int
	}{
		{"plain note: note + user", f.plain, 2},
		{"note with poll: note + user + poll", f.polled, 3},
		{"pure renote of a quote: note + targets + depth-2 targets + users + polls", f.boost, 5},
		{"reply to a polled note: note + target + users + poll", f.reply, 4},
	}
	for _, tc := range cases {
		counter.reset()
		n, err := repo.FindByIDWithRelations(tc.id)
		require.NoError(t, err)
		require.NotNil(t, n.User)
		assert.Equal(t, tc.want, counter.count(), tc.name)
	}

	// 一覧も種類ごとに 1 本: notes + sub + lvl2 + users + polls。
	counter.reset()
	notes, err := repo.ListByUserID("u_hy3611_4", "", "", 20)
	require.NoError(t, err)
	require.Len(t, notes, 2)
	assert.Equal(t, 5, counter.count(), "ListByUserID")
}

// hybrid 等で併合してから hydrate しても、併合の順序を保ったまま配線する。
func TestNoteRepository_HydrateNotesKeepsOrderAndEmpty(t *testing.T) {
	f := seedHydrateFixture(t)
	repo := NewNoteRepository(testDB).(*noteRepository)
	require.NoError(t, repo.HydrateNotes(nil))

	var rows []*model.Note
	require.NoError(t, testDB.Where("id IN ?", []string{f.boost, f.plain, f.reply}).Find(&rows).Error)
	sort.Slice(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })
	before := []string{rows[0].ID, rows[1].ID, rows[2].ID}
	require.NoError(t, repo.HydrateNotes(rows))
	assert.Equal(t, before, []string{rows[0].ID, rows[1].ID, rows[2].ID})
	for _, n := range rows {
		assert.NotNil(t, n.User, n.ID)
	}
}
