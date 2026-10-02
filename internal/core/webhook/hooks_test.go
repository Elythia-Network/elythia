package webhook_test

import (
	"encoding/json"
	"testing"

	"github.com/shiroha-a/mk/internal/core/webhook"
	"github.com/shiroha-a/mk/internal/misc/id"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestService(t *testing.T) (*webhook.Service, *fakeEnqueuer, *fakeWebhookRepo, *fakeSystemWebhookRepo) {
	t.Helper()
	enq := &fakeEnqueuer{}
	userRepo := &fakeWebhookRepo{hooks: make(map[string]*model.Webhook)}
	sysRepo := &fakeSystemWebhookRepo{hooks: make(map[string]*model.SystemWebhook)}
	svc := webhook.NewService(enq, userRepo, sysRepo, "https://example.com")
	return svc, enq, userRepo, sysRepo
}

func TestNoteCreateHook_FiresNoteEventOnAuthor(t *testing.T) {
	svc, enq, userRepo, _ := newTestService(t)
	userRepo.hooks["h1"] = &model.Webhook{
		ID: "h1", UserID: "alice", Active: true, On: model.StringArray{"note"},
	}
	idGen, _ := id.NewGenerator("aidx")
	h := webhook.NewNoteCreateHook(svc, idGen)

	text := "hello"
	h.OnNoteCreated(
		&model.Note{ID: "n1", UserID: "alice", Text: &text, Visibility: model.NoteVisibilityPublic},
		&model.User{ID: "alice", Username: "alice", UsernameLower: "alice"},
		nil, nil,
	)
	require.Len(t, enq.userCalls, 1)
	assert.Equal(t, "h1", enq.userCalls[0].WebhookID)
}

func TestNoteCreateHook_FiresReplyEventOnParentAuthor(t *testing.T) {
	svc, enq, userRepo, _ := newTestService(t)
	userRepo.hooks["h_reply"] = &model.Webhook{
		ID: "h_reply", UserID: "bob", Active: true, On: model.StringArray{"reply"},
	}
	idGen, _ := id.NewGenerator("aidx")
	h := webhook.NewNoteCreateHook(svc, idGen)

	text := "reply body"
	h.OnNoteCreated(
		&model.Note{ID: "n2", UserID: "alice", Text: &text, Visibility: model.NoteVisibilityPublic},
		&model.User{ID: "alice", Username: "alice", UsernameLower: "alice"},
		&model.Note{ID: "np", UserID: "bob"},
		nil,
	)
	require.Len(t, enq.userCalls, 1)
	assert.Equal(t, "reply", enq.userCalls[0].EventType)
}

// #1965: reply 先がスレッドをミュートしていれば reply webhook を出さない。
func TestNoteCreateHook_ReplyThreadMutedSkipped(t *testing.T) {
	svc, enq, userRepo, _ := newTestService(t)
	userRepo.hooks["h_reply"] = &model.Webhook{ID: "h_reply", UserID: "bob", Active: true, On: model.StringArray{"reply"}}
	idGen, _ := id.NewGenerator("aidx")
	h := webhook.NewNoteCreateHook(svc, idGen)
	muteRepo := testutil.NewMockNoteThreadMutingRepository()
	require.NoError(t, muteRepo.Create(&model.NoteThreadMuting{UserID: "bob", ThreadID: "np"}))
	h.SetThreadMutingRepo(muteRepo)

	text := "reply body"
	h.OnNoteCreated(
		&model.Note{ID: "n2", UserID: "alice", Text: &text, Visibility: model.NoteVisibilityPublic},
		&model.User{ID: "alice", Username: "alice", UsernameLower: "alice"},
		&model.Note{ID: "np", UserID: "bob"},
		nil,
	)
	assert.Empty(t, enq.userCalls, "thread-muted reply 先には reply webhook を出さない (#1965)")
}

// #1965: reply 先が threaded note (ThreadID 持ち) の場合、その threadId で判定する。
func TestNoteCreateHook_ReplyThreadMuteUsesThreadID(t *testing.T) {
	svc, enq, userRepo, _ := newTestService(t)
	userRepo.hooks["h_reply"] = &model.Webhook{ID: "h_reply", UserID: "bob", Active: true, On: model.StringArray{"reply"}}
	idGen, _ := id.NewGenerator("aidx")
	h := webhook.NewNoteCreateHook(svc, idGen)
	muteRepo := testutil.NewMockNoteThreadMutingRepository()
	require.NoError(t, muteRepo.Create(&model.NoteThreadMuting{UserID: "bob", ThreadID: "troot"}))
	h.SetThreadMutingRepo(muteRepo)

	troot := "troot"
	text := "reply body"
	h.OnNoteCreated(
		&model.Note{ID: "n2", UserID: "alice", Text: &text, Visibility: model.NoteVisibilityPublic},
		&model.User{ID: "alice", Username: "alice", UsernameLower: "alice"},
		&model.Note{ID: "np", UserID: "bob", ThreadID: &troot},
		nil,
	)
	assert.Empty(t, enq.userCalls, "reply 先ノートの threadId でミュート判定する (#1965)")
}

// #1965: mention 先がスレッドをミュートしていれば mention webhook を出さない。
func TestNoteCreateHook_MentionThreadMutedSkipped(t *testing.T) {
	svc, enq, userRepo, _ := newTestService(t)
	userRepo.hooks["h_m"] = &model.Webhook{ID: "h_m", UserID: "bob", Active: true, On: model.StringArray{"mention"}}
	idGen, _ := id.NewGenerator("aidx")
	h := webhook.NewNoteCreateHook(svc, idGen)
	muteRepo := testutil.NewMockNoteThreadMutingRepository()
	require.NoError(t, muteRepo.Create(&model.NoteThreadMuting{UserID: "bob", ThreadID: "n_m"}))
	h.SetThreadMutingRepo(muteRepo)

	text := "hi @bob"
	h.OnNoteCreated(
		&model.Note{ID: "n_m", UserID: "alice", Text: &text, Visibility: model.NoteVisibilityPublic, Mentions: model.StringArray{"bob"}},
		&model.User{ID: "alice", Username: "alice", UsernameLower: "alice"},
		nil, nil,
	)
	assert.Empty(t, enq.userCalls, "thread-muted mention 先には mention webhook を出さない (#1965)")
}

// #1965: renote はスレッドミュートで gate されない (upstream も ungated)。
func TestNoteCreateHook_RenoteThreadMuteNotGated(t *testing.T) {
	svc, enq, userRepo, _ := newTestService(t)
	userRepo.hooks["h_rn"] = &model.Webhook{ID: "h_rn", UserID: "bob", Active: true, On: model.StringArray{"renote"}}
	idGen, _ := id.NewGenerator("aidx")
	h := webhook.NewNoteCreateHook(svc, idGen)
	muteRepo := testutil.NewMockNoteThreadMutingRepository()
	require.NoError(t, muteRepo.Create(&model.NoteThreadMuting{UserID: "bob", ThreadID: "nr"}))
	h.SetThreadMutingRepo(muteRepo)

	h.OnNoteCreated(
		&model.Note{ID: "n_rn", UserID: "alice", Visibility: model.NoteVisibilityPublic},
		&model.User{ID: "alice", Username: "alice", UsernameLower: "alice"},
		nil, &model.Note{ID: "nr", UserID: "bob"},
	)
	require.Len(t, enq.userCalls, 1, "renote はスレッドミュートで抑制されない (#1965)")
	assert.Equal(t, "renote", enq.userCalls[0].EventType)
}

func TestNoteCreateHook_FiresRenoteEventOnRenoteAuthor(t *testing.T) {
	svc, enq, userRepo, _ := newTestService(t)
	userRepo.hooks["h_renote"] = &model.Webhook{
		ID: "h_renote", UserID: "bob", Active: true, On: model.StringArray{"renote"},
	}
	idGen, _ := id.NewGenerator("aidx")
	h := webhook.NewNoteCreateHook(svc, idGen)

	h.OnNoteCreated(
		&model.Note{ID: "n3", UserID: "alice", Visibility: model.NoteVisibilityPublic},
		&model.User{ID: "alice", Username: "alice", UsernameLower: "alice"},
		nil,
		&model.Note{ID: "nt", UserID: "bob"},
	)
	require.Len(t, enq.userCalls, 1)
	assert.Equal(t, "renote", enq.userCalls[0].EventType)
}

func TestNoteCreateHook_FiresMentionEventOnMentionedUsers(t *testing.T) {
	svc, enq, userRepo, _ := newTestService(t)
	userRepo.hooks["h_mention"] = &model.Webhook{
		ID: "h_mention", UserID: "carol", Active: true, On: model.StringArray{"mention"},
	}
	idGen, _ := id.NewGenerator("aidx")
	h := webhook.NewNoteCreateHook(svc, idGen)

	h.OnNoteCreated(
		&model.Note{ID: "n4", UserID: "alice", Mentions: model.StringArray{"carol"}, Visibility: model.NoteVisibilityPublic},
		&model.User{ID: "alice", Username: "alice", UsernameLower: "alice"},
		nil, nil,
	)
	require.Len(t, enq.userCalls, 1)
	assert.Equal(t, "mention", enq.userCalls[0].EventType)
}

func TestNoteCreateHook_SkipsSelfReplyAndMention(t *testing.T) {
	svc, enq, userRepo, _ := newTestService(t)
	userRepo.hooks["h_alice"] = &model.Webhook{
		ID: "h_alice", UserID: "alice", Active: true,
		On: model.StringArray{"note", "reply", "renote", "mention"},
	}
	idGen, _ := id.NewGenerator("aidx")
	h := webhook.NewNoteCreateHook(svc, idGen)

	h.OnNoteCreated(
		&model.Note{ID: "n5", UserID: "alice", Mentions: model.StringArray{"alice"}, Visibility: model.NoteVisibilityPublic},
		&model.User{ID: "alice", Username: "alice", UsernameLower: "alice"},
		&model.Note{ID: "np", UserID: "alice"},
		&model.Note{ID: "nt", UserID: "alice"},
	)
	// author(alice)のnoteイベントが1件だけ。reply/renote/mentionは自己発火抑制。
	require.Len(t, enq.userCalls, 1)
	assert.Equal(t, "note", enq.userCalls[0].EventType)
}

func TestNoteCreateHook_NilSafe(t *testing.T) {
	var h *webhook.NoteCreateHook
	h.OnNoteCreated(nil, nil, nil, nil)
	idGen, _ := id.NewGenerator("aidx")
	h = webhook.NewNoteCreateHook(nil, idGen)
	h.OnNoteCreated(nil, nil, nil, nil)
}

func TestReactionCreateHook(t *testing.T) {
	svc, enq, userRepo, _ := newTestService(t)
	userRepo.hooks["h1"] = &model.Webhook{
		ID: "h1", UserID: "alice", Active: true, On: model.StringArray{"reaction"},
	}
	idGen, _ := id.NewGenerator("aidx")
	h := webhook.NewReactionCreateHook(svc, idGen)

	h.OnReactionCreated(
		&model.Note{ID: "n1", UserID: "alice", Visibility: model.NoteVisibilityPublic},
		&model.User{ID: "bob", Username: "bob", UsernameLower: "bob"},
		"\U0001F44D",
	)
	require.Len(t, enq.userCalls, 1)
	assert.Equal(t, "reaction", enq.userCalls[0].EventType)
}

func TestReactionCreateHook_NilSafe(t *testing.T) {
	var h *webhook.ReactionCreateHook
	h.OnReactionCreated(nil, nil, "")
	idGen, _ := id.NewGenerator("aidx")
	h = webhook.NewReactionCreateHook(nil, idGen)
	h.OnReactionCreated(nil, nil, "")
}

// --- per-recipient embed visibility gate on the note payload ---

// nestedQuoteNote builds: top (public, by topAuthor) → renote = quote (public, by
// midAuthor) → renote.renote = target (targetVis, by targetAuthor). embed 著者には
// User を attach して AuthorPrefsKnown を立てる。
func nestedQuoteNote(topAuthor, midAuthor, targetAuthor string, targetVis model.NoteVisibility) *model.Note {
	tText, qText, topText := "secret target", "quoting", "top body"
	targetID, quoteID := "t_nested", "q_nested"
	target := &model.Note{
		ID: targetID, UserID: targetAuthor, Visibility: targetVis, Text: &tText,
		User: &model.User{ID: targetAuthor, Username: targetAuthor, UsernameLower: targetAuthor},
	}
	quote := &model.Note{
		ID: quoteID, UserID: midAuthor, Visibility: model.NoteVisibilityPublic, Text: &qText,
		RenoteID: &targetID, Renote: target,
		User: &model.User{ID: midAuthor, Username: midAuthor, UsernameLower: midAuthor},
	}
	return &model.Note{
		ID: "top_nested", UserID: topAuthor, Visibility: model.NoteVisibilityPublic, Text: &topText,
		RenoteID: &quoteID, Renote: quote,
	}
}

// renoteRenoteOf extracts body.note.renote.renote (depth-2 引用先) from an
// enqueued webhook Envelope body.
func renoteRenoteOf(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var env struct {
		Body struct {
			Note map[string]any `json:"note"`
		} `json:"body"`
	}
	require.NoError(t, json.Unmarshal(body, &env))
	require.NotNil(t, env.Body.Note, "note must be present")
	renote, _ := env.Body.Note["renote"].(map[string]any)
	require.NotNil(t, renote, "depth-1 renote (quote) must be present")
	rr, _ := renote["renote"].(map[string]any)
	require.NotNil(t, rr, "depth-2 renote.renote (quote target) must be present")
	return rr
}

// 引用先 (renote.renote) が followers-only のとき、非フォロワー受信者の webhook
// payload で blank される (#timeline nested quote の embed leak を webhook でも防ぐ)。
func TestNoteCreateHook_GatesDepthTwoFollowersQuoteTarget(t *testing.T) {
	svc, enq, userRepo, _ := newTestService(t)
	userRepo.hooks["h1"] = &model.Webhook{ID: "h1", UserID: "alice", Active: true, On: model.StringArray{"note"}}
	idGen, _ := id.NewGenerator("aidx")
	follow := testutil.NewMockFollowingRepository() // alice follows nobody
	h := webhook.NewNoteCreateHook(svc, idGen)
	h.SetFollowingRepo(follow)

	top := nestedQuoteNote("alice", "host", "carol", model.NoteVisibilityFollowers)
	h.OnNoteCreated(top, &model.User{ID: "alice", Username: "alice", UsernameLower: "alice"}, nil, nil)

	require.Len(t, enq.userCalls, 1)
	rr := renoteRenoteOf(t, enq.userCalls[0].Body)
	assert.Equal(t, true, rr["isHidden"], "depth-2 followers quote target must be blanked for non-follower recipient")
	assert.Nil(t, rr["text"], "blanked embed drops text")
}

// 引用先がフォロワー受信者には見える (過剰 blank しない回帰)。
func TestNoteCreateHook_DepthTwoQuoteTargetVisibleToFollower(t *testing.T) {
	svc, enq, userRepo, _ := newTestService(t)
	userRepo.hooks["h1"] = &model.Webhook{ID: "h1", UserID: "alice", Active: true, On: model.StringArray{"note"}}
	idGen, _ := id.NewGenerator("aidx")
	follow := testutil.NewMockFollowingRepository()
	follow.Followings["f"] = &model.Following{ID: "f", FollowerID: "alice", FolloweeID: "carol"}
	h := webhook.NewNoteCreateHook(svc, idGen)
	h.SetFollowingRepo(follow)

	top := nestedQuoteNote("alice", "host", "carol", model.NoteVisibilityFollowers)
	h.OnNoteCreated(top, &model.User{ID: "alice", Username: "alice", UsernameLower: "alice"}, nil, nil)

	require.Len(t, enq.userCalls, 1)
	rr := renoteRenoteOf(t, enq.userCalls[0].Body)
	assert.NotEqual(t, true, rr["isHidden"], "follower recipient sees the depth-2 quote target")
	assert.Equal(t, "secret target", rr["text"])
}

// per-recipient: 同じノートでも受信者ごとに別 body を pack/gate するため、引用先が
// 見える受信者 (follower) と見えない受信者 (non-follower) が混在しても leak しない。
// 共有 body だとこのテストは fail する。
func TestNoteCreateHook_PerRecipientEmbedGate(t *testing.T) {
	svc, enq, userRepo, _ := newTestService(t)
	userRepo.hooks["hd"] = &model.Webhook{ID: "hd", UserID: "dave", Active: true, On: model.StringArray{"mention"}}
	userRepo.hooks["he"] = &model.Webhook{ID: "he", UserID: "eve", Active: true, On: model.StringArray{"mention"}}
	idGen, _ := id.NewGenerator("aidx")
	follow := testutil.NewMockFollowingRepository()
	follow.Followings["f"] = &model.Following{ID: "f", FollowerID: "dave", FolloweeID: "carol"} // dave follows carol, eve does not
	h := webhook.NewNoteCreateHook(svc, idGen)
	h.SetFollowingRepo(follow)

	top := nestedQuoteNote("alice", "host", "carol", model.NoteVisibilityFollowers)
	top.Mentions = model.StringArray{"dave", "eve"}
	h.OnNoteCreated(top, &model.User{ID: "alice", Username: "alice", UsernameLower: "alice"}, nil, nil)

	require.Len(t, enq.userCalls, 2) // alice (note) は webhook 未登録 → call 無し
	byUser := map[string]map[string]any{}
	for _, c := range enq.userCalls {
		byUser[c.UserID] = renoteRenoteOf(t, c.Body)
	}
	require.NotNil(t, byUser["dave"])
	require.NotNil(t, byUser["eve"])
	assert.NotEqual(t, true, byUser["dave"]["isHidden"], "follower dave sees the depth-2 quote target")
	assert.Equal(t, "secret target", byUser["dave"]["text"])
	assert.Equal(t, true, byUser["eve"]["isHidden"], "non-follower eve has the depth-2 quote target blanked")
}

func TestFollowingHook_FiresFollowAndFollowed(t *testing.T) {
	svc, enq, userRepo, _ := newTestService(t)
	userRepo.hooks["h_follow"] = &model.Webhook{
		ID: "h_follow", UserID: "alice", Active: true, On: model.StringArray{"follow"},
	}
	userRepo.hooks["h_followed"] = &model.Webhook{
		ID: "h_followed", UserID: "bob", Active: true, On: model.StringArray{"followed"},
	}
	h := webhook.NewFollowingHook(svc)
	wireFollowLookups(t, h, "bob")

	follower := &model.User{ID: "alice", Username: "alice", UsernameLower: "alice"}
	followee := &model.User{ID: "bob", Username: "bob", UsernameLower: "bob"}
	h.OnFollow(follower, followee)
	h.OnFollowed(follower, followee)

	events := make(map[string]int)
	for _, c := range enq.userCalls {
		events[c.EventType]++
	}
	assert.Equal(t, 1, events["follow"])
	assert.Equal(t, 1, events["followed"])
}

func TestFollowingHook_Unfollow(t *testing.T) {
	svc, enq, userRepo, _ := newTestService(t)
	userRepo.hooks["h_u"] = &model.Webhook{
		ID: "h_u", UserID: "alice", Active: true, On: model.StringArray{"unfollow"},
	}
	h := webhook.NewFollowingHook(svc)
	wireFollowLookups(t, h, "bob")
	h.OnUnfollow(
		&model.User{ID: "alice", Username: "alice", UsernameLower: "alice"},
		&model.User{ID: "bob", Username: "bob", UsernameLower: "bob"},
	)
	require.Len(t, enq.userCalls, 1)
	assert.Equal(t, "unfollow", enq.userCalls[0].EventType)
}

func TestFollowingHook_NilSafe(t *testing.T) {
	var h *webhook.FollowingHook
	h.OnFollow(nil, nil)
	h.OnUnfollow(nil, nil)
	h.OnFollowed(nil, nil)
	h = webhook.NewFollowingHook(nil)
	h.OnFollow(nil, nil)
	h.OnUnfollow(nil, nil)
	h.OnFollowed(nil, nil)
}

func TestSignupHook_FiresUserCreated(t *testing.T) {
	svc, enq, _, sysRepo := newTestService(t)
	sysRepo.hooks["sh1"] = &model.SystemWebhook{
		ID: "sh1", IsActive: true, On: model.StringArray{"userCreated"},
	}
	h := webhook.NewSignupHook(svc)
	h.OnUserCreated(&model.User{ID: "alice", Username: "alice", UsernameLower: "alice"})
	require.Len(t, enq.systemCalls, 1)
	assert.Equal(t, "sh1", enq.systemCalls[0].WebhookID)
	assert.Equal(t, "userCreated", enq.systemCalls[0].EventType)
}

func TestSignupHook_NilSafe(t *testing.T) {
	var h *webhook.SignupHook
	h.OnUserCreated(nil)
	h = webhook.NewSignupHook(nil)
	h.OnUserCreated(nil)
}

// webhook の payload に top-level の可視性ゲートが掛かること。
//
// **`gateNoteEmbeds` は embed しか見ない。** それだけだと note 本体が受信者の
// 可視性を無視して届く。実害は `specified` (DM) で出る — `note.Mentions` と
// `note.VisibleUserIDs` は乖離しうる (本文に `@x` があっても visible 指定は別) と
// `note_create_service.go` 自身が書いており、通知経路と main stream はその前提で
// 弾いているのに、**webhook だけが弾いていなかった**。
func TestNoteCreateHook_TopLevelVisibilityGate(t *testing.T) {
	// noteText extracts body.note.text from the single captured delivery.
	noteBody := func(t *testing.T, enq *fakeEnqueuer) map[string]any {
		t.Helper()
		require.Len(t, enq.userCalls, 1, "配送そのものは残す (upstream と同じ)")
		// payload は `{"server":..., "body": {"note": {...}}}` の形。
		var env struct {
			Body struct {
				Note map[string]any `json:"note"`
			} `json:"body"`
		}
		require.NoError(t, json.Unmarshal(enq.userCalls[0].Body, &env))
		return env.Body.Note
	}

	newHook := func(t *testing.T, ownerID, event string) (*webhook.NoteCreateHook, *fakeEnqueuer, *testutil.MockFollowingRepository) {
		t.Helper()
		svc, enq, userRepo, _ := newTestService(t)
		userRepo.hooks["h1"] = &model.Webhook{
			ID: "h1", UserID: ownerID, Active: true, On: model.StringArray{event},
		}
		idGen, _ := id.NewGenerator("aidx")
		h := webhook.NewNoteCreateHook(svc, idGen)
		follows := testutil.NewMockFollowingRepository()
		h.SetFollowingRepo(follows)
		return h, enq, follows
	}

	author := &model.User{ID: "alice", Username: "alice", UsernameLower: "alice"}

	t.Run("DM の本文は宛先外の mention には届かない", func(t *testing.T) {
		h, enq, _ := newHook(t, "bob", "mention")
		text := "secret @bob"
		h.OnNoteCreated(&model.Note{
			ID: "n1", UserID: "alice", Text: &text,
			Visibility:     model.NoteVisibilitySpecified,
			VisibleUserIDs: []string{"carol"}, // bob は宛先ではない
			Mentions:       []string{"bob"},
		}, author, nil, nil)

		note := noteBody(t, enq)
		assert.Nil(t, note["text"], "DM 本文が宛先外へ漏れている")
		assert.Equal(t, true, note["isHidden"])
	})

	// **弾きすぎていないことを見る。** 常に隠す実装でも上は緑になる。
	t.Run("宛先に入っていれば本文が届く", func(t *testing.T) {
		h, enq, _ := newHook(t, "bob", "mention")
		text := "secret @bob"
		h.OnNoteCreated(&model.Note{
			ID: "n1", UserID: "alice", Text: &text,
			Visibility:     model.NoteVisibilitySpecified,
			VisibleUserIDs: []string{"bob"},
			Mentions:       []string{"bob"},
		}, author, nil, nil)

		assert.Equal(t, "secret @bob", noteBody(t, enq)["text"])
	})

	t.Run("作者本人には常に本文が届く", func(t *testing.T) {
		h, enq, _ := newHook(t, "alice", "note")
		text := "secret"
		h.OnNoteCreated(&model.Note{
			ID: "n1", UserID: "alice", Text: &text,
			Visibility: model.NoteVisibilitySpecified, VisibleUserIDs: []string{"carol"},
		}, author, nil, nil)

		assert.Equal(t, "secret", noteBody(t, enq)["text"])
	})

	// **mention だけでなく renote 経路にも掛かること。** 片方だけ直すと隣に
	// 同じ穴が残る。
	t.Run("followers 限定の引用は非フォロワーの renote に本文を出さない", func(t *testing.T) {
		h, enq, _ := newHook(t, "bob", "renote")
		text := "quote body"
		h.OnNoteCreated(
			&model.Note{ID: "n1", UserID: "alice", Text: &text, Visibility: model.NoteVisibilityFollowers},
			author, nil,
			&model.Note{ID: "t1", UserID: "bob", Visibility: model.NoteVisibilityPublic},
		)

		assert.Nil(t, noteBody(t, enq)["text"], "followers 限定の本文が非フォロワーへ届いている")
	})

	t.Run("フォロワーなら引用本文が届く", func(t *testing.T) {
		h, enq, follows := newHook(t, "bob", "renote")
		require.NoError(t, follows.Create(&model.Following{ID: "f1", FollowerID: "bob", FolloweeID: "alice"}))
		text := "quote body"
		h.OnNoteCreated(
			&model.Note{ID: "n1", UserID: "alice", Text: &text, Visibility: model.NoteVisibilityFollowers},
			author, nil,
			&model.Note{ID: "t1", UserID: "bob", Visibility: model.NoteVisibilityPublic},
		)

		assert.Equal(t, "quote body", noteBody(t, enq)["text"])
	})
}
