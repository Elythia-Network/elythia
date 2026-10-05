package webhooks

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 期待値は本家 WebhookTestService.ts の generateDummyUser / generateDummyNote /
// toPackedUserLite / toPackedUserDetailedNotMe / toPackedNote を書き写したもの (#3330)。
const (
	testNow = "2026-10-03T12:00:00.000Z"

	liteUser1 = `{"id":"dummy-user-1","name":"DummyUser1","username":"dummy1","host":null,"avatarUrl":"","avatarBlurhash":null,"avatarDecorations":[],"isBot":false,"isCat":true,"emojis":{},"onlineStatus":"active","badgeRoles":[]}`
	liteUser2 = `{"id":"dummy-user-2","name":"DummyUser2","username":"dummy2","host":null,"avatarUrl":"","avatarBlurhash":null,"avatarDecorations":[],"isBot":false,"isCat":true,"emojis":{},"onlineStatus":"active","badgeRoles":[]}`

	// detailedNotMe is a dummy UserDetailedNotMe with the per-user values as
	// format verbs: id, name, username, updatedAt, lastFetchedAt,
	// followersCount, followingCount, notesCount.
	detailedNotMe = `{"id":"%s","name":"%s","username":"%s","host":null,"avatarUrl":"","avatarBlurhash":null,"avatarDecorations":[],"isBot":false,"isCat":true,"emojis":{},"onlineStatus":"active","badgeRoles":[],` +
		`"url":null,"uri":null,"movedTo":null,"alsoKnownAs":[],"createdAt":"` + testNow + `","updatedAt":"%s","lastFetchedAt":"%s",` +
		`"bannerUrl":null,"bannerBlurhash":null,"isLocked":false,"isSilenced":false,"isSuspended":false,"description":null,"location":null,"birthday":null,"lang":null,"fields":[],"verifiedLinks":[],` +
		`"followersCount":%d,"followingCount":%d,"notesCount":%d,"pinnedNoteIds":[],"pinnedNotes":[],"pinnedPageId":null,"pinnedPage":null,"publicReactions":true,"followersVisibility":"public","followingVisibility":"public",` +
		`"chatScope":"mutual","canChat":true,"twoFactorEnabled":false,"usePasswordLessLogin":false,"securityKeys":false,"roles":[],"memo":null,` +
		`"isFollowing":false,"isFollowed":false,"hasPendingFollowRequestFromYou":false,"hasPendingFollowRequestToYou":false,"isBlocking":false,"isBlocked":false,"isMuted":false,"isRenoteMuted":false,"notify":"none","withReplies":true}`

	// noteBase has format verbs: id, text, userId, user, replyId, renoteId,
	// mentions.
	noteBase = `"id":"%s","createdAt":"` + testNow + `","deletedAt":null,"text":%s,"cw":null,"userId":"%s","user":%s,"replyId":%s,"renoteId":%s,"isHidden":false,"visibility":"public",` +
		`"mentions":%s,"visibleUserIds":[],"fileIds":[],"files":[],"tags":[],"poll":null,"emojis":{},"channelId":null,"channel":null,"localOnly":true,"reactionAcceptance":"likeOnly",` +
		`"reactionEmojis":{},"reactions":{},"reactionCount":0,"renoteCount":10,"repliesCount":5,"reactionAndUserPairCache":[]`
	dummyText = `"This is a dummy note for testing purposes."`
)

func note1Base() string {
	return fmt.Sprintf(noteBase, "dummy-note-1", dummyText, "dummy-user-1", liteUser1, "null", "null", "[]")
}

func note1Detail() string {
	return `{` + note1Base() + `,"clippedCount":0,"reply":null,"renote":null,"myReaction":null}`
}

func TestDummyWebhookBody_MatchesUpstream(t *testing.T) {
	now, err := time.Parse(time.RFC3339, "2026-10-03T12:00:00Z")
	require.NoError(t, err)
	ago := func(days int) string { return webhookTestISO(now.Add(-time.Duration(days) * 24 * time.Hour)) }

	cases := map[string]string{
		"note": `{"note":` + note1Detail() + `}`,
		"reply": `{"note":{` + fmt.Sprintf(noteBase, "dummy-reply-1", dummyText, "dummy-user-1", liteUser1, `"dummy-note-1"`, "null", "[]") +
			// 本家は reply を detail なしで pack する
			`,"clippedCount":0,"reply":{` + note1Base() + `},"renote":null,"myReaction":null}}`,
		"renote": `{"note":{` + fmt.Sprintf(noteBase, "dummy-renote-1", "null", "dummy-user-2", liteUser2, "null", `"dummy-note-1"`, "[]") +
			`,"clippedCount":0,"reply":null,"renote":` + note1Detail() + `,"myReaction":null}}`,
		"mention": `{"note":{` + fmt.Sprintf(noteBase, "dummy-mention-1", `"@dummy2 This is a mention to you."`, "dummy-user-1", liteUser1, "null", "null", `["dummy-user-2"]`) +
			`,"clippedCount":0,"reply":null,"renote":null,"myReaction":null}}`,
		"follow":   `{"user":` + fmt.Sprintf(detailedNotMe, "dummy-user-1", "DummyUser1", "dummy1", ago(7), ago(5), 10, 5, 30) + `}`,
		"followed": `{"user":` + liteUser2 + `}`,
		"unfollow": `{"user":` + fmt.Sprintf(detailedNotMe, "dummy-user-3", "DummyUser3", "dummy3", ago(15), ago(2), 60, 70, 15900) + `}`,
		// reaction は mk-go の拡張。実際の reaction Webhook と同じ形。
		"reaction": `{"note":` + note1Detail() + `,"userId":"dummy-user-1","reaction":"👍"}`,
	}
	for event, want := range cases {
		t.Run(event, func(t *testing.T) {
			got, err := json.Marshal(dummyWebhookBody(event, now))
			require.NoError(t, err)
			assert.JSONEq(t, want, string(got))
		})
	}
}

// Test は送る時点の時刻で dummy の日時を決める。
func TestTest_DummyBodyIsPacked(t *testing.T) {
	h, repo := newTestHandler()
	repo.webhooks["w1"] = &model.Webhook{ID: "w1", UserID: "u1"}
	disp := &stubDispatcher{}
	h.SetDispatcher(disp)

	before := time.Now().Add(-time.Second)
	rec := post(h.Test, `{"webhookId":"w1","type":"unfollow"}`, &model.User{ID: "u1"})
	require.Equal(t, http.StatusNoContent, rec.Code)
	require.Len(t, disp.calls, 1)
	body := disp.calls[0].body.(map[string]any)
	user := body["user"].(map[string]any)
	assert.Equal(t, "dummy-user-3", user["id"])
	createdAt, err := time.Parse(time.RFC3339, user["createdAt"].(string))
	require.NoError(t, err)
	assert.True(t, createdAt.After(before), "createdAt is the send time")
}
