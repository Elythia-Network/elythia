package notification

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func streamTypes(t *testing.T, svc *Service, userID string) []string {
	t.Helper()
	res, err := testRedis.Client.XRange(context.Background(), svc.streamKey(userID), "-", "+").Result()
	require.NoError(t, err)
	var out []string
	for _, msg := range res {
		out = append(out, msg.Values["data"].(string))
	}
	return out
}

// TestDeleteByNote_RemovesOnlyTheMatchingNotification pins every condition:
// notifiee, notifier, note and type all have to match (#3201).
func TestDeleteByNote_RemovesOnlyTheMatchingNotification(t *testing.T) {
	svc := newTestSvc(t)
	ctx := context.Background()
	mk := func(notifiee, notifier, note string, typ Type) {
		_, err := svc.Create(ctx, CreateInput{NotifieeID: notifiee, NotifierID: notifier, NoteID: note, Type: typ, Reaction: "👍"})
		require.NoError(t, err)
	}
	mk("author", "alice", "n1", TypeReaction) // 消える
	mk("author", "alice", "n1", TypeReaction) // 同じ組が重複していても全部消える
	mk("author", "bob", "n1", TypeReaction)   // 通知者が違う
	mk("author", "alice", "n2", TypeReaction) // ノートが違う
	mk("author", "alice", "n1", TypeReply)    // 型が違う
	mk("other", "alice", "n1", TypeReaction)  // 受信者が違う

	require.NoError(t, svc.DeleteByNote(ctx, "author", "alice", "n1", TypeReaction))

	left, err := svc.List(ctx, "author", "", "", 100, nil, nil)
	require.NoError(t, err)
	var got []string
	for _, n := range left {
		got = append(got, n.NotifierID+"/"+n.NoteID+"/"+string(n.Type))
	}
	assert.ElementsMatch(t, []string{"bob/n1/reaction", "alice/n2/reaction", "alice/n1/reply"}, got)
	assert.Len(t, streamTypes(t, svc, "other"), 1)
}

func TestDeleteByNote_SeveralTypes(t *testing.T) {
	svc := newTestSvc(t)
	ctx := context.Background()
	for _, typ := range []Type{TypeRenote, TypeQuote, TypeMention} {
		_, err := svc.Create(ctx, CreateInput{NotifieeID: "author", NotifierID: "alice", NoteID: "rn", Type: typ})
		require.NoError(t, err)
	}
	require.NoError(t, svc.DeleteByNote(ctx, "author", "alice", "rn", TypeRenote, TypeQuote))
	left, err := svc.List(ctx, "author", "", "", 100, nil, nil)
	require.NoError(t, err)
	require.Len(t, left, 1)
	assert.Equal(t, TypeMention, left[0].Type)
}

// 空の引数は何も消さない (空の notifier / note で全件に一致させない)。
func TestDeleteByNote_EmptyArgumentsAreNoop(t *testing.T) {
	svc := newTestSvc(t)
	ctx := context.Background()
	_, err := svc.Create(ctx, CreateInput{NotifieeID: "author", Type: TypeReaction})
	require.NoError(t, err)
	require.NoError(t, svc.DeleteByNote(ctx, "author", "", "", TypeReaction))
	require.NoError(t, svc.DeleteByNote(ctx, "author", "x", "n"))
	require.NoError(t, svc.DeleteByNote(ctx, "", "x", "n", TypeReaction))
	require.NoError(t, svc.DeleteByNote(ctx, "author", "x", "", TypeReaction))
	assert.Len(t, streamTypes(t, svc, "author"), 1)

	require.NoError(t, svc.DeleteByNote(ctx, "nobody", "x", "n", TypeReaction), "an empty stream is fine")
	broken := NewService(closedClient(t), idGen, "")
	assert.Error(t, broken.DeleteByNote(ctx, "author", "x", "n", TypeReaction))
}

// TestDeletedNotificationIsNotPublishedLate: unreadNotification と Web Push は 2 秒
// 遅れて出る。その間に取り消して消した通知は送らない (#3201)。送ると一覧に無い
// 通知の push が届き、バッジだけが +1 される。
func TestDeletedNotificationIsNotPublishedLate(t *testing.T) {
	svc := newTestSvc(t)
	svc.SetUnreadPublishDelay(500 * time.Millisecond)
	pub := &stubMainPublisher{}
	svc.SetMainStreamPublisher(pub)
	ctx := context.Background()
	var mu sync.Mutex
	pushed := map[string]int{}
	push := func(n *Notification) { mu.Lock(); pushed[n.NoteID]++; mu.Unlock() }

	_, err := svc.CreateWithPush(ctx, CreateInput{NotifieeID: "alice", NotifierID: "bob", NoteID: "gone", Type: TypeReaction, Reaction: "👍"}, push)
	require.NoError(t, err)
	_, err = svc.CreateWithPush(ctx, CreateInput{NotifieeID: "alice", NotifierID: "bob", NoteID: "kept", Type: TypeReaction, Reaction: "👍"}, push)
	require.NoError(t, err)
	require.NoError(t, svc.DeleteByNote(ctx, "alice", "bob", "gone", TypeReaction))

	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return pushed["kept"] == 1
	}, 5*time.Second, 10*time.Millisecond, "the notification that still exists is published")
	time.Sleep(600 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	assert.Zero(t, pushed["gone"], "no Web Push for a withdrawn reaction")
	pub.mu.Lock()
	defer pub.mu.Unlock()
	assert.Len(t, pub.calls, 1, "one unreadNotification: only for the kept notification")
}
