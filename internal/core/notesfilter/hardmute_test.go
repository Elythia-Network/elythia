package notesfilter_test

import (
	"errors"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/core/notesfilter"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
)

func mkNote(authorID, text, cw string) *model.Note {
	var pt, pc *string
	if text != "" {
		t := text
		pt = &t
	}
	if cw != "" {
		c := cw
		pc = &c
	}
	return &model.Note{ID: authorID + ":" + text, UserID: authorID, Text: pt, CW: pc}
}

// hardMuted reports whether ApplyHardMute drops n for viewerID under rules.
func hardMuted(t *testing.T, rules, viewerID string, n *model.Note, idGen id.Generator) bool {
	t.Helper()
	lookup := testutil.NewMockUserRepository()
	lookup.Profiles[viewerID] = &model.UserProfile{UserID: viewerID, HardMutedWords: datatypes.JSON([]byte(rules))}
	return len(notesfilter.ApplyHardMute(lookup, &model.User{ID: viewerID}, []*model.Note{n}, idGen)) == 0
}

func TestApplyHardMute_NoLookupWired(t *testing.T) {
	notes := []*model.Note{mkNote("other", "anything", "")}
	out := notesfilter.ApplyHardMute(nil, &model.User{ID: "viewer"}, notes, nil)
	assert.Len(t, out, 1)
}

func TestApplyHardMute_AnonymousViewer(t *testing.T) {
	lookup := testutil.NewMockUserRepository()
	notes := []*model.Note{mkNote("other", "muted text", "")}
	out := notesfilter.ApplyHardMute(lookup, nil, notes, nil)
	assert.Len(t, out, 1)
}

func TestApplyHardMute_EmptyNotes(t *testing.T) {
	lookup := testutil.NewMockUserRepository()
	lookup.Profiles["viewer"] = &model.UserProfile{
		UserID:         "viewer",
		HardMutedWords: datatypes.JSON([]byte(`[["x"]]`)),
	}
	out := notesfilter.ApplyHardMute(lookup, &model.User{ID: "viewer"}, nil, nil)
	assert.Empty(t, out)
}

func TestApplyHardMute_FiltersMutedExemptsSelfAndKeepsClean(t *testing.T) {
	lookup := testutil.NewMockUserRepository()
	lookup.Profiles["viewer"] = &model.UserProfile{
		UserID:         "viewer",
		HardMutedWords: datatypes.JSON([]byte(`[["politics"],"/spam[0-9]+/i"]`)),
	}
	viewer := &model.User{ID: "viewer"}
	notes := []*model.Note{
		mkNote("other", "today's politics rant", ""),
		mkNote("other", "harmless cat photo", ""),
		mkNote("other", "totally SPAM42 here", ""),
		mkNote("viewer", "politics by me, should not be filtered", ""),
	}
	out := notesfilter.ApplyHardMute(lookup, viewer, notes, nil)
	require.Len(t, out, 2)
	assert.Equal(t, "harmless cat photo", *out[0].Text)
	assert.Equal(t, "viewer", out[1].UserID)
}

func TestApplyHardMute_MatchesCW(t *testing.T) {
	lookup := testutil.NewMockUserRepository()
	lookup.Profiles["viewer"] = &model.UserProfile{
		UserID:         "viewer",
		HardMutedWords: datatypes.JSON([]byte(`[["spoiler"]]`)),
	}
	viewer := &model.User{ID: "viewer"}
	notes := []*model.Note{mkNote("other", "innocuous body", "spoiler ahead")}
	out := notesfilter.ApplyHardMute(lookup, viewer, notes, nil)
	assert.Empty(t, out)
}

func TestApplyHardMute_EmptyRulesIsNoOp(t *testing.T) {
	lookup := testutil.NewMockUserRepository()
	lookup.Profiles["viewer"] = &model.UserProfile{UserID: "viewer"}
	notes := []*model.Note{mkNote("other", "anything", "")}
	out := notesfilter.ApplyHardMute(lookup, &model.User{ID: "viewer"}, notes, nil)
	assert.Len(t, out, 1)
}

func TestApplyHardMute_ProfileFetchErrorDoesNotPanic(t *testing.T) {
	lookup := testutil.NewMockUserRepository()
	// viewer の profile を入れない → mock は ErrNotFound を返す
	notes := []*model.Note{mkNote("other", "any", "")}
	out := notesfilter.ApplyHardMute(lookup, &model.User{ID: "viewer"}, notes, nil)
	assert.Len(t, out, 1)
}

func TestApplyHardMute_RegexAndSelf(t *testing.T) {
	rules := `["/topic/i"]`
	assert.True(t, hardMuted(t, rules, "u1", mkNote("u2", "talking about TOPIC today", ""), nil))
	// viewer 自身の note は match しない
	assert.False(t, hardMuted(t, rules, "u1", mkNote("u1", "topic by self", ""), nil))
}

// #3369: 本家 frontend use-note.ts の checkNoteWordMute(getAppearNote(note)) から
// 書き下した期待値。純粋なリノートはリノート元で判定し、返信先と引用先も見る。
func TestApplyHardMute_UpstreamAppearNote(t *testing.T) {
	rules := `[["muted"]]`
	muted := func(author string) *model.Note {
		return &model.Note{ID: "m-" + author, UserID: author, Text: strp("a muted word"), Visibility: model.NoteVisibilityPublic}
	}
	clean := func(author string) *model.Note {
		return &model.Note{ID: "c-" + author, UserID: author, Text: strp("clean"), Visibility: model.NoteVisibilityPublic}
	}
	pureRenote := func(by string, target *model.Note) *model.Note {
		return &model.Note{ID: "rn-" + by, UserID: by, RenoteID: &target.ID, Renote: target, Visibility: model.NoteVisibilityPublic}
	}
	withReply := func(n, parent *model.Note) *model.Note {
		n.ReplyID = &parent.ID
		n.Reply = parent
		return n
	}
	withQuote := func(n, quoted *model.Note) *model.Note {
		n.RenoteID = &quoted.ID
		n.Renote = quoted
		return n
	}
	followersOnly := func(n *model.Note) *model.Note {
		n.Visibility = model.NoteVisibilityFollowers
		return n
	}
	cases := []struct {
		name string
		note *model.Note
		want bool
	}{
		{"plain muted note", muted("author"), true},
		{"plain clean note", clean("author"), false},
		{"cw is judged", &model.Note{ID: "cw", UserID: "author", CW: strp("muted"), Text: strp("body")}, true},
		{"viewer's own note", muted("viewer"), false},
		{"pure renote of a muted note", pureRenote("booster", muted("author")), true},
		{"pure renote by the viewer of a muted note", pureRenote("viewer", muted("author")), true},
		{"pure renote of the viewer's own muted note", pureRenote("booster", muted("viewer")), false},
		{"pure renote of a clean note", pureRenote("booster", clean("author")), false},
		{"pure renote of a reply to a muted note", pureRenote("booster", withReply(clean("author"), muted("other"))), true},
		{"pure renote of a quote of a muted note", pureRenote("booster", withQuote(clean("author"), muted("other"))), true},
		{"quote of a muted note", withQuote(clean("author"), muted("other")), true},
		{"quote with muted comment", withQuote(muted("author"), clean("other")), true},
		{"quote of a quote of a muted note is not looked into", withQuote(clean("author"), withQuote(clean("other"), muted("third"))), false},
		{"reply to a muted note", withReply(clean("author"), muted("other")), true},
		{"reply to the viewer's own muted note", withReply(clean("author"), muted("viewer")), false},
		{"reply to a followers-only muted note the viewer cannot see", withReply(clean("author"), followersOnly(muted("other"))), false},
		{"pure renote of a followers-only muted note the viewer cannot see", pureRenote("booster", followersOnly(muted("other"))), false},
		{"pure renote of a quote whose target the viewer cannot see", pureRenote("booster", withQuote(clean("author"), followersOnly(muted("other")))), false},
		{"pure renote of a reply whose target the viewer cannot see", pureRenote("booster", withReply(clean("author"), followersOnly(muted("other")))), false},
		{"reply to a followers-only muted note that mentions the viewer", withReply(clean("author"), func() *model.Note {
			n := followersOnly(muted("other"))
			n.Mentions = []string{"viewer"}
			return n
		}()), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, hardMuted(t, rules, "viewer", tc.note, nil))
		})
	}
}

func TestApplyHardMute_PureRenoteIsJudgedByTarget(t *testing.T) {
	lookup := testutil.NewMockUserRepository()
	lookup.Profiles["viewer"] = &model.UserProfile{UserID: "viewer", HardMutedWords: datatypes.JSON([]byte(`[["muted"]]`))}
	target := &model.Note{ID: "t1", UserID: "author", Text: strp("muted"), Visibility: model.NoteVisibilityPublic}
	boost := &model.Note{ID: "b1", UserID: "booster", RenoteID: &target.ID, Renote: target, Visibility: model.NoteVisibilityPublic}
	keep := &model.Note{ID: "k1", UserID: "author", Text: strp("clean"), Visibility: model.NoteVisibilityPublic}
	out := notesfilter.ApplyHardMute(lookup, &model.User{ID: "viewer"}, []*model.Note{boost, keep}, nil)
	require.Len(t, out, 1)
	assert.Equal(t, "k1", out[0].ID)
}

// 著者の makeNotesHiddenBefore で本文が隠されるノートは判定しない (本家は
// hideNote の後の本文を判定する)。ID の時刻は idGen で読む。
func TestApplyHardMute_AuthorTimeGateUsesIDTime(t *testing.T) {
	gen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	window := -3600 // 作成から 1 時間を過ぎたら隠す
	author := &model.User{ID: "author", MakeNotesHiddenBefore: &window}
	rules := `[["muted"]]`
	fresh := &model.Note{ID: gen.Generate(time.Now()), UserID: "author", User: author, Text: strp("muted"), Visibility: model.NoteVisibilityPublic}
	old := &model.Note{ID: gen.Generate(time.Now().Add(-2 * time.Hour)), UserID: "author", User: author, Text: strp("muted"), Visibility: model.NoteVisibilityPublic}
	assert.True(t, hardMuted(t, rules, "viewer", fresh, gen))
	assert.False(t, hardMuted(t, rules, "viewer", old, gen))
	// idGen が無ければ時刻が分からないので、隠される側 (判定しない側) に倒す。
	assert.False(t, hardMuted(t, rules, "viewer", fresh, nil))
}

// makeNotesFollowersOnlyBefore で降格したノートでも、viewer への返信なら viewer
// には見えるので判定する (返信先の著者は top-level の判定に渡す)。
func TestApplyHardMute_DowngradedReplyToViewerIsJudged(t *testing.T) {
	gen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	window := -3600
	author := &model.User{ID: "author", MakeNotesFollowersOnlyBefore: &window}
	parent := &model.Note{ID: gen.Generate(time.Now()), UserID: "viewer", Text: strp("question"), Visibility: model.NoteVisibilityPublic}
	reply := &model.Note{ID: gen.Generate(time.Now().Add(-2 * time.Hour)), UserID: "author", User: author, Text: strp("muted"), Visibility: model.NoteVisibilityPublic, ReplyID: &parent.ID, Reply: parent}
	assert.True(t, hardMuted(t, `[["muted"]]`, "viewer", reply, gen))
	// 返信先が viewer でなければ、フォローしていない viewer には見えないので判定しない。
	other := &model.Note{ID: gen.Generate(time.Now()), UserID: "someone", Text: strp("question"), Visibility: model.NoteVisibilityPublic}
	reply2 := *reply
	reply2.ReplyID, reply2.Reply = &other.ID, other
	assert.False(t, hardMuted(t, `[["muted"]]`, "viewer", &reply2, gen))
}

// failingIDGen cannot decode any note ID.
type failingIDGen struct{ id.Generator }

func (failingIDGen) ParseTime(string) (time.Time, error) {
	return time.Time{}, errors.New("undecodable")
}

// An author's time-window setting needs the note's age. When the ID cannot be
// decoded the note must not be judged (treated as unseen), never as fresh.
func TestApplyHardMute_UndecodableIDIsNotJudged(t *testing.T) {
	gen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	window := -3600
	author := &model.User{ID: "author", MakeNotesHiddenBefore: &window}
	n := &model.Note{ID: gen.Generate(time.Now()), UserID: "author", User: author, Text: strp("muted"), Visibility: model.NoteVisibilityPublic}
	assert.True(t, hardMuted(t, `[["muted"]]`, "viewer", n, gen), "control: a fresh note is judged")
	assert.False(t, hardMuted(t, `[["muted"]]`, "viewer", n, failingIDGen{gen}))
}

// The lists that reach ApplyHardMute only carry top-level notes the viewer may
// see, so a followers-only top-level note (of an author the viewer follows) is
// judged; only the author's preference gates apply at the top level.
func TestApplyHardMute_FollowersOnlyTopLevelIsJudged(t *testing.T) {
	n := &model.Note{ID: "f1", UserID: "author", Text: strp("muted"), Visibility: model.NoteVisibilityFollowers}
	assert.True(t, hardMuted(t, `[["muted"]]`, "viewer", n, nil))
}

// A renote that is also a reply is not a pure renote (misskey-js isPureRenote
// looks at replyId), so it is judged as itself, not by the renoted note.
func TestApplyHardMute_RenoteWithReplyIsNotPure(t *testing.T) {
	inner := &model.Note{ID: "inner", UserID: "third", Text: strp("muted"), Visibility: model.NoteVisibilityPublic}
	target := &model.Note{ID: "t", UserID: "author", Text: strp("clean"), Visibility: model.NoteVisibilityPublic, ReplyID: &inner.ID, Reply: inner}
	parent := &model.Note{ID: "p", UserID: "other", Text: strp("clean"), Visibility: model.NoteVisibilityPublic}
	n := &model.Note{ID: "n", UserID: "booster", RenoteID: &target.ID, Renote: target, ReplyID: &parent.ID, Reply: parent, Visibility: model.NoteVisibilityPublic}
	assert.False(t, hardMuted(t, `[["muted"]]`, "viewer", n, nil))
	// 対照: 返信でなければ純粋なリノートで、リノート元の返信先まで見る。
	pure := &model.Note{ID: "n2", UserID: "booster", RenoteID: &target.ID, Renote: target, Visibility: model.NoteVisibilityPublic}
	assert.True(t, hardMuted(t, `[["muted"]]`, "viewer", pure, nil))
}

// 埋め込み (リノート元) にも著者の時刻の窓を当てる。窓を過ぎて本文が隠される
// 埋め込みの中身では判定しない。ID の時刻は idGen で読む。
func TestApplyHardMute_EmbedTimeGateUsesIDTime(t *testing.T) {
	gen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	window := -3600
	author := &model.User{ID: "author", MakeNotesHiddenBefore: &window}
	boost := func(target *model.Note) *model.Note {
		return &model.Note{ID: gen.Generate(time.Now()), UserID: "booster", RenoteID: &target.ID, Renote: target, Visibility: model.NoteVisibilityPublic}
	}
	fresh := &model.Note{ID: gen.Generate(time.Now()), UserID: "author", User: author, Text: strp("muted"), Visibility: model.NoteVisibilityPublic}
	old := &model.Note{ID: gen.Generate(time.Now().Add(-2 * time.Hour)), UserID: "author", User: author, Text: strp("muted"), Visibility: model.NoteVisibilityPublic}
	assert.True(t, hardMuted(t, `[["muted"]]`, "viewer", boost(fresh), gen), "control: a fresh renoted note is judged")
	assert.False(t, hardMuted(t, `[["muted"]]`, "viewer", boost(old), gen))
}
