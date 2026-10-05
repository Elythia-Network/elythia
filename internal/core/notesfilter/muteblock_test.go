package notesfilter_test

import (
	"errors"
	"testing"

	"gorm.io/datatypes"
	"gorm.io/gorm"

	"github.com/elythia-network/elythia/internal/core/notesfilter"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func strp(s string) *string { return &s }

// --- LoadMuteBlockSets ------------------------------------------------------

func TestLoadMuteBlockSets_NilViewer(t *testing.T) {
	sets, err := notesfilter.LoadMuteBlockSets(nil,
		testutil.NewMockMutingRepository(),
		testutil.NewMockBlockingRepository(),
		testutil.NewMockChannelMutingRepository(), nil)
	require.NoError(t, err)
	assert.Empty(t, sets.MutedUserIDs)
	assert.Empty(t, sets.BlockerIDs)
	assert.Empty(t, sets.MutedChannelIDs)
}

func TestLoadMuteBlockSets_NilRepos(t *testing.T) {
	// 未配線リポジトリは該当 dimension を no-op として扱う。
	sets, err := notesfilter.LoadMuteBlockSets(&model.User{ID: "viewer"}, nil, nil, nil, nil)
	require.NoError(t, err)
	assert.Nil(t, sets.MutedUserIDs)
	assert.Nil(t, sets.BlockerIDs)
	assert.Nil(t, sets.MutedChannelIDs)
}

func TestLoadMuteBlockSets_PopulatesAllSets(t *testing.T) {
	muting := testutil.NewMockMutingRepository()
	require.NoError(t, muting.Create(&model.Muting{ID: "m1", MuterID: "viewer", MuteeID: "muted"}))
	blocking := testutil.NewMockBlockingRepository()
	require.NoError(t, blocking.Create(&model.Blocking{ID: "b1", BlockerID: "blocker", BlockeeID: "viewer"}))
	channelMuting := testutil.NewMockChannelMutingRepository()
	require.NoError(t, channelMuting.Create(&model.ChannelMuting{UserID: "viewer", ChannelID: "ch1"}))

	sets, err := notesfilter.LoadMuteBlockSets(&model.User{ID: "viewer"}, muting, blocking, channelMuting, nil)
	require.NoError(t, err)
	assert.Contains(t, sets.MutedUserIDs, "muted")
	assert.Contains(t, sets.BlockerIDs, "blocker")
	assert.Contains(t, sets.MutedChannelIDs, "ch1")
}

func TestLoadMuteBlockSets_NoChannelMutesYieldsNilSet(t *testing.T) {
	sets, err := notesfilter.LoadMuteBlockSets(&model.User{ID: "viewer"},
		testutil.NewMockMutingRepository(),
		testutil.NewMockBlockingRepository(),
		testutil.NewMockChannelMutingRepository(), nil)
	require.NoError(t, err)
	assert.Nil(t, sets.MutedChannelIDs, "no channel mutes leaves the set nil so Apply skips it")
}

// failing repos to exercise fail-closed error propagation.

type failingMutingRepo struct{ repository.MutingRepository }

func (failingMutingRepo) ListMuteeIDs(string) ([]string, error) {
	return nil, errors.New("muting boom")
}

type failingChannelMutingRepo struct {
	repository.ChannelMutingRepository
}

func (failingChannelMutingRepo) ListByUser(string) ([]*model.ChannelMuting, error) {
	return nil, errors.New("channel boom")
}

func TestLoadMuteBlockSets_MutingError(t *testing.T) {
	_, err := notesfilter.LoadMuteBlockSets(&model.User{ID: "viewer"},
		failingMutingRepo{testutil.NewMockMutingRepository()},
		testutil.NewMockBlockingRepository(),
		testutil.NewMockChannelMutingRepository(), nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "load muted users")
}

func TestLoadMuteBlockSets_BlockingError(t *testing.T) {
	blocking := testutil.NewMockBlockingRepository()
	blocking.ExistsErr = errors.New("block boom") // ListBlockerIDs もこのエラーで失敗する
	_, err := notesfilter.LoadMuteBlockSets(&model.User{ID: "viewer"},
		testutil.NewMockMutingRepository(), blocking, testutil.NewMockChannelMutingRepository(), nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "load blockers")
}

func TestLoadMuteBlockSets_ChannelMutingError(t *testing.T) {
	_, err := notesfilter.LoadMuteBlockSets(&model.User{ID: "viewer"},
		testutil.NewMockMutingRepository(),
		testutil.NewMockBlockingRepository(),
		failingChannelMutingRepo{testutil.NewMockChannelMutingRepository()}, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "load muted channels")
}

// --- ApplyMuteBlockChannel --------------------------------------------------

func TestApplyMuteBlockChannel_EmptyNotes(t *testing.T) {
	out, err := notesfilter.ApplyMuteBlockChannel(nil, notesfilter.MuteBlockSets{
		MutedUserIDs: map[string]struct{}{"x": {}},
	}, nil)
	require.NoError(t, err)
	assert.Empty(t, out)
}

func TestApplyMuteBlockChannel_AllEmptySetsIsNoOp(t *testing.T) {
	notes := []*model.Note{{ID: "n1", UserID: "muted"}}
	out, err := notesfilter.ApplyMuteBlockChannel(notes, notesfilter.MuteBlockSets{}, nil)
	require.NoError(t, err)
	assert.Len(t, out, 1)
}

func TestApplyMuteBlockChannel_MutedAuthorReplyRenote(t *testing.T) {
	sets := notesfilter.MuteBlockSets{MutedUserIDs: map[string]struct{}{"muted": {}}}
	notes := []*model.Note{
		{ID: "keep", UserID: "author"},
		{ID: "by-muted", UserID: "muted"},
		{ID: "reply-to-muted", UserID: "author", ReplyUserID: strp("muted")},
		{ID: "renote-of-muted", UserID: "author", RenoteUserID: strp("muted")},
	}
	out, err := notesfilter.ApplyMuteBlockChannel(notes, sets, nil)
	require.NoError(t, err)
	require.Len(t, out, 1)
	assert.Equal(t, "keep", out[0].ID)
}

func TestApplyMuteBlockChannel_BlockerAuthorReplyRenote(t *testing.T) {
	sets := notesfilter.MuteBlockSets{BlockerIDs: map[string]struct{}{"blocker": {}}}
	notes := []*model.Note{
		{ID: "keep", UserID: "author"},
		{ID: "by-blocker", UserID: "blocker"},
		{ID: "reply-to-blocker", UserID: "author", ReplyUserID: strp("blocker")},
		{ID: "renote-of-blocker", UserID: "author", RenoteUserID: strp("blocker")},
	}
	out, err := notesfilter.ApplyMuteBlockChannel(notes, sets, nil)
	require.NoError(t, err)
	require.Len(t, out, 1)
	assert.Equal(t, "keep", out[0].ID)
}

func TestApplyMuteBlockChannel_MutedChannelAndRenoteChannel(t *testing.T) {
	sets := notesfilter.MuteBlockSets{MutedChannelIDs: map[string]struct{}{"ch-muted": {}}}
	notes := []*model.Note{
		{ID: "no-channel", UserID: "author"},
		{ID: "other-channel", UserID: "author", ChannelID: strp("ch-ok")},
		{ID: "muted-channel", UserID: "author", ChannelID: strp("ch-muted")},
		{ID: "muted-renote-channel", UserID: "author", RenoteChannelID: strp("ch-muted")},
	}
	out, err := notesfilter.ApplyMuteBlockChannel(notes, sets, nil)
	require.NoError(t, err)
	require.Len(t, out, 2)
	assert.Equal(t, "no-channel", out[0].ID)
	assert.Equal(t, "other-channel", out[1].ID)
}

func TestApplyMuteBlockChannel_SkipsNilEntries(t *testing.T) {
	sets := notesfilter.MuteBlockSets{MutedUserIDs: map[string]struct{}{"muted": {}}}
	notes := []*model.Note{nil, {ID: "keep", UserID: "author"}}
	out, err := notesfilter.ApplyMuteBlockChannel(notes, sets, nil)
	require.NoError(t, err)
	require.Len(t, out, 1)
	assert.Equal(t, "keep", out[0].ID)
}

// --- #1630: muted-instances ---

// stubProfileLookup returns a fixed profile or error.
type stubProfileLookup struct {
	profile *model.UserProfile
	err     error
}

func (s *stubProfileLookup) FindProfileByUserID(string) (*model.UserProfile, error) {
	return s.profile, s.err
}

func TestLoadMuteBlockSets_MutedInstances(t *testing.T) {
	profiles := &stubProfileLookup{profile: &model.UserProfile{
		UserID:         "viewer",
		MutedInstances: datatypes.JSON(`["Muted.Example","other.example"]`),
	}}
	sets, err := notesfilter.LoadMuteBlockSets(&model.User{ID: "viewer"}, nil, nil, nil, profiles)
	require.NoError(t, err)
	// lower-case 化して格納される
	assert.Contains(t, sets.MutedInstances, "muted.example")
	assert.Contains(t, sets.MutedInstances, "other.example")
}

func TestLoadMuteBlockSets_MutedInstancesEdgeCases(t *testing.T) {
	// profile 行なし (record not found) は空 set で正常
	sets, err := notesfilter.LoadMuteBlockSets(&model.User{ID: "viewer"}, nil, nil, nil,
		&stubProfileLookup{err: gorm.ErrRecordNotFound})
	require.NoError(t, err)
	assert.Nil(t, sets.MutedInstances)

	// 空配列 / 空文字 entry のみは nil set
	sets, err = notesfilter.LoadMuteBlockSets(&model.User{ID: "viewer"}, nil, nil, nil,
		&stubProfileLookup{profile: &model.UserProfile{MutedInstances: datatypes.JSON(`["",""]`)}})
	require.NoError(t, err)
	assert.Nil(t, sets.MutedInstances)

	// repo error は fail-closed
	_, err = notesfilter.LoadMuteBlockSets(&model.User{ID: "viewer"}, nil, nil, nil,
		&stubProfileLookup{err: errors.New("profile boom")})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "load muted instances")

	// jsonb 破損も fail-closed
	_, err = notesfilter.LoadMuteBlockSets(&model.User{ID: "viewer"}, nil, nil, nil,
		&stubProfileLookup{profile: &model.UserProfile{MutedInstances: datatypes.JSON(`{broken`)}})
	require.Error(t, err)
}

func TestLoadMuteBlockSets_MutedInstancesNilProfile(t *testing.T) {
	// profile が nil (err も nil) なら空 set。lookup 実装が not-found を
	// (nil, nil) で返すケースを想定。
	sets, err := notesfilter.LoadMuteBlockSets(&model.User{ID: "viewer"}, nil, nil, nil,
		&stubProfileLookup{profile: nil})
	require.NoError(t, err)
	assert.Nil(t, sets.MutedInstances)
}

func TestApplyMuteBlockChannel_MutedInstances(t *testing.T) {
	sets := notesfilter.MuteBlockSets{MutedInstances: map[string]struct{}{"muted.example": {}}}
	notes := []*model.Note{
		{ID: "local", UserID: "a"}, // host nil = local は常に通す
		{ID: "ok-host", UserID: "a", UserHost: strp("ok.example")},
		{ID: "by-muted-host", UserID: "a", UserHost: strp("muted.example")},
		{ID: "case-insensitive", UserID: "a", UserHost: strp("Muted.Example")},
		{ID: "reply-muted-host", UserID: "a", ReplyUserHost: strp("muted.example")},
		{ID: "renote-muted-host", UserID: "a", RenoteUserHost: strp("muted.example")},
		// upstream jsonb `?` は exact 一致なので subdomain は通す
		{ID: "subdomain-passes", UserID: "a", UserHost: strp("sub.muted.example")},
	}
	out, err := notesfilter.ApplyMuteBlockChannel(notes, sets, nil)
	require.NoError(t, err)
	ids := make([]string, 0, len(out))
	for _, n := range out {
		ids = append(ids, n.ID)
	}
	assert.Equal(t, []string{"local", "ok-host", "subdomain-passes"}, ids)
}

// --- #1630: renote 入れ子変種 ---

// stubRenoteLookup serves renote rows from a map (and can fail).
type stubRenoteLookup struct {
	rows map[string]*model.Note
	err  error
	got  []string
}

func (s *stubRenoteLookup) FindManyByIDsWithUser(ids []string) ([]*model.Note, error) {
	s.got = append(s.got, ids...)
	if s.err != nil {
		return nil, s.err
	}
	out := make([]*model.Note, 0, len(ids))
	for _, id := range ids {
		if n, ok := s.rows[id]; ok {
			out = append(out, n)
		}
	}
	return out, nil
}

func TestApplyMuteBlockChannel_RenoteNestedAuthors(t *testing.T) {
	sets := notesfilter.MuteBlockSets{
		MutedUserIDs:   map[string]struct{}{"muted": {}},
		BlockerIDs:     map[string]struct{}{"blocker": {}},
		MutedInstances: map[string]struct{}{"muted.example": {}},
	}
	renotes := &stubRenoteLookup{rows: map[string]*model.Note{
		// renote 先が muted user への reply
		"r1": {ID: "r1", UserID: "a", ReplyUserID: strp("muted")},
		// renote 先が blocker の note の renote (quote 連鎖)
		"r2": {ID: "r2", UserID: "a", RenoteUserID: strp("blocker")},
		// renote 先の reply author が muted instance
		"r3": {ID: "r3", UserID: "a", ReplyUserHost: strp("muted.example")},
		// renote 先がクリーン
		"r4": {ID: "r4", UserID: "a"},
	}}
	notes := []*model.Note{
		{ID: "renotes-reply-to-muted", UserID: "b", RenoteID: strp("r1"), RenoteUserID: strp("a")},
		{ID: "renotes-quote-of-blocker", UserID: "b", RenoteID: strp("r2"), RenoteUserID: strp("a")},
		{ID: "renotes-instance-reply", UserID: "b", RenoteID: strp("r3"), RenoteUserID: strp("a")},
		{ID: "renotes-clean", UserID: "b", RenoteID: strp("r4"), RenoteUserID: strp("a")},
		// renote 先行が見つからない場合は upstream leftJoin null 相当で通す
		{ID: "renote-ref-missing", UserID: "b", RenoteID: strp("gone"), RenoteUserID: strp("a")},
		{ID: "plain", UserID: "b"},
	}
	out, err := notesfilter.ApplyMuteBlockChannel(notes, sets, renotes)
	require.NoError(t, err)
	ids := make([]string, 0, len(out))
	for _, n := range out {
		ids = append(ids, n.ID)
	}
	assert.Equal(t, []string{"renotes-clean", "renote-ref-missing", "plain"}, ids)
	// renote ID は重複なしで 1 回の batch fetch にまとまる
	assert.Len(t, renotes.got, 5)
}

func TestApplyMuteBlockChannel_RenoteFetchErrorFailsClosed(t *testing.T) {
	sets := notesfilter.MuteBlockSets{MutedUserIDs: map[string]struct{}{"muted": {}}}
	renotes := &stubRenoteLookup{err: errors.New("renote boom")}
	notes := []*model.Note{{ID: "n1", UserID: "a", RenoteID: strp("r1")}}
	_, err := notesfilter.ApplyMuteBlockChannel(notes, sets, renotes)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "load renote refs")
}

func TestApplyMuteBlockChannel_NoRenoteFetchWhenOnlyChannelMute(t *testing.T) {
	// channel mute 単独では renote 入れ子検査対象が無いので fetch しない
	sets := notesfilter.MuteBlockSets{MutedChannelIDs: map[string]struct{}{"ch": {}}}
	renotes := &stubRenoteLookup{rows: map[string]*model.Note{}}
	notes := []*model.Note{{ID: "n1", UserID: "a", RenoteID: strp("r1")}}
	out, err := notesfilter.ApplyMuteBlockChannel(notes, sets, renotes)
	require.NoError(t, err)
	assert.Len(t, out, 1)
	assert.Empty(t, renotes.got, "renote fetch must be skipped when only channel mutes are set")
}

func TestApplyMuteBlockChannel_NilLookupSkipsNestedCheck(t *testing.T) {
	// lookup 未配線 (nil) は入れ子検査を degrade skip する
	sets := notesfilter.MuteBlockSets{MutedUserIDs: map[string]struct{}{"muted": {}}}
	notes := []*model.Note{{ID: "n1", UserID: "a", RenoteID: strp("r1")}}
	out, err := notesfilter.ApplyMuteBlockChannel(notes, sets, nil)
	require.NoError(t, err)
	assert.Len(t, out, 1)
}
