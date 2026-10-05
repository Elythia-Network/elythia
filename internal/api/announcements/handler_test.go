package announcements_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/api/announcements"
	"github.com/elythia-network/elythia/internal/core/moderationlog"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/server/middleware"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestHandler(t *testing.T) (*announcements.Handler, *testutil.MockAnnouncementRepository) {
	t.Helper()
	repo := testutil.NewMockAnnouncementRepository()
	idGen, _ := id.NewGenerator("aidx")
	return announcements.NewHandler(repo, idGen), repo
}

func doPost(h func(echo.Context) error, body string, user *model.User) *httptest.ResponseRecorder {
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	if user != nil {
		c.Set(string(middleware.UserContextKey), user)
	}
	_ = h(c)
	return rec
}

// --- Public ---

func TestList_Empty(t *testing.T) {
	h, _ := newTestHandler(t)
	rec := doPost(h.List, `{}`, nil)
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestList_WithItems(t *testing.T) {
	h, repo := newTestHandler(t)
	// 有効なAIDX IDを使ってcreatedAt解析パスをカバー
	idGen, _ := id.NewGenerator("aidx")
	validID := idGen.Generate(javaTime())
	repo.Items[validID] = &model.Announcement{ID: validID, Title: "Hi", Text: "Hello", IsActive: true}
	rec := doPost(h.List, `{}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusOK, rec.Code)
	var resp []any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Len(t, resp, 1)
	item := resp[0].(map[string]any)
	assert.NotEmpty(t, item["createdAt"])
}

func javaTime() time.Time {
	return time.Date(2024, 6, 15, 12, 0, 0, 0, time.UTC)
}

// #1955: 匿名 (requireCredential:false) の List では isRead key を省略する。
func TestList_AnonymousOmitsIsRead(t *testing.T) {
	h, repo := newTestHandler(t)
	repo.Items["a1"] = &model.Announcement{ID: "a1", Title: "Hi", Text: "Hello", IsActive: true}
	rec := doPost(h.List, `{}`, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	var resp []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp, 1)
	_, hasIsRead := resp[0]["isRead"]
	assert.False(t, hasIsRead, "匿名 List には isRead key を出さない (#1955)")
}

// #1955: authed List は item ごとに実 read 状態を isRead に反映する。
func TestList_AuthenticatedReflectsReadState(t *testing.T) {
	h, repo := newTestHandler(t)
	repo.Items["a_read"] = &model.Announcement{ID: "a_read", Title: "R", Text: "read", IsActive: true}
	repo.Items["a_unread"] = &model.Announcement{ID: "a_unread", Title: "U", Text: "unread", IsActive: true}
	require.NoError(t, repo.MarkRead(&model.AnnouncementRead{UserID: "u1", AnnouncementID: "a_read"}))

	rec := doPost(h.List, `{}`, &model.User{ID: "u1"})
	require.Equal(t, http.StatusOK, rec.Code)
	var resp []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	byID := map[string]map[string]any{}
	for _, it := range resp {
		byID[it["id"].(string)] = it
	}
	require.Contains(t, byID, "a_read")
	require.Contains(t, byID, "a_unread")
	assert.Equal(t, true, byID["a_read"]["isRead"], "既読 item は isRead:true (#1955)")
	assert.Equal(t, false, byID["a_unread"]["isRead"], "未読 item は isRead:false (#1955)")
}

func TestList_InvalidJSON(t *testing.T) {
	h, _ := newTestHandler(t)
	rec := doPost(h.List, `invalid`, nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestList_ActiveFalse(t *testing.T) {
	h, repo := newTestHandler(t)
	repo.Items["a1"] = &model.Announcement{ID: "a1", IsActive: false}
	f := false
	_ = f
	rec := doPost(h.List, `{"isActive":false}`, nil)
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestList_UntilID(t *testing.T) {
	h, repo := newTestHandler(t)
	idGen, _ := id.NewGenerator("aidx")
	id1 := idGen.Generate(time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC))
	id2 := idGen.Generate(time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC))
	id3 := idGen.Generate(time.Date(2024, 12, 1, 0, 0, 0, 0, time.UTC))
	repo.Items[id1] = &model.Announcement{ID: id1, Title: "Old", Text: "t", IsActive: true}
	repo.Items[id2] = &model.Announcement{ID: id2, Title: "Mid", Text: "t", IsActive: true}
	repo.Items[id3] = &model.Announcement{ID: id3, Title: "New", Text: "t", IsActive: true}

	// untilId=id3 → id3より古いものだけ返る（id1, id2）
	rec := doPost(h.List, `{"untilId":"`+id3+`"}`, nil)
	assert.Equal(t, http.StatusOK, rec.Code)
	var resp []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Len(t, resp, 2)
	for _, item := range resp {
		assert.NotEqual(t, id3, item["id"])
	}
}

func TestList_SinceID(t *testing.T) {
	h, repo := newTestHandler(t)
	idGen, _ := id.NewGenerator("aidx")
	id1 := idGen.Generate(time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC))
	id2 := idGen.Generate(time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC))
	id3 := idGen.Generate(time.Date(2024, 12, 1, 0, 0, 0, 0, time.UTC))
	repo.Items[id1] = &model.Announcement{ID: id1, Title: "Old", Text: "t", IsActive: true}
	repo.Items[id2] = &model.Announcement{ID: id2, Title: "Mid", Text: "t", IsActive: true}
	repo.Items[id3] = &model.Announcement{ID: id3, Title: "New", Text: "t", IsActive: true}

	// sinceId=id1 → id1より新しいものだけ返る（id2, id3）
	rec := doPost(h.List, `{"sinceId":"`+id1+`"}`, nil)
	assert.Equal(t, http.StatusOK, rec.Code)
	var resp []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Len(t, resp, 2)
	for _, item := range resp {
		assert.NotEqual(t, id1, item["id"])
	}
}

// --- ReadAnnouncement ---

func TestReadAnnouncement_Success(t *testing.T) {
	h, repo := newTestHandler(t)
	repo.Items["a1"] = &model.Announcement{ID: "a1", IsActive: true}
	rec := doPost(h.ReadAnnouncement, `{"announcementId":"a1"}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusNoContent, rec.Code)
}

func TestReadAnnouncement_AlreadyRead(t *testing.T) {
	h, repo := newTestHandler(t)
	repo.Items["a1"] = &model.Announcement{ID: "a1"}
	doPost(h.ReadAnnouncement, `{"announcementId":"a1"}`, &model.User{ID: "u1"})
	rec := doPost(h.ReadAnnouncement, `{"announcementId":"a1"}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusNoContent, rec.Code)
}

// #1767: upstream read-announcement は不明な id を silent 204 で返す
// (errors:{}、insert の FK 違反を握り潰す)。404 NO_SUCH_ANNOUNCEMENT は返さない。
func TestReadAnnouncement_UnknownIsNoOp(t *testing.T) {
	h, _ := newTestHandler(t)
	rec := doPost(h.ReadAnnouncement, `{"announcementId":"ghost"}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusNoContent, rec.Code)
}

// #1767: 自分宛て per-user announcement は初回 read で isActive=false に
// deactivate する (upstream AnnouncementService.read:214-217)。
func TestReadAnnouncement_DeactivatesOwnPerUser(t *testing.T) {
	h, repo := newTestHandler(t)
	uid := "u1"
	repo.Items["a1"] = &model.Announcement{ID: "a1", UserID: &uid, IsActive: true}
	rec := doPost(h.ReadAnnouncement, `{"announcementId":"a1"}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.False(t, repo.Items["a1"].IsActive, "自分宛て per-user は read で deactivate")
}

// #1767: 他ユーザー宛て per-user は silent 204 で、isActive も変えない。
func TestReadAnnouncement_ForeignPerUserIsNoOp(t *testing.T) {
	h, repo := newTestHandler(t)
	other := "u2"
	repo.Items["a1"] = &model.Announcement{ID: "a1", UserID: &other, IsActive: true}
	rec := doPost(h.ReadAnnouncement, `{"announcementId":"a1"}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.True(t, repo.Items["a1"].IsActive, "他ユーザー宛ては deactivate しない")
}

func TestReadAnnouncement_InvalidParam(t *testing.T) {
	h, _ := newTestHandler(t)
	rec := doPost(h.ReadAnnouncement, `{}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// --- Admin ---

func TestAdminCreate_Success(t *testing.T) {
	h, repo := newTestHandler(t)
	rec := doPost(h.AdminCreate, `{"title":"News","text":"Big news!","imageUrl":null}`, nil)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Len(t, repo.Items, 1)
	// #1545: レスポンスは packed で createdAt (ID 由来) を含む。
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	createdAt, ok := resp["createdAt"].(string)
	require.True(t, ok, "create response must include createdAt")
	assert.NotEmpty(t, createdAt)
}

// #1977: admin/announcements/create のレスポンスは upstream の wire 実体 (= pack の
// full object) と一致し、imageUrl だけ raw を返す。upstream は `packed` をそのまま返し
// res schema (6 フィールド) では strip されないため、forYou/isRead 等は wire に含まれる。
// 唯一 imageUrl は upstream が raw を返すので mk-go も proxy しない。
func TestAdminCreate_ResponseImageURLRaw(t *testing.T) {
	h, _ := newTestHandler(t)
	rec := doPost(h.AdminCreate, `{"title":"News","text":"x","imageUrl":"https://remote.example/i.png"}`, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	// imageUrl は raw (proxy しない)。これが #1977 の本質的な修正点。
	assert.Equal(t, "https://remote.example/i.png", resp["imageUrl"], "imageUrl は raw (proxy しない、#1977)")
	// upstream wire は full pack なので forYou/isRead 等は含まれる (strip しない)。
	assert.Contains(t, resp, "forYou", "upstream wire は full pack (forYou を含む、#1977)")
	assert.Contains(t, resp, "isRead")
	for _, k := range []string{"id", "createdAt", "updatedAt", "title", "text", "imageUrl"} {
		assert.Contains(t, resp, k, "create response に %s が必要", k)
	}
}

func TestAdminCreate_InvalidParam(t *testing.T) {
	h, _ := newTestHandler(t)
	rec := doPost(h.AdminCreate, `{}`, nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// PR #1108 sweep: icon が upstream enum 外の値で送られたら 400 reject。
func TestAdminCreate_InvalidIconReturns400(t *testing.T) {
	h, _ := newTestHandler(t)
	rec := doPost(h.AdminCreate, `{"title":"X","text":"Y","icon":"weird","imageUrl":null}`, nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// display が upstream enum 外の値で送られたら 400 reject。
func TestAdminCreate_InvalidDisplayReturns400(t *testing.T) {
	h, _ := newTestHandler(t)
	rec := doPost(h.AdminCreate, `{"title":"X","text":"Y","display":"weird","imageUrl":null}`, nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// 新規 fields (forExistingUsers / silence / needConfirmationToRead) が persist される。
func TestAdminCreate_PersistsExtraBoolFields(t *testing.T) {
	h, repo := newTestHandler(t)
	rec := doPost(h.AdminCreate,
		`{"title":"X","text":"Y","imageUrl":null,"forExistingUsers":true,"silence":true,"needConfirmationToRead":true}`, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Len(t, repo.Items, 1)
	var a *model.Announcement
	for _, v := range repo.Items {
		a = v
	}
	assert.True(t, a.ForExistingUsers)
	assert.True(t, a.Silence)
	assert.True(t, a.NeedConfirmationToRead)
}

// stubMainStreamPublisher captures PublishMainEvent calls.
type stubMainStreamPublisher struct {
	calls []mainEventCall
}

type mainEventCall struct {
	userID    string
	eventType string
	body      any
}

func (s *stubMainStreamPublisher) PublishMainEvent(userID, eventType string, body any) {
	s.calls = append(s.calls, mainEventCall{userID, eventType, body})
}

func TestAdminCreate_PerUserPublishesAnnouncementCreated(t *testing.T) {
	h, _ := newTestHandler(t)
	pub := &stubMainStreamPublisher{}
	h.SetMainStreamPublisher(pub)

	rec := doPost(h.AdminCreate, `{"title":"You","text":"Hi","userId":"u1","imageUrl":null}`, nil)
	require.Equal(t, http.StatusOK, rec.Code)

	require.Len(t, pub.calls, 1)
	assert.Equal(t, "u1", pub.calls[0].userID)
	assert.Equal(t, "announcementCreated", pub.calls[0].eventType)
	body, ok := pub.calls[0].body.(map[string]any)
	require.True(t, ok)
	// TSと同じく {announcement: <packed>} でラップ。
	packed, ok := body["announcement"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "You", packed["title"])
	// #2106 L24/L52: create event は me-less pack なので forYou=false。
	assert.Equal(t, false, packed["forYou"])
	assert.Equal(t, false, packed["isRead"])
}

func TestAdminCreate_GlobalDoesNotPublish(t *testing.T) {
	h, _ := newTestHandler(t)
	pub := &stubMainStreamPublisher{}
	h.SetMainStreamPublisher(pub)

	// userId 未指定 → global announcement。main にはemitしない (TSの
	// publishBroadcastStream相当は別レイヤ)。
	rec := doPost(h.AdminCreate, `{"title":"All","text":"Everyone","imageUrl":null}`, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Empty(t, pub.calls)
}

func TestReadAnnouncement_PublishesReadAllWhenUnreadEmpty(t *testing.T) {
	h, repo := newTestHandler(t)
	pub := &stubMainStreamPublisher{}
	h.SetMainStreamPublisher(pub)
	repo.Items["a1"] = &model.Announcement{ID: "a1", IsActive: true}

	// 唯一のannouncementを読むとunread=0になるのでreadAllAnnouncementsがemit。
	rec := doPost(h.ReadAnnouncement, `{"announcementId":"a1"}`, &model.User{ID: "u1"})
	require.Equal(t, http.StatusNoContent, rec.Code)

	require.Len(t, pub.calls, 1)
	assert.Equal(t, "u1", pub.calls[0].userID)
	assert.Equal(t, "readAllAnnouncements", pub.calls[0].eventType)
	assert.Nil(t, pub.calls[0].body)
}

func TestReadAnnouncement_DoesNotPublishWhenUnreadRemains(t *testing.T) {
	h, repo := newTestHandler(t)
	pub := &stubMainStreamPublisher{}
	h.SetMainStreamPublisher(pub)
	repo.Items["a1"] = &model.Announcement{ID: "a1", IsActive: true}
	repo.Items["a2"] = &model.Announcement{ID: "a2", IsActive: true}

	// a1 を読んでも a2 が未読のためemit無し。
	rec := doPost(h.ReadAnnouncement, `{"announcementId":"a1"}`, &model.User{ID: "u1"})
	require.Equal(t, http.StatusNoContent, rec.Code)
	assert.Empty(t, pub.calls)
}

func TestReadAnnouncement_IgnoresOtherUsersPerUserAnnouncements(t *testing.T) {
	// 他ユーザー宛のper-user announcementは自分のunread countに含まれない
	// ので、それ以外を全て読んだら readAllAnnouncements が emit される。
	h, repo := newTestHandler(t)
	pub := &stubMainStreamPublisher{}
	h.SetMainStreamPublisher(pub)
	otherUID := "other"
	repo.Items["a1"] = &model.Announcement{ID: "a1", IsActive: true}                    // global
	repo.Items["a2"] = &model.Announcement{ID: "a2", IsActive: true, UserID: &otherUID} // 他ユーザー宛

	rec := doPost(h.ReadAnnouncement, `{"announcementId":"a1"}`, &model.User{ID: "u1"})
	require.Equal(t, http.StatusNoContent, rec.Code)
	require.Len(t, pub.calls, 1)
	assert.Equal(t, "readAllAnnouncements", pub.calls[0].eventType)
}

func TestList_Unauthenticated_ExcludesAllPerUserAnnouncements(t *testing.T) {
	h, repo := newTestHandler(t)
	otherUID := "other"
	idGen, _ := id.NewGenerator("aidx")
	aid1 := idGen.Generate(javaTime())
	aid2 := idGen.Generate(javaTime())
	repo.Items[aid1] = &model.Announcement{ID: aid1, Title: "G", IsActive: true}
	// per-user announcement は未認証ユーザーには一切見せない。
	repo.Items[aid2] = &model.Announcement{ID: aid2, Title: "ForOther", IsActive: true, UserID: &otherUID}

	rec := doPost(h.List, `{}`, nil) // 未認証
	assert.Equal(t, http.StatusOK, rec.Code)
	var resp []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp, 1)
	assert.Equal(t, "G", resp[0]["title"])
}

func TestList_AuthenticatedUser_ExcludesOtherUsersPerUserAnnouncements(t *testing.T) {
	h, repo := newTestHandler(t)
	otherUID := "other"
	myUID := "u1"
	// global / 自分宛 / 他人宛 の3種
	idGen, _ := id.NewGenerator("aidx")
	aid1 := idGen.Generate(javaTime())
	aid2 := idGen.Generate(javaTime())
	aid3 := idGen.Generate(javaTime())
	repo.Items[aid1] = &model.Announcement{ID: aid1, Title: "G", IsActive: true}
	repo.Items[aid2] = &model.Announcement{ID: aid2, Title: "Mine", IsActive: true, UserID: &myUID}
	repo.Items[aid3] = &model.Announcement{ID: aid3, Title: "Other", IsActive: true, UserID: &otherUID}

	rec := doPost(h.List, `{}`, &model.User{ID: myUID})
	assert.Equal(t, http.StatusOK, rec.Code)
	var resp []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	// aid3 は他ユーザー宛なので除外される (2件: global + 自分宛)。
	titles := make(map[string]bool)
	for _, item := range resp {
		titles[item["title"].(string)] = true
	}
	assert.True(t, titles["G"])
	assert.True(t, titles["Mine"])
	assert.False(t, titles["Other"])
}

func TestAdminUpdate_Success(t *testing.T) {
	h, repo := newTestHandler(t)
	repo.Items["a1"] = &model.Announcement{ID: "a1", Title: "Old"}
	rec := doPost(h.AdminUpdate, `{"id":"a1","title":"New"}`, nil)
	assert.Equal(t, http.StatusNoContent, rec.Code)
}

func TestAdminUpdate_AllFields(t *testing.T) {
	h, repo := newTestHandler(t)
	repo.Items["a1"] = &model.Announcement{ID: "a1", Title: "Old", IsActive: true}
	rec := doPost(h.AdminUpdate, `{"id":"a1","title":"New","text":"txt","isActive":false}`, nil)
	assert.Equal(t, http.StatusNoContent, rec.Code)
}

// PR #1108 sweep: 旧 mk-go は icon / display / imageUrl / forExistingUsers /
// silence / needConfirmationToRead を AdminUpdate で受け取っておらず、admin
// UI で変えても DB に反映されない drop-in regression があった。本 test
// で全 6 field の persist を guard する (= PR #1102 RolesUpdate と同種の
// guard pattern)。
func TestAdminUpdate_PersistsExtendedFields(t *testing.T) {
	h, repo := newTestHandler(t)
	repo.Items["a1"] = &model.Announcement{ID: "a1", Title: "Old", Icon: "info", Display: "normal"}
	rec := doPost(h.AdminUpdate,
		`{"id":"a1","imageUrl":"https://example.com/i.png","icon":"warning","display":"banner","forExistingUsers":true,"silence":true,"needConfirmationToRead":true}`,
		nil)
	require.Equal(t, http.StatusNoContent, rec.Code)
	a := repo.Items["a1"]
	require.NotNil(t, a.ImageURL)
	assert.Equal(t, "https://example.com/i.png", *a.ImageURL)
	assert.Equal(t, "warning", a.Icon)
	assert.Equal(t, "banner", a.Display)
	assert.True(t, a.ForExistingUsers)
	assert.True(t, a.Silence)
	assert.True(t, a.NeedConfirmationToRead)
}

// icon が enum 外 → 400 reject。
func TestAdminUpdate_InvalidIconReturns400(t *testing.T) {
	h, repo := newTestHandler(t)
	repo.Items["a1"] = &model.Announcement{ID: "a1", Title: "Old", Icon: "info"}
	rec := doPost(h.AdminUpdate, `{"id":"a1","icon":"weird"}`, nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	// silent corruption しない
	assert.Equal(t, "info", repo.Items["a1"].Icon)
}

// display が enum 外 → 400 reject。
func TestAdminUpdate_InvalidDisplayReturns400(t *testing.T) {
	h, repo := newTestHandler(t)
	repo.Items["a1"] = &model.Announcement{ID: "a1", Title: "Old", Display: "normal"}
	rec := doPost(h.AdminUpdate, `{"id":"a1","display":"weird"}`, nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Equal(t, "normal", repo.Items["a1"].Display)
}

// imageUrl="" は null クリア (color/iconUrl の RolesUpdate と同方針)。
func TestAdminUpdate_ImageURLEmptyClears(t *testing.T) {
	h, repo := newTestHandler(t)
	img := "https://example.com/i.png"
	repo.Items["a1"] = &model.Announcement{ID: "a1", Title: "Old", ImageURL: &img}
	rec := doPost(h.AdminUpdate, `{"id":"a1","imageUrl":""}`, nil)
	require.Equal(t, http.StatusNoContent, rec.Code)
	assert.Nil(t, repo.Items["a1"].ImageURL)
}

func TestAdminUpdate_InvalidParam(t *testing.T) {
	h, _ := newTestHandler(t)
	rec := doPost(h.AdminUpdate, `{}`, nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestAdminDelete_Success(t *testing.T) {
	h, repo := newTestHandler(t)
	repo.Items["a1"] = &model.Announcement{ID: "a1"}
	rec := doPost(h.AdminDelete, `{"id":"a1"}`, nil)
	assert.Equal(t, http.StatusNoContent, rec.Code)
}

func TestAdminDelete_InvalidParam(t *testing.T) {
	h, _ := newTestHandler(t)
	rec := doPost(h.AdminDelete, `{}`, nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestAdminList_Success(t *testing.T) {
	h, repo := newTestHandler(t)
	repo.Items["a1"] = &model.Announcement{ID: "a1", IsActive: true}
	rec := doPost(h.AdminList, `{}`, nil)
	assert.Equal(t, http.StatusOK, rec.Code)
}

// #1967: admin/announcements/list は upstream の生 row 形 (forYou/isRead 無し、
// imageUrl は raw)。public list 用の packAnnouncement を流用しない。
func TestAdminList_ShapeMatchesUpstream(t *testing.T) {
	h, repo := newTestHandler(t)
	remoteImg := "https://remote.example/img.png"
	updated := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	// global announcement (userId IS NULL) — mock ListForAdmin は userId 無し要求で
	// global のみ返す。updatedAt non-null で .000Z 形式の経路も踏む。
	repo.Items["a1"] = &model.Announcement{ID: "a1", Title: "t", Text: "x", IsActive: true, ImageURL: &remoteImg, UpdatedAt: &updated}
	rec := doPost(h.AdminList, `{}`, nil)
	require.Equal(t, http.StatusOK, rec.Code)

	var resp []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp, 1)
	row := resp[0]
	assert.Equal(t, "2026-01-02T03:04:05.000Z", row["updatedAt"], "updatedAt は toISOString 互換 .000Z 形式 (#1967)")
	_, hasForYou := row["forYou"]
	assert.False(t, hasForYou, "admin/list に forYou key は出さない (#1967)")
	_, hasIsRead := row["isRead"]
	assert.False(t, hasIsRead, "admin/list に isRead key は出さない (#1967)")
	// imageUrl は proxy せず raw を返す。
	assert.Equal(t, remoteImg, row["imageUrl"], "imageUrl は raw (proxy しない、#1967)")
	// upstream の生 row フィールドは揃う (userId は global なので null)。
	assert.Contains(t, row, "userId")
	assert.Nil(t, row["userId"])
	assert.Contains(t, row, "reads")
	assert.Contains(t, row, "forExistingUsers")
}

// /admin/user/<id> のお知らせタブが userId で narrow したときに、対象
// ユーザー宛 announcement のみ返り、サーバー全体 announcement (userId
// IS NULL) が混ざらないこと (#472)。
func TestAdminList_UserScopeExcludesGlobal(t *testing.T) {
	h, repo := newTestHandler(t)
	target := "u_target"
	other := "u_other"
	repo.Items["a_global"] = &model.Announcement{ID: "a_global", IsActive: true}
	repo.Items["a_target"] = &model.Announcement{ID: "a_target", IsActive: true, UserID: &target}
	repo.Items["a_other"] = &model.Announcement{ID: "a_other", IsActive: true, UserID: &other}

	rec := doPost(h.AdminList, `{"userId":"u_target","status":"active"}`, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	var rows []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &rows))
	require.Len(t, rows, 1)
	assert.Equal(t, "a_target", rows[0]["id"])
	assert.Equal(t, "u_target", rows[0]["userId"])
}

// userId 未指定なら upstream 通り userId IS NULL の global only を返す。
func TestAdminList_NoUserIDReturnsGlobalOnly(t *testing.T) {
	h, repo := newTestHandler(t)
	target := "u_target"
	repo.Items["a_global"] = &model.Announcement{ID: "a_global", IsActive: true}
	repo.Items["a_target"] = &model.Announcement{ID: "a_target", IsActive: true, UserID: &target}

	rec := doPost(h.AdminList, `{"status":"active"}`, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	var rows []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &rows))
	require.Len(t, rows, 1)
	assert.Equal(t, "a_global", rows[0]["id"])
}

// status="archived" を指定すると isActive=false のみ返る。
func TestAdminList_StatusArchived(t *testing.T) {
	h, repo := newTestHandler(t)
	repo.Items["a_active"] = &model.Announcement{ID: "a_active", IsActive: true}
	repo.Items["a_archived"] = &model.Announcement{ID: "a_archived", IsActive: false}

	rec := doPost(h.AdminList, `{"status":"archived"}`, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	var rows []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &rows))
	require.Len(t, rows, 1)
	assert.Equal(t, "a_archived", rows[0]["id"])
}

// reads count が announcement_read 行数で埋まること。
func TestAdminList_ReadsCount(t *testing.T) {
	h, repo := newTestHandler(t)
	repo.Items["a1"] = &model.Announcement{ID: "a1", IsActive: true}
	require.NoError(t, repo.MarkRead(&model.AnnouncementRead{ID: "r1", UserID: "u1", AnnouncementID: "a1"}))
	require.NoError(t, repo.MarkRead(&model.AnnouncementRead{ID: "r2", UserID: "u2", AnnouncementID: "a1"}))

	rec := doPost(h.AdminList, `{"status":"active"}`, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	var rows []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &rows))
	require.Len(t, rows, 1)
	assert.EqualValues(t, 2, rows[0]["reads"])
}

func TestAdminList_InvalidJSON(t *testing.T) {
	h, _ := newTestHandler(t)
	rec := doPost(h.AdminList, `invalid`, nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// --- Failing repo tests ---

type failingListAnnouncementRepo struct {
	*testutil.MockAnnouncementRepository
}

func (f *failingListAnnouncementRepo) List(_ bool, _, _ int, _, _ string) ([]*model.Announcement, error) {
	return nil, assert.AnError
}

func (f *failingListAnnouncementRepo) ListGlobal(_ bool, _, _ int, _, _ string) ([]*model.Announcement, error) {
	return nil, assert.AnError
}

func (f *failingListAnnouncementRepo) ListForUser(_ string, _ bool, _, _ int, _, _ string) ([]*model.Announcement, error) {
	return nil, assert.AnError
}

func (f *failingListAnnouncementRepo) ListForAdmin(_, _ string, _, _ int, _, _ string) ([]*model.Announcement, error) {
	return nil, assert.AnError
}

func TestList_Error(t *testing.T) {
	idGen, _ := id.NewGenerator("aidx")
	h := announcements.NewHandler(&failingListAnnouncementRepo{testutil.NewMockAnnouncementRepository()}, idGen)
	rec := doPost(h.List, `{}`, nil)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

func TestAdminList_Error(t *testing.T) {
	idGen, _ := id.NewGenerator("aidx")
	h := announcements.NewHandler(&failingListAnnouncementRepo{testutil.NewMockAnnouncementRepository()}, idGen)
	rec := doPost(h.AdminList, `{}`, nil)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

type failingCreateAnnouncementRepo struct {
	*testutil.MockAnnouncementRepository
}

func (f *failingCreateAnnouncementRepo) Create(_ *model.Announcement) error { return assert.AnError }

func TestAdminCreate_Error(t *testing.T) {
	idGen, _ := id.NewGenerator("aidx")
	h := announcements.NewHandler(&failingCreateAnnouncementRepo{testutil.NewMockAnnouncementRepository()}, idGen)
	rec := doPost(h.AdminCreate, `{"title":"x","text":"y","imageUrl":null}`, nil)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

type failingMarkReadRepo struct {
	*testutil.MockAnnouncementRepository
}

func (f *failingMarkReadRepo) MarkRead(_ *model.AnnouncementRead) error { return assert.AnError }

type failingUpdateAnnouncementRepo struct {
	*testutil.MockAnnouncementRepository
}

func (f *failingUpdateAnnouncementRepo) UpdateFields(_ string, _ map[string]any) error {
	return assert.AnError
}

func TestAdminUpdate_Error(t *testing.T) {
	idGen, _ := id.NewGenerator("aidx")
	h := announcements.NewHandler(&failingUpdateAnnouncementRepo{testutil.NewMockAnnouncementRepository()}, idGen)
	rec := doPost(h.AdminUpdate, `{"id":"x","title":"y"}`, nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

type failingDeleteAnnouncementRepo struct {
	*testutil.MockAnnouncementRepository
}

func (f *failingDeleteAnnouncementRepo) Delete(_ string) error { return assert.AnError }

func TestAdminDelete_Error(t *testing.T) {
	idGen, _ := id.NewGenerator("aidx")
	h := announcements.NewHandler(&failingDeleteAnnouncementRepo{testutil.NewMockAnnouncementRepository()}, idGen)
	rec := doPost(h.AdminDelete, `{"id":"x"}`, nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestReadAnnouncement_MarkReadError(t *testing.T) {
	repo := &failingMarkReadRepo{testutil.NewMockAnnouncementRepository()}
	repo.Items["a1"] = &model.Announcement{ID: "a1"}
	idGen, _ := id.NewGenerator("aidx")
	h := announcements.NewHandler(repo, idGen)
	rec := doPost(h.ReadAnnouncement, `{"announcementId":"a1"}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

// --- moderation log assertions (#662) ---

// attachAnnouncementModLog wires a fresh modlog service backed by the
// race-safe testutil mock and returns it.
func attachAnnouncementModLog(t *testing.T, h *announcements.Handler) *testutil.MockModerationLogRepository {
	t.Helper()
	repo := testutil.NewMockModerationLogRepository()
	gen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	h.SetModLogService(moderationlog.New(repo, gen))
	return repo
}

func TestAdminCreate_WritesGlobalAnnouncementLog(t *testing.T) {
	h, _ := newTestHandler(t)
	repo := attachAnnouncementModLog(t, h)

	rec := doPost(h.AdminCreate, `{"title":"hi","text":"world","imageUrl":null}`, &model.User{ID: "admin1"})
	require.Equal(t, http.StatusOK, rec.Code)

	require.Eventually(t, func() bool { return len(repo.Snapshot()) == 1 }, 500*time.Millisecond, 5*time.Millisecond)
	logs := repo.Snapshot()
	assert.Equal(t, "createGlobalAnnouncement", logs[0].Type)
}

func TestAdminCreate_WritesUserAnnouncementLog(t *testing.T) {
	h, _ := newTestHandler(t)
	userRepo := testutil.NewMockUserRepository()
	userRepo.Users["u1"] = &model.User{ID: "u1", Username: "alice"}
	h.SetUserRepo(userRepo)
	repo := attachAnnouncementModLog(t, h)

	rec := doPost(h.AdminCreate, `{"title":"hi","text":"world","userId":"u1","imageUrl":null}`, &model.User{ID: "admin1"})
	require.Equal(t, http.StatusOK, rec.Code)

	require.Eventually(t, func() bool { return len(repo.Snapshot()) == 1 }, 500*time.Millisecond, 5*time.Millisecond)
	logs := repo.Snapshot()
	assert.Equal(t, "createUserAnnouncement", logs[0].Type)
	var info map[string]any
	require.NoError(t, json.Unmarshal(logs[0].Info, &info))
	assert.Equal(t, "u1", info["userId"])
	assert.Equal(t, "alice", info["userUsername"])
}

func TestAdminUpdate_WritesAnnouncementLog(t *testing.T) {
	h, mockRepo := newTestHandler(t)
	require.NoError(t, mockRepo.Create(&model.Announcement{ID: "a1", Title: "old", Text: "old"}))
	repo := attachAnnouncementModLog(t, h)

	rec := doPost(h.AdminUpdate, `{"id":"a1","title":"new"}`, &model.User{ID: "admin1"})
	require.Equal(t, http.StatusNoContent, rec.Code)

	require.Eventually(t, func() bool { return len(repo.Snapshot()) == 1 }, 500*time.Millisecond, 5*time.Millisecond)
	assert.Equal(t, "updateGlobalAnnouncement", repo.Snapshot()[0].Type)
}

func TestAdminDelete_WritesAnnouncementLog(t *testing.T) {
	h, mockRepo := newTestHandler(t)
	require.NoError(t, mockRepo.Create(&model.Announcement{ID: "a1", Title: "doomed"}))
	repo := attachAnnouncementModLog(t, h)

	rec := doPost(h.AdminDelete, `{"id":"a1"}`, &model.User{ID: "admin1"})
	require.Equal(t, http.StatusNoContent, rec.Code)

	require.Eventually(t, func() bool { return len(repo.Snapshot()) == 1 }, 500*time.Millisecond, 5*time.Millisecond)
	assert.Equal(t, "deleteGlobalAnnouncement", repo.Snapshot()[0].Type)
}

// fakeBroadcastPub records PublishBroadcast calls (#2056)。
type fakeBroadcastPub struct{ events []string }

func (f *fakeBroadcastPub) PublishBroadcast(eventType string, _ any) {
	f.events = append(f.events, eventType)
}

// #2056: global announcement (userId 無し) は broadcast stream へ announcementCreated を流す。
func TestAdminCreate_GlobalBroadcasts(t *testing.T) {
	h, _ := newTestHandler(t)
	bc := &fakeBroadcastPub{}
	h.SetBroadcastPublisher(bc)
	rec := doPost(h.AdminCreate, `{"title":"News","text":"Big news!","imageUrl":null}`, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, []string{"announcementCreated"}, bc.events, "global は broadcast へ (#2056)")
}

// #2101: user-facing List は admin 管理用 field (forExistingUsers / isActive) を
// 露出しない (upstream user-facing pack に揃える)。admin は AdminList が返す。
func TestList_OmitsAdminFields(t *testing.T) {
	h, repo := newTestHandler(t)
	idGen, _ := id.NewGenerator("aidx")
	validID := idGen.Generate(javaTime())
	repo.Items[validID] = &model.Announcement{ID: validID, Title: "Hi", Text: "Hello", IsActive: true, ForExistingUsers: true}
	rec := doPost(h.List, `{}`, &model.User{ID: "u1"})
	require.Equal(t, http.StatusOK, rec.Code)
	var resp []any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp, 1)
	item := resp[0].(map[string]any)
	_, hasForExisting := item["forExistingUsers"]
	_, hasIsActive := item["isActive"]
	assert.False(t, hasForExisting, "user-facing List は forExistingUsers を出さない")
	assert.False(t, hasIsActive, "user-facing List は isActive を出さない")
	assert.Equal(t, "Hi", item["title"], "user-facing field は維持")
}

// upstream paramDef の required (`['title','text','imageUrl']`) を欠く payload は
// 400 INVALID_PARAM で弾く (#2284)。imageUrl は nullable なので `null` は valid
// だが、省略は invalid — この区別のため json.RawMessage で受けている。
func TestAdminCreate_RequiresImageURL(t *testing.T) {
	cases := []struct {
		name string
		body string
		want int
	}{
		{"imageUrl 省略", `{"title":"X","text":"Y"}`, http.StatusBadRequest},
		{"imageUrl null は valid", `{"title":"X","text":"Y","imageUrl":null}`, http.StatusOK},
		{"imageUrl 文字列は valid", `{"title":"X","text":"Y","imageUrl":"https://e.example/i.png"}`, http.StatusOK},
		{"imageUrl の型違いは 400", `{"title":"X","text":"Y","imageUrl":123}`, http.StatusBadRequest},
		{"title 省略", `{"text":"Y","imageUrl":null}`, http.StatusBadRequest},
		{"text 省略", `{"title":"X","imageUrl":null}`, http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := newTestHandler(t)
			rec := doPost(h.AdminCreate, tc.body, nil)
			assert.Equal(t, tc.want, rec.Code)
		})
	}
}
