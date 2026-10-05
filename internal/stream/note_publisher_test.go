package stream

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	coredrive "github.com/elythia-network/elythia/internal/core/drive"
	corenotification "github.com/elythia-network/elythia/internal/core/notification"
	"github.com/elythia-network/elythia/internal/entity"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
)

// stubPublisher captures Publish calls.
type stubPublisher struct {
	mu       sync.Mutex
	topics   []string
	payloads []any
	err      error
}

func (s *stubPublisher) Publish(_ context.Context, channel string, payload any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	s.topics = append(s.topics, channel)
	s.payloads = append(s.payloads, payload)
	return nil
}

func TestNotePublisher_PublishesPackedNote(t *testing.T) {
	pub := &stubPublisher{}
	idGen, _ := id.NewGenerator("aidx")
	np := NewNotePublisher(pub, idGen)

	text := "hello"
	n := &model.Note{
		ID:         idGen.Generate(time.Now()),
		UserID:     "u1",
		Text:       &text,
		Visibility: model.NoteVisibilityPublic,
	}
	user := &model.User{ID: "u1", Username: "alice"}
	np.PublishNote("homeTimeline:u1", n, user)

	require.Len(t, pub.topics, 1)
	assert.Equal(t, "homeTimeline:u1", pub.topics[0])
}

// stubInstanceLookup implements entity.InstanceLookup for tests.
type stubInstanceLookup struct {
	byHost map[string]*model.Instance
}

func (s *stubInstanceLookup) FindManyByHosts(hosts []string) ([]*model.Instance, error) {
	out := make([]*model.Instance, 0, len(hosts))
	for _, h := range hosts {
		if inst, ok := s.byHost[h]; ok {
			out = append(out, inst)
		}
	}
	return out, nil
}

// stubBufferedReactionsReader is a test double for entity.BufferedReactionsReader.
type stubBufferedReactionsReader struct {
	deltas map[string]map[string]int64
}

func (s *stubBufferedReactionsReader) GetBufferedMany(_ context.Context, ids []string) (map[string]map[string]int64, error) {
	out := make(map[string]map[string]int64)
	for _, id := range ids {
		if d, ok := s.deltas[id]; ok {
			out[id] = d
		}
	}
	return out, nil
}

// reactionReader が配線されていれば streaming payload も buffered deltas
// を merge してから publish する (#647)。配線が無いと publish 直後の
// reaction count が flush まで反映されない。
func TestNotePublisher_MergesBufferedReactions(t *testing.T) {
	pub := &stubPublisher{}
	idGen, _ := id.NewGenerator("aidx")
	np := NewNotePublisher(pub, idGen)
	noteID := idGen.Generate(time.Now())
	np.SetReactionReader(&stubBufferedReactionsReader{deltas: map[string]map[string]int64{
		noteID: {":heart@.:": 7},
	}})

	n := &model.Note{
		ID:         noteID,
		UserID:     "u1",
		Visibility: model.NoteVisibilityPublic,
		Reactions:  datatypes.JSON([]byte(`{":existing@.:": 1}`)),
	}
	author := &model.User{ID: "u1", Username: "alice"}
	np.PublishNote("homeTimeline:u1", n, author)

	require.Len(t, pub.payloads, 1)
	raw := pub.payloads[0].(json.RawMessage)
	var body map[string]any
	require.NoError(t, json.Unmarshal(raw, &body))
	reactionCount, ok := body["reactionCount"].(float64)
	require.True(t, ok)
	assert.Equal(t, float64(8), reactionCount, "DB(1) + buffered(7) = 8 が streaming payload に乗る")
	reactions := body["reactions"].(map[string]any)
	assert.Equal(t, float64(1), reactions[":existing@.:"])
	assert.Equal(t, float64(7), reactions[":heart@.:"])
}

// InstanceLookup が配線されていれば author の Instance が streaming payload
// に乗る (#416 InstanceTicker テーマカラー反映)。
func TestNotePublisher_PopulatesAuthorInstance(t *testing.T) {
	pub := &stubPublisher{}
	idGen, _ := id.NewGenerator("aidx")
	np := NewNotePublisher(pub, idGen)
	themeColor := "#deadbe"
	host := "remote.example"
	np.SetInstanceLookup(&stubInstanceLookup{byHost: map[string]*model.Instance{
		host: {Host: host, ThemeColor: &themeColor},
	}})

	n := &model.Note{
		ID:         idGen.Generate(time.Now()),
		UserID:     "u_remote",
		UserHost:   &host,
		Visibility: model.NoteVisibilityPublic,
	}
	author := &model.User{ID: "u_remote", Username: "remote", Host: &host}
	np.PublishNote("globalTimeline", n, author)

	require.Len(t, pub.payloads, 1)
	raw := pub.payloads[0].(json.RawMessage)
	var body map[string]any
	require.NoError(t, json.Unmarshal(raw, &body))
	user := body["user"].(map[string]any)
	inst, ok := user["instance"].(map[string]any)
	require.True(t, ok, "user.instance must be present on streaming payload")
	assert.Equal(t, themeColor, inst["themeColor"])
}

// FieldResolver が配線されている場合、ストリーミング payload に Files が
// 乗る (#460/#461 follow-up)。配線無しでは files が空配列のままなので、
// frontend が画像を表示できずリロードが必要になる。
func TestNotePublisher_FieldResolverPopulatesFiles(t *testing.T) {
	pub := &stubPublisher{}
	idGen, _ := id.NewGenerator("aidx")
	np := NewNotePublisher(pub, idGen)

	driveRepo := testutil.NewMockDriveFileRepository()
	noteID := idGen.Generate(time.Now())
	fileID := idGen.Generate(time.Now())
	driveRepo.Files[fileID] = &model.DriveFile{
		ID:   fileID,
		Name: "remote.png",
		Type: "image/png",
		URL:  "https://r/remote.png",
	}
	resolver := entity.NewNoteFieldResolver(driveRepo, nil, nil, nil, nil, idGen)
	np.SetFieldResolver(resolver)

	n := &model.Note{
		ID:         noteID,
		UserID:     "u1",
		FileIDs:    []string{fileID},
		Visibility: model.NoteVisibilityPublic,
	}
	np.PublishNote("homeTimeline:u1", n, &model.User{ID: "u1", Username: "alice"})

	require.Len(t, pub.payloads, 1)
	raw := pub.payloads[0].(json.RawMessage)
	var body map[string]any
	require.NoError(t, json.Unmarshal(raw, &body))
	files, ok := body["files"].([]any)
	require.True(t, ok, "files key must be present in streaming payload")
	require.Len(t, files, 1, "files array must contain the resolved drive file")
	first := files[0].(map[string]any)
	assert.Equal(t, fileID, first["id"])
	// PackDriveFile fallback で thumbnailUrl が url にフォールバックする
	// ことも合わせて確認する (#460)。
	assert.Equal(t, "https://r/remote.png", first["thumbnailUrl"])
}

// n.Renote が preload 済みならストリーミング payload に renote オブジェクトが
// 乗ることを確認する (#416)。
func TestNotePublisher_PublishesEmbeddedRenote(t *testing.T) {
	pub := &stubPublisher{}
	idGen, _ := id.NewGenerator("aidx")
	np := NewNotePublisher(pub, idGen)

	renoteID := idGen.Generate(time.Now())
	noteID := idGen.Generate(time.Now())
	renoteText := "original"
	n := &model.Note{
		ID:         noteID,
		UserID:     "u1",
		RenoteID:   &renoteID,
		Visibility: model.NoteVisibilityPublic,
		Renote: &model.Note{
			ID:         renoteID,
			UserID:     "u2",
			Text:       &renoteText,
			Visibility: model.NoteVisibilityPublic,
			User:       &model.User{ID: "u2", Username: "author2"},
		},
	}
	np.PublishNote("homeTimeline:u1", n, &model.User{ID: "u1", Username: "alice"})

	require.Len(t, pub.payloads, 1)
	raw, ok := pub.payloads[0].(json.RawMessage)
	require.True(t, ok)
	var body map[string]any
	require.NoError(t, json.Unmarshal(raw, &body))

	renote, ok := body["renote"].(map[string]any)
	require.True(t, ok, "renote must be embedded in streaming payload")
	assert.Equal(t, renoteID, renote["id"])
	assert.Equal(t, renoteText, renote["text"])
}

func TestNotePublisher_NilPubIsNoOp(t *testing.T) {
	idGen, _ := id.NewGenerator("aidx")
	np := NewNotePublisher(nil, idGen)
	np.PublishNote("topic", &model.Note{}, &model.User{})
}

func TestNotePublisher_NilNoteIsNoOp(t *testing.T) {
	pub := &stubPublisher{}
	idGen, _ := id.NewGenerator("aidx")
	np := NewNotePublisher(pub, idGen)
	np.PublishNote("topic", nil, &model.User{})
	assert.Empty(t, pub.topics)
}

func TestNotePublisher_NilAuthorIsNoOp(t *testing.T) {
	pub := &stubPublisher{}
	idGen, _ := id.NewGenerator("aidx")
	np := NewNotePublisher(pub, idGen)
	np.PublishNote("topic", &model.Note{}, nil)
	assert.Empty(t, pub.topics)
}

func TestNotePublisher_MarshalErrorIsLoggedNotPropagated(t *testing.T) {
	pub := &stubPublisher{}
	idGen, _ := id.NewGenerator("aidx")
	np := NewNotePublisher(pub, idGen)
	// datatypes.JSON は invalid JSON で MarshalJSON が失敗する
	n := &model.Note{
		ID:        idGen.Generate(time.Now()),
		UserID:    "u1",
		Reactions: []byte("{not json"),
	}
	np.PublishNote("topic", n, &model.User{ID: "u1"})
	// publish 自体は呼ばれない (Marshal 失敗で early return)
	assert.Empty(t, pub.topics)
}

func TestNotePublisher_PublishErrorIsLoggedNotPropagated(t *testing.T) {
	pub := &stubPublisher{err: errors.New("redis down")}
	idGen, _ := id.NewGenerator("aidx")
	np := NewNotePublisher(pub, idGen)

	n := &model.Note{ID: idGen.Generate(time.Now()), UserID: "u1"}
	np.PublishNote("topic", n, &model.User{ID: "u1"})
	// Publish errored but the call still returned without panic.
}

// --- NotificationPublisher --------------------------------------------------

func TestNotificationPublisher_PublishesPayload(t *testing.T) {
	pub := &stubPublisher{}
	np := NewNotificationPublisher(pub)
	n := &corenotification.Notification{ID: "n1", Type: corenotification.TypeFollow, NotifierID: "u2"}
	np.PublishNotification("alice", n)
	require.Len(t, pub.topics, 1)
	assert.Equal(t, "notifications:alice", pub.topics[0])
}

// stub repos for NotificationPublisher.SetRepos
type stubNotifUserRepo struct {
	user *model.User
	err  error
}

func (s *stubNotifUserRepo) FindByID(_ string) (*model.User, error) {
	return s.user, s.err
}

type stubNotifNoteRepo struct {
	note *model.Note
	err  error
}

func (s *stubNotifNoteRepo) FindByIDWithRelations(_ string) (*model.Note, error) {
	return s.note, s.err
}

func TestNotificationPublisher_WithRepos_PacksUserAndNote(t *testing.T) {
	pub := &stubPublisher{}
	np := NewNotificationPublisher(pub)
	idGen, _ := id.NewGenerator("aidx")
	// #1471: visibility gate が入ったので test data も visibility を明示。
	// public は誰でも見られるので note embed が残ることを確認する path。
	np.SetRepos(
		&stubNotifUserRepo{user: &model.User{ID: "u2", Username: "bob"}},
		&stubNotifNoteRepo{note: &model.Note{ID: "note1", UserID: "u2", Visibility: model.NoteVisibilityPublic}},
		idGen,
	)
	n := &corenotification.Notification{
		ID:         idGen.Generate(time.Now()),
		Type:       corenotification.TypeReply,
		NotifierID: "u2",
		NoteID:     "note1",
	}
	np.PublishNotification("alice", n)
	require.Len(t, pub.payloads, 1)
	// 型がjson.RawMessageでTS互換のkey(userId / user / note)を含むことを確認。
	raw, ok := pub.payloads[0].(json.RawMessage)
	require.True(t, ok)
	var body map[string]any
	require.NoError(t, json.Unmarshal(raw, &body))
	assert.Equal(t, "u2", body["userId"])
	_, hasUser := body["user"]
	assert.True(t, hasUser)
	_, hasNote := body["note"]
	assert.True(t, hasNote)
}

// #1559 roleAssigned: SetRoleLookup で配線した lookup が role を packed して
// streaming payload に embed する。
func TestNotificationPublisher_RoleAssigned_PacksRole(t *testing.T) {
	pub := &stubPublisher{}
	np := NewNotificationPublisher(pub)
	idGen, _ := id.NewGenerator("aidx")
	np.SetRepos(&stubNotifUserRepo{user: &model.User{ID: "alice", Username: "alice"}}, &stubNotifNoteRepo{}, idGen)
	np.SetRoleLookup(func(roleID string) (map[string]any, bool) {
		return map[string]any{"id": roleID, "name": "VIP"}, true
	})
	n := &corenotification.Notification{
		ID:    idGen.Generate(time.Now()),
		Type:  corenotification.TypeRoleAssigned,
		Extra: map[string]any{"roleId": "r1"},
	}
	np.PublishNotification("alice", n)
	require.Len(t, pub.payloads, 1)
	raw, ok := pub.payloads[0].(json.RawMessage)
	require.True(t, ok)
	var body map[string]any
	require.NoError(t, json.Unmarshal(raw, &body))
	assert.Equal(t, "roleAssigned", body["type"])
	role, ok := body["role"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "VIP", role["name"])
}

// role 削除済 (lookup が false) の roleAssigned は stream に publish しない
// (Pack が true nil を返して downstream の nil guard で drop される、#1559)。
func TestNotificationPublisher_RoleAssigned_DroppedWhenRoleDeleted(t *testing.T) {
	pub := &stubPublisher{}
	np := NewNotificationPublisher(pub)
	idGen, _ := id.NewGenerator("aidx")
	np.SetRepos(&stubNotifUserRepo{user: &model.User{ID: "alice", Username: "alice"}}, &stubNotifNoteRepo{}, idGen)
	np.SetRoleLookup(func(string) (map[string]any, bool) { return nil, false })
	n := &corenotification.Notification{
		ID:    idGen.Generate(time.Now()),
		Type:  corenotification.TypeRoleAssigned,
		Extra: map[string]any{"roleId": "gone"},
	}
	assert.Nil(t, np.Pack("alice", n), "Pack は drop 時 true nil を返す")
	np.PublishNotification("alice", n)
	assert.Empty(t, pub.payloads, "削除済 role の roleAssigned は publish しない")
}

// #1559 chatRoomInvitationReceived: SetChatInvitationLookup で配線した lookup が
// invitation を pack して streaming payload に embed する。viewer は notifiee。
func TestNotificationPublisher_ChatInvitation_PacksInvitation(t *testing.T) {
	pub := &stubPublisher{}
	np := NewNotificationPublisher(pub)
	idGen, _ := id.NewGenerator("aidx")
	np.SetRepos(&stubNotifUserRepo{user: &model.User{ID: "inviter", Username: "inviter"}}, &stubNotifNoteRepo{}, idGen)
	np.SetChatInvitationLookup(func(invID, viewerID string) (map[string]any, bool) {
		assert.Equal(t, "invitee", viewerID, "viewer は notifiee")
		return map[string]any{"id": invID, "roomId": "room1"}, true
	})
	n := &corenotification.Notification{
		ID:         idGen.Generate(time.Now()),
		Type:       corenotification.TypeChatRoomInvitationReceived,
		NotifierID: "inviter",
		Extra:      map[string]any{"invitationId": "inv1"},
	}
	np.PublishNotification("invitee", n)
	require.Len(t, pub.payloads, 1)
	raw, ok := pub.payloads[0].(json.RawMessage)
	require.True(t, ok)
	var body map[string]any
	require.NoError(t, json.Unmarshal(raw, &body))
	assert.Equal(t, "chatRoomInvitationReceived", body["type"])
	inv, ok := body["invitation"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "room1", inv["roomId"])
}

func TestNotificationPublisher_Pack_NilNotification(t *testing.T) {
	np := NewNotificationPublisher(nil)
	assert.Nil(t, np.Pack("alice", nil))
}

func TestNotificationPublisher_Pack_WithoutRepos_ReturnsRawNotification(t *testing.T) {
	np := NewNotificationPublisher(nil)
	n := &corenotification.Notification{ID: "x"}
	// SetReposを呼ばなければ raw *Notificationがそのまま返る。
	assert.Equal(t, n, np.Pack("alice", n))
}

func TestNotificationPublisher_Pack_WithRepos_ReturnsPackedMap(t *testing.T) {
	np := NewNotificationPublisher(nil)
	idGen, _ := id.NewGenerator("aidx")
	np.SetRepos(
		&stubNotifUserRepo{user: &model.User{ID: "u_b", Username: "b"}},
		&stubNotifNoteRepo{err: errors.New("missing")},
		idGen,
	)
	n := &corenotification.Notification{
		ID:         "x",
		Type:       corenotification.TypeFollow,
		NotifierID: "u_b",
	}
	out := np.Pack("alice", n)
	body, ok := out.(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "u_b", body["userId"])
	// noteRepoがerror返すのでnoteは含まれない
	_, hasNote := body["note"]
	assert.False(t, hasNote)
}

// stubNotifFollowingChecker は CanSeeNote の followers branch を test 用に
// stub する。`followers` map に (follower, followee) を入れておくと
// Exists が true を返す。
type stubNotifFollowingChecker struct {
	followers map[string]map[string]bool
}

func (s *stubNotifFollowingChecker) Exists(followerID, followeeID string) (bool, error) {
	if s == nil || s.followers == nil {
		return false, nil
	}
	return s.followers[followerID][followeeID], nil
}

// #1471 IDOR fix: followers visibility note への mention/reply 通知が
// non-follower notifiee の stream に流れた際、note embed を nil 化して
// 本文を漏らさないこと。
func TestNotificationPublisher_Pack_FollowersNote_NonFollowerNotifiee_DropsNote(t *testing.T) {
	np := NewNotificationPublisher(nil)
	idGen, _ := id.NewGenerator("aidx")
	np.SetRepos(
		&stubNotifUserRepo{user: &model.User{ID: "bob"}},
		&stubNotifNoteRepo{note: &model.Note{
			ID:         "note1",
			UserID:     "bob",
			Visibility: model.NoteVisibilityFollowers,
			Text:       strPtr("secret followers content"),
		}},
		idGen,
	)
	np.SetFollowingChecker(&stubNotifFollowingChecker{followers: map[string]map[string]bool{
		// alice は bob を follow していない
	}})
	n := &corenotification.Notification{
		ID:         "x",
		Type:       corenotification.TypeMention,
		NotifierID: "bob",
		NoteID:     "note1",
	}
	out := np.Pack("alice", n)
	body, ok := out.(map[string]any)
	require.True(t, ok)
	// notifierID は残る (= 通知行は出す) が note は落とす
	assert.Equal(t, "bob", body["userId"])
	assert.Nil(t, body["note"], "non-follower notifiee には followers note の embed を返さない")
}

// follower 関係がある notifiee には従来通り note を embed する (regress
// guard: visibility gate が overshoot して全 follower まで落としていないか)。
func TestNotificationPublisher_Pack_FollowersNote_FollowerNotifiee_KeepsNote(t *testing.T) {
	np := NewNotificationPublisher(nil)
	idGen, _ := id.NewGenerator("aidx")
	np.SetRepos(
		&stubNotifUserRepo{user: &model.User{ID: "bob"}},
		&stubNotifNoteRepo{note: &model.Note{
			ID:         "note1",
			UserID:     "bob",
			Visibility: model.NoteVisibilityFollowers,
		}},
		idGen,
	)
	np.SetFollowingChecker(&stubNotifFollowingChecker{followers: map[string]map[string]bool{
		"alice": {"bob": true}, // alice → bob follow
	}})
	n := &corenotification.Notification{
		ID:         "x",
		Type:       corenotification.TypeReply,
		NotifierID: "bob",
		NoteID:     "note1",
	}
	out := np.Pack("alice", n)
	body, ok := out.(map[string]any)
	require.True(t, ok)
	assert.NotNil(t, body["note"])
}

// specified note は visibleUserIDs に含まれない notifiee には note を
// 落とす (mention 経路で visibleUserIDs 外の user に通知が走る ill-formed
// state での leak 防止)。
func TestNotificationPublisher_Pack_SpecifiedNote_NonRecipient_DropsNote(t *testing.T) {
	np := NewNotificationPublisher(nil)
	idGen, _ := id.NewGenerator("aidx")
	np.SetRepos(
		&stubNotifUserRepo{user: &model.User{ID: "bob"}},
		&stubNotifNoteRepo{note: &model.Note{
			ID:             "note1",
			UserID:         "bob",
			Visibility:     model.NoteVisibilitySpecified,
			VisibleUserIDs: []string{"charlie"}, // alice は含まれない
		}},
		idGen,
	)
	n := &corenotification.Notification{
		ID:         "x",
		Type:       corenotification.TypeMention,
		NotifierID: "bob",
		NoteID:     "note1",
	}
	out := np.Pack("alice", n)
	body, ok := out.(map[string]any)
	require.True(t, ok)
	assert.Nil(t, body["note"], "visibleUserIDs 外の notifiee には specified note embed を返さない")
}

// specified note でも author 本人 / visibleUserIDs 内の notifiee には
// 従来通り note を embed する。
func TestNotificationPublisher_Pack_SpecifiedNote_Recipient_KeepsNote(t *testing.T) {
	np := NewNotificationPublisher(nil)
	idGen, _ := id.NewGenerator("aidx")
	np.SetRepos(
		&stubNotifUserRepo{user: &model.User{ID: "bob"}},
		&stubNotifNoteRepo{note: &model.Note{
			ID:             "note1",
			UserID:         "bob",
			Visibility:     model.NoteVisibilitySpecified,
			VisibleUserIDs: []string{"alice"},
		}},
		idGen,
	)
	n := &corenotification.Notification{
		ID:         "x",
		Type:       corenotification.TypeMention,
		NotifierID: "bob",
		NoteID:     "note1",
	}
	out := np.Pack("alice", n)
	body, ok := out.(map[string]any)
	require.True(t, ok)
	assert.NotNil(t, body["note"])
}

// followingRepo 未配線時は CanSeeNote の semantics (followers branch で
// 本人外不可) に倣って fail-closed。production の router は必ず
// SetFollowingChecker するが、test の partial setup で followers note の
// embed が抜けないことを固定する。
func TestNotificationPublisher_Pack_FollowersNote_NilFollowingChecker_DropsForNonAuthor(t *testing.T) {
	np := NewNotificationPublisher(nil)
	idGen, _ := id.NewGenerator("aidx")
	np.SetRepos(
		&stubNotifUserRepo{user: &model.User{ID: "bob"}},
		&stubNotifNoteRepo{note: &model.Note{
			ID:         "note1",
			UserID:     "bob",
			Visibility: model.NoteVisibilityFollowers,
		}},
		idGen,
	)
	// SetFollowingChecker は呼ばない
	n := &corenotification.Notification{
		ID:         "x",
		Type:       corenotification.TypeReply,
		NotifierID: "bob",
		NoteID:     "note1",
	}
	out := np.Pack("alice", n)
	body, _ := out.(map[string]any)
	require.NotNil(t, body)
	assert.Nil(t, body["note"], "followingRepo 未配線時は followers note を非著者に embed しない")
}

// PublishNotification は内部で Pack(notifieeID, n) を呼んでいるので、
// followers note への non-follower mention で stream payload にも note が
// 漏れないことを end-to-end で固定。
func TestNotificationPublisher_PublishNotification_GatesNoteByVisibility(t *testing.T) {
	pub := &stubPublisher{}
	np := NewNotificationPublisher(pub)
	idGen, _ := id.NewGenerator("aidx")
	np.SetRepos(
		&stubNotifUserRepo{user: &model.User{ID: "bob"}},
		&stubNotifNoteRepo{note: &model.Note{
			ID:         "note1",
			UserID:     "bob",
			Visibility: model.NoteVisibilityFollowers,
			Text:       strPtr("secret"),
		}},
		idGen,
	)
	np.SetFollowingChecker(&stubNotifFollowingChecker{})
	n := &corenotification.Notification{
		ID:         "x",
		Type:       corenotification.TypeMention,
		NotifierID: "bob",
		NoteID:     "note1",
	}
	np.PublishNotification("alice", n)
	require.Len(t, pub.payloads, 1)
	raw, ok := pub.payloads[0].(json.RawMessage)
	require.True(t, ok)
	var body map[string]any
	require.NoError(t, json.Unmarshal(raw, &body))
	assert.Nil(t, body["note"], "stream payload にも本文を流さない")
	// 本文 string が JSON 内に出現しないこと (defensive: 別 field 経由の
	// leak も塞ぐ)。
	assert.NotContains(t, string(raw), "secret")
}

func strPtr(s string) *string { return &s }

func TestNotificationPublisher_NilPubIsNoOp(t *testing.T) {
	np := NewNotificationPublisher(nil)
	np.PublishNotification("alice", &corenotification.Notification{})
}

func TestNotificationPublisher_NilNotificationIsNoOp(t *testing.T) {
	pub := &stubPublisher{}
	np := NewNotificationPublisher(pub)
	np.PublishNotification("alice", nil)
	assert.Empty(t, pub.topics)
}

func TestNotificationPublisher_EmptyNotifieeIsNoOp(t *testing.T) {
	pub := &stubPublisher{}
	np := NewNotificationPublisher(pub)
	np.PublishNotification("", &corenotification.Notification{})
	assert.Empty(t, pub.topics)
}

func TestNotificationPublisher_PublishErrorIsLogged(t *testing.T) {
	pub := &stubPublisher{err: errors.New("redis down")}
	np := NewNotificationPublisher(pub)
	np.PublishNotification("alice", &corenotification.Notification{ID: "n1"})
}

func TestNotificationPublisher_MarshalErrorIsLogged(t *testing.T) {
	pub := &stubPublisher{}
	np := NewNotificationPublisher(pub)
	// chan は JSON.Marshal で失敗する
	np.PublishNotification("alice", &corenotification.Notification{
		ID:    "n1",
		Extra: map[string]any{"ch": make(chan int)},
	})
	assert.Empty(t, pub.topics)
}

// --- DrivePublisher ---------------------------------------------------------

func TestDrivePublisher_PublishesEvent(t *testing.T) {
	pub := &stubPublisher{}
	dp := NewDrivePublisher(pub)
	f := &model.DriveFile{ID: "f1", Name: "x.png"}
	dp.PublishDriveEvent("alice", "fileCreated", f)
	require.Len(t, pub.topics, 1)
	assert.Equal(t, "drive:alice", pub.topics[0])
}

func TestDrivePublisher_NilPubIsNoOp(t *testing.T) {
	dp := NewDrivePublisher(nil)
	dp.PublishDriveEvent("alice", "fileCreated", &model.DriveFile{})
}

func TestDrivePublisher_EmptyArgsAreNoOps(t *testing.T) {
	pub := &stubPublisher{}
	dp := NewDrivePublisher(pub)
	dp.PublishDriveEvent("", "fileCreated", &model.DriveFile{})
	dp.PublishDriveEvent("alice", "", &model.DriveFile{})
	dp.PublishDriveEvent("alice", "fileCreated", nil)
	assert.Empty(t, pub.topics)
}

func TestDrivePublisher_PublishErrorIsLogged(t *testing.T) {
	pub := &stubPublisher{err: errors.New("redis down")}
	dp := NewDrivePublisher(pub)
	dp.PublishDriveEvent("alice", "fileCreated", &model.DriveFile{ID: "f1"})
}

// DrivePublisher が core/drive.StreamingPublisher を実装していることの動的確認。
// 静的アサーションは note_publisher.go に置いてある。
func TestDrivePublisher_ImplementsServiceInterface(t *testing.T) {
	var _ coredrive.StreamingPublisher = (*DrivePublisher)(nil)
	var _ coredrive.FolderStreamingPublisher = (*DrivePublisher)(nil)
}

// --- DrivePublisher folder events (#1564) -----------------------------------

func TestDrivePublisher_PublishesFolderEvent(t *testing.T) {
	pub := &stubPublisher{}
	dp := NewDrivePublisher(pub)
	dp.PublishDriveFolderEvent("alice", "folderCreated", map[string]any{"id": "fo1", "name": "docs"})
	require.Len(t, pub.topics, 1)
	assert.Equal(t, "drive:alice", pub.topics[0])
	raw := pub.payloads[0].(json.RawMessage)
	assert.Contains(t, string(raw), `"type":"folderCreated"`)
	assert.Contains(t, string(raw), `"docs"`)
}

func TestDrivePublisher_FolderDeletedBodyIsID(t *testing.T) {
	// folderDeleted の body は packed folder ではなく id 文字列。
	pub := &stubPublisher{}
	dp := NewDrivePublisher(pub)
	dp.PublishDriveFolderEvent("alice", "folderDeleted", "fo1")
	require.Len(t, pub.topics, 1)
	assert.Contains(t, string(pub.payloads[0].(json.RawMessage)), `"body":"fo1"`)
}

func TestDrivePublisher_FolderEventEmptyArgsAreNoOps(t *testing.T) {
	pub := &stubPublisher{}
	dp := NewDrivePublisher(pub)
	dp.PublishDriveFolderEvent("", "folderCreated", "fo1")
	dp.PublishDriveFolderEvent("alice", "", "fo1")
	dp.PublishDriveFolderEvent("alice", "folderCreated", nil)
	assert.Empty(t, pub.topics)
	NewDrivePublisher(nil).PublishDriveFolderEvent("alice", "folderCreated", "fo1")
}

func TestDrivePublisher_FolderEventMarshalErrorIsLogged(t *testing.T) {
	pub := &stubPublisher{}
	dp := NewDrivePublisher(pub)
	// chan は json.Marshal 不能なので marshal error 経路を踏む
	dp.PublishDriveFolderEvent("alice", "folderCreated", make(chan int))
	assert.Empty(t, pub.topics)
}

func TestDrivePublisher_FolderEventPublishErrorIsLogged(t *testing.T) {
	pub := &stubPublisher{err: errors.New("redis down")}
	dp := NewDrivePublisher(pub)
	dp.PublishDriveFolderEvent("alice", "folderCreated", "fo1")
}

func TestDrivePublisher_MarshalErrorIsLogged(t *testing.T) {
	pub := &stubPublisher{}
	dp := NewDrivePublisher(pub)
	// datatypes.JSON は invalid JSON で MarshalJSON が失敗する
	f := &model.DriveFile{ID: "f1", Properties: []byte("{not json")}
	dp.PublishDriveEvent("alice", "fileCreated", f)
	assert.Empty(t, pub.topics)
}

// NotificationPublisher が core/notification.StreamingPublisher を実装している
// ことを確認。
func TestNotificationPublisher_ImplementsServiceInterface(t *testing.T) {
	var _ corenotification.StreamingPublisher = (*NotificationPublisher)(nil)
}

// #1640: streaming note event は reactionAndUserPairCache を出力する
// (upstream NoteCreateService の withReactionAndUserPairCache=true 経路相当)。
// REST pack は出さないが streaming は空でも [] を出す。
func TestNotePublisher_EmitsReactionAndUserPairCache(t *testing.T) {
	idGen, _ := id.NewGenerator("aidx")

	// pair あり → 配列を出力
	pub := &stubPublisher{}
	np := NewNotePublisher(pub, idGen)
	n := &model.Note{
		ID: idGen.Generate(time.Now()), UserID: "u1", Visibility: model.NoteVisibilityPublic,
		ReactionAndUserPairCache: []string{"userA/👍", "userB/❤"},
	}
	np.PublishNote("homeTimeline:u1", n, &model.User{ID: "u1", Username: "alice"})
	require.Len(t, pub.payloads, 1)
	var m map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(pub.payloads[0].(json.RawMessage), &m))
	require.Contains(t, m, "reactionAndUserPairCache")
	assert.JSONEq(t, `["userA/👍","userB/❤"]`, string(m["reactionAndUserPairCache"]))

	// pair 空 → streaming は [] を出す (REST と異なり key を残す)
	pub2 := &stubPublisher{}
	np2 := NewNotePublisher(pub2, idGen)
	n2 := &model.Note{
		ID: idGen.Generate(time.Now()), UserID: "u1", Visibility: model.NoteVisibilityPublic,
	}
	np2.PublishNote("homeTimeline:u1", n2, &model.User{ID: "u1", Username: "alice"})
	require.Len(t, pub2.payloads, 1)
	var m2 map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(pub2.payloads[0].(json.RawMessage), &m2))
	require.Contains(t, m2, "reactionAndUserPairCache")
	assert.Equal(t, "[]", string(m2["reactionAndUserPairCache"]))
}

// #2058: pure renote の renote embed にも reactionAndUserPairCache を載せ、subscriber
// 側 channel が renote.myReaction を算出できるようにする。
func TestNotePublisher_EmitsRenoteReactionAndUserPairCache(t *testing.T) {
	idGen, _ := id.NewGenerator("aidx")
	pub := &stubPublisher{}
	np := NewNotePublisher(pub, idGen)

	renote := &model.Note{
		ID: idGen.Generate(time.Now()), UserID: "u2", Visibility: model.NoteVisibilityPublic,
		ReactionAndUserPairCache: []string{"userA/👍"},
	}
	n := &model.Note{
		ID: idGen.Generate(time.Now()), UserID: "u1", Visibility: model.NoteVisibilityPublic,
		RenoteID: &renote.ID, Renote: renote,
	}
	np.PublishNote("homeTimeline:u1", n, &model.User{ID: "u1", Username: "alice"})
	require.Len(t, pub.payloads, 1)
	var m map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(pub.payloads[0].(json.RawMessage), &m))
	require.Contains(t, m, "renote")
	var renoteObj map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(m["renote"], &renoteObj))
	require.Contains(t, renoteObj, "reactionAndUserPairCache", "renote embed に cache を載せる (#2058)")
	assert.JSONEq(t, `["userA/👍"]`, string(renoteObj["reactionAndUserPairCache"]))
}

// stubReactionPairReader records ListReactionPairs calls and returns canned pairs.
type stubReactionPairReader struct {
	pairs map[string][]string
	calls []string
}

func (s *stubReactionPairReader) ListReactionPairs(noteID string, _ int) ([]string, error) {
	s.calls = append(s.calls, noteID)
	return s.pairs[noteID], nil
}

// #2067: pure renote で renote に reaction がある場合、renote embed の
// reactionAndUserPairCache を note_reaction (reader) から埋める。
func TestNotePublisher_RenotePairCacheFromReader(t *testing.T) {
	pub := &stubPublisher{}
	idGen, _ := id.NewGenerator("aidx")
	np := NewNotePublisher(pub, idGen)
	renoteID := idGen.Generate(time.Now())
	noteID := idGen.Generate(time.Now())
	reader := &stubReactionPairReader{pairs: map[string][]string{renoteID: {"alice/👍", "bob/❤"}}}
	np.SetReactionPairReader(reader)

	n := &model.Note{
		ID: noteID, UserID: "u1", RenoteID: &renoteID, Visibility: model.NoteVisibilityPublic,
		Renote: &model.Note{
			ID: renoteID, UserID: "u2", Visibility: model.NoteVisibilityPublic,
			Reactions: datatypes.JSON([]byte(`{"👍":2}`)),
			User:      &model.User{ID: "u2", Username: "author2"},
		},
	}
	np.PublishNote("homeTimeline:u1", n, &model.User{ID: "u1", Username: "alice"})

	require.Len(t, pub.payloads, 1)
	var body map[string]any
	require.NoError(t, json.Unmarshal(pub.payloads[0].(json.RawMessage), &body))
	renote := body["renote"].(map[string]any)
	assert.Equal(t, []any{"alice/👍", "bob/❤"}, renote["reactionAndUserPairCache"])
	assert.Equal(t, []string{renoteID}, reader.calls, "pure renote + reactions のときだけ reader を引く")
}

// quote renote (text あり = 非 pure) では reader を引かない。
func TestNotePublisher_QuoteRenoteSkipsPairReader(t *testing.T) {
	pub := &stubPublisher{}
	idGen, _ := id.NewGenerator("aidx")
	np := NewNotePublisher(pub, idGen)
	renoteID := idGen.Generate(time.Now())
	reader := &stubReactionPairReader{pairs: map[string][]string{renoteID: {"alice/👍"}}}
	np.SetReactionPairReader(reader)
	quoteText := "quote"
	n := &model.Note{
		ID: idGen.Generate(time.Now()), UserID: "u1", RenoteID: &renoteID, Text: &quoteText,
		Visibility: model.NoteVisibilityPublic,
		Renote: &model.Note{
			ID: renoteID, UserID: "u2", Visibility: model.NoteVisibilityPublic,
			Reactions: datatypes.JSON([]byte(`{"👍":1}`)),
			User:      &model.User{ID: "u2", Username: "author2"},
		},
	}
	np.PublishNote("homeTimeline:u1", n, &model.User{ID: "u1", Username: "alice"})
	assert.Empty(t, reader.calls, "非 pure renote では reader を引かない")
}

// reaction の無い renote では reader を引かない。
func TestNotePublisher_NoReactionsSkipsPairReader(t *testing.T) {
	pub := &stubPublisher{}
	idGen, _ := id.NewGenerator("aidx")
	np := NewNotePublisher(pub, idGen)
	renoteID := idGen.Generate(time.Now())
	reader := &stubReactionPairReader{}
	np.SetReactionPairReader(reader)
	n := &model.Note{
		ID: idGen.Generate(time.Now()), UserID: "u1", RenoteID: &renoteID, Visibility: model.NoteVisibilityPublic,
		Renote: &model.Note{
			ID: renoteID, UserID: "u2", Visibility: model.NoteVisibilityPublic,
			Reactions: datatypes.JSON([]byte(`{}`)),
			User:      &model.User{ID: "u2", Username: "author2"},
		},
	}
	np.PublishNote("homeTimeline:u1", n, &model.User{ID: "u1", Username: "alice"})
	assert.Empty(t, reader.calls, "reaction 無しの renote では reader を引かない")
}

// #2735: NotificationPublisher 側にも FieldResolver を配線すると、通知の
// streaming payload の note に Files が乗る。未配線だと通知ページの realtime
// 追加分だけ添付メディアが消え、リロードするまで出てこない。
func TestNotificationPublisher_FieldResolverPopulatesNoteFiles(t *testing.T) {
	pub := &stubPublisher{}
	np := NewNotificationPublisher(pub)
	idGen, _ := id.NewGenerator("aidx")

	driveRepo := testutil.NewMockDriveFileRepository()
	fileID := idGen.Generate(time.Now())
	driveRepo.Files[fileID] = &model.DriveFile{
		ID: fileID, Name: "a.png", Type: "image/png", URL: "https://r/a.png",
	}
	np.SetRepos(
		&stubNotifUserRepo{user: &model.User{ID: "u2", Username: "bob"}},
		&stubNotifNoteRepo{note: &model.Note{
			ID: "note1", UserID: "u2", Visibility: model.NoteVisibilityPublic,
			FileIDs: []string{fileID},
		}},
		idGen,
	)
	np.SetFieldResolver(entity.NewNoteFieldResolver(driveRepo, nil, nil, nil, nil, idGen))

	np.PublishNotification("alice", &corenotification.Notification{
		ID:         idGen.Generate(time.Now()),
		Type:       corenotification.TypeReply,
		NotifierID: "u2",
		NoteID:     "note1",
	})

	require.Len(t, pub.payloads, 1)
	raw := pub.payloads[0].(json.RawMessage)
	var body map[string]any
	require.NoError(t, json.Unmarshal(raw, &body))
	note := body["note"].(map[string]any)
	files, ok := note["files"].([]any)
	require.True(t, ok, "note.files must be present on the notification payload")
	require.Len(t, files, 1)
	assert.Equal(t, fileID, files[0].(map[string]any)["id"])
}

// #2735: streaming の通知は per-viewer topic (notifications:<notifieeID>) なので
// viewer が確定している。upstream も notifiee を渡して pack するため、
// myReaction のような viewer 依存 field も解決されること。
func TestNotificationPublisher_ResolvesViewerFieldsForNotifiee(t *testing.T) {
	pub := &stubPublisher{}
	np := NewNotificationPublisher(pub)
	idGen, _ := id.NewGenerator("aidx")

	reactionRepo := &stubStreamReactionLookup{rows: map[string]*model.NoteReaction{
		"note1": {NoteID: "note1", Reaction: "👍"},
	}}
	np.SetRepos(
		&stubNotifUserRepo{user: &model.User{ID: "u2", Username: "bob"}},
		&stubNotifNoteRepo{note: &model.Note{
			ID: "note1", UserID: "alice", Visibility: model.NoteVisibilityPublic,
			Reactions: datatypes.JSON([]byte(`{"👍":1}`)),
		}},
		idGen,
	)
	np.SetFieldResolver(entity.NewNoteFieldResolver(nil, nil, nil, reactionRepo, nil, idGen))

	np.PublishNotification("alice", &corenotification.Notification{
		ID:         idGen.Generate(time.Now()),
		Type:       corenotification.TypeReaction,
		NotifierID: "u2",
		NoteID:     "note1",
		Reaction:   "👍",
	})

	require.Len(t, pub.payloads, 1)
	raw := pub.payloads[0].(json.RawMessage)
	var body map[string]any
	require.NoError(t, json.Unmarshal(raw, &body))
	note := body["note"].(map[string]any)
	assert.Equal(t, "👍", note["myReaction"], "notifiee 視点の myReaction が入ること")
	assert.Equal(t, "alice", reactionRepo.viewerID, "notifiee を viewer として引くこと")
}

type stubStreamReactionLookup struct {
	rows     map[string]*model.NoteReaction
	viewerID string
}

func (s *stubStreamReactionLookup) FindByUserAndNoteIDs(userID string, _ []string) (map[string]*model.NoteReaction, error) {
	s.viewerID = userID
	return s.rows, nil
}

// 未配線 (nil resolver) では従来どおり files が空配列で、panic もしないこと。
func TestNotificationPublisher_NoFieldResolverKeepsFilesEmpty(t *testing.T) {
	pub := &stubPublisher{}
	np := NewNotificationPublisher(pub)
	idGen, _ := id.NewGenerator("aidx")
	np.SetRepos(
		&stubNotifUserRepo{user: &model.User{ID: "u2", Username: "bob"}},
		&stubNotifNoteRepo{note: &model.Note{
			ID: "note1", UserID: "u2", Visibility: model.NoteVisibilityPublic,
			FileIDs: []string{"f1"},
		}},
		idGen,
	)

	np.PublishNotification("alice", &corenotification.Notification{
		ID:         idGen.Generate(time.Now()),
		Type:       corenotification.TypeReply,
		NotifierID: "u2",
		NoteID:     "note1",
	})

	require.Len(t, pub.payloads, 1)
	raw := pub.payloads[0].(json.RawMessage)
	var body map[string]any
	require.NoError(t, json.Unmarshal(raw, &body))
	note := body["note"].(map[string]any)
	assert.Empty(t, note["files"])
}
