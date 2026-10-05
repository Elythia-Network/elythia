package emojiapplications_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/elythia-network/elythia/internal/api/emojiapplications"
	"github.com/elythia-network/elythia/internal/model"
)

// 「自分の申請」の画像プレビュー (#2989)。**参照先は申請の状態と種別で決まる。**
// 常に `fileId` を見る形だと、リモート申請は画像が出ず、承認済みは元ファイルの
// 削除に巻き込まれる。

// previewEmojis resolves both lookups the preview needs.
type previewEmojis struct {
	byID      *model.Emoji
	byName    *model.Emoji
	idErr     error
	nameErr   error
	askedID   string
	askedName string
	askedHost *string
}

func (p *previewEmojis) FindByID(id string) (*model.Emoji, error) {
	p.askedID = id
	if p.idErr != nil {
		return nil, p.idErr
	}
	if p.byID == nil {
		return nil, gorm.ErrRecordNotFound
	}
	return p.byID, nil
}

func (p *previewEmojis) FindByNameAndHost(name string, host *string) (*model.Emoji, error) {
	p.askedName, p.askedHost = name, host
	if p.nameErr != nil {
		return nil, p.nameErr
	}
	if p.byName == nil {
		return nil, gorm.ErrRecordNotFound
	}
	return p.byName, nil
}

// previewOf runs ListMine for one row and returns its preview object.
func previewOf(t *testing.T, app model.EmojiApplication, files emojiapplications.DriveFileLookup, emojis emojiapplications.EmojiLookup) map[string]any {
	t.Helper()
	h := emojiapplications.NewHandler(nil, &stubApps{rows: []model.EmojiApplication{app}}, files)
	if emojis != nil {
		h.SetEmojiLookup(emojis)
	}
	rec := doPost(h.ListMine, `{}`, alice)
	require.Equal(t, http.StatusOK, rec.Code)

	var body []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Len(t, body, 1)
	p, ok := body[0]["preview"].(map[string]any)
	require.Truef(t, ok, "preview が返っていない: %v", body[0])
	return p
}

func strp(s string) *string { return &s }

// --- 未承認 + 自作画像 → 申請元の drive ファイル ---

func TestPreview_PendingOwn_UsesApplicationFile(t *testing.T) {
	for _, status := range []string{"pending", "rejected", "canceled"} {
		p := previewOf(t,
			model.EmojiApplication{ID: "a1", Status: status, FileID: strp("f1")},
			&stubFiles{file: &model.DriveFile{ID: "f1", URL: "https://local/f1.png"}}, nil)
		assert.Equal(t, "applicationFile", p["source"], status)
		assert.Equal(t, "available", p["state"], status)
		assert.Equal(t, "https://local/f1.png", p["url"], status)
	}
}

// 元ファイルが消えていたら「削除済み」。**申請の経緯は読めるので一覧は落とさない。**
func TestPreview_Own_FileGone(t *testing.T) {
	p := previewOf(t,
		model.EmojiApplication{ID: "a1", Status: "rejected", FileID: strp("f1")},
		&stubFiles{}, nil)
	assert.Equal(t, "applicationFile", p["source"])
	assert.Equal(t, "sourceGone", p["state"])
	assert.Equal(t, "", p["url"])
}

// **DB 障害を「削除済み」に丸めない (#2792)。** 丸めると、残っている画像に
// ついて利用者が「消えた」と判断して再申請する。
func TestPreview_Own_DBFailureIsUnknown(t *testing.T) {
	p := previewOf(t,
		model.EmojiApplication{ID: "a1", Status: "pending", FileID: strp("f1")},
		&stubFiles{err: errors.New("db down")}, nil)
	assert.Equal(t, "unknown", p["state"])
}

// --- 未承認 + リモート → remoteHost + remoteName ---

func TestPreview_PendingRemote_UsesRemoteEmoji(t *testing.T) {
	emojis := &previewEmojis{byName: &model.Emoji{ID: "e9", PublicURL: "https://remote/e9.webp"}}
	p := previewOf(t,
		model.EmojiApplication{ID: "a1", Status: "pending", RemoteHost: strp("other.example"), RemoteName: strp("sushi")},
		nil, emojis)
	assert.Equal(t, "remoteEmoji", p["source"])
	assert.Equal(t, "available", p["state"])
	assert.Equal(t, "https://remote/e9.webp", p["url"])
	assert.Equal(t, "sushi", emojis.askedName)
	require.NotNil(t, emojis.askedHost)
	assert.Equal(t, "other.example", *emojis.askedHost)
	assert.Empty(t, emojis.askedID, "未承認のリモート申請で emojiId を引いている")
}

func TestPreview_Remote_SourceGone(t *testing.T) {
	p := previewOf(t,
		model.EmojiApplication{ID: "a1", Status: "canceled", RemoteHost: strp("other.example"), RemoteName: strp("sushi")},
		nil, &previewEmojis{})
	assert.Equal(t, "remoteEmoji", p["source"])
	assert.Equal(t, "sourceGone", p["state"])
}

func TestPreview_Remote_DBFailureIsUnknown(t *testing.T) {
	p := previewOf(t,
		model.EmojiApplication{ID: "a1", Status: "pending", RemoteHost: strp("other.example"), RemoteName: strp("sushi")},
		nil, &previewEmojis{nameErr: errors.New("db down")})
	assert.Equal(t, "unknown", p["state"])
}

// publicUrl が空なら originalUrl (リモート絵文字の行は publicUrl が空のことがある)。
func TestPreview_Remote_FallsBackToOriginalURL(t *testing.T) {
	p := previewOf(t,
		model.EmojiApplication{ID: "a1", Status: "pending", RemoteHost: strp("o.example"), RemoteName: strp("s")},
		nil, &previewEmojis{byName: &model.Emoji{ID: "e9", OriginalURL: "https://remote/orig.png"}})
	assert.Equal(t, "https://remote/orig.png", p["url"])
}

// --- 承認済み → emojiId のローカル絵文字 ---

// **承認済みは申請元を見ない。** #2966 以降、承認画像は system 所有の drive
// ファイルへ複製されるので、申請者が元ファイルを消しても出せる。
func TestPreview_Approved_UsesApprovedEmoji(t *testing.T) {
	emojis := &previewEmojis{byID: &model.Emoji{ID: "e1", PublicURL: "https://local/e1.webp"}}
	// 申請元は消えている (files は見つからない stub)。
	p := previewOf(t,
		model.EmojiApplication{ID: "a1", Status: "approved", FileID: strp("f1"), EmojiID: strp("e1")},
		&stubFiles{}, emojis)
	assert.Equal(t, "approvedEmoji", p["source"])
	assert.Equal(t, "available", p["state"])
	assert.Equal(t, "https://local/e1.webp", p["url"])
	assert.Equal(t, "e1", emojis.askedID)
	assert.Empty(t, emojis.askedName, "承認済みなのにリモート元を引いている")
}

// リモート由来の承認済みでも、参照先は承認後のローカル絵文字。
func TestPreview_ApprovedRemote_UsesApprovedEmoji(t *testing.T) {
	emojis := &previewEmojis{byID: &model.Emoji{ID: "e1", PublicURL: "https://local/e1.webp"}}
	p := previewOf(t,
		model.EmojiApplication{
			ID: "a1", Status: "approved", EmojiID: strp("e1"),
			RemoteHost: strp("other.example"), RemoteName: strp("sushi"),
		}, nil, emojis)
	assert.Equal(t, "approvedEmoji", p["source"])
	assert.Equal(t, "e1", emojis.askedID)
	assert.Empty(t, emojis.askedName, "承認済みなのにリモート元を引いている")
}

// **承認後に絵文字が消された場合は専用の state。** 「申請元が消えた」ではない。
func TestPreview_Approved_EmojiGone(t *testing.T) {
	p := previewOf(t,
		model.EmojiApplication{ID: "a1", Status: "approved", EmojiID: strp("e1")},
		nil, &previewEmojis{})
	assert.Equal(t, "approvedEmoji", p["source"])
	assert.Equal(t, "approvedEmojiGone", p["state"])
}

func TestPreview_Approved_DBFailureIsUnknown(t *testing.T) {
	p := previewOf(t,
		model.EmojiApplication{ID: "a1", Status: "approved", EmojiID: strp("e1")},
		nil, &previewEmojis{idErr: errors.New("db down")})
	assert.Equal(t, "unknown", p["state"])
}

// lookup 未配線では「消えた」と断定しない。
func TestPreview_UnwiredLookupIsUnknown(t *testing.T) {
	p := previewOf(t,
		model.EmojiApplication{ID: "a1", Status: "approved", EmojiID: strp("e1")}, nil, nil)
	assert.Equal(t, "unknown", p["state"])

	p = previewOf(t,
		model.EmojiApplication{ID: "a1", Status: "pending", RemoteHost: strp("o"), RemoteName: strp("s")}, nil, nil)
	assert.Equal(t, "unknown", p["state"])
}

// **`url` は返さない (#2989)。** 空文字だけでは理由を区別できず、クライアントが
// 推測することになる。
func TestPreview_ReplacesTheOldURLField(t *testing.T) {
	h := emojiapplications.NewHandler(nil, &stubApps{rows: []model.EmojiApplication{
		{ID: "a1", Status: "pending", FileID: strp("f1")},
	}}, &stubFiles{file: &model.DriveFile{ID: "f1", URL: "https://local/f1.png"}})
	rec := doPost(h.ListMine, `{}`, alice)
	var body []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Len(t, body, 1)
	assert.NotContains(t, body[0], "url")
}

// **承認済みなのに emojiId が無い行は「消えた」と断定しない。** 承認の記録
// として壊れている状態で、絵文字が削除されたわけではない (migration は FK を
// 張らないので、削除されても id は残る)。
func TestPreview_Approved_WithoutEmojiIDIsUnknown(t *testing.T) {
	p := previewOf(t,
		model.EmojiApplication{ID: "a1", Status: "approved"},
		nil, &previewEmojis{byID: &model.Emoji{ID: "e1", PublicURL: "u"}})
	assert.Equal(t, "approvedEmoji", p["source"])
	assert.Equal(t, "unknown", p["state"])

	empty := ""
	p = previewOf(t,
		model.EmojiApplication{ID: "a1", Status: "approved", EmojiID: &empty},
		nil, &previewEmojis{byID: &model.Emoji{ID: "e1", PublicURL: "u"}})
	assert.Equal(t, "unknown", p["state"])
}
