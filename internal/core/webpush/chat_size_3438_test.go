package webpush_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	corechat "github.com/elythia-network/elythia/internal/core/chat"
	"github.com/elythia-network/elythia/internal/core/notification"
	"github.com/elythia-network/elythia/internal/core/webpush"
	"github.com/elythia-network/elythia/internal/entity"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
)

// heavyLiteFiller fills every lite with an instance and many emojis, the way
// a remote user with custom emojis in the display name is packed.
type heavyLiteFiller struct{}

func (heavyLiteFiller) FillLites(users []*model.User, lites []*entity.UserLite) {
	for i := range users {
		for j := range 10 {
			lites[i].Emojis[strings.Repeat("e", 30)+string(rune('a'+j))] = "https://media.remote.example/emoji/" + strings.Repeat("x", 100) + ".png"
		}
		lites[i].Instance = &entity.InstanceLite{Name: strPtr3438(strings.Repeat("インスタンス", 20))}
	}
}

func strPtr3438(s string) *string { return &s }

// maxRemoteUser3438 is a remote user with every pushed column at its maximum
// length (username / name / host varchar(128), avatarUrl varchar(1024)) and an
// avatar decoration.
func maxRemoteUser3438(userID string) *model.User {
	host := strings.Repeat("h", 120) + ".example"
	name := strings.Repeat("名", 128)
	avatar := "https://" + host + "/" + strings.Repeat("a", 1024-len("https://"+host+"/"))
	return &model.User{
		ID: userID, Username: strings.Repeat("u", 127) + userID[len(userID)-1:], Host: &host, Name: &name, AvatarURL: &avatar,
		AvatarDecorations: datatypes.JSON(`[{"id":"d1","angle":0.5,"flipH":true,"offsetX":0.1,"offsetY":0.1}]`),
	}
}

// TestChatPush_BodyFitsTheWebPushLimitAtMaximumSize pins that the Web Push of
// `newChatMessage` stays within the Web Push size limit (3800 B, see
// maxPushBodyBytes) for the largest message: 2000 characters of Japanese text,
// a room with the longest name and description, and remote sender / owner with
// maximum-length columns, emojis, instance and avatar decorations (#3438).
//
// 以前は main stream の newChatMessage (UserLite 3 人分 + room の description) を
// そのまま送っており、本文 1000 文字の room で上限を超えて通知が消えた。
func TestChatPush_BodyFitsTheWebPushLimitAtMaximumSize(t *testing.T) {
	const limit = 3800
	jaText := strings.Repeat("あ", 2000)
	asciiText := strings.Repeat("a", 2000)

	for _, tc := range []struct {
		name string
		room bool
		text string
	}{
		{"room / 2000 Japanese characters", true, jaText},
		{"room / 2000 ASCII characters", true, asciiText},
		{"room / no text", true, ""},
		{"direct / 2000 Japanese characters", false, jaText},
		{"direct / 2000 ASCII characters", false, asciiText},
	} {
		t.Run(tc.name, func(t *testing.T) {
			idGen, _ := id.NewGenerator("aidx")
			repo := testutil.NewMockChatRepository()
			chat := corechat.NewService(repo, idGen, nil, heavyLiteFiller{}, nil)
			users := testutil.NewMockUserRepository()
			sender := maxRemoteUser3438("sender1")
			owner := maxRemoteUser3438("owner_1")
			recipient := &model.User{ID: "local1", Username: "local1"}
			for _, u := range []*model.User{sender, owner, recipient} {
				users.Users[u.ID] = u
			}
			chat.SetUserRepo(users)
			enq := &fakeEnqueuer{}
			chat.SetMainStreamPublisher(noopMain3438{})
			chat.SetChatPusher(webpush.NewService(enq))

			if tc.room {
				roomID := idGen.Generate(timeNow3438())
				repo.Rooms[roomID] = &model.ChatRoom{
					ID: roomID, OwnerID: owner.ID, Owner: owner,
					Name:        strings.Repeat("部", 256),
					Description: strings.Repeat("説", 2048),
				}
				repo.Memberships[sender.ID+":"+roomID] = &model.ChatRoomMembership{UserID: sender.ID, RoomID: roomID}
				repo.Memberships[recipient.ID+":"+roomID] = &model.ChatRoomMembership{UserID: recipient.ID, RoomID: roomID}
				_, err := chat.CreateMessageToRoom(context.Background(), sender.ID, roomID, tc.text, "")
				require.NoError(t, err)
			} else {
				_, err := chat.CreateMessageViaAP(context.Background(), "https://remote.example/m/1", sender, recipient.ID, tc.text, tc.text)
				require.NoError(t, err)
			}

			require.NotEmpty(t, enq.calls)
			for _, call := range enq.calls {
				t.Logf("push body: %d B", len(call.Body))
				assert.LessOrEqual(t, len(call.Body), limit, "push body must fit the Web Push limit")
				var body map[string]any
				require.NoError(t, json.Unmarshal(call.Body, &body))
				from, ok := body["fromUser"].(map[string]any)
				require.True(t, ok, "the service worker reads fromUser")
				assert.Equal(t, *sender.Name, from["name"])
				assert.NotEmpty(t, from["avatarUrl"])
				assert.NotContains(t, body, "toUser")
				if tc.room {
					toRoom, ok := body["toRoom"].(map[string]any)
					require.True(t, ok)
					assert.Equal(t, strings.Repeat("部", 256), toRoom["name"])
				}
				text, _ := body["text"].(string)
				if tc.text == "" {
					assert.Empty(t, text)
					continue
				}
				// 切り詰めても先頭は残る。
				require.NotEmpty(t, text, "text must not be dropped entirely")
				assert.True(t, utf8.ValidString(text))
				head := strings.TrimSuffix(text, "…")
				assert.True(t, strings.HasPrefix(tc.text, head), "the head of the text must be kept")
			}
		})
	}
}

type noopMain3438 struct{}

func (noopMain3438) PublishMainEvent(string, string, any) {}

func timeNow3438() time.Time { return time.Now() }

// TestChatRoomInvitationPush_BodyFitsTheWebPushLimit pins the Web Push of a
// chatRoomInvitationReceived notification built by the real notification Hook
// and the real webpush Service: it carries the inviter as UserLite but not the
// invitation (so neither the room's description nor its owner), and stays
// within the Web Push size limit (3800 B) for an inviter with every pushed
// column at its maximum length (#3438).
//
// 本家は packed 通知 (invitation に room と owner を含む) をそのまま push するので、
// description の長い room では上限を超える。Elythia の push は buildPushBody で
// 組み、種類ごとの埋め込み (invitation など) を載せない。SW の composeNotification
// はこの種類を扱わない (null を返して空の通知になる) ので、読む項目は無い。
func TestChatRoomInvitationPush_BodyFitsTheWebPushLimit(t *testing.T) {
	const limit = 3800
	ctx := context.Background()
	testRedis.FlushAll(ctx)
	idGen, _ := id.NewGenerator("aidx")
	notifSvc := notification.NewService(testRedis.Client, idGen, "")
	notifSvc.SetUnreadPublishDelay(0)
	users := testutil.NewMockUserRepository()
	inviter := maxRemoteUser3438("inviter")
	invitee := &model.User{ID: "invitee", Username: "invitee", UsernameLower: "invitee"}
	users.Users[inviter.ID] = inviter
	users.Users[invitee.ID] = invitee
	hook := notification.NewHook(notifSvc, users)
	enq := &fakeEnqueuer{}
	hook.SetWebPushPublisher(webpush.NewService(enq))
	hook.SetPackers(webpush.NewUserRepoPacker(users, nil), nil)

	hook.OnChatRoomInvitationReceived(invitee.ID, inviter.ID, "inv1")

	require.Len(t, enq.calls, 1)
	call := enq.calls[0]
	assert.Equal(t, webpush.TypeNotification, call.Type)
	t.Logf("push body: %d B", len(call.Body))
	assert.LessOrEqual(t, len(call.Body), limit, "push body must fit the Web Push limit")
	var body map[string]any
	require.NoError(t, json.Unmarshal(call.Body, &body))
	assert.Equal(t, "chatRoomInvitationReceived", body["type"])
	assert.NotContains(t, body, "invitation", "the invitation (room description / owner) is not pushed")
	user, ok := body["user"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, inviter.ID, user["id"])
}
