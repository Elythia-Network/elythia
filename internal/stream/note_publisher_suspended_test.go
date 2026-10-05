package stream

import (
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// #2624: 凍結ユーザーが絡む note を WebSocket へ publish しない。
//
// gate を PublishNote に置いているので、fanout (home/local/global/userList/
// channel/hashtag/roleTimeline) と antenna の**全 publish 経路**がまとめて
// 塞がる。Redis の timeline list には従来どおり積まれるため、凍結を解除すれば
// 取得時のフィルタが判断して普通に出る。

func newSuspendedTestPublisher(t *testing.T) (*NotePublisher, *stubPublisher, id.Generator) {
	t.Helper()
	pub := &stubPublisher{}
	idGen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	return NewNotePublisher(pub, idGen), pub, idGen
}

func TestPublishNote_SuspendedAuthorIsNotPublished(t *testing.T) {
	np, pub, idGen := newSuspendedTestPublisher(t)

	n := &model.Note{ID: idGen.Generate(time.Now()), UserID: "u1", Visibility: model.NoteVisibilityPublic}
	np.PublishNote("homeTimeline:u1", n, &model.User{ID: "u1", IsSuspended: true})

	assert.Empty(t, pub.topics, "凍結ユーザーの note は publish しない")
}

func TestPublishNote_SuspendedRenoteTargetIsNotPublished(t *testing.T) {
	np, pub, idGen := newSuspendedTestPublisher(t)

	targetID := "target_note"
	n := &model.Note{
		ID: idGen.Generate(time.Now()), UserID: "u1", Visibility: model.NoteVisibilityPublic,
		RenoteID: &targetID,
		Renote:   &model.Note{ID: targetID, UserID: "x", User: &model.User{ID: "x", IsSuspended: true}},
	}
	np.PublishNote("homeTimeline:u1", n, &model.User{ID: "u1"})

	assert.Empty(t, pub.topics, "凍結ユーザーへのリノートは publish しない")
}

func TestPublishNote_SuspendedReplyTargetIsNotPublished(t *testing.T) {
	np, pub, idGen := newSuspendedTestPublisher(t)

	targetID := "target_note"
	n := &model.Note{
		ID: idGen.Generate(time.Now()), UserID: "u1", Visibility: model.NoteVisibilityPublic,
		ReplyID: &targetID,
		Reply:   &model.Note{ID: targetID, UserID: "x", User: &model.User{ID: "x", IsSuspended: true}},
	}
	np.PublishNote("homeTimeline:u1", n, &model.User{ID: "u1"})

	assert.Empty(t, pub.topics, "凍結ユーザーへの返信は publish しない")
}

// gate は topic に依存しない。channel / hashtag / antenna も同じ関数を通る。
func TestPublishNote_GateAppliesToEveryTopic(t *testing.T) {
	for _, topic := range []string{
		"homeTimeline:u1", "localTimeline", "globalTimeline",
		"userListTimeline:l1", "channel:c1", "hashtag:tag", "roleTimeline:r1", "antennaStream:a1",
	} {
		np, pub, idGen := newSuspendedTestPublisher(t)
		n := &model.Note{ID: idGen.Generate(time.Now()), UserID: "u1", Visibility: model.NoteVisibilityPublic}
		np.PublishNote(topic, n, &model.User{ID: "u1", IsSuspended: true})
		assert.Empty(t, pub.topics, "topic %q でも gate が効く", topic)
	}
}

func TestPublishNote_ActiveAuthorIsPublished(t *testing.T) {
	np, pub, idGen := newSuspendedTestPublisher(t)

	targetID := "target_note"
	n := &model.Note{
		ID: idGen.Generate(time.Now()), UserID: "u1", Visibility: model.NoteVisibilityPublic,
		RenoteID: &targetID,
		Renote:   &model.Note{ID: targetID, UserID: "x", User: &model.User{ID: "x"}},
	}
	np.PublishNote("homeTimeline:u1", n, &model.User{ID: "u1"})

	require.Len(t, pub.topics, 1, "凍結していなければ従来どおり publish する")
	assert.Equal(t, "homeTimeline:u1", pub.topics[0])
}

// relation が未取得なら判定できないので素通しする。SQL 側の
// `"renoteUserId" IS NULL OR NOT EXISTS (...)` も user 行が無ければ通すため、
// 削除済みユーザーの扱いが両経路で揃う。
func TestPublishNote_UnloadedRelationIsPublished(t *testing.T) {
	np, pub, idGen := newSuspendedTestPublisher(t)

	targetID := "target_note"
	targetUser := "x"
	n := &model.Note{
		ID: idGen.Generate(time.Now()), UserID: "u1", Visibility: model.NoteVisibilityPublic,
		RenoteID:     &targetID,
		RenoteUserID: &targetUser,
		// Renote は preload されていない
	}
	np.PublishNote("homeTimeline:u1", n, &model.User{ID: "u1"})

	require.Len(t, pub.topics, 1, "relation 未取得なら判定せず素通しする")
}
