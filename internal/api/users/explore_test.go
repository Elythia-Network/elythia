package users

import (
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
)

func addExplorable(repo *testutil.MockUserRepository, id, username string, host *string) *model.User {
	u := &model.User{
		ID: id, Username: username, UsernameLower: username,
		Host: host, IsExplorable: true,
		AvatarDecorations: datatypes.JSON([]byte("[]")),
	}
	repo.Users[id] = u
	return u
}

func TestList(t *testing.T) {
	t.Run("returns explorable users", func(t *testing.T) {
		h, repo := newTestHandler(t)
		h.SetUserRepo(repo)
		addExplorable(repo, "u1", "alice", nil)

		rec := postStub(h.List, `{}`, nil)
		require.Equal(t, http.StatusOK, rec.Code)

		var got []map[string]any
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
		require.Len(t, got, 1)
		assert.Equal(t, "alice", got[0]["username"])
	})

	t.Run("rejects a state outside the upstream enum", func(t *testing.T) {
		// upstream の state enum は ['all','alive']。moderator / admin を
		// 通すと ListUsers の role filter に到達してしまう (#1996)。
		h, repo := newTestHandler(t)
		h.SetUserRepo(repo)

		rec := postStub(h.List, `{"state":"moderator"}`, nil)
		assert.Equal(t, http.StatusBadRequest, rec.Code)
		assert.Contains(t, rec.Body.String(), "INVALID_PARAM")
	})

	t.Run("mistyped param returns 400 INVALID_PARAM", func(t *testing.T) {
		// 本家は paramDef の ajv 検査で 400 にする (#3330)。読めない場合に
		// 空配列で返すのは DB の失敗だけ。
		h, repo := newTestHandler(t)
		h.SetUserRepo(repo)

		rec := postStub(h.List, `{"limit":"10"}`, nil)
		assert.Equal(t, http.StatusBadRequest, rec.Code)
		assert.Contains(t, rec.Body.String(), "INVALID_PARAM")
	})

	t.Run("unwired repo returns an empty array", func(t *testing.T) {
		h, _ := newTestHandler(t)

		rec := postStub(h.List, `{}`, nil)
		assert.Equal(t, http.StatusOK, rec.Code)
		assert.JSONEq(t, `[]`, rec.Body.String())
	})
}

func TestPinnedUsers(t *testing.T) {
	t.Run("resolves acct entries from meta", func(t *testing.T) {
		h, repo := newTestHandler(t)
		h.SetUserRepo(repo)
		addExplorable(repo, "u1", "alice", nil)

		meta := testutil.NewMockMetaRepository()
		meta.Meta = &model.Meta{PinnedUsers: model.StringArray{"@alice"}}
		h.SetMetaRepo(meta)

		rec := postStub(h.PinnedUsers, `{}`, nil)
		require.Equal(t, http.StatusOK, rec.Code)

		var got []map[string]any
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
		require.Len(t, got, 1)
		assert.Equal(t, "alice", got[0]["username"])
	})

	t.Run("skips entries that cannot be resolved", func(t *testing.T) {
		// pinnedUsers は管理者が手で書く文字列。typo や退会済みが混ざっても
		// 残りは返す。1 件のせいで全体が空になるほうが困る。
		h, repo := newTestHandler(t)
		h.SetUserRepo(repo)
		addExplorable(repo, "u1", "alice", nil)

		meta := testutil.NewMockMetaRepository()
		meta.Meta = &model.Meta{PinnedUsers: model.StringArray{"@nobody", "@alice", "@"}}
		h.SetMetaRepo(meta)

		rec := postStub(h.PinnedUsers, `{}`, nil)
		var got []map[string]any
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
		require.Len(t, got, 1)
		assert.Equal(t, "alice", got[0]["username"])
	})

	t.Run("empty pinnedUsers returns an empty array", func(t *testing.T) {
		h, repo := newTestHandler(t)
		h.SetUserRepo(repo)
		meta := testutil.NewMockMetaRepository()
		meta.Meta = &model.Meta{}
		h.SetMetaRepo(meta)

		rec := postStub(h.PinnedUsers, `{}`, nil)
		assert.JSONEq(t, `[]`, rec.Body.String())
	})

	t.Run("meta fetch failure returns an empty array", func(t *testing.T) {
		// meta を引けなくても about ページを開けるようにする。
		h, repo := newTestHandler(t)
		h.SetUserRepo(repo)
		h.SetMetaRepo(testutil.NewMockMetaRepository()) // Meta が nil = ErrNotFound

		rec := postStub(h.PinnedUsers, `{}`, nil)
		assert.JSONEq(t, `[]`, rec.Body.String())
	})

	t.Run("unwired meta returns an empty array", func(t *testing.T) {
		h, repo := newTestHandler(t)
		h.SetUserRepo(repo)

		rec := postStub(h.PinnedUsers, `{}`, nil)
		assert.JSONEq(t, `[]`, rec.Body.String())
	})
}

func TestParseAcct(t *testing.T) {
	local := "own.example"

	tests := []struct {
		name     string
		acct     string
		wantUser string
		wantHost *string
	}{
		{"bare username", "alice", "alice", nil},
		{"leading at", "@alice", "alice", nil},
		{"own host resolves to local", "@alice@own.example", "alice", nil},
		{"host is lowercased", "@alice@REMOTE.example", "alice", strptr("remote.example")},
		{"remote host", "@alice@remote.example", "alice", strptr("remote.example")},
		{"empty", "", "", nil},
		{"at only", "@", "", nil},
		{"missing username", "@@remote.example", "", nil},
		{"trailing at means local", "@alice@", "alice", nil},
		{"surrounding space is trimmed", "  @alice  ", "alice", nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotUser, gotHost := ParseAcct(tt.acct, local)
			assert.Equal(t, tt.wantUser, gotUser)
			if tt.wantHost == nil {
				assert.Nil(t, gotHost)
				return
			}
			require.NotNil(t, gotHost)
			assert.Equal(t, *tt.wantHost, *gotHost)
		})
	}
}

// **localHost 側も小文字化する。** `url.Parse(config.URL).Host` は host を
// 正規化しないので、`config.url` に大文字が混ざる構成では経路ごとに local /
// remote の判定が食い違う。実際 avatar 側だけが小文字化していて、
// pinned-users から `@user@Own.Example` が黙って消えていた (#2791)。
func TestParseAcct_MixedCaseLocalHost(t *testing.T) {
	// **localHost に大文字を入れる。** 呼び出し側が既に小文字化した値を渡す
	// ケースだけだと、この正規化を外す変異が生き残る (実際に生き残った)。
	for _, local := range []string{"Own.Example", "OWN.EXAMPLE"} {
		t.Run(local, func(t *testing.T) {
			user, host := ParseAcct("@alice@own.example", local)
			assert.Equal(t, "alice", user)
			assert.Nil(t, host, "自ホストなのに remote 判定になっている")
		})
	}

	// 別ホストは大文字小文字を問わず remote のまま。
	user, host := ParseAcct("@alice@remote.example", "Own.Example")
	assert.Equal(t, "alice", user)
	require.NotNil(t, host)
	assert.Equal(t, "remote.example", *host)
}

func strptr(s string) *string { return &s }

// countingOnlineRepo returns a fixed online count.
//
// **MockUserRepository の CountOnlineUsers は 0 固定**なので、そのままだと
// 「repo を呼ばず 0 を返す」実装でもテストが通る (実際に空振りしていた)。
type countingOnlineRepo struct {
	*testutil.MockUserRepository
	n    int64
	err  error
	call int
}

func (r *countingOnlineRepo) CountOnlineUsers() (int64, error) {
	r.call++
	return r.n, r.err
}

func TestOnlineCount(t *testing.T) {
	t.Run("returns the repo count", func(t *testing.T) {
		h, repo := newTestHandler(t)
		online := &countingOnlineRepo{MockUserRepository: repo, n: 42}
		h.SetUserRepo(online)

		rec := postStub(h.OnlineCount, `{}`, nil)
		require.Equal(t, http.StatusOK, rec.Code)

		assert.JSONEq(t, `{"count":42}`, rec.Body.String())
		assert.Equal(t, 1, online.call, "repo を呼んでいない")
	})

	t.Run("repo failure still returns 200 with whatever came back", func(t *testing.T) {
		// **エラーを握って 200 を返す** (元の `count, _ :=` と同じ)。ここで 500 に
		// すると admin overview 全体が開かなくなる。値は repo が返したものを
		// そのまま使う — GORM の Count は失敗時に 0 のままなので実害は無い。
		h, repo := newTestHandler(t)
		online := &countingOnlineRepo{MockUserRepository: repo, n: 7, err: errors.New("boom")}
		h.SetUserRepo(online)

		rec := postStub(h.OnlineCount, `{}`, nil)
		assert.Equal(t, http.StatusOK, rec.Code)
		assert.JSONEq(t, `{"count":7}`, rec.Body.String())
	})

	t.Run("unwired repo reports zero, not an error", func(t *testing.T) {
		// ここで 500 にすると admin overview 全体が開かなくなる。
		h, _ := newTestHandler(t)

		rec := postStub(h.OnlineCount, `{}`, nil)
		assert.Equal(t, http.StatusOK, rec.Code)
		assert.JSONEq(t, `{"count":0}`, rec.Body.String())
	})
}
