package server

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/config"
	"github.com/elythia-network/elythia/internal/model"
)

// **組み立てで作った`notehide.Hider`が、匿名の閲覧者へ
// `ugcVisibilityForVisitor: none`を効かせること(#3786)。**
//
// 以前は`notehide.SetUGCVisibilityLookup`でパッケージの変数に1度登録していた。
// 組み立てで作るHiderに公開範囲の読み方を持たせたので、Hiderにその読み方を
// 渡し忘れると、匿名の閲覧者に投稿の中身が見える(未配線は隠さない側に倒れる)。
// handlerがHiderを受け取ったかは、構築子の必須の引数(#3817、notesは#3831)と
// `TestBuiltHandlersHaveNoNilDependencies`が見るが、Hiderそのものの中身は
// そこでは見えないので、実際の経路で確かめる。
//
// 匿名で`none`のとき、notes/showや一覧はhandler自身が拒否するか空を返すので、
// notehideまで届かない。届くのは、ローカルの利用者のプロフィール(users/show)の
// ピン留めの投稿で、プロフィールは返り、投稿の中身だけが隠れる。
//
// notes/showの拒否もあわせて見る。notesのhandlerは公開範囲の読み方を構築子で
// 受ける(#3831)。nilでないことは`TestBuiltHandlersHaveNoNilDependencies`が見るが、
// 空文字を返す読み方は`all`と同じ扱いになって拒否が消えるので、実際の経路で確かめる。
func TestNewServer_NoteHiderAppliesTheVisitorPolicy(t *testing.T) {
	db := serverIntegrationDB
	if db == nil {
		t.Skip("PostgreSQL unavailable")
	}
	var meta struct{ UgcVisibilityForVisitor string }
	require.NoError(t, db.Raw(`SELECT "ugcVisibilityForVisitor" AS ugc_visibility_for_visitor FROM "meta" LIMIT 1`).Scan(&meta).Error)
	t.Cleanup(func() {
		require.NoError(t, db.Exec(`UPDATE "meta" SET "ugcVisibilityForVisitor" = ?`, meta.UgcVisibilityForVisitor).Error)
	})

	const (
		userID      = "nhuser3786aaaaaa"
		viewerToken = "nhviewertokn3786"
	)
	idGen := testIDGen(t)
	noteID := idGen.Generate(time.Now())
	pinID := idGen.Generate(time.Now())
	cleanupPluginAccountUsers(t, db, "nh_user", "nh_viewer")
	createLocalUser(t, db, userID, "nh_user", "nhusertokenx3786", "nh-pass", false)
	// ログイン済みのケースは、持ち主本人ではない利用者で見る。本人は自分の投稿を
	// 常にそのまま見られるので、本人で見ても「ログイン済みなら隠さない」の判定に
	// ならない。
	createLocalUser(t, db, "nhviewer3786aaaa", "nh_viewer", viewerToken, "nh-pass", false)
	text := "pinned by nh_user"
	require.NoError(t, db.Create(&model.Note{ID: noteID, UserID: userID, Visibility: model.NoteVisibility("public"), Text: &text}).Error)
	require.NoError(t, db.Create(&model.UserNotePining{ID: pinID, UserID: userID, NoteID: noteID}).Error)

	pinned := func(t *testing.T, srv *Server, token string) map[string]any {
		t.Helper()
		rec := postJSON(t, srv, "/api/users/show", token, map[string]any{"userId": userID})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var u struct {
			PinnedNotes []map[string]any `json:"pinnedNotes"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &u), rec.Body.String())
		require.Len(t, u.PinnedNotes, 1, rec.Body.String())
		return u.PinnedNotes[0]
	}

	for _, tt := range []struct {
		policy string
		token  string
		hidden bool
	}{
		{policy: "none", token: "", hidden: true},
		{policy: "none", token: viewerToken, hidden: false},
		{policy: "all", token: "", hidden: false},
	} {
		name := tt.policy + "/anonymous"
		if tt.token != "" {
			name = tt.policy + "/signed-in"
		}
		t.Run(name, func(t *testing.T) {
			require.NoError(t, db.Exec(`UPDATE "meta" SET "ugcVisibilityForVisitor" = ?`, tt.policy).Error)
			srv := newServerWithConfig(t, func(*config.Config) {})
			n := pinned(t, srv, tt.token)
			assert.Equal(t, tt.hidden, n["isHidden"] == true, "isHidden: %v", n["isHidden"])
			if tt.hidden {
				assert.Nil(t, n["text"], "the content must be blanked")
			} else {
				assert.Equal(t, text, n["text"])
			}

			rec := postJSON(t, srv, "/api/notes/show", tt.token, map[string]any{"noteId": noteID})
			if tt.hidden {
				assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
				assert.Contains(t, rec.Body.String(), "CONTENT_RESTRICTED_BY_SERVER")
			} else {
				assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			}
		})
	}
}
