package notification

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/model"
)

func listTypes(t *testing.T, svc *Service, userID string) []string {
	t.Helper()
	ns, err := svc.List(context.Background(), userID, "", "", 100, nil, nil)
	require.NoError(t, err)
	var out []string
	for _, n := range ns {
		out = append(out, n.NotifierID+"/"+n.NoteID+"/"+string(n.Type))
	}
	return out
}

// #3201: 取り消したリアクションの通知を消す。
func TestHook_OnReactionRemoved(t *testing.T) {
	h, svc, repo := newTestHook(t)
	addLocalUser(repo, "alice", "alice")
	addLocalUser(repo, "bob", "bob")
	h.OnReactionCreated("alice", "bob", "n1", "👍")
	h.OnReactionCreated("alice", "bob", "n2", "👍")
	require.Len(t, listTypes(t, svc, "alice"), 2)

	h.OnReactionRemoved("alice", "bob", "n1")
	assert.Equal(t, []string{"bob/n2/reaction"}, listTypes(t, svc, "alice"))
}

func renote(id, author, target string, quote bool) *model.Note {
	targetNote := id + "-target"
	n := &model.Note{ID: id, UserID: author, RenoteID: &targetNote, RenoteUserID: &target}
	if quote {
		text := "quote"
		n.Text = &text
	}
	return n
}

// #3201: 取り消したリノート (と削除した引用) の通知を消す。一覧からは read 時に落ちるが、
// stream に残ると未読件数に数えられる。
func TestHook_OnNoteDeleted_RemovesRenoteAndQuoteNotifications(t *testing.T) {
	h, svc, _ := newTestHook(t)
	ctx := context.Background()
	for _, in := range []CreateInput{
		{NotifieeID: "alice", NotifierID: "bob", NoteID: "rn", Type: TypeRenote},
		{NotifieeID: "alice", NotifierID: "bob", NoteID: "q", Type: TypeQuote},
		{NotifieeID: "alice", NotifierID: "bob", NoteID: "q", Type: TypeMention},
		{NotifieeID: "alice", NotifierID: "bob", NoteID: "other", Type: TypeRenote},
	} {
		_, err := svc.Create(ctx, in)
		require.NoError(t, err)
	}

	h.OnNoteDeleted(renote("rn", "bob", "alice", false))
	h.OnNoteDeleted(renote("q", "bob", "alice", true))
	assert.ElementsMatch(t, []string{"bob/q/mention", "bob/other/renote"}, listTypes(t, svc, "alice"))

	n, err := svc.UnreadCount(ctx, "alice")
	require.NoError(t, err)
	assert.EqualValues(t, 2, n, "deleted renotes no longer count as unread")
}

// 通知を作っていないノート (リノートでない / 自分のノートのリノート) では何もしない。
func TestHook_OnNoteDeleted_Noops(t *testing.T) {
	h, svc, _ := newTestHook(t)
	_, err := svc.Create(context.Background(), CreateInput{NotifieeID: "bob", NotifierID: "bob2", NoteID: "self", Type: TypeRenote})
	require.NoError(t, err)

	h.OnNoteDeleted(nil)
	h.OnNoteDeleted(&model.Note{ID: "plain", UserID: "bob"})
	h.OnNoteDeleted(renote("self", "bob", "bob", false))
	assert.Len(t, listTypes(t, svc, "bob"), 1)

	var nilSvc Hook
	nilSvc.OnNoteDeleted(renote("rn", "bob", "alice", false))
	nilSvc.OnReactionRemoved("alice", "bob", "n1")

	broken := NewHook(NewService(closedClient(t), idGen, ""), nil)
	broken.OnNoteDeleted(renote("rn", "bob", "alice", false))
	broken.OnReactionRemoved("alice", "bob", "n1")
}
