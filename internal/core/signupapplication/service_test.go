package signupapplication

import (
	"os"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

var testDB *gorm.DB

// testAnswers is the standard set of submitted answers used across the tests.
func testAnswers() []Answer {
	return []Answer{{Label: "参加の動機", Value: "よろしくお願いします"}}
}

func TestMain(m *testing.M) {
	db, err := testutil.OpenTestDB()
	if err != nil {
		panic("failed to open test DB: " + err.Error())
	}
	testDB = db
	testutil.ApplyMigrations(testDB)
	os.Exit(m.Run())
}

// newService builds a Service on a clean table with a fixed clock.
func newService(t *testing.T) (*Service, *time.Time) {
	t.Helper()
	require.NoError(t, testDB.Exec(`DELETE FROM "signup_application"`).Error)
	t.Cleanup(func() { testDB.Exec(`DELETE FROM "signup_application"`) })

	idGen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	now := time.Now().UTC().Truncate(time.Second)
	svc := NewService(repository.NewSignupApplicationRepository(testDB), idGen)
	svc.SetClock(func() time.Time { return now })
	return svc, &now
}

func TestApply_CreatesPendingWithClaimCode(t *testing.T) {
	svc, now := newService(t)

	app, code, err := svc.Apply(testAnswers())
	require.NoError(t, err)
	assert.Equal(t, model.SignupApplicationPending, app.Status)
	assert.Equal(t, now.Add(DefaultTTL), app.ExpiresAt)
	// 回答はラベル付きで保存される (#2570)。
	assert.Contains(t, string(app.Answers), "参加の動機")

	// 256bit を hex にした長さ。
	assert.Len(t, code, 64)
	// **平文は保存しない。** 保存すると DB が漏れた時点で全申請が乗っ取れる。
	assert.Equal(t, HashClaimCode(code), app.ClaimCodeHash)
	assert.NotEqual(t, code, app.ClaimCodeHash)
}

// 連絡先という自然キーが無くなったので、DB は重複申請を妨げない。
// **抑止は captcha とレート制限が担う** (#2569)。
func TestApply_AllowsRepeatedApplications(t *testing.T) {
	svc, _ := newService(t)

	first, code1, err := svc.Apply(testAnswers())
	require.NoError(t, err)
	second, code2, err := svc.Apply(testAnswers())
	require.NoError(t, err)

	assert.NotEqual(t, first.ID, second.ID)
	assert.NotEqual(t, code1, code2, "コードは毎回別")
}

func TestByClaimCode(t *testing.T) {
	svc, now := newService(t)

	app, code, err := svc.Apply(testAnswers())
	require.NoError(t, err)

	t.Run("returns the application", func(t *testing.T) {
		got, err := svc.ByClaimCode(code)
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.Equal(t, app.ID, got.ID)
	})

	// **存在しないコードと期限切れのコードを区別しない。** 区別できると、
	// 総当たりで「そのコードは実在する」ことだけ漏れる。
	t.Run("unknown code is nil, not an error", func(t *testing.T) {
		got, err := svc.ByClaimCode("no-such-code")
		require.NoError(t, err)
		assert.Nil(t, got)
	})

	t.Run("empty code is nil", func(t *testing.T) {
		got, err := svc.ByClaimCode("")
		require.NoError(t, err)
		assert.Nil(t, got)
	})

	t.Run("expires lazily", func(t *testing.T) {
		*now = now.Add(DefaultTTL)
		got, err := svc.ByClaimCode(code)
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.Equal(t, model.SignupApplicationExpired, got.Status)

		stored, err := svc.Get(app.ID)
		require.NoError(t, err)
		assert.Equal(t, model.SignupApplicationExpired, stored.Status, "保存側にも反映されること")
	})
}

func TestApprove(t *testing.T) {
	svc, now := newService(t)

	app, _, err := svc.Apply(testAnswers())
	require.NoError(t, err)
	require.NoError(t, svc.Approve(app.ID, "mod1"))

	stored, err := svc.Get(app.ID)
	require.NoError(t, err)
	assert.Equal(t, model.SignupApplicationApproved, stored.Status)
	require.NotNil(t, stored.ProcessedByID)
	assert.Equal(t, "mod1", *stored.ProcessedByID)
	// 承認時点ではチケットを発行しない (発行するのはメール確認の経路だけ、#2813)。
	assert.Nil(t, stored.TicketID)

	t.Run("cannot approve twice", func(t *testing.T) {
		assert.ErrorIs(t, svc.Approve(app.ID, "mod1"), ErrNotPending)
	})

	t.Run("not found", func(t *testing.T) {
		assert.ErrorIs(t, svc.Approve("no-such-id", "mod1"), ErrNotFound)
	})

	// **ErrNotPending ではなく ErrExpired。** 掃除は遅延反映なので行は pending の
	// まま残っており、まとめると「審査待ちに見えるのに審査待ちではない」になる。
	t.Run("expired cannot be approved", func(t *testing.T) {
		other, _, err := svc.Apply(testAnswers())
		require.NoError(t, err)
		*now = now.Add(DefaultTTL)
		assert.ErrorIs(t, svc.Approve(other.ID, "mod1"), ErrExpired)

		stored, err := svc.Get(other.ID)
		require.NoError(t, err)
		assert.Equal(t, model.SignupApplicationExpired, stored.Status)
		assert.Nil(t, stored.ProcessedByID, "期限切れは審査の結果ではない")
	})
}

func TestReject(t *testing.T) {
	svc, now := newService(t)

	app, _, err := svc.Apply(testAnswers())
	require.NoError(t, err)
	require.NoError(t, svc.Reject(app.ID, "mod2"))

	stored, err := svc.Get(app.ID)
	require.NoError(t, err)
	assert.Equal(t, model.SignupApplicationRejected, stored.Status)

	t.Run("cannot reject twice", func(t *testing.T) {
		assert.ErrorIs(t, svc.Reject(app.ID, "mod2"), ErrNotPending)
	})

	t.Run("not found", func(t *testing.T) {
		assert.ErrorIs(t, svc.Reject("no-such-id", "mod2"), ErrNotFound)
	})

	// 期限切れは却下として記録しない。**審査していないものを「審査して落とした」と
	// 残すと、監査の意味が壊れる。**
	t.Run("expired cannot be rejected", func(t *testing.T) {
		other, _, err := svc.Apply(testAnswers())
		require.NoError(t, err)
		*now = now.Add(DefaultTTL)
		assert.ErrorIs(t, svc.Reject(other.ID, "mod2"), ErrExpired)

		stored, err := svc.Get(other.ID)
		require.NoError(t, err)
		assert.Nil(t, stored.ProcessedByID, "却下として記録しないこと")
	})
}

func TestMarkCompleted(t *testing.T) {
	svc, now := newService(t)

	app, _, err := svc.Apply(testAnswers())
	require.NoError(t, err)

	t.Run("pending cannot be completed", func(t *testing.T) {
		assert.ErrorIs(t, svc.MarkCompleted(app.ID, "u1", "t1"), ErrNotApproved)
	})

	require.NoError(t, svc.Approve(app.ID, "mod1"))
	require.NoError(t, svc.MarkCompleted(app.ID, "u1", "t1"))

	stored, err := svc.Get(app.ID)
	require.NoError(t, err)
	assert.Equal(t, model.SignupApplicationCompleted, stored.Status)
	require.NotNil(t, stored.UsedByID)
	assert.Equal(t, "u1", *stored.UsedByID)
	require.NotNil(t, stored.TicketID)
	assert.Equal(t, "t1", *stored.TicketID)

	t.Run("cannot complete twice", func(t *testing.T) {
		assert.ErrorIs(t, svc.MarkCompleted(app.ID, "u1", "t1"), ErrNotApproved)
	})

	t.Run("not found", func(t *testing.T) {
		assert.ErrorIs(t, svc.MarkCompleted("no-such-id", "u1", "t1"), ErrNotFound)
	})

	t.Run("expired cannot be completed", func(t *testing.T) {
		other, _, err := svc.Apply(testAnswers())
		require.NoError(t, err)
		require.NoError(t, svc.Approve(other.ID, "mod1"))
		*now = now.Add(DefaultTTL)
		assert.ErrorIs(t, svc.MarkCompleted(other.ID, "u2", "t2"), ErrExpired)
	})

	t.Run("empty ticket id is not recorded", func(t *testing.T) {
		svc2, _ := newService(t)
		a, _, err := svc2.Apply(testAnswers())
		require.NoError(t, err)
		require.NoError(t, svc2.Approve(a.ID, "mod1"))
		require.NoError(t, svc2.MarkCompleted(a.ID, "u3", ""))

		stored, err := svc2.Get(a.ID)
		require.NoError(t, err)
		assert.Nil(t, stored.TicketID)
	})
}

// MarkTicket は承認済みのまま ticket だけを記録する (#2571)。**completed には
// しない** — メール確認が終わるまでアカウントは無い。
func TestMarkTicket(t *testing.T) {
	svc, now := newService(t)

	app, _, err := svc.Apply(testAnswers())
	require.NoError(t, err)

	t.Run("pending cannot record a ticket", func(t *testing.T) {
		_, err := svc.MarkTicket(app.ID, "t1")
		assert.ErrorIs(t, err, ErrNotApproved)
	})

	require.NoError(t, svc.Approve(app.ID, "mod1"))
	previous, err := svc.MarkTicket(app.ID, "t1")
	require.NoError(t, err)
	assert.Empty(t, previous, "初回は置き換える前の ticket が無い")

	stored, err := svc.Get(app.ID)
	require.NoError(t, err)
	assert.Equal(t, model.SignupApplicationApproved, stored.Status, "承認済みのまま")
	require.NotNil(t, stored.TicketID)
	assert.Equal(t, "t1", *stored.TicketID)
	assert.Nil(t, stored.UsedByID, "まだ誰も登録していない")

	// やり直しで別の ticket に差し替わる。**置き換えた前の ticket を返す** —
	// 行ロックの中で読んだ値なので、これを破棄すれば消費済みの ticket を消して
	// usedById の記録を失うことが無い (#2576)。
	previous, err = svc.MarkTicket(app.ID, "t2")
	require.NoError(t, err)
	assert.Equal(t, "t1", previous)
	stored, err = svc.Get(app.ID)
	require.NoError(t, err)
	assert.Equal(t, "t2", *stored.TicketID)

	// 記録した後でも完了できる (確認メールの経路の終点)。
	require.NoError(t, svc.MarkCompleted(app.ID, "u1", "t2"))
	_, err = svc.MarkTicket(app.ID, "t3")
	assert.ErrorIs(t, err, ErrNotApproved)

	t.Run("not found", func(t *testing.T) {
		_, err := svc.MarkTicket("no-such-id", "t1")
		assert.ErrorIs(t, err, ErrNotFound)
	})

	t.Run("expired", func(t *testing.T) {
		other, _, err := svc.Apply(testAnswers())
		require.NoError(t, err)
		require.NoError(t, svc.Approve(other.ID, "mod1"))
		*now = now.Add(DefaultTTL)
		_, err = svc.MarkTicket(other.ID, "t9")
		assert.ErrorIs(t, err, ErrExpired)

		expired, err := svc.Get(other.ID)
		require.NoError(t, err)
		assert.Equal(t, model.SignupApplicationExpired, expired.Status, "期限切れに落ちること")
	})
}

func TestGet_NotFound(t *testing.T) {
	svc, _ := newService(t)
	_, err := svc.Get("no-such-id")
	assert.ErrorIs(t, err, ErrNotFound)
}

func TestListAndCount(t *testing.T) {
	svc, _ := newService(t)

	pending, _, err := svc.Apply(testAnswers())
	require.NoError(t, err)
	approved, _, err := svc.Apply(testAnswers())
	require.NoError(t, err)
	require.NoError(t, svc.Approve(approved.ID, "mod1"))
	rejected, _, err := svc.Apply(testAnswers())
	require.NoError(t, err)
	require.NoError(t, svc.Reject(rejected.ID, "mod1"))

	rows, err := svc.List(repository.SignupApplicationFilterPending, 50, 0)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, pending.ID, rows[0].ID)

	n, err := svc.Count(repository.SignupApplicationFilterAll)
	require.NoError(t, err)
	assert.Equal(t, 3, n)
}

func TestExpireStale(t *testing.T) {
	svc, now := newService(t)

	app, _, err := svc.Apply(testAnswers())
	require.NoError(t, err)

	changed, err := svc.ExpireStale()
	require.NoError(t, err)
	assert.Equal(t, 0, changed, "期限内は触らない")

	*now = now.Add(DefaultTTL)
	changed, err = svc.ExpireStale()
	require.NoError(t, err)
	assert.Equal(t, 1, changed)

	stored, err := svc.Get(app.ID)
	require.NoError(t, err)
	assert.Equal(t, model.SignupApplicationExpired, stored.Status)
}

// SetTTL / SetClock は不正な値を無視する。**設定ミスで全申請が即座に期限切れに
// なる、という壊れ方をさせない。**
func TestSetters_IgnoreInvalidValues(t *testing.T) {
	svc, now := newService(t)

	svc.SetTTL(0)
	svc.SetTTL(-time.Hour)
	app, code, err := svc.Apply(testAnswers())
	require.NoError(t, err)
	assert.Equal(t, now.Add(DefaultTTL), app.ExpiresAt)

	svc.SetClock(nil)
	got, err := svc.ByClaimCode(code)
	require.NoError(t, err)
	require.NotNil(t, got, "clock が nil で潰れていないこと")
}

func TestSetTTL(t *testing.T) {
	svc, now := newService(t)
	svc.SetTTL(time.Hour)

	app, _, err := svc.Apply(testAnswers())
	require.NoError(t, err)
	assert.Equal(t, now.Add(time.Hour), app.ExpiresAt)
}

// コードは推測されると他人の申請を乗っ取れる (状態確認と登録の両方の入口)。
func TestNewClaimCode_IsUnique(t *testing.T) {
	seen := make(map[string]bool, 32)
	for range 32 {
		code, err := NewClaimCode()
		require.NoError(t, err)
		assert.Len(t, code, 64)
		assert.False(t, seen[code], "duplicate claim code")
		seen[code] = true
	}
}

func TestHashClaimCode(t *testing.T) {
	h1 := HashClaimCode("abc")
	assert.Len(t, h1, 64)
	assert.Equal(t, h1, HashClaimCode("abc"), "同じ入力は同じ hash")
	assert.NotEqual(t, h1, HashClaimCode("abd"))
	assert.NotEqual(t, "abc", h1)
}
