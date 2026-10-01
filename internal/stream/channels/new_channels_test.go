package channels

import (
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/stream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var (
	errAntennaLookup  = errors.New("antenna lookup failed")
	errUserListLookup = errors.New("user list lookup failed")
)

// mockRoleChecker is a test double for AdminRoleChecker.
type mockRoleChecker struct {
	mods map[string]bool
}

func (m *mockRoleChecker) IsModerator(userID string) bool {
	return m.mods[userID]
}

// newAdminCh builds an admin channel where the connecting user is a moderator
// (= admin stream は moderator scope で gate される、#1948-20) per the flag.
func newAdminCh(ctx stream.ChannelContext, isMod bool) stream.Channel {
	userID := ""
	if u, ok := ctx.User().(*model.User); ok && u != nil {
		userID = u.ID
	}
	checker := &mockRoleChecker{mods: map[string]bool{userID: isMod}}
	return NewAdminFactory(checker).New(ctx)
}

// mockExplorable is a test double for RoleExplorableChecker. A role streams
// only when it is in both public and explorable, mirroring role.Service.
type mockExplorable struct{ public, explorable map[string]bool }

func (m mockExplorable) IsPublicExplorable(roleID string) bool {
	return m.public[roleID] && m.explorable[roleID]
}

// newRoleCh builds a roleTimeline channel where role "r1" is public and
// explorable per the flag, via the gated factory (#1549).
func newRoleCh(ctx stream.ChannelContext, explorable bool) stream.Channel {
	return NewRoleTimelineFactory(mockExplorable{
		public:     map[string]bool{"r1": true},
		explorable: map[string]bool{"r1": explorable},
	}).New(ctx)
}

// stubAntennaOwners is a test double for AntennaOwnerLookup. A miss returns
// (nil, nil) (Init rejects via a == nil); set err to exercise the lookup-error path.
type stubAntennaOwners struct {
	byID map[string]*model.Antenna
	err  error
}

func (s *stubAntennaOwners) FindByID(id string) (*model.Antenna, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.byID[id], nil
}

// stubUserListOwners is a test double for UserListOwnerLookup. A miss returns
// (nil, nil) (Init rejects via list == nil); set err to exercise the lookup-error path.
type stubUserListOwners struct {
	byID map[string]*model.UserList
	err  error
}

func (s *stubUserListOwners) FindByID(id string) (*model.UserList, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.byID[id], nil
}

// newUserListCh builds a userList channel whose lookup knows list "l1" owned by
// ownerID. membership lookup は配線しないので per-member reply gate は skip される。
func newUserListCh(ctx stream.ChannelContext, ownerID string) stream.Channel {
	owners := &stubUserListOwners{byID: map[string]*model.UserList{
		"l1": {ID: "l1", UserID: ownerID},
	}}
	return (&UserListFactory{owners: owners}).New(ctx)
}

// newAntennaCh builds an antenna channel whose lookup knows antenna "a1" owned
// by ownerID, via the gated factory (#1569).
func newAntennaCh(ctx stream.ChannelContext, ownerID string) stream.Channel {
	owners := &stubAntennaOwners{byID: map[string]*model.Antenna{
		"a1": {ID: "a1", UserID: ownerID},
	}}
	return NewAntennaFactory(owners).New(ctx)
}

// interface conformance checks
var (
	_ stream.Channel = (*HashtagChannel)(nil)
	_ stream.Channel = (*AntennaChannel)(nil)
	_ stream.Channel = (*ChannelTimelineChannel)(nil)
	_ stream.Channel = (*UserListChannel)(nil)
	_ stream.Channel = (*RoleTimelineChannel)(nil)
	_ stream.Channel = (*AdminChannel)(nil)
	_ stream.Channel = (*ServerStatsChannel)(nil)
	_ stream.Channel = (*QueueStatsChannel)(nil)
	_ stream.Channel = (*ReversiChannel)(nil)
)

// --- Hashtag ---

func TestHashtag_Lifecycle(t *testing.T) {
	ctx := newCtx(nil)
	ch := NewHashtag(ctx)
	require.NoError(t, ch.Init(json.RawMessage(`{"q":[["golang"]]}`)))
	assert.Equal(t, []string{"hashtag:golang"}, ctx.subs)

	ch.OnRedisEvent([]byte(`{"id":"n1","tags":["golang"],"visibility":"public"}`))
	require.Len(t, ctx.sentType, 1)
	assert.Equal(t, "note", ctx.sentType[0])

	ch.Dispose()
	assert.Equal(t, []string{"hashtag:golang"}, ctx.unsubs)
}

func TestHashtag_EmptyQ(t *testing.T) {
	ctx := newCtx(nil)
	ch := NewHashtag(ctx)
	err := ch.Init(json.RawMessage(`{}`))
	assert.ErrorIs(t, err, stream.ErrInvalidParams)
	assert.Empty(t, ctx.subs)
	ch.Dispose()
}

// #1948-20: 全グループが非空でなければ接続拒否する (upstream q.every(x.length>=1))。
// 第1グループが非空でも後続グループが空なら reject。
func TestHashtag_EmptyGroupRejected(t *testing.T) {
	ctx := newCtx(nil)
	ch := NewHashtag(ctx)
	err := ch.Init(json.RawMessage(`{"q":[["a"],[]]}`))
	assert.ErrorIs(t, err, stream.ErrInvalidParams, "後続グループが空なら接続拒否 (#1948-20)")
	assert.Empty(t, ctx.subs)
	ch.Dispose()
}

// #1549: hashtag now receives live notes (publisher added) and re-evaluates the
// OR-of-ANDs query + normalizes + dedupes per subscriber.
func TestHashtag_MultiTagSubscribeUnion(t *testing.T) {
	ctx := newCtx(nil)
	ch := NewHashtag(ctx)
	// q = (a AND b) OR (c): subscribe distinct tags a,b,c.
	require.NoError(t, ch.Init(json.RawMessage(`{"q":[["a","b"],["c"]]}`)))
	assert.ElementsMatch(t, []string{"hashtag:a", "hashtag:b", "hashtag:c"}, ctx.subs)
}

func TestHashtag_OrOfAndsMatch(t *testing.T) {
	t.Run("AND group requires all tags", func(t *testing.T) {
		ctx := newCtx(nil)
		ch := NewHashtag(ctx)
		require.NoError(t, ch.Init(json.RawMessage(`{"q":[["a","b"]]}`)))
		ch.OnRedisEvent([]byte(`{"id":"n1","tags":["a"],"visibility":"public"}`)) // only a
		assert.Empty(t, ctx.sentType, "AND group needs both a and b")
		ch.OnRedisEvent([]byte(`{"id":"n2","tags":["a","b","x"],"visibility":"public"}`))
		require.Len(t, ctx.sentType, 1, "superset of {a,b} matches")
	})
	t.Run("OR group matches either", func(t *testing.T) {
		ctx := newCtx(nil)
		ch := NewHashtag(ctx)
		require.NoError(t, ch.Init(json.RawMessage(`{"q":[["a"],["b"]]}`)))
		ch.OnRedisEvent([]byte(`{"id":"n1","tags":["b"],"visibility":"public"}`))
		require.Len(t, ctx.sentType, 1)
	})
}

func TestHashtag_NormalizationParity(t *testing.T) {
	ctx := newCtx(nil)
	ch := NewHashtag(ctx)
	require.NoError(t, ch.Init(json.RawMessage(`{"q":[["GoLang"]]}`))) // subscribes hashtag:golang
	assert.Equal(t, []string{"hashtag:golang"}, ctx.subs)
	// payload tag in different case/width still matches after NFKC+lower.
	ch.OnRedisEvent([]byte(`{"id":"n1","tags":["ＧＯＬＡＮＧ"],"visibility":"public"}`))
	require.Len(t, ctx.sentType, 1, "full-width upper tag must match normalized subscription")
}

func TestHashtag_DedupeAcrossTopics(t *testing.T) {
	ctx := newCtx(nil)
	ch := NewHashtag(ctx)
	require.NoError(t, ch.Init(json.RawMessage(`{"q":[["a"],["b"]]}`)))
	// same note arrives via both hashtag:a and hashtag:b (fanout fires per topic).
	payload := []byte(`{"id":"dup1","tags":["a","b"],"visibility":"public"}`)
	ch.OnRedisEvent(payload)
	ch.OnRedisEvent(payload)
	require.Len(t, ctx.sentType, 1, "a note matching two subscribed tags must deliver once")
}

func TestHashtag_FollowersVisibilityGate(t *testing.T) {
	ctx := newCtx(&model.User{ID: "alice"})
	ctx.followingSnap = map[string]bool{} // not a follower
	ch := NewHashtag(ctx)
	require.NoError(t, ch.Init(json.RawMessage(`{"q":[["a"]]}`)))
	ch.OnRedisEvent([]byte(`{"id":"n1","tags":["a"],"userId":"author","visibility":"followers"}`))
	assert.Empty(t, ctx.sentType, "followers note from non-followed author must be dropped")
}

// #1549: anon viewer + 著者 requireSigninToViewContents は note を丸ごと drop。
// note/renote/reply のいずれかの author が requireSignin なら drop する。
func TestHashtag_AnonRequireSigninDropped(t *testing.T) {
	t.Run("top-level author", func(t *testing.T) {
		ctx := newCtx(nil)
		ch := NewHashtag(ctx)
		require.NoError(t, ch.Init(json.RawMessage(`{"q":[["a"]]}`)))
		ch.OnRedisEvent([]byte(`{"id":"n1","tags":["a"],"visibility":"public","user":{"id":"author","requireSigninToViewContents":true}}`))
		assert.Empty(t, ctx.sentType)
	})
	t.Run("renote author", func(t *testing.T) {
		ctx := newCtx(nil)
		ch := NewHashtag(ctx)
		require.NoError(t, ch.Init(json.RawMessage(`{"q":[["a"]]}`)))
		ch.OnRedisEvent([]byte(`{"id":"n1","tags":["a"],"visibility":"public","user":{"id":"u"},"renote":{"id":"r1","user":{"id":"ra","requireSigninToViewContents":true}}}`))
		assert.Empty(t, ctx.sentType)
	})
	t.Run("reply author", func(t *testing.T) {
		ctx := newCtx(nil)
		ch := NewHashtag(ctx)
		require.NoError(t, ch.Init(json.RawMessage(`{"q":[["a"]]}`)))
		ch.OnRedisEvent([]byte(`{"id":"n1","tags":["a"],"visibility":"public","user":{"id":"u"},"reply":{"id":"p1","user":{"id":"pa","requireSigninToViewContents":true}}}`))
		assert.Empty(t, ctx.sentType)
	})
	t.Run("authed viewer exempt", func(t *testing.T) {
		ctx := newCtx(&model.User{ID: "viewer"})
		ch := NewHashtag(ctx)
		require.NoError(t, ch.Init(json.RawMessage(`{"q":[["a"]]}`)))
		ch.OnRedisEvent([]byte(`{"id":"n1","tags":["a"],"visibility":"public","user":{"id":"author","requireSigninToViewContents":true}}`))
		require.Len(t, ctx.sentType, 1)
	})
}

// --- Antenna ---

func TestAntenna_Lifecycle(t *testing.T) {
	ctx := newCtx(&model.User{ID: "alice"})
	ch := newAntennaCh(ctx, "alice") // a1 owned by alice
	require.NoError(t, ch.Init(json.RawMessage(`{"antennaId":"a1"}`)))
	assert.Equal(t, []string{"antennaTimeline:a1"}, ctx.subs)

	// #1573: OnRedisEvent now runs the canonical per-subscriber pipeline, so a
	// public note passes the visibility + filter gates and is sent.
	ch.OnRedisEvent([]byte(`{"id":"n1","userId":"author","visibility":"public"}`))
	require.Len(t, ctx.sentType, 1)
	assert.Equal(t, "note", ctx.sentType[0])

	ch.Dispose()
	assert.Equal(t, []string{"antennaTimeline:a1"}, ctx.unsubs)
}

// --- Antenna per-subscriber re-filter (#1573 課題2) ---

// TestAntenna_FollowersVisibilityGate is the #1573 stale-narrowing regression
// guard: a note that was visible to the owner at push time but is followers-only
// must be dropped per-subscriber when the viewer no longer follows the author.
func TestAntenna_FollowersVisibilityGate(t *testing.T) {
	t.Run("non-follower dropped", func(t *testing.T) {
		ctx := newCtx(&model.User{ID: "alice"})
		ctx.followingSnap = map[string]bool{} // alice does not follow author
		ch := newAntennaCh(ctx, "alice")
		require.NoError(t, ch.Init(json.RawMessage(`{"antennaId":"a1"}`)))
		ch.OnRedisEvent([]byte(`{"id":"n1","userId":"author","visibility":"followers"}`))
		assert.Empty(t, ctx.sentType, "followers note from a no-longer-followed author must be dropped")
	})
	t.Run("follower emitted", func(t *testing.T) {
		ctx := newCtx(&model.User{ID: "alice"})
		ctx.followingSnap = map[string]bool{"author": false}
		ch := newAntennaCh(ctx, "alice")
		require.NoError(t, ch.Init(json.RawMessage(`{"antennaId":"a1"}`)))
		ch.OnRedisEvent([]byte(`{"id":"n1","userId":"author","visibility":"followers"}`))
		require.Len(t, ctx.sentType, 1)
		assert.Equal(t, "note", ctx.sentType[0])
	})
	t.Run("self-authored emitted with nil snapshot", func(t *testing.T) {
		ctx := newCtx(&model.User{ID: "alice"})
		ch := newAntennaCh(ctx, "alice")
		require.NoError(t, ch.Init(json.RawMessage(`{"antennaId":"a1"}`)))
		ch.OnRedisEvent([]byte(`{"id":"n1","userId":"alice","visibility":"followers"}`))
		require.Len(t, ctx.sentType, 1)
	})
}

// TestAntenna_MalformedDropped guards fail-closed on an unparseable payload.
func TestAntenna_MalformedDropped(t *testing.T) {
	ctx := newCtx(&model.User{ID: "alice"})
	ch := newAntennaCh(ctx, "alice")
	require.NoError(t, ch.Init(json.RawMessage(`{"antennaId":"a1"}`)))
	ch.OnRedisEvent([]byte(`{not json`))
	assert.Empty(t, ctx.sentType, "malformed payload must be dropped (fail-closed)")
}

// TestAntenna_HardMuteDropped verifies the shouldEmit hardmute gate fires for
// an antenna subscriber whose hardMutedWords match the note (#1573 課題2).
func TestAntenna_HardMuteDropped(t *testing.T) {
	ctx := newCtx(&model.User{ID: "alice"})
	ctx.hardMuteRules = []byte(`[["spam"]]`)
	ch := newAntennaCh(ctx, "alice")
	require.NoError(t, ch.Init(json.RawMessage(`{"antennaId":"a1"}`)))
	ch.OnRedisEvent([]byte(`{"id":"n1","userId":"author","visibility":"public","text":"this is spam"}`))
	assert.Empty(t, ctx.sentType, "note matching a hardMutedWord must be dropped")
}

// TestAntenna_FilteredPureRenote verifies the shouldEmit renote filter applies
// to the antenna channel via the connect param withRenotes=false.
func TestAntenna_FilteredPureRenote(t *testing.T) {
	ctx := newCtx(&model.User{ID: "alice"})
	ch := newAntennaCh(ctx, "alice")
	require.NoError(t, ch.Init(json.RawMessage(`{"antennaId":"a1","withRenotes":false}`)))
	ch.OnRedisEvent([]byte(`{"id":"n1","userId":"author","visibility":"public","renoteId":"r1","fileIds":[]}`))
	assert.Empty(t, ctx.sentType, "pure renote must be dropped when withRenotes=false")
}

// TestAntenna_HideEmbedsPreserved guards that the #1536 embedded-note IDOR gate
// stays applied in the antenna pipeline: a non-visible embedded renote must be
// blanked before Send rather than leaked through the antenna feed.
func TestAntenna_HideEmbedsPreserved(t *testing.T) {
	ctx := newCtx(&model.User{ID: "alice"})
	ctx.followingSnap = map[string]bool{} // alice follows nobody
	ch := newAntennaCh(ctx, "alice")
	require.NoError(t, ch.Init(json.RawMessage(`{"antennaId":"a1"}`)))
	// Top-level public note carrying a followers-only renote from an author
	// alice does not follow. The top-level passes the visibility gate, but the
	// embedded renote must be blanked by hideEmbeds.
	ch.OnRedisEvent([]byte(`{"id":"n1","userId":"author","visibility":"public","text":"top",` +
		`"renote":{"id":"r1","userId":"other","visibility":"followers","text":"secret"}}`))
	require.Len(t, ctx.sentType, 1)
	body, ok := ctx.sentBody[0].(json.RawMessage)
	require.True(t, ok)
	assert.NotContains(t, string(body), "secret",
		"non-visible embedded renote text must be blanked (hideEmbeds #1536 must stay applied)")
}

func TestAntenna_NoAuth(t *testing.T) {
	ctx := newCtx(nil)
	ch := newAntennaCh(ctx, "alice")
	err := ch.Init(json.RawMessage(`{"antennaId":"a1"}`))
	assert.ErrorIs(t, err, stream.ErrInvalidParams)
	assert.Empty(t, ctx.subs)
}

func TestAntenna_MissingID(t *testing.T) {
	ctx := newCtx(&model.User{ID: "alice"})
	ch := newAntennaCh(ctx, "alice")
	err := ch.Init(json.RawMessage(`{}`))
	assert.ErrorIs(t, err, stream.ErrInvalidParams)
	assert.Empty(t, ctx.subs)
}

// TestAntenna_NonOwnerRejected is the #1569 cross-user IDOR regression guard: a
// user must not be able to subscribe to another user's antenna feed.
func TestAntenna_NonOwnerRejected(t *testing.T) {
	ctx := newCtx(&model.User{ID: "bob"})
	ch := newAntennaCh(ctx, "alice") // a1 owned by alice, bob connects
	err := ch.Init(json.RawMessage(`{"antennaId":"a1"}`))
	assert.ErrorIs(t, err, stream.ErrInvalidParams)
	assert.Empty(t, ctx.subs, "non-owner must not subscribe to another user's antenna")
}

func TestAntenna_UnknownAntennaRejected(t *testing.T) {
	ctx := newCtx(&model.User{ID: "alice"})
	ch := newAntennaCh(ctx, "alice")
	err := ch.Init(json.RawMessage(`{"antennaId":"does-not-exist"}`))
	assert.ErrorIs(t, err, stream.ErrInvalidParams)
	assert.Empty(t, ctx.subs)
}

// TestAntenna_NilOwnersFailClosed guarantees an unwired factory rejects every
// subscription rather than silently opening the gate.
func TestAntenna_NilOwnersFailClosed(t *testing.T) {
	ctx := newCtx(&model.User{ID: "alice"})
	ch := NewAntennaFactory(nil).New(ctx)
	err := ch.Init(json.RawMessage(`{"antennaId":"a1"}`))
	assert.ErrorIs(t, err, stream.ErrInvalidParams)
	assert.Empty(t, ctx.subs)
}

// TestAntenna_LookupErrorRejected covers the FindByID error path (fail-closed).
func TestAntenna_LookupErrorRejected(t *testing.T) {
	ctx := newCtx(&model.User{ID: "alice"})
	owners := &stubAntennaOwners{err: errAntennaLookup}
	ch := NewAntennaFactory(owners).New(ctx)
	err := ch.Init(json.RawMessage(`{"antennaId":"a1"}`))
	assert.ErrorIs(t, err, stream.ErrInvalidParams)
	assert.Empty(t, ctx.subs)
}

// --- ChannelTimeline ---

func TestChannelTimeline_Lifecycle(t *testing.T) {
	ctx := newCtx(nil)
	ch := NewChannelTimeline(ctx)
	require.NoError(t, ch.Init(json.RawMessage(`{"channelId":"ch1"}`)))
	assert.Equal(t, []string{"channel:ch1"}, ctx.subs)

	ch.OnRedisEvent([]byte(`{"id":"n1","channelId":"ch1","visibility":"public"}`))
	require.Len(t, ctx.sentType, 1)

	ch.Dispose()
	assert.Equal(t, []string{"channel:ch1"}, ctx.unsubs)
}

func TestChannelTimeline_MissingID(t *testing.T) {
	ctx := newCtx(nil)
	ch := NewChannelTimeline(ctx)
	// TS本家はchannelId欠如時もinit成功とするため、errorは返さずno-op
	err := ch.Init(json.RawMessage(`{}`))
	assert.NoError(t, err)
	assert.Empty(t, ctx.subs)
}

// #1549: channel timeline now receives live notes (publisher added) and gates
// per-subscriber. These cover the new OnRedisEvent gates.
func TestChannelTimeline_WrongChannelDropped(t *testing.T) {
	ctx := newCtx(&model.User{ID: "alice"})
	ch := NewChannelTimeline(ctx)
	require.NoError(t, ch.Init(json.RawMessage(`{"channelId":"ch1"}`)))
	ch.OnRedisEvent([]byte(`{"id":"n1","channelId":"ch2","visibility":"public"}`))
	assert.Empty(t, ctx.sentType, "note for a different channel must be dropped")
}

func TestChannelTimeline_MalformedDropped(t *testing.T) {
	ctx := newCtx(&model.User{ID: "alice"})
	ch := NewChannelTimeline(ctx)
	require.NoError(t, ch.Init(json.RawMessage(`{"channelId":"ch1"}`)))
	ch.OnRedisEvent([]byte(`{not json`))
	assert.Empty(t, ctx.sentType, "malformed payload must be dropped (fail-closed)")
}

func TestChannelTimeline_FollowersVisibilityGate(t *testing.T) {
	t.Run("non-follower dropped", func(t *testing.T) {
		ctx := newCtx(&model.User{ID: "alice"})
		ctx.followingSnap = map[string]bool{} // alice does not follow author
		ch := NewChannelTimeline(ctx)
		require.NoError(t, ch.Init(json.RawMessage(`{"channelId":"ch1"}`)))
		ch.OnRedisEvent([]byte(`{"id":"n1","channelId":"ch1","userId":"author","visibility":"followers"}`))
		assert.Empty(t, ctx.sentType, "followers note from non-followed author must be dropped")
	})
	t.Run("follower emitted", func(t *testing.T) {
		ctx := newCtx(&model.User{ID: "alice"})
		ctx.followingSnap = map[string]bool{"author": false}
		ch := NewChannelTimeline(ctx)
		require.NoError(t, ch.Init(json.RawMessage(`{"channelId":"ch1"}`)))
		ch.OnRedisEvent([]byte(`{"id":"n1","channelId":"ch1","userId":"author","visibility":"followers"}`))
		require.Len(t, ctx.sentType, 1)
		assert.Equal(t, "note", ctx.sentType[0])
	})
	t.Run("self-authored emitted with nil snapshot", func(t *testing.T) {
		ctx := newCtx(&model.User{ID: "alice"})
		ch := NewChannelTimeline(ctx)
		require.NoError(t, ch.Init(json.RawMessage(`{"channelId":"ch1"}`)))
		ch.OnRedisEvent([]byte(`{"id":"n1","channelId":"ch1","userId":"alice","visibility":"followers"}`))
		require.Len(t, ctx.sentType, 1)
	})
}

// #1549: anon viewer + 著者 requireSigninToViewContents は note を丸ごと drop。
func TestChannelTimeline_AnonRequireSigninDropped(t *testing.T) {
	ctx := newCtx(nil) // anonymous connection
	ch := NewChannelTimeline(ctx)
	require.NoError(t, ch.Init(json.RawMessage(`{"channelId":"ch1"}`)))
	ch.OnRedisEvent([]byte(`{"id":"n1","channelId":"ch1","visibility":"public","user":{"id":"author","requireSigninToViewContents":true}}`))
	assert.Empty(t, ctx.sentType, "anon viewer must not receive a requireSignin author's note")
}

// 認証済み viewer には requireSignin gate が適用されない。
func TestChannelTimeline_AuthedRequireSigninPassed(t *testing.T) {
	ctx := newCtx(&model.User{ID: "viewer"})
	ch := NewChannelTimeline(ctx)
	require.NoError(t, ch.Init(json.RawMessage(`{"channelId":"ch1"}`)))
	ch.OnRedisEvent([]byte(`{"id":"n1","channelId":"ch1","visibility":"public","user":{"id":"author","requireSigninToViewContents":true}}`))
	require.Len(t, ctx.sentType, 1, "authenticated viewer is exempt from the requireSignin gate")
}

// #1812: channel timeline は upstream channel.ts の override に倣い、視聴中の
// channel 自身が mute されていても note を流す (直接見ている以上は配信する)。
func TestChannelTimeline_MutedOwnChannelStillStreamed(t *testing.T) {
	ctx := newCtx(&model.User{ID: "viewer"})
	// viewer は視聴中の ch1 を mute している。
	ctx.muteBlockSnap = &stream.MuteBlockSnapshot{MutingChannels: map[string]struct{}{"ch1": {}}}
	ch := NewChannelTimeline(ctx)
	require.NoError(t, ch.Init(json.RawMessage(`{"channelId":"ch1"}`)))
	ch.OnRedisEvent([]byte(`{"id":"n1","channelId":"ch1","userId":"author","visibility":"public"}`))
	require.Len(t, ctx.sentType, 1, "視聴中 channel の mute では own-channel note を落とさない")
}

// #1812: ただし他の mute された channel への renote は channel timeline でも落とす。
func TestChannelTimeline_MutedRenoteChannelDropped(t *testing.T) {
	ctx := newCtx(&model.User{ID: "viewer"})
	// viewer は ch1 を見ているが、別 channel ch2 を mute している。
	ctx.muteBlockSnap = &stream.MuteBlockSnapshot{MutingChannels: map[string]struct{}{"ch2": {}}}
	ch := NewChannelTimeline(ctx)
	require.NoError(t, ch.Init(json.RawMessage(`{"channelId":"ch1"}`)))
	// ch1 に流れた、ch2 の note への renote。
	ch.OnRedisEvent([]byte(`{"id":"n2","channelId":"ch1","userId":"author","visibility":"public","renoteId":"r1","renote":{"channelId":"ch2","userId":"x","user":{"id":"x"}}}`))
	assert.Empty(t, ctx.sentType, "他の mute された channel への renote は drop する")
}

// --- UserList ---

func TestUserList_Lifecycle(t *testing.T) {
	ctx := newCtx(&model.User{ID: "alice"})
	ch := newUserListCh(ctx, "alice")
	require.NoError(t, ch.Init(json.RawMessage(`{"listId":"l1"}`)), "所有 list の Init は成功する")
	// note 配信 topic と membership event topic の両方を subscribe する (#1549)。
	assert.Equal(t, []string{"userListTimeline:l1", "userListStream:l1"}, ctx.subs)

	ch.OnRedisEvent([]byte(`{"id":"n1"}`))
	require.Len(t, ctx.sentType, 1)

	ch.Dispose()
	assert.Equal(t, []string{"userListTimeline:l1", "userListStream:l1"}, ctx.unsubs)
}

// #1549: userListStream: topic に流れる userAdded/userRemoved envelope は
// note filter を経由せず、そのまま client に forward される。
func TestUserList_MembershipEventForwarded(t *testing.T) {
	ctx := newCtx(&model.User{ID: "alice"})
	ch := newUserListCh(ctx, "alice")
	require.NoError(t, ch.Init(json.RawMessage(`{"listId":"l1"}`)), "所有 list の Init は成功する")

	ch.OnRedisEvent([]byte(`{"type":"userAdded","body":{"id":"u2","username":"bob"}}`))
	require.Len(t, ctx.sentType, 1)
	assert.Equal(t, "userAdded", ctx.sentType[0])
	body, ok := ctx.sentBody[0].(json.RawMessage)
	require.True(t, ok)
	assert.Contains(t, string(body), "bob")

	ch.OnRedisEvent([]byte(`{"type":"userRemoved","body":{"id":"u2","username":"bob"}}`))
	require.Len(t, ctx.sentType, 2)
	assert.Equal(t, "userRemoved", ctx.sentType[1])
}

func TestUserList_NoAuth(t *testing.T) {
	ctx := newCtx(nil)
	ch := newUserListCh(ctx, "alice")
	err := ch.Init(json.RawMessage(`{"listId":"l1"}`))
	assert.ErrorIs(t, err, stream.ErrInvalidParams)
	assert.Empty(t, ctx.subs)
}

func TestUserList_MissingID(t *testing.T) {
	ctx := newCtx(&model.User{ID: "alice"})
	ch := newUserListCh(ctx, "alice")
	err := ch.Init(json.RawMessage(`{}`))
	assert.ErrorIs(t, err, stream.ErrInvalidParams)
	assert.Empty(t, ctx.subs)
}

func TestUserList_WithReplies(t *testing.T) {
	ctx := newCtx(&model.User{ID: "alice"})
	ch := newUserListCh(ctx, "alice")
	require.NoError(t, ch.Init(json.RawMessage(`{"listId":"l1","withReplies":true}`)), "所有 list の Init は成功する")
	assert.Equal(t, []string{"userListTimeline:l1", "userListStream:l1"}, ctx.subs)

	// リプライが通過する
	ch.OnRedisEvent([]byte(`{"text":"reply","replyId":"p1"}`))
	require.Len(t, ctx.sentType, 1)
}

// #1063: UserList channel は per-membership の withReplies (= upstream
// `MiUserListMembership.withReplies`) を持つべきだが mk-go 側に model が
// 無いため未実装。本 PR で noteFilter.shouldEmit から reply blanket-drop
// を撤廃した副作用で、connect param `withReplies=false` でも reply が
// pass-through する暫定挙動になっている。完全 upstream 互換 (= per-member
// gate) は別 issue で対応する想定。本テストは暫定挙動の regression guard
// で、将来 per-member gate を入れたら drop 側に書き換える。
func TestUserList_WithRepliesFalse_ReplyPassthrough(t *testing.T) {
	ctx := newCtx(&model.User{ID: "alice"})
	ch := newUserListCh(ctx, "alice")
	require.NoError(t, ch.Init(json.RawMessage(`{"listId":"l1","withReplies":false}`)), "所有 list の Init は成功する")
	ch.OnRedisEvent([]byte(`{"text":"reply","replyId":"p1"}`))
	require.Len(t, ctx.sentType, 1)
	assert.Equal(t, "note", ctx.sentType[0])
}

// #1465: followers visibility note は viewer が author を follow している
// 場合のみ emit される (defense-in-depth)。fanout 段で list owner の follow
// を check してから push する設計だが、stream 残留 entry に対するフォール
// バックとして channel 側でも 1 段 gate する。
func TestUserList_FollowersVisibility_NonFollowerDropped(t *testing.T) {
	ctx := newCtx(&model.User{ID: "alice"})
	ctx.followingSnap = map[string]bool{} // alice は author を follow していない
	ch := newUserListCh(ctx, "alice")
	require.NoError(t, ch.Init(json.RawMessage(`{"listId":"l1"}`)), "所有 list の Init は成功する")

	ch.OnRedisEvent([]byte(`{"id":"n1","userId":"author","visibility":"followers"}`))
	assert.Empty(t, ctx.sentType, "non-follower viewer must not receive followers note")
}

func TestUserList_FollowersVisibility_FollowerAccepted(t *testing.T) {
	ctx := newCtx(&model.User{ID: "alice"})
	ctx.followingSnap = map[string]bool{"author": false} // value (withReplies) は関係ない
	ch := newUserListCh(ctx, "alice")
	require.NoError(t, ch.Init(json.RawMessage(`{"listId":"l1"}`)), "所有 list の Init は成功する")

	ch.OnRedisEvent([]byte(`{"id":"n1","userId":"author","visibility":"followers"}`))
	require.Len(t, ctx.sentType, 1)
	assert.Equal(t, "note", ctx.sentType[0])
}

func TestUserList_FollowersVisibility_SelfAuthored(t *testing.T) {
	ctx := newCtx(&model.User{ID: "alice"})
	// snap は nil でも本人 short-circuit で通る
	ch := newUserListCh(ctx, "alice")
	require.NoError(t, ch.Init(json.RawMessage(`{"listId":"l1"}`)), "所有 list の Init は成功する")

	ch.OnRedisEvent([]byte(`{"id":"n1","userId":"alice","visibility":"followers"}`))
	require.Len(t, ctx.sentType, 1)
}

func TestUserList_FollowersVisibility_NilSnapshotFailClosed(t *testing.T) {
	ctx := newCtx(&model.User{ID: "alice"})
	// followingSnap=nil (= snapshot lookup 未配線 / 取得失敗) で他人の followers
	// note は drop される。本人は別 case (上) で通すことを確認済。
	ch := newUserListCh(ctx, "alice")
	require.NoError(t, ch.Init(json.RawMessage(`{"listId":"l1"}`)), "所有 list の Init は成功する")

	ch.OnRedisEvent([]byte(`{"id":"n1","userId":"author","visibility":"followers"}`))
	assert.Empty(t, ctx.sentType, "nil snapshot must fail-closed for followers note from non-self author")
}

func TestUserList_PublicVisibility_NoFollowCheck(t *testing.T) {
	ctx := newCtx(&model.User{ID: "alice"})
	// public は snap 無くても、follow 関係に関係なく通る
	ch := newUserListCh(ctx, "alice")
	require.NoError(t, ch.Init(json.RawMessage(`{"listId":"l1"}`)), "所有 list の Init は成功する")

	ch.OnRedisEvent([]byte(`{"id":"n1","userId":"author","visibility":"public"}`))
	require.Len(t, ctx.sentType, 1)
}

func TestUserList_HomeVisibility_NoFollowCheck(t *testing.T) {
	ctx := newCtx(&model.User{ID: "alice"})
	ch := newUserListCh(ctx, "alice")
	require.NoError(t, ch.Init(json.RawMessage(`{"listId":"l1"}`)), "所有 list の Init は成功する")

	ch.OnRedisEvent([]byte(`{"id":"n1","userId":"author","visibility":"home"}`))
	require.Len(t, ctx.sentType, 1)
}

// visibility 欠如 payload は parse 自体は通るが visibility != "followers" の
// branch で素通りする (regression guard / 後方互換)。
func TestUserList_NoVisibilityField_Passthrough(t *testing.T) {
	ctx := newCtx(&model.User{ID: "alice"})
	ch := newUserListCh(ctx, "alice")
	require.NoError(t, ch.Init(json.RawMessage(`{"listId":"l1"}`)), "所有 list の Init は成功する")

	ch.OnRedisEvent([]byte(`{"id":"n1"}`))
	require.Len(t, ctx.sentType, 1)
}

// 不正 payload はパース失敗で conservative drop される (IDOR fail-closed)。
func TestUserList_BrokenPayload_Dropped(t *testing.T) {
	ctx := newCtx(&model.User{ID: "alice"})
	ch := newUserListCh(ctx, "alice")
	require.NoError(t, ch.Init(json.RawMessage(`{"listId":"l1"}`)), "所有 list の Init は成功する")

	ch.OnRedisEvent([]byte(`{not json`))
	assert.Empty(t, ctx.sentType)
}

// #1491 audit 指摘 5: userListVisibilityShouldEmit は visibility == "followers"
// 以外の payload を素通りする (user_list.go:112)。コメントには「specified は
// fanout (shouldFanoutToFollowers) で除外されるからここに来ない」と書いてある
// が、その assumption は untested。fanout 側の refactor がうっかり specified
// payload を user_list stream へ送り始めた瞬間に WS gate も passthrough して
// しまうので、その日の defense-in-depth がどう動くかを test で確定させておく。
//
// 現実装は specified payload を非宛先 viewer にも emit する (passthrough)。
// この test が落ちる条件 = ゲートが visibility 別 path を追加して specified を
// 個別に判定するようになったとき、そのときは併せて fanout 側のテスト
// (TestFanoutHook_FanoutToUserLists_SpecifiedVisibilitySkipped) を見直すべき。
func TestUserList_SpecifiedVisibility_AssumesFanoutSkips(t *testing.T) {
	ctx := newCtx(&model.User{ID: "alice"})
	ch := newUserListCh(ctx, "alice")
	require.NoError(t, ch.Init(json.RawMessage(`{"listId":"l1"}`)), "所有 list の Init は成功する")

	// alice 宛ではない specified payload。fanout 側で normally この shape は
	// user_list stream に流れない (= shouldFanoutToFollowers で specified が
	// 除外される) が、もし流れた場合 WS gate は passthrough する現状動作。
	ch.OnRedisEvent([]byte(`{"id":"n1","userId":"author","visibility":"specified","visibleUserIds":["other"]}`))
	require.Len(t, ctx.sentType, 1,
		"現状の userListVisibilityShouldEmit は specified payload を passthrough する "+
			"(fanout 側 shouldFanoutToFollowers が specified を弾く前提)。fanout の "+
			"assumption が崩れた瞬間に WS leak になるので、本 test を grep して "+
			"両層をセットで見直すこと。")
}

// --- RoleTimeline ---

func TestRoleTimeline_Lifecycle(t *testing.T) {
	ctx := newCtx(nil)
	ch := newRoleCh(ctx, true) // r1 explorable
	require.NoError(t, ch.Init(json.RawMessage(`{"roleId":"r1"}`)))
	assert.Equal(t, []string{"roleTimeline:r1"}, ctx.subs)

	ch.OnRedisEvent([]byte(`{"id":"n1","visibility":"public"}`))
	require.Len(t, ctx.sentType, 1)

	ch.Dispose()
	assert.Equal(t, []string{"roleTimeline:r1"}, ctx.unsubs)
}

func TestRoleTimeline_MissingID(t *testing.T) {
	ctx := newCtx(nil)
	ch := newRoleCh(ctx, true)
	// 本家 2026.10.0 は roleId が無ければ init で false を返し、接続を受け付けない。
	for _, params := range []string{`{}`, `{"roleId":""}`} {
		require.Error(t, ch.Init(json.RawMessage(params)), params)
	}
	assert.Empty(t, ctx.subs)
}

// #1549: roleTimeline now receives live notes; only isPublic && isExplorable
// roles + public-visibility notes are emitted (本家 role-timeline.ts parity)。
// 非 explorable なロールは Init の時点で拒否する (#17987)。
func TestRoleTimeline_NonExplorableRefused(t *testing.T) {
	ctx := newCtx(nil)
	ch := newRoleCh(ctx, false) // r1 NOT explorable
	require.Error(t, ch.Init(json.RawMessage(`{"roleId":"r1"}`)))
	assert.Empty(t, ctx.subs, "refused channel must not subscribe")
}

// explorable でも非公開のロールは、Init で拒否し、event も流さない (#17987)。
func TestRoleTimeline_ExplorableButNotPublic(t *testing.T) {
	t.Run("init refused", func(t *testing.T) {
		ctx := newCtx(nil)
		ch := NewRoleTimelineFactory(mockExplorable{
			public:     map[string]bool{"r1": false},
			explorable: map[string]bool{"r1": true},
		}).New(ctx)
		require.Error(t, ch.Init(json.RawMessage(`{"roleId":"r1"}`)))
		assert.Empty(t, ctx.subs, "refused channel must not subscribe")
	})

	t.Run("event dropped after the role turns private", func(t *testing.T) {
		// 接続後にロールが非公開へ変わった場合も、per-event の gate で止める。
		ctx := newCtx(nil)
		m := mockExplorable{
			public:     map[string]bool{"r1": true},
			explorable: map[string]bool{"r1": true},
		}
		ch := NewRoleTimelineFactory(m).New(ctx)
		require.NoError(t, ch.Init(json.RawMessage(`{"roleId":"r1"}`)))
		m.public["r1"] = false
		ch.OnRedisEvent([]byte(`{"id":"n1","visibility":"public"}`))
		assert.Empty(t, ctx.sentType, "non-public role must not stream notes")
	})

	t.Run("event dropped after the role turns non-explorable", func(t *testing.T) {
		ctx := newCtx(nil)
		m := mockExplorable{
			public:     map[string]bool{"r1": true},
			explorable: map[string]bool{"r1": true},
		}
		ch := NewRoleTimelineFactory(m).New(ctx)
		require.NoError(t, ch.Init(json.RawMessage(`{"roleId":"r1"}`)))
		m.explorable["r1"] = false
		ch.OnRedisEvent([]byte(`{"id":"n1","visibility":"public"}`))
		assert.Empty(t, ctx.sentType, "non-explorable role must not stream notes")
	})
}

// checker が未配線なら Init を拒否する (fail-closed)。
func TestRoleTimeline_UnwiredCheckerRefused(t *testing.T) {
	ctx := newCtx(nil)
	ch := NewRoleTimelineFactory(nil).New(ctx)
	require.Error(t, ch.Init(json.RawMessage(`{"roleId":"r1"}`)))
	assert.Empty(t, ctx.subs)
}

func TestRoleTimeline_NonPublicDropped(t *testing.T) {
	ch := newRoleCh(newCtx(nil), true)
	require.NoError(t, ch.Init(json.RawMessage(`{"roleId":"r1"}`)))
	for _, vis := range []string{"home", "followers", "specified"} {
		ctx := newCtx(nil)
		c := newRoleCh(ctx, true)
		require.NoError(t, c.Init(json.RawMessage(`{"roleId":"r1"}`)))
		c.OnRedisEvent([]byte(`{"id":"n1","visibility":"` + vis + `"}`))
		assert.Empty(t, ctx.sentType, "roleTimeline emits public only, got "+vis)
	}
	_ = ch
}

func TestRoleTimeline_ExplorablePublicEmitted(t *testing.T) {
	ctx := newCtx(nil)
	ch := newRoleCh(ctx, true)
	require.NoError(t, ch.Init(json.RawMessage(`{"roleId":"r1"}`)))
	ch.OnRedisEvent([]byte(`{"id":"n1","visibility":"public"}`))
	require.Len(t, ctx.sentType, 1)
	assert.Equal(t, "note", ctx.sentType[0])
}

// #1780: anon viewer + 著者 requireSigninToViewContents の public note は drop する
// (upstream role-timeline.ts:53-55、channel/hashtag と同じ gate が role-timeline に
// だけ漏れていた)。
func TestRoleTimeline_AnonRequireSigninDropped(t *testing.T) {
	ctx := newCtx(nil) // anonymous
	ch := newRoleCh(ctx, true)
	require.NoError(t, ch.Init(json.RawMessage(`{"roleId":"r1"}`)))
	ch.OnRedisEvent([]byte(`{"id":"n1","visibility":"public","user":{"requireSigninToViewContents":true}}`))
	assert.Empty(t, ctx.sentType, "anon viewer must not receive a requireSignin note")
}

// authed viewer なら requireSigninToViewContents の note でも受信する。
func TestRoleTimeline_AuthedRequireSigninEmitted(t *testing.T) {
	ctx := newCtx(&model.User{ID: "viewer"})
	ch := newRoleCh(ctx, true)
	require.NoError(t, ch.Init(json.RawMessage(`{"roleId":"r1"}`)))
	ch.OnRedisEvent([]byte(`{"id":"n1","visibility":"public","user":{"requireSigninToViewContents":true}}`))
	require.Len(t, ctx.sentType, 1)
}

// --- Admin ---

func TestAdmin_Lifecycle(t *testing.T) {
	ctx := newCtx(&model.User{ID: "admin1"})
	ch := newAdminCh(ctx, true)
	require.NoError(t, ch.Init(nil))
	// #1549: per-user topic adminStream:<userId> を購読する。
	assert.Equal(t, []string{"adminStream:admin1"}, ctx.subs)

	// エンベロープ展開
	ch.OnRedisEvent([]byte(`{"type":"newReport","body":{"id":"r1"}}`))
	require.Len(t, ctx.sentType, 1)
	assert.Equal(t, "newReport", ctx.sentType[0])

	ch.Dispose()
	assert.Equal(t, []string{"adminStream:admin1"}, ctx.unsubs)
}

func TestAdmin_NoAuth(t *testing.T) {
	ctx := newCtx(nil)
	ch := newAdminCh(ctx, false)
	err := ch.Init(nil)
	assert.ErrorIs(t, err, stream.ErrInvalidParams)
	assert.Empty(t, ctx.subs)
}

func TestAdmin_NotAdmin(t *testing.T) {
	ctx := newCtx(&model.User{ID: "normaluser"})
	ch := newAdminCh(ctx, false)
	err := ch.Init(nil)
	assert.ErrorIs(t, err, stream.ErrInvalidParams)
	assert.Empty(t, ctx.subs)
}

func TestAdmin_RawPayload(t *testing.T) {
	ctx := newCtx(&model.User{ID: "admin1"})
	ch := newAdminCh(ctx, true)
	require.NoError(t, ch.Init(nil))
	// エンベロープでないペイロード
	ch.OnRedisEvent([]byte(`{"data":"raw"}`))
	require.Len(t, ctx.sentType, 1)
	assert.Equal(t, "adminEvent", ctx.sentType[0])
}

// --- ServerStats ---

func TestServerStats_Lifecycle(t *testing.T) {
	ctx := newCtx(nil)
	ch := NewServerStats(ctx)
	require.NoError(t, ch.Init(nil))
	assert.Equal(t, []string{"serverStats"}, ctx.subs)

	ch.OnRedisEvent([]byte(`{"cpu":0.5}`))
	require.Len(t, ctx.sentType, 1)
	assert.Equal(t, "stats", ctx.sentType[0])

	ch.Dispose()
	assert.Equal(t, []string{"serverStats"}, ctx.unsubs)
}

func TestServerStats_RequestLog(t *testing.T) {
	ctx := newCtx(nil)
	ch := NewServerStats(ctx)
	require.NoError(t, ch.Init(nil))
	ch.OnClientMessage("requestLog", json.RawMessage(`{"id":"req1","length":50}`))
	require.Len(t, ctx.sentType, 1)
	assert.Equal(t, "statsLog", ctx.sentType[0])
}

func TestServerStats_RequestLog_IgnoresOtherTypes(t *testing.T) {
	ctx := newCtx(nil)
	ch := NewServerStats(ctx)
	require.NoError(t, ch.Init(nil))
	ch.OnClientMessage("other", nil)
	assert.Empty(t, ctx.sentType)
}

// --- QueueStats ---

func TestQueueStats_Lifecycle(t *testing.T) {
	ctx := newCtx(nil)
	ch := NewQueueStats(ctx)
	require.NoError(t, ch.Init(nil))
	assert.Equal(t, []string{"queueStats"}, ctx.subs)

	ch.OnRedisEvent([]byte(`{"deliver":10}`))
	require.Len(t, ctx.sentType, 1)
	assert.Equal(t, "stats", ctx.sentType[0])

	ch.Dispose()
	assert.Equal(t, []string{"queueStats"}, ctx.unsubs)
}

func TestQueueStats_RequestLog(t *testing.T) {
	ctx := newCtx(nil)
	ch := NewQueueStats(ctx)
	require.NoError(t, ch.Init(nil))
	ch.OnClientMessage("requestLog", json.RawMessage(`{"id":"req1","length":50}`))
	require.Len(t, ctx.sentType, 1)
	assert.Equal(t, "statsLog", ctx.sentType[0])
}

func TestQueueStats_RequestLog_IgnoresOtherTypes(t *testing.T) {
	ctx := newCtx(nil)
	ch := NewQueueStats(ctx)
	require.NoError(t, ch.Init(nil))
	ch.OnClientMessage("other", nil)
	assert.Empty(t, ctx.sentType)
}

// --- Reversi ---

func TestReversi_Lifecycle(t *testing.T) {
	ctx := newCtx(&model.User{ID: "alice"})
	ch := NewReversi(ctx)
	require.NoError(t, ch.Init(nil))
	assert.Equal(t, []string{"reversi:alice"}, ctx.subs)

	ch.OnRedisEvent([]byte(`{"type":"invited","body":{"gameId":"g1"}}`))
	require.Len(t, ctx.sentType, 1)
	assert.Equal(t, "invited", ctx.sentType[0])

	ch.Dispose()
	assert.Equal(t, []string{"reversi:alice"}, ctx.unsubs)
}

func TestReversi_NoAuth(t *testing.T) {
	ctx := newCtx(nil)
	ch := NewReversi(ctx)
	err := ch.Init(nil)
	assert.ErrorIs(t, err, stream.ErrInvalidParams)
	assert.Empty(t, ctx.subs)
}

// --- OnClientMessage no-op coverage ---

func TestNoOpClientMessages(t *testing.T) {
	body := json.RawMessage(`{}`)
	cases := []struct {
		name string
		ch   stream.Channel
	}{
		{"hashtag", NewHashtag(newCtx(nil))},
		{"antenna", newAntennaCh(newCtx(&model.User{ID: "u1"}), "u1")},
		{"channelTimeline", NewChannelTimeline(newCtx(nil))},
		{"userList", newUserListCh(newCtx(&model.User{ID: "u1"}), "u1")},
		{"roleTimeline", newRoleCh(newCtx(nil), true)},
		{"admin", newAdminCh(newCtx(&model.User{ID: "u1"}), true)},
		{"serverStats", NewServerStats(newCtx(nil))},
		{"queueStats", NewQueueStats(newCtx(nil))},
		{"reversi", NewReversi(newCtx(&model.User{ID: "u1"}))},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.ch.OnClientMessage("test", body)
		})
	}
}

// --- Filter branch coverage for new channels ---

func TestHashtag_FilteredRenote(t *testing.T) {
	ctx := newCtx(nil)
	ch := NewHashtag(ctx)
	require.NoError(t, ch.Init(json.RawMessage(`{"q":[["go"]],"withRenotes":false}`)))
	ch.OnRedisEvent([]byte(`{"renoteId":"r1","fileIds":[]}`))
	assert.Empty(t, ctx.sentType)
}

// #1063: ChannelTimeline (Misskey の channel 機能 timeline) は upstream
// `channel.ts` に reply gate を持たない。withReplies connect param に関わらず
// reply は pass-through する。旧テストは「withReplies=false で reply を drop」
// する drift 挙動を assertion していたので新 semantics に揃える。
func TestChannelTimeline_ReplyPassthrough(t *testing.T) {
	ctx := newCtx(nil)
	ch := NewChannelTimeline(ctx)
	require.NoError(t, ch.Init(json.RawMessage(`{"channelId":"ch1","withReplies":false}`)))
	ch.OnRedisEvent([]byte(`{"text":"reply","replyId":"p1","channelId":"ch1","visibility":"public"}`))
	require.Len(t, ctx.sentType, 1)
	assert.Equal(t, "note", ctx.sentType[0])
}

func TestRoleTimeline_FilteredRenote(t *testing.T) {
	ctx := newCtx(nil)
	ch := newRoleCh(ctx, true)
	require.NoError(t, ch.Init(json.RawMessage(`{"roleId":"r1","withRenotes":false}`)))
	// public + explorable で gate を通過させ、純リノート filter (shouldEmit) で drop。
	ch.OnRedisEvent([]byte(`{"renoteId":"r1","fileIds":[],"visibility":"public"}`))
	assert.Empty(t, ctx.sentType)
}

func TestUserList_FilteredNoFiles(t *testing.T) {
	ctx := newCtx(&model.User{ID: "alice"})
	ch := newUserListCh(ctx, "alice")
	require.NoError(t, ch.Init(json.RawMessage(`{"listId":"l1","withFiles":true}`)), "所有 list の Init は成功する")
	ch.OnRedisEvent([]byte(`{"text":"hello","fileIds":[]}`))
	assert.Empty(t, ctx.sentType)
}

func TestUserList_MissingListID(t *testing.T) {
	ctx := newCtx(&model.User{ID: "alice"})
	ch := newUserListCh(ctx, "alice")
	// listId が無いので Init は失敗する。購読が張られないことが主題。
	require.Error(t, ch.Init(json.RawMessage(`{}`)))
	assert.Empty(t, ctx.subs)
}

func TestReversi_RawPayload(t *testing.T) {
	ctx := newCtx(&model.User{ID: "alice"})
	ch := NewReversi(ctx)
	require.NoError(t, ch.Init(nil))
	// エンベロープでないペイロード
	ch.OnRedisEvent([]byte(`{"data":"raw"}`))
	require.Len(t, ctx.sentType, 1)
	assert.Equal(t, "reversiEvent", ctx.sentType[0])
}

// --- Timeline filter integration ---

func TestLocalTimeline_FilterPureRenote(t *testing.T) {
	ctx := newCtx(nil)
	ch := NewLocalTimeline(ctx)
	require.NoError(t, ch.Init(json.RawMessage(`{"withRenotes":false}`)))

	// 純リノートはフィルタされる
	ch.OnRedisEvent([]byte(`{"renoteId":"r1","fileIds":[]}`))
	assert.Empty(t, ctx.sentType)

	// 通常ノートは通過
	ch.OnRedisEvent([]byte(`{"text":"hello"}`))
	require.Len(t, ctx.sentType, 1)
}

// #1063: upstream `global-timeline.ts` は reply gate を持たない。reply は
// 常に pass-through する。旧テストは drift 挙動の assertion だったので新
// semantics に揃える。
func TestGlobalTimeline_ReplyPassthrough(t *testing.T) {
	ctx := newCtx(nil)
	ch := NewGlobalTimeline(ctx)
	require.NoError(t, ch.Init(json.RawMessage(`{"withReplies":false}`)))

	ch.OnRedisEvent([]byte(`{"text":"reply","replyId":"p1"}`))
	require.Len(t, ctx.sentType, 1)
	assert.Equal(t, "note", ctx.sentType[0])
}

func TestHomeTimeline_WithFiles(t *testing.T) {
	ctx := newCtx(&model.User{ID: "alice"})
	ch := NewHomeTimeline(ctx)
	require.NoError(t, ch.Init(json.RawMessage(`{"withFiles":true}`)))

	// ファイルなしはフィルタされる
	ch.OnRedisEvent([]byte(`{"text":"hello","fileIds":[]}`))
	assert.Empty(t, ctx.sentType)

	// ファイルありは通過
	ch.OnRedisEvent([]byte(`{"text":"hello","fileIds":["f1"]}`))
	require.Len(t, ctx.sentType, 1)
}

func TestHybridTimeline_FilterDefault(t *testing.T) {
	ctx := newCtx(&model.User{ID: "alice"})
	ch := NewHybridTimeline(ctx)
	require.NoError(t, ch.Init(nil)) // デフォルトフィルタ

	// 通常ノートは通過
	ch.OnRedisEvent([]byte(`{"text":"hello"}`))
	require.Len(t, ctx.sentType, 1)

	// リプライはデフォルトでフィルタ
	ch.OnRedisEvent([]byte(`{"text":"reply","replyId":"p1"}`))
	assert.Len(t, ctx.sentType, 1) // 増えない
}

// stubMembershipLookup is a test double for UserListLookup. FindByID reports
// every list as owned by ownerID so the Init ownership gate passes.
type stubMembershipLookup struct {
	members []*model.UserListMembership
	ownerID string
}

func (s *stubMembershipLookup) ListMembers(string) ([]*model.UserListMembership, error) {
	return s.members, nil
}

func (s *stubMembershipLookup) FindByID(id string) (*model.UserList, error) {
	return &model.UserList{ID: id, UserID: s.ownerID}, nil
}

// #2020: userList channel は Init で per-member withReplies を snapshot し、reply を
// upstream user-list.ts と同じ per-member gate で drop する。
func TestUserList_PerMemberWithRepliesGate(t *testing.T) {
	ctx := newCtx(&model.User{ID: "alice"}) // viewer = alice (list owner)
	lookup := &stubMembershipLookup{ownerID: "alice", members: []*model.UserListMembership{
		{UserID: "bob", WithReplies: false},
		{UserID: "carol", WithReplies: true},
	}}
	ch := NewUserListFactory(lookup).New(ctx)
	require.NoError(t, ch.Init(json.RawMessage(`{"listId":"l1"}`)))

	send := func(payload string) int {
		ctx.sentType = nil
		ch.OnRedisEvent([]byte(payload))
		return len(ctx.sentType)
	}

	// bob (withReplies=false) の第三者 reply → drop。
	assert.Equal(t, 0, send(`{"id":"n1","userId":"bob","visibility":"public","reply":{"userId":"dave","visibility":"public"}}`),
		"withReplies=false member の第三者 reply は drop (#2020)")
	// bob の接続主 (alice) への reply → pass。
	assert.Equal(t, 1, send(`{"id":"n2","userId":"bob","visibility":"public","reply":{"userId":"alice","visibility":"public"}}`),
		"接続主への reply は pass")
	// bob の自己 reply (bob→bob) → pass。
	assert.Equal(t, 1, send(`{"id":"n2b","userId":"bob","visibility":"public","reply":{"userId":"bob","visibility":"public"}}`),
		"自己 reply は pass")
	// carol (withReplies=true) の通常 public reply → pass。
	assert.Equal(t, 1, send(`{"id":"n3","userId":"carol","visibility":"public","reply":{"userId":"dave","visibility":"public"}}`),
		"withReplies=true member の reply は pass")
	// carol の followers reply で viewer が reply 先を follow していない → drop。
	ctx.followingSnap = map[string]bool{}
	assert.Equal(t, 0, send(`{"id":"n4","userId":"carol","visibility":"public","reply":{"userId":"dave","visibility":"followers"}}`),
		"withReplies=true で followers reply 先を非フォローなら drop (#2020)")
	// carol の followers reply で viewer が reply 先を follow → pass。
	ctx.followingSnap = map[string]bool{"dave": true}
	assert.Equal(t, 1, send(`{"id":"n4b","userId":"carol","visibility":"public","reply":{"userId":"dave","visibility":"followers"}}`),
		"reply 先を follow していれば pass")
	// reply でない note は gate 対象外。
	assert.Equal(t, 1, send(`{"id":"n5","userId":"bob","visibility":"public"}`), "reply でない note は pass")
}

// 接続主が所有しない list は購読できない (本家 user-list.ts の owner check 相当)。
func TestUserList_NotOwner_Rejected(t *testing.T) {
	ctx := newCtx(&model.User{ID: "bob"}) // l1 の owner は alice
	ch := newUserListCh(ctx, "alice")
	err := ch.Init(json.RawMessage(`{"listId":"l1"}`))
	assert.ErrorIs(t, err, stream.ErrInvalidParams, "他人の list は購読拒否")
	assert.Empty(t, ctx.subs, "拒否時は topic を subscribe しない")
}

// 存在しない list id は lookup が (nil, nil) を返すので拒否される。
func TestUserList_UnknownList_Rejected(t *testing.T) {
	ctx := newCtx(&model.User{ID: "alice"})
	ch := newUserListCh(ctx, "alice")
	err := ch.Init(json.RawMessage(`{"listId":"nope"}`))
	assert.ErrorIs(t, err, stream.ErrInvalidParams)
	assert.Empty(t, ctx.subs)
}

// lookup が error を返した場合も fail-closed で拒否する。
func TestUserList_OwnerLookupError_Rejected(t *testing.T) {
	ctx := newCtx(&model.User{ID: "alice"})
	owners := &stubUserListOwners{err: errUserListLookup}
	ch := (&UserListFactory{owners: owners}).New(ctx)
	err := ch.Init(json.RawMessage(`{"listId":"l1"}`))
	assert.ErrorIs(t, err, stream.ErrInvalidParams)
	assert.Empty(t, ctx.subs)
}

// owner lookup 未配線 (nil) は全購読を拒否する (antenna #1569 と同方針)。
func TestUserList_NoOwnerLookup_FailsClosed(t *testing.T) {
	ctx := newCtx(&model.User{ID: "alice"})
	ch := NewUserListFactory(nil).New(ctx)
	err := ch.Init(json.RawMessage(`{"listId":"l1"}`))
	assert.ErrorIs(t, err, stream.ErrInvalidParams)
	assert.Empty(t, ctx.subs)
	ch.Dispose() // topic 未設定でも Dispose は no-op で安全
	assert.Empty(t, ctx.unsubs)
}

// #2020: membership lookup 未配線 (owner lookup のみ) では reply gate を skip する。
func TestUserList_NoLookupSkipsReplyGate(t *testing.T) {
	ctx := newCtx(&model.User{ID: "alice"})
	ch := newUserListCh(ctx, "alice")
	require.NoError(t, ch.Init(json.RawMessage(`{"listId":"l1"}`)))
	ctx.sentType = nil
	ch.OnRedisEvent([]byte(`{"id":"n1","userId":"bob","visibility":"public","reply":{"userId":"dave","visibility":"public"}}`))
	assert.Len(t, ctx.sentType, 1, "lookup 未配線時は reply gate skip (後方互換、#2020)")
}

// #2051: userUpdated event で per-member withReplies snapshot を live 更新し、
// userRemoved で除去する。userUpdated は client へ forward しない。
func TestUserList_MembershipSnapshotLiveUpdate(t *testing.T) {
	ctx := newCtx(&model.User{ID: "alice"})
	lookup := &stubMembershipLookup{ownerID: "alice", members: []*model.UserListMembership{{UserID: "bob", WithReplies: false}}}
	ch := NewUserListFactory(lookup).New(ctx)
	require.NoError(t, ch.Init(json.RawMessage(`{"listId":"l1"}`)))

	replyNote := `{"id":"n1","userId":"bob","visibility":"public","reply":{"userId":"dave","visibility":"public"}}`
	send := func(payload string) int {
		ctx.sentType = nil
		ch.OnRedisEvent([]byte(payload))
		return len(ctx.sentType)
	}

	// 初期 (bob withReplies=false): 第三者 reply は drop。
	assert.Equal(t, 0, send(replyNote), "withReplies=false で drop")
	// userUpdated で bob を withReplies=true に (client へは流さない)。
	assert.Equal(t, 0, send(`{"type":"userUpdated","body":{"id":"bob","withReplies":true}}`),
		"userUpdated は client へ流さない (#2051)")
	// 更新後: bob の第三者 reply が pass。
	assert.Equal(t, 1, send(replyNote), "userUpdated で withReplies=true 反映 → reply pass (#2051)")
	// userRemoved で bob を除去 (client へ forward) → 以降 absent (default false)。
	assert.Equal(t, 1, send(`{"type":"userRemoved","body":{"id":"bob"}}`), "userRemoved は forward")
	assert.Equal(t, 0, send(replyNote), "userRemoved 後は absent → withReplies=false → drop")
}

// #2051: note 配信と membership event は別 topic で並行 fanout されうるため、
// withRepliesByUser は mu で保護される。-race で並行アクセスの安全性を検証する。
func TestUserList_ConcurrentSnapshotAndNote(t *testing.T) {
	ctx := newCtx(&model.User{ID: "alice"})
	lookup := &stubMembershipLookup{ownerID: "alice", members: []*model.UserListMembership{{UserID: "bob", WithReplies: false}}}
	ch := NewUserListFactory(lookup).New(ctx)
	require.NoError(t, ch.Init(json.RawMessage(`{"listId":"l1"}`)))

	var wg sync.WaitGroup
	wg.Add(2)
	// goroutine A: membership event で snapshot を書き換え続ける。
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			ch.OnRedisEvent([]byte(`{"type":"userUpdated","body":{"id":"bob","withReplies":true}}`))
			ch.OnRedisEvent([]byte(`{"type":"userUpdated","body":{"id":"bob","withReplies":false}}`))
		}
	}()
	// goroutine B: note 配信で snapshot を読み続ける。
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			ch.OnRedisEvent([]byte(`{"id":"n1","userId":"bob","visibility":"public","reply":{"userId":"dave","visibility":"public"}}`))
		}
	}()
	wg.Wait()
}
