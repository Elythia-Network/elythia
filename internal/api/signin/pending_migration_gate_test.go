package signin

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/argon2"
)

// 未検証の password を移行対象にしない (#2853)。
//
// **`setPendingPasswordMigration` の呼び出し側を守る。** 引数の `passwordOK` を
// true に固定すると、`h.ok` に到達したときに**一度も検証していない平文**の
// bcrypt ハッシュで保存済みハッシュが上書きされ、利用者が自分のパスワードで
// 締め出される。
//
// 本 PR は passwordOK=false のまま `h.ok` へ至る経路 (passwordless の credential
// 分岐) を 1 本増やしているので、ここを固定しないと回帰に気付けない。
// helper 自体は `TestSetPendingPasswordMigration_RequiresVerifiedPassword` が
// 見ているが、**呼び出し側の引数**は誰も見ていなかった。
func TestSigninFlow_DoesNotStageMigrationForUnverifiedPassword(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	tok := "Tk-1"
	// **正規の profile でないと OutcomeUnsupported になり、枠の経路に入らない。**
	// 初版は digest を短くした偽物を置いたせいで空振りしていた (実測)。
	salt := []byte("0123456789abcdef")
	digest := argon2.IDKey([]byte("never-verified"), salt, 3, 64*1024, 4, 32)
	stored := fmt.Sprintf("$argon2id$v=19$m=65536,t=3,p=4$%s$%s",
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(digest))
	repo.Users["u1"] = &model.User{ID: "u1", Username: "alice", UsernameLower: "alice", Token: &tok}
	repo.Profiles["u1"] = &model.UserProfile{
		UserID:               "u1",
		Password:             &stored,
		UsePasswordLessLogin: true,
		TwoFactorEnabled:     true,
	}
	h := NewHandler(repo)

	// ctx をキャンセルすると Argon2id 経路が OutcomeUnavailable になる。
	// tolerate 条件が成立するので 503 ではなく credential 分岐へ落ちる。
	e := echo.New()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	body := `{"username":"alice","password":"never-verified","credential":{"id":"x"}}`
	req := httptest.NewRequest(http.MethodPost, "/api/signin-flow", strings.NewReader(body)).WithContext(ctx)
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	require.NoError(t, h.SigninFlow(c))

	// **踏んだ経路も固定する。** `pendingPasswordMigrationKey` はほぼ全ての
	// 早期 return でも nil なので、これだけだと SigninFlow が手前で return する
	// ようになったとき黙って空振りに変わる (初版は実際に 404 で空振りしていた)。
	require.Equal(t, http.StatusForbidden, rec.Code, "credential 分岐に到達していない: %s", rec.Body.String())
	require.Contains(t, rec.Body.String(), "93b86c4b-72f9-40eb-9815-798928603d1e")

	assert.Nil(t, c.Get(pendingPasswordMigrationKey),
		"検証していない password が移行対象として積まれている")
}
