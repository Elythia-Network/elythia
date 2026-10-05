package smtp

import (
	"errors"
	"strings"
	"testing"

	"github.com/elythia-network/elythia/internal/model"
)

// stubMetaFetcher is a fake MetaFetcher for sender helper tests.
type stubMetaFetcher struct {
	meta *model.Meta
	err  error
}

func (s *stubMetaFetcher) Fetch() (*model.Meta, error) {
	return s.meta, s.err
}

// SenderFromMeta: meta Fetch が error を返した場合は no-op (= panic せず、
// SMTP 接続を試みない)。warn log は出るが test では log の有無は assert
// しない (slog の global handler に依存するため brittle)。
func TestSenderFromMeta_FetchError_NoOp(t *testing.T) {
	sender := SenderFromMeta(&stubMetaFetcher{err: errors.New("DB down")}, "")
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("sender panicked on fetch error: %v", r)
		}
	}()
	sender("to@example.com", Message{Subject: "s", Text: "b"})
}

// SenderFromMeta: EnableEmail=false は SMTP 接続を試みない。
func TestSenderFromMeta_EmailDisabled_NoOp(t *testing.T) {
	from := "f@e.test"
	host := "smtp.example.com"
	port := 587
	meta := &model.Meta{
		EnableEmail: false,
		Email:       &from,
		SMTPHost:    &host,
		SMTPPort:    &port,
	}
	sender := SenderFromMeta(&stubMetaFetcher{meta: meta}, "")
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("sender panicked: %v", r)
		}
	}()
	sender("to@example.com", Message{Subject: "s", Text: "b"})
}

// SenderFromMeta: SMTPHost が nil または空文字列なら no-op。空文字列
// edge case の guard を持たないと SendMessage が "0:port" に dial して
// 異常な error log を吐くため明示的に test する。
func TestSenderFromMeta_HostMissingOrEmpty_NoOp(t *testing.T) {
	from := "f@e.test"
	cases := []struct {
		name string
		host *string
	}{
		{"nil", nil},
		{"empty string", strPtr("")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			meta := &model.Meta{
				EnableEmail: true,
				Email:       &from,
				SMTPHost:    tc.host,
			}
			sender := SenderFromMeta(&stubMetaFetcher{meta: meta}, "")
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("sender panicked: %v", r)
				}
			}()
			sender("to@example.com", Message{Subject: "s", Text: "b"})
		})
	}
}

// SenderFromMeta: From アドレスが nil または空文字列なら no-op (=
// SMTP MAIL FROM が空欄になり server reject になるのを未然に防ぐ)。
func TestSenderFromMeta_FromMissingOrEmpty_NoOp(t *testing.T) {
	host := "smtp.example.com"
	cases := []struct {
		name string
		from *string
	}{
		{"nil", nil},
		{"empty string", strPtr("")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			meta := &model.Meta{
				EnableEmail: true,
				Email:       tc.from,
				SMTPHost:    &host,
			}
			sender := SenderFromMeta(&stubMetaFetcher{meta: meta}, "")
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("sender panicked: %v", r)
				}
			}()
			sender("to@example.com", Message{Subject: "s", Text: "b"})
		})
	}
}

// SenderFromMeta: happy path で実際に SendMessage が呼ばれて fake SMTP
// に EHLO / MAIL / RCPT / DATA が届くことを end-to-end で確認する。
func TestSenderFromMeta_HappyPath_DeliversToFakeSMTP(t *testing.T) {
	srv := newFakeSMTP(t, false)
	from := "noreply@example.com"
	host := "127.0.0.1"
	port := srv.port()
	meta := &model.Meta{
		EnableEmail: true,
		Email:       &from,
		SMTPHost:    &host,
		SMTPPort:    &port,
	}
	sender := SenderFromMeta(&stubMetaFetcher{meta: meta}, "")
	sender("to@example.com", Message{Subject: "hello", Text: "body text"})

	msgs := waitForMessages(srv, 3)
	if len(msgs) < 3 {
		t.Fatalf("expected at least 3 SMTP exchanges, got %d: %v", len(msgs), msgs)
	}
	// MAIL FROM に meta.Email がそのまま入ること
	foundMail := false
	for _, m := range msgs {
		if strings.HasPrefix(m, "MAIL FROM:") && strings.Contains(m, from) {
			foundMail = true
			break
		}
	}
	if !foundMail {
		t.Errorf("expected MAIL FROM containing %q, got %v", from, msgs)
	}
}

// SenderFromMeta: caller が close した meta (= 後から admin が SMTP 設定を
// 入れた相当) でも、closure が次回 Fetch するので即時反映されることを
// 確認する。前提: stubMetaFetcher の meta pointer を test 内で差し替える。
//
// この test は #1112 の中核 (= per-call 再 Fetch) の regression guard。
func TestSenderFromMeta_RuntimeConfigChange_TakesEffectImmediately(t *testing.T) {
	srv := newFakeSMTP(t, false)
	stub := &stubMetaFetcher{meta: &model.Meta{EnableEmail: false}}
	sender := SenderFromMeta(stub, "")

	// 1 回目: 未設定 → no-op。
	sender("to@example.com", Message{Subject: "1", Text: "b"})
	if got := waitForMessages(srv, 1); len(got) != 0 {
		t.Fatalf("expected no SMTP traffic when disabled, got %v", got)
	}

	// 設定変更: admin が SMTP を有効化した相当。再起動なしで 2 回目の
	// 呼び出しが fake SMTP に届けばよい。
	from := "noreply@example.com"
	host := "127.0.0.1"
	port := srv.port()
	stub.meta = &model.Meta{
		EnableEmail: true,
		Email:       &from,
		SMTPHost:    &host,
		SMTPPort:    &port,
	}
	sender("to@example.com", Message{Subject: "2", Text: "b"})
	msgs := waitForMessages(srv, 3)
	if len(msgs) < 3 {
		t.Fatalf("expected SMTP delivery after enabling, got %v", msgs)
	}
}

// SubjectBodySenderFromMeta: (to, subject, body) 形式の wrapper が
// 内部で Message に変換して同じ pipeline を通すことを確認する。
func TestSubjectBodySenderFromMeta_DeliversToFakeSMTP(t *testing.T) {
	srv := newFakeSMTP(t, false)
	from := "admin@example.com"
	host := "127.0.0.1"
	port := srv.port()
	meta := &model.Meta{
		EnableEmail: true,
		Email:       &from,
		SMTPHost:    &host,
		SMTPPort:    &port,
	}
	sender := SubjectBodySenderFromMeta(&stubMetaFetcher{meta: meta}, "")
	sender("user@example.com", "Password reset", "Your temporary password is X")

	msgs := waitForMessages(srv, 3)
	// DATA part に subject / body が両方含まれること
	var body string
	for _, m := range msgs {
		if strings.Contains(m, "Subject: Password reset") {
			body = m
			break
		}
	}
	if body == "" {
		t.Fatalf("expected DATA with subject, got %v", msgs)
	}
	if !strings.Contains(body, "Your temporary password is X") {
		t.Errorf("body missing text content: %q", body)
	}
}

// SubjectBodySenderFromMeta: 未設定 meta でも panic せず no-op。
func TestSubjectBodySenderFromMeta_UnconfiguredIsNoOp(t *testing.T) {
	sender := SubjectBodySenderFromMeta(&stubMetaFetcher{meta: &model.Meta{}}, "")
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("sender panicked: %v", r)
		}
	}()
	sender("u@example.com", "s", "b")
}

// helper: strPtr is locally defined in this test file to avoid pulling
// the server helper into a different package. Kept tiny + private.
func strPtr(s string) *string { return &s }
