package signin_test

import (
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/api/signin"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type ipRecordCall struct{ userID, ip string }

type ipFixture struct {
	h          *signin.Handler
	userID     string
	seen       func() []ipRecordCall
	signinRepo *testutil.MockSigninRepository
}

// 失敗したサインインの IP を `user_ip` に記録しないことを固定する (#3135)。
//
// **これは関連アカウント検索 (#3105) の前提。** あちらの関連度は `user_ip` の
// 観測だけを見るので、失敗ログインの IP がここに入ると**第三者が他人の関連候補を
// 作れてしまう** — 攻撃者が対象アカウントの ID で自分の IP から失敗を繰り返せば、
// その IP が対象の「使用した IP」として記録され、攻撃者自身のアカウントが候補に
// 並ぶ。#3066 が完了条件に挙げているのはこの攻撃面。
//
// **`SigninFlow` を先に見る。** `/api/signin` は `docs/divergence.md` が書いて
// いるとおり upstream に無い mk-go の互換 shim で、同梱フロントは `signin-flow`
// しか呼ばない。**攻撃されるのはそちら。**
//
// **「呼ぶ場所がここだけ」は静的ゲートが持つ**
// (`internal/entitycompat/ip_record_sites_gate_test.go`)。振る舞いだけでは叩いた
// 経路しか守れず (実測: `/api/signin` だけのテストは `SigninFlow` への記録追加を
// 素通りさせた)、静的ゲートだけでは「その経路が本当に失敗として扱われているか」が
// 分からないので、両方で挟む。
func TestSignin_FailedAttemptsDoNotRecordIP(t *testing.T) {
	newFixture := func(t *testing.T) ipFixture {
		t.Helper()
		h, repo := newTestHandler(t)
		user := createTestUser(repo, "testuser", "password123")
		// **`signinRepo` を配線する。** 無いと `fail()` が中で分岐して goroutine を
		// 起動しなくなり、本番と違う経路を測ることになる。記録は
		// `go h.recordSignin(...)` で非同期だが、mock は mutex 付きの `Len()` を
		// 公開しているので `require.Eventually` で待てる (同 package の
		// `handler_test.go` が既にその形で待っている)。**待たないと空虚化に
		// 気付けない** — 将来 `fail()` に入る前に 403 を返すようになっても、
		// IP は記録されないままなのでアサーションは通ってしまう。
		signinRepo := testutil.NewMockSigninRepository()
		idGen, err := id.NewGenerator("aidx")
		require.NoError(t, err)
		h.SetSigninRepo(signinRepo, idGen)

		var mu sync.Mutex
		var calls []ipRecordCall
		h.SetIPRecorder(&mockIPRecorder{fn: func(userID, ip string) {
			mu.Lock()
			defer mu.Unlock()
			calls = append(calls, ipRecordCall{userID, ip})
		}})
		return ipFixture{
			h:          h,
			userID:     user.ID,
			signinRepo: signinRepo,
			seen: func() []ipRecordCall {
				mu.Lock()
				defer mu.Unlock()
				return append([]ipRecordCall(nil), calls...)
			},
		}
	}

	endpoints := []struct {
		name string
		pick func(*signin.Handler) func(echo.Context) error
	}{
		{"signin-flow", func(h *signin.Handler) func(echo.Context) error { return h.SigninFlow }},
		{"signin", func(h *signin.Handler) func(echo.Context) error { return h.Signin }},
	}

	// **endpoint ごとに fixture を作り直す。** 共有すると、前の subtest が拾った
	// 漏れで後続が落ちて「どの経路が漏らしたか」が分からなくなる (記録を非同期に
	// する変異が、偶然そこで落ちたせいで見逃されかけた)。
	for _, ep := range endpoints {
		t.Run(ep.name+"/wrong password", func(t *testing.T) {
			f := newFixture(t)
			rec := doPost(ep.pick(f.h), `{"username":"testuser","password":"wrong"}`)
			require.Equal(t, http.StatusForbidden, rec.Code)
			// **`fail()` に届いたことを確かめる。** ここが空振りすると「IP を
			// 記録しない」は自明に真になる。
			require.Eventually(t, func() bool { return f.signinRepo.Len() == 1 },
				2*time.Second, 10*time.Millisecond, "`fail()` が失敗を記録していない")
			assert.Empty(t, f.seen(), "失敗したサインインの IP を記録している")
		})

		t.Run(ep.name+"/unknown user", func(t *testing.T) {
			f := newFixture(t)
			rec := doPost(ep.pick(f.h), `{"username":"nosuchuser","password":"password123"}`)
			// **「200 でなければよい」にしない。** handler が別の理由 (400 など) で
			// 早期 return しても緑になり、測りたい分岐に入らなくなる (実測で
			// 404 が返る。この経路は利用者が解決できないので `fail()` は通らない)。
			require.Equal(t, http.StatusNotFound, rec.Code)
			assert.Empty(t, f.seen(), "存在しない利用者のサインイン試行で IP を記録している")
		})

		t.Run(ep.name+"/success records the caller ip", func(t *testing.T) {
			// **成功側も見る。** 失敗側だけだと、recorder を丸ごと配線し忘れた
			// 状態 (= 何も記録されない) が緑で通る。**IP の値まで見る** — 争点は
			// 「誰の IP が誰に紐づくか」なので、記録されたこと自体では足りない。
			f := newFixture(t)
			rec := doPost(ep.pick(f.h), `{"username":"testuser","password":"password123"}`)
			require.Equal(t, http.StatusOK, rec.Code)
			got := f.seen()
			require.Len(t, got, 1, "成功したサインインの IP が記録されていない")
			assert.Equal(t, f.userID, got[0].userID)
			// `httptest.NewRequest` の既定 RemoteAddr (`192.0.2.1:1234`) から
			// `c.RealIP()` が返す値。
			assert.Equal(t, "192.0.2.1", got[0].ip, "呼び出し元と別の IP を記録している")
		})
	}
}
