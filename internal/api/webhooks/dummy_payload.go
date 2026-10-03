package webhooks

import "time"

// webhookTestDay is one day, the unit upstream WebhookTestService uses to date
// its dummy users.
const webhookTestDay = 24 * time.Hour

// dummyUserSpec mirrors the fields of upstream WebhookTestService's
// generateDummyUser that reach the packed payload.
type dummyUserSpec struct {
	id, username, name                         string
	updatedAgo, lastFetchedAgo                 time.Duration
	followersCount, followingCount, notesCount int
}

// 本家 WebhookTestService.ts の dummyUser1 / 2 / 3 と同じ値。日時は送る時点からの
// 相対で決まる。
var (
	webhookDummyUser1 = dummyUserSpec{id: "dummy-user-1", username: "dummy1", name: "DummyUser1",
		updatedAgo: 7 * webhookTestDay, lastFetchedAgo: 5 * webhookTestDay,
		followersCount: 10, followingCount: 5, notesCount: 30}
	webhookDummyUser2 = dummyUserSpec{id: "dummy-user-2", username: "dummy2", name: "DummyUser2",
		updatedAgo: 30 * webhookTestDay, lastFetchedAgo: webhookTestDay,
		followersCount: 40, followingCount: 50, notesCount: 900}
	webhookDummyUser3 = dummyUserSpec{id: "dummy-user-3", username: "dummy3", name: "DummyUser3",
		updatedAgo: 15 * webhookTestDay, lastFetchedAgo: 2 * webhookTestDay,
		followersCount: 60, followingCount: 70, notesCount: 15900}
)

// dummyNoteSpec mirrors upstream generateDummyNote overrides.
type dummyNoteSpec struct {
	id       string
	text     *string
	author   dummyUserSpec
	replyID  *string
	reply    *dummyNoteSpec
	renoteID *string
	renote   *dummyNoteSpec
	mentions []string
}

func webhookTestISO(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05.000Z")
}

func strPtr(s string) *string { return &s }

// toPackedUserLite mirrors upstream WebhookTestService.toPackedUserLite.
func (u dummyUserSpec) toPackedUserLite() map[string]any {
	return map[string]any{
		"id":       u.id,
		"name":     u.name,
		"username": u.username,
		"host":     nil,
		// 本家は avatarId が null なので `null ?? ''` で空文字になる。
		"avatarUrl":         "",
		"avatarBlurhash":    nil,
		"avatarDecorations": []any{},
		"isBot":             false,
		"isCat":             true,
		"emojis":            map[string]any{},
		"onlineStatus":      "active",
		"badgeRoles":        []any{},
	}
}

// toPackedUserDetailedNotMe mirrors upstream
// WebhookTestService.toPackedUserDetailedNotMe (moderationNote is undefined
// there, so it is left out).
func (u dummyUserSpec) toPackedUserDetailedNotMe(now time.Time) map[string]any {
	out := u.toPackedUserLite()
	for k, v := range map[string]any{
		"url":                            nil,
		"uri":                            nil,
		"movedTo":                        nil,
		"alsoKnownAs":                    []any{},
		"createdAt":                      webhookTestISO(now),
		"updatedAt":                      webhookTestISO(now.Add(-u.updatedAgo)),
		"lastFetchedAt":                  webhookTestISO(now.Add(-u.lastFetchedAgo)),
		"bannerUrl":                      nil,
		"bannerBlurhash":                 nil,
		"isLocked":                       false,
		"isSilenced":                     false,
		"isSuspended":                    false,
		"description":                    nil,
		"location":                       nil,
		"birthday":                       nil,
		"lang":                           nil,
		"fields":                         []any{},
		"verifiedLinks":                  []any{},
		"followersCount":                 u.followersCount,
		"followingCount":                 u.followingCount,
		"notesCount":                     u.notesCount,
		"pinnedNoteIds":                  []any{},
		"pinnedNotes":                    []any{},
		"pinnedPageId":                   nil,
		"pinnedPage":                     nil,
		"publicReactions":                true,
		"followersVisibility":            "public",
		"followingVisibility":            "public",
		"chatScope":                      "mutual",
		"canChat":                        true,
		"twoFactorEnabled":               false,
		"usePasswordLessLogin":           false,
		"securityKeys":                   false,
		"roles":                          []any{},
		"memo":                           nil,
		"isFollowing":                    false,
		"isFollowed":                     false,
		"hasPendingFollowRequestFromYou": false,
		"hasPendingFollowRequestToYou":   false,
		"isBlocking":                     false,
		"isBlocked":                      false,
		"isMuted":                        false,
		"isRenoteMuted":                  false,
		"notify":                         "none",
		"withReplies":                    true,
	} {
		out[k] = v
	}
	return out
}

// toPackedNote mirrors upstream WebhookTestService.toPackedNote. uri / url are
// undefined there (the dummy notes have none), so they are left out.
func (n dummyNoteSpec) toPackedNote(now time.Time, detail bool) map[string]any {
	mentions := n.mentions
	if mentions == nil {
		mentions = []string{}
	}
	var text any
	if n.text != nil {
		text = *n.text
	}
	var replyID, renoteID any
	if n.replyID != nil {
		replyID = *n.replyID
	}
	if n.renoteID != nil {
		renoteID = *n.renoteID
	}
	out := map[string]any{
		"id":                       n.id,
		"createdAt":                webhookTestISO(now),
		"deletedAt":                nil,
		"text":                     text,
		"cw":                       nil,
		"userId":                   n.author.id,
		"user":                     n.author.toPackedUserLite(),
		"replyId":                  replyID,
		"renoteId":                 renoteID,
		"isHidden":                 false,
		"visibility":               "public",
		"mentions":                 mentions,
		"visibleUserIds":           []string{},
		"fileIds":                  []string{},
		"files":                    []any{},
		"tags":                     []string{},
		"poll":                     nil,
		"emojis":                   map[string]any{},
		"channelId":                nil,
		"channel":                  nil,
		"localOnly":                true,
		"reactionAcceptance":       "likeOnly",
		"reactionEmojis":           map[string]any{},
		"reactions":                map[string]any{},
		"reactionCount":            0,
		"renoteCount":              10,
		"repliesCount":             5,
		"reactionAndUserPairCache": []string{},
	}
	if detail {
		out["clippedCount"] = 0
		out["myReaction"] = nil
		// 本家は reply を detail なし、renote を detail ありで pack する。
		out["reply"] = nil
		if n.reply != nil {
			out["reply"] = n.reply.toPackedNote(now, false)
		}
		out["renote"] = nil
		if n.renote != nil {
			out["renote"] = n.renote.toPackedNote(now, true)
		}
	}
	return out
}

// dummyWebhookBody builds the test payload of i/webhooks/test per event type,
// matching upstream WebhookTestService.testUserWebhook: packed dummy notes for
// note / reply / renote / mention, UserDetailedNotMe for follow / unfollow and
// UserLite for followed.
//
// reaction は本家では送らない (本家のコメントは「まだ実装されていない」) が、mk-go は reaction の Webhook を
// 実際に配信する (hooks.go の #2106 L42)。テスト送信も実際の本文と同じ形
// ({note, userId, reaction}) で送る。
func dummyWebhookBody(eventType string, now time.Time) map[string]any {
	note1 := dummyNoteSpec{
		id: "dummy-note-1", text: strPtr("This is a dummy note for testing purposes."),
		author: webhookDummyUser1,
	}
	switch eventType {
	case "note":
		return map[string]any{"note": note1.toPackedNote(now, true)}
	case "reply":
		reply := dummyNoteSpec{
			id: "dummy-reply-1", text: note1.text, author: webhookDummyUser1,
			replyID: strPtr(note1.id), reply: &note1,
		}
		return map[string]any{"note": reply.toPackedNote(now, true)}
	case "renote":
		renote := dummyNoteSpec{
			id: "dummy-renote-1", author: webhookDummyUser2,
			renoteID: strPtr(note1.id), renote: &note1,
		}
		return map[string]any{"note": renote.toPackedNote(now, true)}
	case "mention":
		mention := dummyNoteSpec{
			id: "dummy-mention-1", author: webhookDummyUser1,
			text:     strPtr("@" + webhookDummyUser2.username + " This is a mention to you."),
			mentions: []string{webhookDummyUser2.id},
		}
		return map[string]any{"note": mention.toPackedNote(now, true)}
	case "follow":
		return map[string]any{"user": webhookDummyUser1.toPackedUserDetailedNotMe(now)}
	case "followed":
		return map[string]any{"user": webhookDummyUser2.toPackedUserLite()}
	case "unfollow":
		return map[string]any{"user": webhookDummyUser3.toPackedUserDetailedNotMe(now)}
	default: // reaction
		return map[string]any{"note": note1.toPackedNote(now, true), "userId": webhookDummyUser1.id, "reaction": "👍"}
	}
}
