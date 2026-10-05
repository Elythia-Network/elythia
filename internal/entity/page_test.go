package entity

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPackPage_Basic(t *testing.T) {
	idGen := newTestIDGen(t)
	pageID := idGen.Generate(time.Now())
	summary := "a summary"
	imgID := "img_1"
	updated := time.Date(2026, 4, 10, 1, 2, 3, 0, time.UTC)

	p := &model.Page{
		ID:                  pageID,
		UpdatedAt:           updated,
		Title:               "Hello",
		Name:                "hello",
		Summary:             &summary,
		AlignCenter:         true,
		HideTitleWhenPinned: false,
		Font:                "sans-serif",
		UserID:              "u1",
		EyeCatchingImageID:  &imgID,
		Content:             []byte(`[{"x":1}]`),
		Variables:           []byte(`[]`),
		Script:              "",
		Visibility:          model.PageVisibilityPublic,
		LikedCount:          3,
	}

	out := PackPage(p, idGen)
	assert.Equal(t, pageID, out["id"])
	assert.Equal(t, "Hello", out["title"])
	assert.Equal(t, "hello", out["name"])
	assert.Equal(t, &summary, out["summary"])
	assert.Equal(t, true, out["alignCenter"])
	assert.Equal(t, false, out["hideTitleWhenPinned"])
	assert.Equal(t, "sans-serif", out["font"])
	assert.Equal(t, "u1", out["userId"])
	assert.Equal(t, &imgID, out["eyeCatchingImageId"])
	assert.Equal(t, json.RawMessage(`[{"x":1}]`), out["content"])
	assert.Equal(t, json.RawMessage(`[]`), out["variables"])
	assert.Equal(t, "public", out["visibility"])
	assert.Equal(t, 3, out["likedCount"])
	assert.Equal(t, "2026-04-10T01:02:03.000Z", out["updatedAt"])
	// attachedFiles は misskey_dart の Page.fromJson が非null List 必須なので
	// 常に出ること (#1237)。
	assert.Equal(t, []any{}, out["attachedFiles"])
	// createdAtはaidx-IDから復元されるためkey自体が存在することだけ確認。
	_, ok := out["createdAt"]
	assert.True(t, ok)
}

// 空 / nil の variables カラムでも null ではなく [] を返すこと (#1237)。
// misskey_dart の Page.fromJson は variables を非null List として cast する。
func TestPackPage_EmptyVariablesIsArray(t *testing.T) {
	p := &model.Page{ID: "bogus", Visibility: model.PageVisibilityPublic, Variables: nil}
	out := PackPage(p)
	assert.Equal(t, []any{}, out["variables"])
	assert.Equal(t, []any{}, out["attachedFiles"])
}

func TestPackPage_NilInput(t *testing.T) {
	assert.Nil(t, PackPage(nil))
}

func TestPackPage_WithoutIDGen_OmitsCreatedAt(t *testing.T) {
	p := &model.Page{ID: "bogus-id", Visibility: model.PageVisibilityPublic}
	out := PackPage(p)
	_, ok := out["createdAt"]
	assert.False(t, ok)
}

func TestPackPage_InvalidIDOmitsCreatedAt(t *testing.T) {
	idGen := newTestIDGen(t)
	p := &model.Page{ID: "not-a-valid-aidx", Visibility: model.PageVisibilityPublic}
	out := PackPage(p, idGen)
	_, ok := out["createdAt"]
	assert.False(t, ok)
}

func TestRawJSONBytes(t *testing.T) {
	assert.Nil(t, rawJSONBytes(nil))
	assert.Nil(t, rawJSONBytes([]byte{}))
	assert.Equal(t, json.RawMessage(`[1]`), rawJSONBytes([]byte(`[1]`)))
}

// TestPackPageWithContext_AttachesOwnerAndOptionalFields guards #1134:
// owner と option field が正しく entity に乗ること、optional は nil で omit
// されることを確認する。
func TestPackPageWithContext_AttachesOwnerAndOptionalFields(t *testing.T) {
	idGen := newTestIDGen(t)
	p := &model.Page{ID: idGen.Generate(time.Now()), UserID: "u1", Visibility: model.PageVisibilityPublic}
	liked := true
	out := PackPageWithContext(p, PackPageContext{
		IDGen:            idGen,
		Owner:            &model.User{ID: "u1", Username: "alice", UsernameLower: "alice"},
		EyeCatchingImage: map[string]any{"id": "f1"},
		IsLiked:          &liked,
	})
	user, ok := out["user"].(UserLite)
	require.True(t, ok)
	assert.Equal(t, "alice", user.Username)
	assert.Equal(t, map[string]any{"id": "f1"}, out["eyeCatchingImage"])
	assert.Equal(t, true, out["isLiked"])
}

func TestPackPageWithContext_OmitsAbsentOptionals(t *testing.T) {
	idGen := newTestIDGen(t)
	p := &model.Page{ID: idGen.Generate(time.Now()), UserID: "u1", Visibility: model.PageVisibilityPublic}
	out := PackPageWithContext(p, PackPageContext{IDGen: idGen})
	// Owner=nil → user field 自体を omit (null 出しすると frontend が
	// page.user.username で再 throw するため)。isLiked も viewer 無し時は omit
	// (golden isLiked? は optional)。
	_, hasUser := out["user"]
	assert.False(t, hasUser)
	_, hasLiked := out["isLiked"]
	assert.False(t, hasLiked)
	// 一方 eyeCatchingImage は golden で present 必須 (DriveFile | null) なので、
	// 画像が無くても null で present にする (omit しない) (#1264 follow-up)。
	eye, hasEye := out["eyeCatchingImage"]
	assert.True(t, hasEye, "eyeCatchingImage must be present (golden: DriveFile|null)")
	assert.Nil(t, eye, "eyeCatchingImage is null when no image")
}

func TestPackPageWithContext_NilPage(t *testing.T) {
	assert.Nil(t, PackPageWithContext(nil, PackPageContext{}))
}

// #1662 CollectPageImageFileIDs は image block の fileId を document 順 (children
// 再帰込み) で返す。
func TestCollectPageImageFileIDs(t *testing.T) {
	content := []byte(`[
		{"type":"image","fileId":"a"},
		{"type":"text"},
		{"type":"section","children":[{"type":"image","fileId":"b"},{"type":"section","children":[{"type":"image","fileId":"c"}]}]},
		{"type":"image"}
	]`)
	assert.Equal(t, []string{"a", "b", "c"}, CollectPageImageFileIDs(content))
	assert.Nil(t, CollectPageImageFileIDs([]byte(`not json`)))
	assert.Nil(t, CollectPageImageFileIDs(nil))
}

// #1773: legacy `type:'input'` block を pack 時に textInput/numberInput へ migrate
// する (upstream PageEntityService.migrate)。
func TestMigratePageContent_TextInput(t *testing.T) {
	out := migratePageContent([]byte(`[{"type":"input","inputType":"text","title":"t"}]`))
	var got []map[string]any
	b, _ := json.Marshal(out)
	require.NoError(t, json.Unmarshal(b, &got))
	assert.Equal(t, "textInput", got[0]["type"])
}

// numberInput は default を parseInt(base10) で整数化する ("7.9" -> 7)。
func TestMigratePageContent_NumberInputParsesDefault(t *testing.T) {
	out := migratePageContent([]byte(`[{"type":"input","inputType":"number","default":"7.9"}]`))
	var got []map[string]any
	b, _ := json.Marshal(out)
	require.NoError(t, json.Unmarshal(b, &got))
	assert.Equal(t, "numberInput", got[0]["type"])
	assert.Equal(t, float64(7), got[0]["default"]) // JSON number は float64 に decode
}

// 非数値 default は parseInt が NaN になり JSON.stringify(NaN)=null と一致する。
func TestMigratePageContent_NumberInputDefaultNaNNull(t *testing.T) {
	out := migratePageContent([]byte(`[{"type":"input","inputType":"number","default":"abc"}]`))
	var got []map[string]any
	b, _ := json.Marshal(out)
	require.NoError(t, json.Unmarshal(b, &got))
	v, has := got[0]["default"]
	require.True(t, has)
	assert.Nil(t, v)
}

// children を再帰的に walk して入れ子の input block も migrate する。
func TestMigratePageContent_RecursesChildren(t *testing.T) {
	out := migratePageContent([]byte(`[{"type":"section","children":[{"type":"input","inputType":"text"}]}]`))
	var got []map[string]any
	b, _ := json.Marshal(out)
	require.NoError(t, json.Unmarshal(b, &got))
	child := got[0]["children"].([]any)[0].(map[string]any)
	assert.Equal(t, "textInput", child["type"])
}

// modern content (input block 無し) は再 marshal せず stored bytes を verbatim 返す。
func TestMigratePageContent_ModernUnchangedVerbatim(t *testing.T) {
	raw := []byte(`[{"type":"text","text":"hello"},{"type":"numberInput","default":3}]`)
	out := migratePageContent(raw)
	rm, ok := out.(json.RawMessage)
	require.True(t, ok, "modern content must be returned verbatim as RawMessage")
	assert.Equal(t, raw, []byte(rm))
}

// inputType が text/number 以外なら type を書き換えず verbatim 返す。
func TestMigratePageContent_UnknownInputTypeVerbatim(t *testing.T) {
	raw := []byte(`[{"type":"input","inputType":"color"}]`)
	out := migratePageContent(raw)
	rm, ok := out.(json.RawMessage)
	require.True(t, ok)
	assert.Equal(t, raw, []byte(rm))
}

// 空は []、array でない / 不正 JSON は verbatim。
func TestMigratePageContent_EmptyAndInvalid(t *testing.T) {
	assert.Equal(t, []any{}, migratePageContent(nil))
	out := migratePageContent([]byte(`not json`))
	rm, ok := out.(json.RawMessage)
	require.True(t, ok)
	assert.Equal(t, []byte(`not json`), []byte(rm))
}

// stubPageDriveLookup is a minimal PageDriveFileLookup for ResolvePageDriveFiles.
type stubPageDriveLookup struct{ files map[string]*model.DriveFile }

func (s stubPageDriveLookup) FindByIDs(ids []string) ([]*model.DriveFile, error) {
	out := make([]*model.DriveFile, 0, len(ids))
	for _, id := range ids {
		if f, ok := s.files[id]; ok {
			out = append(out, f)
		}
	}
	return out, nil
}

func (s stubPageDriveLookup) FindByID(id string) (*model.DriveFile, error) {
	if f, ok := s.files[id]; ok {
		return f, nil
	}
	return nil, assert.AnError
}

// #1662 ResolvePageDriveFiles は owner-scope の image-block file を解決し、
// eyeCatchingImage を id から pack する。lookup nil は (nil, nil)。
func TestResolvePageDriveFiles(t *testing.T) {
	idGen := newTestIDGen(t)
	alice, bob := "alice", "bob"
	lookup := stubPageDriveLookup{files: map[string]*model.DriveFile{
		"f1":  {ID: "f1", UserID: &alice, Name: "a.png", Type: "image/png"},
		"f2":  {ID: "f2", UserID: &bob, Name: "b.png", Type: "image/png"}, // owner mismatch
		"eye": {ID: "eye", UserID: &alice, Name: "e.png", Type: "image/png"},
	}}
	eyeID := "eye"
	p := &model.Page{ID: "p1", UserID: "alice", EyeCatchingImageID: &eyeID,
		Content: []byte(`[{"type":"image","fileId":"f1"},{"type":"image","fileId":"f2"}]`)}

	attached, eye := ResolvePageDriveFiles(p, lookup, idGen)
	require.Len(t, attached, 1, "owner alice の f1 のみ。bob の f2 は除外")
	assert.Equal(t, "f1", attached[0].(DriveFileEntity).ID)
	require.NotNil(t, eye)
	assert.Equal(t, "eye", eye.(DriveFileEntity).ID)

	// lookup nil → (nil, nil) で default 維持。
	a2, e2 := ResolvePageDriveFiles(p, nil, idGen)
	assert.Nil(t, a2)
	assert.Nil(t, e2)
}
