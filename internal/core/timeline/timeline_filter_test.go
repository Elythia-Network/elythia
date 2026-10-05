package timeline

import (
	"testing"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
)

func boolPtr(b bool) *bool    { return &b }
func strPtr(s string) *string { return &s }

// テスト用ノート生成ヘルパ
func makeNote(id string, opts ...func(*model.Note)) *model.Note {
	n := &model.Note{
		ID:         id,
		UserID:     "author",
		Visibility: model.NoteVisibilityPublic,
	}
	for _, o := range opts {
		o(n)
	}
	return n
}

func withFiles(n *model.Note)               { n.FileIDs = model.StringArray{"file1"} }
func withRenote(n *model.Note)              { n.RenoteID = strPtr("rn1") }
func withText(n *model.Note)                { n.Text = strPtr("hello") }
func withReply(n *model.Note)               { n.ReplyID = strPtr("rp1"); n.ReplyUserID = strPtr("other") }
func withReplySelf(n *model.Note)           { n.ReplyID = strPtr("rp1"); n.ReplyUserID = strPtr("viewer") }
func withUser(uid string) func(*model.Note) { return func(n *model.Note) { n.UserID = uid } }
func withRenoteUser(uid string) func(*model.Note) {
	return func(n *model.Note) { n.RenoteUserID = strPtr(uid) }
}
func withRenoteUserHost(host string) func(*model.Note) {
	return func(n *model.Note) { n.RenoteUserHost = strPtr(host) }
}
func withLocalRenoteUser(n *model.Note) {
	n.RenoteUserHost = nil
}

func TestIsPureRenote(t *testing.T) {
	assert.True(t, isPureRenote(makeNote("1", withRenote)))
	assert.False(t, isPureRenote(makeNote("2")))
	assert.False(t, isPureRenote(makeNote("3", withRenote, withText)))
	assert.False(t, isPureRenote(makeNote("4", withRenote, withFiles)))
	// quote 判定は cw / poll / reply も含む (#1886、upstream isPureRenote = !isQuote)。
	assert.False(t, isPureRenote(makeNote("5", withRenote, func(n *model.Note) { n.CW = strPtr("cw") })))
	assert.False(t, isPureRenote(makeNote("6", withRenote, func(n *model.Note) { n.HasPoll = true })))
	assert.False(t, isPureRenote(makeNote("7", withRenote, withReply)), "renote + reply = quote")
}

func withReplyUser(uid string) func(*model.Note) {
	return func(n *model.Note) { n.ReplyID = strPtr("rp1"); n.ReplyUserID = strPtr(uid) }
}
func withHost(host string) func(*model.Note) {
	return func(n *model.Note) { n.UserHost = strPtr(host) }
}

// #1686 suspended: note/reply/renote のいずれかの author が suspended なら除外。
// User relation は FindManyByIDsWithUser が配線する前提なのでテストでも seed する。
func withAuthorSuspended(n *model.Note) {
	n.User = &model.User{ID: n.UserID, IsSuspended: true}
}
func withReplyAuthorSuspended(n *model.Note) {
	n.ReplyID = strPtr("rp1")
	n.ReplyUserID = strPtr("suspended")
	n.Reply = &model.Note{ID: "rp1", UserID: "suspended", User: &model.User{ID: "suspended", IsSuspended: true}}
}
func withRenoteAuthorSuspended(n *model.Note) {
	n.RenoteID = strPtr("rn1")
	n.RenoteUserID = strPtr("suspended")
	n.Renote = &model.Note{ID: "rn1", UserID: "suspended", User: &model.User{ID: "suspended", IsSuspended: true}}
}

func TestApplyFilter_Suspended(t *testing.T) {
	notes := []*model.Note{
		makeNote("a", withAuthorSuspended),       // note author が suspended
		makeNote("b", withReplyAuthorSuspended),  // reply 先 author が suspended
		makeNote("c", withRenoteAuthorSuspended), // renote 元 author が suspended
		// User relation が nil なら pass-through (suspended 不明扱い)
		makeNote("nilrel"),
		// 非 suspended author は通す
		func() *model.Note {
			n := makeNote("ok")
			n.User = &model.User{ID: "author", IsSuspended: false}
			return n
		}(),
	}
	out := ApplyFilter(notes, "viewer", TimelineFilter{})
	ids := make([]string, 0, len(out))
	for _, n := range out {
		ids = append(ids, n.ID)
	}
	assert.ElementsMatch(t, []string{"nilrel", "ok"}, ids)
}

// #1681 被block: note/reply/renote のいずれかの author が viewer を block して
// いれば除外。
func TestApplyFilter_Blocker(t *testing.T) {
	notes := []*model.Note{
		makeNote("a", withUser("blocker")),                   // note author が blocker
		makeNote("b", withReplyUser("blocker")),              // reply 先が blocker
		makeNote("c", withRenote, withRenoteUser("blocker")), // renote 元が blocker
		makeNote("ok", withUser("friend")),
	}
	out := ApplyFilter(notes, "viewer", TimelineFilter{BlockerIDs: []string{"blocker"}})
	assert.Len(t, out, 1)
	assert.Equal(t, "ok", out[0].ID)
}

// #1681 reply著者mute: reply 先が muted user なら除外 (mutedUsers が reply
// author も見る)。
func TestApplyFilter_MutedReplyAuthor(t *testing.T) {
	notes := []*model.Note{
		makeNote("a", withReplyUser("muted")),
		makeNote("ok", withReplyUser("other")),
	}
	out := ApplyFilter(notes, "viewer", TimelineFilter{MutedUserIDs: []string{"muted"}})
	assert.Len(t, out, 1)
	assert.Equal(t, "ok", out[0].ID)
}

// #1681 instance-mute: note/reply/renote のいずれかの author host が muted
// instance なら除外。case-insensitive。
func TestApplyFilter_MutedInstance(t *testing.T) {
	notes := []*model.Note{
		makeNote("a", withHost("Bad.Example")), // 大文字混じり host
		makeNote("b", func(n *model.Note) { n.ReplyUserHost = strPtr("bad.example") }),
		makeNote("local"), // host nil = local、常に通す
		makeNote("ok", withHost("good.example")),
	}
	out := ApplyFilter(notes, "viewer", TimelineFilter{MutedInstances: []string{"bad.example"}})
	ids := map[string]bool{}
	for _, n := range out {
		ids[n.ID] = true
	}
	assert.False(t, ids["a"])
	assert.False(t, ids["b"])
	assert.True(t, ids["local"])
	assert.True(t, ids["ok"])
}

func TestApplyFilter_WithFiles(t *testing.T) {
	notes := []*model.Note{
		makeNote("1"),
		makeNote("2", withFiles),
	}
	out := ApplyFilter(notes, "", TimelineFilter{WithFiles: true})
	assert.Len(t, out, 1)
	assert.Equal(t, "2", out[0].ID)
}

func TestApplyFilter_WithRenotes_False(t *testing.T) {
	notes := []*model.Note{
		makeNote("1"),
		makeNote("2", withRenote),            // pure renote → 除外
		makeNote("3", withRenote, withText),  // quote renote → 残る
		makeNote("4", withRenote, withFiles), // quote renote (file) → 残る
	}
	out := ApplyFilter(notes, "", TimelineFilter{WithRenotes: boolPtr(false)})
	assert.Len(t, out, 3)
}

func TestApplyFilter_WithRenotes_Default(t *testing.T) {
	notes := []*model.Note{
		makeNote("1"),
		makeNote("2", withRenote),
	}
	// WithRenotes nil → true (全部残る)
	out := ApplyFilter(notes, "", TimelineFilter{})
	assert.Len(t, out, 2)
}

// #1047: ApplyFilter (in-memory) は withReplies を参照しない pass-through。
// reply の表示制御は fanout 側で `following.withReplies` を見て push を
// 制御するため、cache に乗った note は全部 TL に表示される (= upstream の
// fanoutTimelineEndpointService.timeline と同 logic)。
func TestApplyFilter_WithReplies_PassThrough(t *testing.T) {
	notes := []*model.Note{
		makeNote("1"),
		makeNote("2", withReply),     // 他人への返信 → cache 経路では残る
		makeNote("3", withReplySelf), // 自分への返信 → 残る
	}
	out := ApplyFilter(notes, "viewer", TimelineFilter{WithReplies: boolPtr(false)})
	assert.Len(t, out, 3, "in-memory filter は withReplies を参照しない (#1047)")
}

func TestApplyFilter_WithReplies_TruePassThrough(t *testing.T) {
	notes := []*model.Note{
		makeNote("1"),
		makeNote("2", withReply),
	}
	out := ApplyFilter(notes, "viewer", TimelineFilter{WithReplies: boolPtr(true)})
	assert.Len(t, out, 2)
}

// viewer 不在 (= anonymous) でも reply filter は cache 経路で適用しない。
// upstream の noteFilter にも anonymous 向けの reply filter は無い。
func TestApplyFilter_WithReplies_NoViewerPassThrough(t *testing.T) {
	notes := []*model.Note{
		makeNote("1"),
		makeNote("2", withReply),
	}
	out := ApplyFilter(notes, "", TimelineFilter{WithReplies: boolPtr(false)})
	assert.Len(t, out, 2, "anonymous でも in-memory filter は適用しない (#1047)")
}

func TestApplyFilter_IncludeMyRenotes_False(t *testing.T) {
	notes := []*model.Note{
		makeNote("1"),
		makeNote("2", withRenote, withUser("viewer")),           // viewerのpure renote → 除外
		makeNote("3", withRenote, withUser("other")),            // 他人のpure renote → 残る
		makeNote("4", withRenote, withText, withUser("viewer")), // viewerのquote → 残る
	}
	out := ApplyFilter(notes, "viewer", TimelineFilter{IncludeMyRenotes: boolPtr(false)})
	assert.Len(t, out, 3)
}

func TestApplyFilter_IncludeRenotedMyNotes_False(t *testing.T) {
	notes := []*model.Note{
		makeNote("1"),
		makeNote("2", withRenote, withRenoteUser("viewer")), // viewerのノートのpure renote → 除外
		makeNote("3", withRenote, withRenoteUser("other")),  // 他人のノートのpure renote → 残る
	}
	out := ApplyFilter(notes, "viewer", TimelineFilter{IncludeRenotedMyNotes: boolPtr(false)})
	assert.Len(t, out, 2)
}

func TestApplyFilter_IncludeLocalRenotes_False(t *testing.T) {
	notes := []*model.Note{
		makeNote("1"),
		makeNote("2", withRenote, withLocalRenoteUser),              // ローカルpure renote → 除外
		makeNote("3", withRenote, withRenoteUserHost("remote.tld")), // リモートpure renote → 残る
	}
	out := ApplyFilter(notes, "viewer", TimelineFilter{IncludeLocalRenotes: boolPtr(false)})
	assert.Len(t, out, 2)
}

func TestApplyFilter_CombinedFilters(t *testing.T) {
	notes := []*model.Note{
		makeNote("1", withFiles),                       // ファイル付き、通常ノート → 残る
		makeNote("2"),                                  // ファイルなし → 除外 (withFiles)
		makeNote("3", withRenote),                      // pure renote (ファイルなし) → 除外 (both)
		makeNote("4", withFiles, withRenote, withText), // ファイル付きquote → 残る
	}
	out := ApplyFilter(notes, "", TimelineFilter{
		WithFiles:   true,
		WithRenotes: boolPtr(false),
	})
	assert.Len(t, out, 2)
	assert.Equal(t, "1", out[0].ID)
	assert.Equal(t, "4", out[1].ID)
}

func TestBoolDefault(t *testing.T) {
	assert.True(t, boolDefault(nil, true))
	assert.False(t, boolDefault(nil, false))
	assert.True(t, boolDefault(boolPtr(true), false))
	assert.False(t, boolDefault(boolPtr(false), true))
}

func withChannel(id string) func(*model.Note) {
	return func(n *model.Note) { n.ChannelID = strPtr(id) }
}

func TestApplyFilter_MutedChannelsExcluded(t *testing.T) {
	notes := []*model.Note{
		makeNote("1"),                            // channelId なし → 残る
		makeNote("2", withChannel("ch-muted")),   // mute 対象 → 除外
		makeNote("3", withChannel("ch-allowed")), // 非 mute → 残る
		makeNote("4", withChannel("ch-muted")),   // mute 対象 (重複) → 除外
	}
	out := ApplyFilter(notes, "viewer", TimelineFilter{
		MutedChannelIDs: []string{"ch-muted"},
	})
	assert.Len(t, out, 2)
	assert.Equal(t, "1", out[0].ID)
	assert.Equal(t, "3", out[1].ID)
}

func TestApplyFilter_MutedChannelsEmptyIsNoop(t *testing.T) {
	notes := []*model.Note{
		makeNote("1", withChannel("any")),
		makeNote("2"),
	}
	// 空 slice は全件通す (従来挙動の維持)
	out := ApplyFilter(notes, "", TimelineFilter{})
	assert.Len(t, out, 2)
}

func TestApplyFilter_MutedUsersExcluded(t *testing.T) {
	// author = note 投稿者。muted-author の note は除外される (#874)。
	notes := []*model.Note{
		makeNote("1", withUser("normal")),
		makeNote("2", withUser("muted-author")),
		makeNote("3", withUser("normal2")),
	}
	out := ApplyFilter(notes, "viewer", TimelineFilter{
		MutedUserIDs: []string{"muted-author"},
	})
	assert.Len(t, out, 2)
	assert.Equal(t, "1", out[0].ID)
	assert.Equal(t, "3", out[1].ID)
}

func TestApplyFilter_MutedRenoteSourceExcluded(t *testing.T) {
	// renote 元 user が muted の場合、renote note 自体も除外する
	// (= upstream Misskey TS の muting JOIN と同 semantics、#874)。
	notes := []*model.Note{
		makeNote("1", withUser("normal"), withRenote, withRenoteUser("muted-source")),
		makeNote("2", withUser("normal"), withRenote, withRenoteUser("ok-source")),
	}
	out := ApplyFilter(notes, "viewer", TimelineFilter{
		MutedUserIDs: []string{"muted-source"},
	})
	assert.Len(t, out, 1)
	assert.Equal(t, "2", out[0].ID)
}

func TestApplyFilter_MutedUsersEmptyIsNoop(t *testing.T) {
	notes := []*model.Note{
		makeNote("1", withUser("any")),
		makeNote("2", withUser("any2")),
	}
	// nil / 空 slice は filter 無効 (= 従来挙動)
	out := ApplyFilter(notes, "viewer", TimelineFilter{MutedUserIDs: nil})
	assert.Len(t, out, 2)
	out2 := ApplyFilter(notes, "viewer", TimelineFilter{MutedUserIDs: []string{}})
	assert.Len(t, out2, 2)
}

// renote-mute は投稿者が renote-muted で **かつ** pure renote の場合のみ
// 除外 (#903)。投稿者の plain note / quote renote はそのまま通る。
func TestApplyFilter_RenoteMutedPureRenoteExcluded(t *testing.T) {
	notes := []*model.Note{
		makeNote("1", withUser("muted-renoter"), withRenote),
		makeNote("2", withUser("ok-renoter"), withRenote),
	}
	out := ApplyFilter(notes, "viewer", TimelineFilter{
		RenoteMutedUserIDs: []string{"muted-renoter"},
	})
	assert.Len(t, out, 1)
	assert.Equal(t, "2", out[0].ID)
}

// renote-mute は **plain note は通す** (regular mute との違い、#903)。
// 投稿者が renote-mute されていても、その人の plain な note は表示する。
func TestApplyFilter_RenoteMutedPlainNoteKept(t *testing.T) {
	notes := []*model.Note{
		makeNote("1", withUser("muted-renoter"), withText),   // plain note
		makeNote("2", withUser("muted-renoter"), withRenote), // pure renote → skip
	}
	out := ApplyFilter(notes, "viewer", TimelineFilter{
		RenoteMutedUserIDs: []string{"muted-renoter"},
	})
	assert.Len(t, out, 1)
	assert.Equal(t, "1", out[0].ID, "plain note (text 付き) は renote-mute 対象外")
}

// renote-mute は **quote renote (text 付き / file 付き renote) も通す**
// (#903、isPureRenote が false なので skip 対象外)。
func TestApplyFilter_RenoteMutedQuoteRenoteKept(t *testing.T) {
	notes := []*model.Note{
		makeNote("1", withUser("muted-renoter"), withRenote, withText),  // quote renote
		makeNote("2", withUser("muted-renoter"), withRenote, withFiles), // file 付き renote
		makeNote("3", withUser("muted-renoter"), withRenote),            // pure renote → skip
	}
	out := ApplyFilter(notes, "viewer", TimelineFilter{
		RenoteMutedUserIDs: []string{"muted-renoter"},
	})
	assert.Len(t, out, 2)
	assert.Equal(t, "1", out[0].ID)
	assert.Equal(t, "2", out[1].ID)
}

// upstream timeline.ts の noteFilter: 返信先が followers 限定の投稿で、その
// 投稿者をフォローしていない (かつ自分でもない) 場合は HTL に出さない。
func TestApplyFilter_HidesFollowersOnlyReplyFromNonFollowee(t *testing.T) {
	reply := &model.Note{ID: "src", UserID: "carol", Visibility: model.NoteVisibilityFollowers}
	n := &model.Note{ID: "n1", UserID: "bob", ReplyID: &reply.ID, Reply: reply}

	// carol をフォローしていない -> 除外
	out := ApplyFilter([]*model.Note{n}, "alice", TimelineFilter{FollowingIDs: map[string]struct{}{"bob": {}}})
	assert.Empty(t, out)

	// carol をフォローしている -> 残る
	out = ApplyFilter([]*model.Note{n}, "alice", TimelineFilter{FollowingIDs: map[string]struct{}{"bob": {}, "carol": {}}})
	assert.Len(t, out, 1)

	// 自分の投稿への返信なら残る
	self := &model.Note{ID: "src2", UserID: "alice", Visibility: model.NoteVisibilityFollowers}
	n2 := &model.Note{ID: "n2", UserID: "bob", ReplyID: &self.ID, Reply: self}
	out = ApplyFilter([]*model.Note{n2}, "alice", TimelineFilter{FollowingIDs: map[string]struct{}{"bob": {}}})
	assert.Len(t, out, 1)

	// FollowingIDs 未配線なら判定しない (従来挙動)
	out = ApplyFilter([]*model.Note{n}, "alice", TimelineFilter{})
	assert.Len(t, out, 1)
}

// 返信先が public なら素通し。
func TestApplyFilter_PublicReplyPasses(t *testing.T) {
	reply := &model.Note{ID: "src", UserID: "carol", Visibility: model.NoteVisibilityPublic}
	n := &model.Note{ID: "n1", UserID: "bob", ReplyID: &reply.ID, Reply: reply}
	out := ApplyFilter([]*model.Note{n}, "alice", TimelineFilter{FollowingIDs: map[string]struct{}{"bob": {}}})
	assert.Len(t, out, 1)
}

// LocalUsersOnly drops notes whose own author is remote but keeps a local
// note that replies to / renotes a remote note (upstream leaves attached
// remote notes visible for visitors under ugcVisibilityForVisitor=local).
func TestApplyFilter_LocalUsersOnly(t *testing.T) {
	remote := func(n *model.Note) { n.UserHost = strPtr("remote.example") }
	renoteOfRemote := func(n *model.Note) {
		n.RenoteID = strPtr("rn1")
		n.RenoteUserHost = strPtr("remote.example")
	}
	notes := []*model.Note{makeNote("local"), makeNote("remote", remote), makeNote("renote", renoteOfRemote)}

	got := ApplyFilter(notes, "", TimelineFilter{LocalUsersOnly: true})
	ids := make([]string, 0, len(got))
	for _, n := range got {
		ids = append(ids, n.ID)
	}
	assert.Equal(t, []string{"local", "renote"}, ids)

	assert.Len(t, ApplyFilter(notes, "", TimelineFilter{}), 3, "unset keeps remote authors")
}
