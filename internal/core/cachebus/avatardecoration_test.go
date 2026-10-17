package cachebus

import (
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/core/avatardecoration"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAvatarDecorations_ReachOtherProcess runs two "processes" on one Redis
// and one DB and pins that a decoration (from the catalog, or from a local
// emoji) changed through the admin of one process is resolved at once by the
// other, whose caches were already filled for the 30-second TTL (#3664).
func TestAvatarDecorations_ReachOtherProcess(t *testing.T) {
	const decoID, emojiID = "cbdeco3664", "cbdecoemoji3664"
	cleanup := func() {
		testDB.Exec(`DELETE FROM avatar_decoration WHERE id = ?`, decoID)
		testDB.Exec(`DELETE FROM emoji WHERE id = ?`, emojiID)
	}
	cleanup()
	t.Cleanup(cleanup)

	busA, busB := twoBuses(t, "cbdeco3664:")
	catalogA := avatardecoration.NewResolver(repository.NewAvatarDecorationRepository(testDB))
	catalogB := avatardecoration.NewResolver(repository.NewAvatarDecorationRepository(testDB))
	emojiA := avatardecoration.NewEmojiResolver(repository.NewEmojiRepository(testDB))
	emojiB := avatardecoration.NewEmojiResolver(repository.NewEmojiRepository(testDB))
	t.Cleanup(ConnectAvatarDecorations(catalogA, emojiA, busA, "A"))
	t.Cleanup(ConnectAvatarDecorations(catalogB, emojiB, busB, "B"))
	waitSubscribed(t, busA, busB)

	_, ok := catalogB.LookupURL(decoID)
	require.False(t, ok, "B caches the catalog without the decoration")
	_, _, ok = emojiB.LookupEmojiDecoration(emojiID)
	require.False(t, ok, "B caches the emoji map without the emoji")

	// A の管理画面がデコレーションを作り、自プロセスのキャッシュを捨てる。
	require.NoError(t, testDB.Create(&model.AvatarDecoration{ID: decoID, URL: "https://local.example/deco.png", Name: "deco"}).Error)
	catalogA.Invalidate()
	require.Eventually(t, func() bool {
		_, ok := catalogB.LookupURL(decoID)
		return ok
	}, 5*time.Second, 10*time.Millisecond, "process B must resolve the decoration created in process A")

	require.NoError(t, testDB.Create(&model.Emoji{ID: emojiID, Name: "cbdeco3664", OriginalURL: "https://local.example/e.png"}).Error)
	emojiA.Invalidate()
	require.Eventually(t, func() bool {
		_, _, ok := emojiB.LookupEmojiDecoration(emojiID)
		return ok
	}, 5*time.Second, 10*time.Millisecond, "process B must resolve the emoji decoration created in process A")
}

func TestConnectAvatarDecorations_Wire(t *testing.T) {
	decoRepo := testutil.NewMockAvatarDecorationRepository()
	catalog := avatardecoration.NewResolver(decoRepo)
	emojiSrc := &staticEmojiSource{}
	emoji := avatardecoration.NewEmojiResolver(emojiSrc)
	bus := newLoopBus()
	stop := ConnectAvatarDecorations(catalog, emoji, bus, "me")
	defer stop()

	catalog.Invalidate()
	emoji.Invalidate()
	assert.Equal(t, []Message[AvatarDecorationInvalidation]{
		{From: "me", Inv: AvatarDecorationInvalidation{Catalog: true}},
		{From: "me", Inv: AvatarDecorationInvalidation{Emoji: true}},
	}, messages[AvatarDecorationInvalidation](t, bus, TopicAvatarDecoration))

	// 両方を埋めてから、行を足す (キャッシュから返っているうちは見えない)。
	fill := func() {
		catalog.LookupURL("x")
		emoji.LookupEmojiDecoration("x")
	}
	seen := func() (bool, bool) {
		_, okC := catalog.LookupURL("d1")
		_, _, okE := emoji.LookupEmojiDecoration("e1")
		return okC, okE
	}
	fill()
	decoRepo.Decorations["d1"] = &model.AvatarDecoration{ID: "d1", URL: "https://e/d1.png"}
	emojiSrc.rows = []*model.Emoji{{ID: "e1", Name: "e1", OriginalURL: "https://e/e1.png"}}

	// 自分の通知は受け流す。
	deliverMessage(t, bus, TopicAvatarDecoration, Message[AvatarDecorationInvalidation]{From: "me", Inv: AvatarDecorationInvalidation{Catalog: true, Emoji: true}})
	okC, okE := seen()
	assert.False(t, okC)
	assert.False(t, okE)
	// 他のプロセスの通知は、名指しされた方だけ捨てる。
	deliverMessage(t, bus, TopicAvatarDecoration, Message[AvatarDecorationInvalidation]{From: "other", Inv: AvatarDecorationInvalidation{Catalog: true}})
	okC, okE = seen()
	assert.True(t, okC)
	assert.False(t, okE)
	deliverMessage(t, bus, TopicAvatarDecoration, Message[AvatarDecorationInvalidation]{From: "other", Inv: AvatarDecorationInvalidation{Emoji: true}})
	_, okE = seen()
	assert.True(t, okE)

	// 読めない通知は両方を捨てる。
	delete(decoRepo.Decorations, "d1")
	emojiSrc.rows = nil
	okC, okE = seen()
	require.True(t, okC, "cached")
	require.True(t, okE, "cached")
	bus.deliver(t, TopicAvatarDecoration, []byte("{not json"))
	okC, okE = seen()
	assert.False(t, okC)
	assert.False(t, okE)
}

// TestConnectAvatarDecorations_OneResolver wires only one of the two
// resolvers, as a process without the other would; the other kind of message
// must be a no-op instead of a nil dereference.
func TestConnectAvatarDecorations_OneResolver(t *testing.T) {
	bus := newLoopBus()
	stop := ConnectAvatarDecorations(avatardecoration.NewResolver(testutil.NewMockAvatarDecorationRepository()), nil, bus, "me")
	deliverMessage(t, bus, TopicAvatarDecoration, Message[AvatarDecorationInvalidation]{From: "other", Inv: AvatarDecorationInvalidation{Catalog: true, Emoji: true}})
	stop()

	bus = newLoopBus()
	stop = ConnectAvatarDecorations(nil, avatardecoration.NewEmojiResolver(&staticEmojiSource{}), bus, "me")
	deliverMessage(t, bus, TopicAvatarDecoration, Message[AvatarDecorationInvalidation]{From: "other", Inv: AvatarDecorationInvalidation{Catalog: true, Emoji: true}})
	stop()
}

func TestConnectAvatarDecorations_NilIsNoop(t *testing.T) {
	ConnectAvatarDecorations(nil, nil, newLoopBus(), "x")()
	ConnectAvatarDecorations(avatardecoration.NewResolver(nil), nil, nil, "x")()
}

type staticEmojiSource struct{ rows []*model.Emoji }

func (s *staticEmojiSource) ListLocal() ([]*model.Emoji, error) { return s.rows, nil }
