package signin_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	miscsmtp "github.com/elythia-network/elythia/internal/misc/smtp"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// sentMail captures one asynchronous send. 送信は goroutine なので channel で待つ。
type sentMail struct {
	to  string
	msg miscsmtp.Message
}

type chanEmailSender struct{ ch chan sentMail }

func newChanEmailSender() *chanEmailSender {
	return &chanEmailSender{ch: make(chan sentMail, 4)}
}

func (s *chanEmailSender) send(to string, msg miscsmtp.Message) {
	s.ch <- sentMail{to: to, msg: msg}
}

// waitMail waits for one send, failing the test on timeout.
func (s *chanEmailSender) waitMail(t *testing.T) sentMail {
	t.Helper()
	select {
	case m := <-s.ch:
		return m
	case <-time.After(2 * time.Second):
		t.Fatal("new-login email was not sent")
		return sentMail{}
	}
}

// assertNoMail gives the goroutine a window to send, then asserts it did not.
func (s *chanEmailSender) assertNoMail(t *testing.T) {
	t.Helper()
	select {
	case m := <-s.ch:
		t.Fatalf("new-login email should not have been sent (to=%s)", m.to)
	case <-time.After(200 * time.Millisecond):
	}
}

// setProfileEmail sets the address on the user's profile, creating the row only
// when it does not exist yet.
//
// **既存 profile を差し替えない。** createTestUser が password hash を profile に
// 置くので、丸ごと置き換えると signin が 403 になる。
func setProfileEmail(repo *testutil.MockUserRepository, userID string, email *string, verified bool) {
	p, ok := repo.Profiles[userID]
	if !ok {
		p = &model.UserProfile{UserID: userID}
		repo.Profiles[userID] = p
	}
	p.Email = email
	p.EmailVerified = verified
}

func strptr(s string) *string { return &s }

// #2454: upstream SigninService はメール確認済みの user にログイン通知を送る。
// `login` 通知はアプリ内にしか出ないので、乗っ取りに気付ける唯一の外向き経路。
func TestNewLoginEmail_SentInJapaneseWhenProfileLangIsJa(t *testing.T) {
	h, repo := newTestHandler(t)
	sender := newChanEmailSender()
	h.SetEmailSender("https://example.test", sender.send)
	setProfileEmail(repo, "u1", strptr("user@example.test"), true)
	repo.Profiles["u1"].Lang = strptr("ja-JP")

	h.RecordSuccessfulSignin("u1", "1.2.3.4", http.Header{})

	m := sender.waitMail(t)
	assert.Equal(t, "ログインがありました", m.msg.Subject)
	assert.Contains(t, m.msg.Text, "新しいログインがありました")
}

func TestNewLoginEmail_SentInJapaneseFromMetaLangsWhenProfileLangUnset(t *testing.T) {
	h, repo := newTestHandler(t)
	sender := newChanEmailSender()
	h.SetEmailSender("https://example.test", sender.send)
	metaRepo := testutil.NewMockMetaRepository()
	metaRepo.Meta = &model.Meta{ID: "x", Langs: []string{"ja-JP"}}
	h.SetMetaRepo(metaRepo)
	setProfileEmail(repo, "u1", strptr("user@example.test"), true)

	h.RecordSuccessfulSignin("u1", "1.2.3.4", http.Header{})

	m := sender.waitMail(t)
	assert.Equal(t, "ログインがありました", m.msg.Subject)
	assert.Contains(t, m.msg.Text, "新しいログインがありました")
}

func TestNewLoginEmail_SentWhenVerified(t *testing.T) {
	h, repo := newTestHandler(t)
	sender := newChanEmailSender()
	h.SetEmailSender("https://example.test", sender.send)
	setProfileEmail(repo, "u1", strptr("user@example.test"), true)

	h.RecordSuccessfulSignin("u1", "1.2.3.4", http.Header{})

	m := sender.waitMail(t)
	assert.Equal(t, "user@example.test", m.to)
	assert.Equal(t, "New login / ログインがありました", m.msg.Subject)
	assert.Contains(t, m.msg.Text, "There is a new login.")
	assert.Contains(t, m.msg.Text, "新しいログインがありました")
	// HTML 版は wrapper に載り、認証済 user 向けの email 設定 link を持つ。
	assert.Contains(t, m.msg.HTML, "https://example.test/settings/email")
}

func TestNewLoginEmail_SkippedWhenNotVerified(t *testing.T) {
	h, repo := newTestHandler(t)
	sender := newChanEmailSender()
	h.SetEmailSender("https://example.test", sender.send)
	// 確認前のアドレスは他人のものを登録できてしまう。送ると「他人のログイン通知が
	// 自分に届く」経路になるので upstream と同じく送らない。
	setProfileEmail(repo, "u1", strptr("user@example.test"), false)

	h.RecordSuccessfulSignin("u1", "1.2.3.4", http.Header{})

	sender.assertNoMail(t)
}

func TestNewLoginEmail_SkippedWhenNoAddress(t *testing.T) {
	h, repo := newTestHandler(t)
	sender := newChanEmailSender()
	h.SetEmailSender("https://example.test", sender.send)
	setProfileEmail(repo, "u1", nil, true)
	setProfileEmail(repo, "u2", strptr(""), true)

	h.RecordSuccessfulSignin("u1", "1.2.3.4", http.Header{})
	h.RecordSuccessfulSignin("u2", "1.2.3.4", http.Header{})

	sender.assertNoMail(t)
}

// profile 行そのものが無いケース。メール未設定と同じ扱いで、警告も出さない。
func TestNewLoginEmail_SkippedWhenProfileMissing(t *testing.T) {
	h, _ := newTestHandler(t)
	sender := newChanEmailSender()
	h.SetEmailSender("https://example.test", sender.send)

	h.RecordSuccessfulSignin("no-such-user", "1.2.3.4", http.Header{})

	sender.assertNoMail(t)
}

// lookup が実エラーで落ちても副作用は止めない (メールは検知手段であって、他の
// 副作用の前提条件ではない)。行が無いケースと違い、こちらは warn を残す。
//
// signin 経路ごと通す形にはできない。password hash も同じ profile から読むので、
// lookup を失敗させると認証そのものが 403 になり、メール送信まで到達しない。
func TestNewLoginEmail_ProfileLookupErrorIsSwallowed(t *testing.T) {
	h, repo := newTestHandler(t)
	sender := newChanEmailSender()
	h.SetEmailSender("https://example.test", sender.send)
	notifier := &chanLoginNotifier{ch: make(chan string, 1)}
	h.SetLoginNotifier(notifier)
	repo.FindProfileErr = assert.AnError

	h.RecordSuccessfulSignin("u1", "1.2.3.4", http.Header{})

	// メールは飛ばないが、他の副作用 (login 通知) は止まらない。
	select {
	case uid := <-notifier.ch:
		assert.Equal(t, "u1", uid)
	case <-time.After(2 * time.Second):
		t.Fatal("login notifier was not fired")
	}
	sender.assertNoMail(t)
}

// sender 未配線でも signin の副作用は従来どおり動く (SMTP を使わない構成)。
func TestNewLoginEmail_NoSenderStillRecordsSignin(t *testing.T) {
	h, repo := newTestHandler(t)
	setProfileEmail(repo, "u1", strptr("user@example.test"), true)
	notifier := &chanLoginNotifier{ch: make(chan string, 1)}
	h.SetLoginNotifier(notifier)

	h.RecordSuccessfulSignin("u1", "1.2.3.4", http.Header{})

	select {
	case uid := <-notifier.ch:
		assert.Equal(t, "u1", uid)
	case <-time.After(2 * time.Second):
		t.Fatal("login notifier was not fired")
	}
}

// 実際の signin 経路 (POST /api/signin) からも飛ぶこと。RecordSuccessfulSignin を
// 直接叩くだけだと、handler 側の呼び出しが外れても検知できない。
func TestNewLoginEmail_SentFromSigninEndpoint(t *testing.T) {
	h, repo := newTestHandler(t)
	createTestUser(repo, "admin", "pass123")
	sender := newChanEmailSender()
	h.SetEmailSender("https://example.test", sender.send)
	setProfileEmail(repo, "u1", strptr("admin@example.test"), true)

	rec := doPost(h.Signin, `{"username":"admin","password":"pass123"}`)
	require.Equal(t, http.StatusOK, rec.Code)

	m := sender.waitMail(t)
	assert.Equal(t, "admin@example.test", m.to)
}

// ログイン失敗ではメールを送らない。送ると総当たり中の attacker が任意の利用者へ
// メールを送りつけられる (= 通知そのものが迷惑行為の踏み台になる)。
func TestNewLoginEmail_NotSentOnFailedSignin(t *testing.T) {
	h, repo := newTestHandler(t)
	createTestUser(repo, "admin", "pass123")
	sender := newChanEmailSender()
	h.SetEmailSender("https://example.test", sender.send)
	setProfileEmail(repo, "u1", strptr("admin@example.test"), true)

	rec := doPost(h.Signin, `{"username":"admin","password":"wrongpass"}`)
	require.Equal(t, http.StatusForbidden, rec.Code)

	sender.assertNoMail(t)
}

// HTML 本文の escape。表示名等は載せていないが、wrapper に流す件名は escape される。
func TestNewLoginEmail_HTMLIsWrapped(t *testing.T) {
	h, repo := newTestHandler(t)
	sender := newChanEmailSender()
	h.SetEmailSender("https://example.test", sender.send)
	setProfileEmail(repo, "u1", strptr("user@example.test"), true)

	h.RecordSuccessfulSignin("u1", "1.2.3.4", http.Header{})

	m := sender.waitMail(t)
	assert.True(t, strings.HasPrefix(strings.TrimSpace(m.msg.HTML), "<"), "HTML 版が wrapper に載る")
	assert.Contains(t, m.msg.HTML, "<p>")
	// 平文版に HTML タグが混ざらない。
	assert.NotContains(t, m.msg.Text, "<p>")
}
