// Package signupapplication implements the state machine behind approval-based
// registration (#2554 / #2555).
//
// 申請は管理者の審査を経て登録に至る。**承認待ちを user 行として持たない**のが
// 設計の核で、user が作られるのは承認後の登録時だけ。
//
// 本人性は**申請時に発行するクレームコード**が担保する。外部サーバーには一切
// 依存しない (#2568)。コードは hash で保存し、平文はこの package の外へ 1 度だけ
// 返す。
//
//	無し       -> pending                (Apply)
//	pending    -> approved / rejected    (Approve / Reject)
//	approved   -> completed              (MarkCompleted、#2556 の登録経路)
//	pending    -> expired                (期限切れ)
//	approved   -> expired                (期限切れ)
//
// rejected / expired / completed は終端。再申請は別レコードになる。
package signupapplication

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

// DefaultTTL is how long an application stays usable, counted from creation.
//
// **短くしないこと。** 承認から登録までの間に相手インスタンスが一時的に落ちる
// ことは珍しくない。1 日にすると、復旧すれば通ったはずの人が期限切れで落ちる。
const DefaultTTL = 7 * 24 * time.Hour

var (
	// ErrNotFound is returned when the application does not exist.
	ErrNotFound = errors.New("signupapplication: not found")
	// ErrNotPending is returned when approving / rejecting an application that
	// is no longer awaiting review.
	ErrNotPending = errors.New("signupapplication: application is not pending")
	// ErrNotApproved is returned when completing an application that is not in
	// the approved state.
	ErrNotApproved = errors.New("signupapplication: application is not approved")
	// ErrExpired is returned when acting on an application past its deadline.
	//
	// **ErrNotPending と分けるのが要点。** 期限切れの行は status が pending の
	// まま残りうる (掃除は遅延反映) ので、まとめて「審査待ちではない」と返すと
	// 管理者には「審査待ちに見えるのに審査待ちではない」という説明になる。
	ErrExpired = errors.New("signupapplication: application has expired")
)

// Service manages signup applications.
type Service struct {
	repo  repository.SignupApplicationRepository
	idGen id.Generator
	clock func() time.Time
	ttl   time.Duration
	// receivedNotifier は申請の受付をモデレーターへ知らせる (#2987)。
	// 未配線なら通知を出さない (既存の挙動)。
	receivedNotifier ReceivedNotifier
}

// SetReceivedNotifier wires the moderator notification emitted on Apply (#2987).
func (s *Service) SetReceivedNotifier(n ReceivedNotifier) { s.receivedNotifier = n }

// NewService constructs a Service.
func NewService(repo repository.SignupApplicationRepository, idGen id.Generator) *Service {
	return &Service{
		repo:  repo,
		idGen: idGen,
		clock: time.Now,
		ttl:   DefaultTTL,
	}
}

// SetClock replaces the clock, primarily for tests.
func (s *Service) SetClock(now func() time.Time) {
	if now != nil {
		s.clock = now
	}
}

// SetTTL replaces how long an application stays usable. Non-positive values are
// ignored so a misconfiguration cannot make every application expire instantly.
func (s *Service) SetTTL(d time.Duration) {
	if d > 0 {
		s.ttl = d
	}
}

// Apply records a new application and returns it along with the claim code.
//
// **コードの平文を返すのはここだけ。** 保存するのは hash で、呼び出し側が申請者へ
// 1 度だけ見せる。失くしたら再申請してもらうしかない (伝える経路が無い)。
func (s *Service) Apply(answers []Answer) (*model.SignupApplication, string, error) {
	encoded, err := json.Marshal(answers)
	if err != nil {
		return nil, "", fmt.Errorf("signupapplication: marshal answers: %w", err)
	}

	now := s.clock()
	// 衝突は crypto/rand なので実質起きないが、**黙って別の申請を上書きしない**
	// ように採り直す。
	for attempt := range claimCodeAttempts {
		code, cerr := NewClaimCode()
		if cerr != nil {
			return nil, "", cerr
		}
		app := &model.SignupApplication{
			ID:            s.idGen.Generate(now),
			ClaimCodeHash: HashClaimCode(code),
			Status:        model.SignupApplicationPending,
			Answers:       datatypes.JSON(encoded),
			CreatedAt:     now,
			UpdatedAt:     now,
			ExpiresAt:     now.Add(s.ttl),
		}
		err = s.repo.Create(app)
		switch {
		case err == nil:
			// **申請は永続化済みなので、通知の失敗で申請を失わせない。**
			// 呼び出し元の ctx も使わない — 申請者のブラウザが切断しても
			// モデレーターへの通知は出す (abuseReport #2868 と同じ判断)。
			if s.receivedNotifier != nil {
				if nerr := s.receivedNotifier.NotifySignupApplicationReceived(context.Background(), app); nerr != nil {
					slog.Warn("signup-application: notify moderators failed", "application", app.ID, "err", nerr)
				}
			}
			return app, code, nil
		case errors.Is(err, repository.ErrSignupApplicationCodeCollision):
			_ = attempt // 採り直す
			continue
		default:
			return nil, "", fmt.Errorf("signupapplication: create: %w", err)
		}
	}
	return nil, "", errors.New("signupapplication: could not allocate a claim code")
}

// ByClaimCode returns the application the code stands for, lazily expiring it
// when it is past its deadline.
//
// 見つからないときは nil を返す (エラーにしない)。**存在しないコードと期限切れの
// コードを呼び出し側から区別させない** — 区別できると、コードの総当たりで「その
// コードは実在する」ことだけ漏れる。
func (s *Service) ByClaimCode(code string) (*model.SignupApplication, error) {
	if code == "" {
		return nil, nil
	}
	app, err := s.repo.FindByClaimCodeHash(HashClaimCode(code))
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("signupapplication: find by claim code: %w", err)
	}
	now := s.clock()
	if !app.IsLive() || now.Before(app.ExpiresAt) {
		return app, nil
	}
	// 期限切れは参照のたびに反映する。掃除ジョブが無くても実態に寄る。
	if err := s.transition(app.ID, now, func(cur *model.SignupApplication) decision {
		if !cur.IsLive() || now.Before(cur.ExpiresAt) {
			return decision{}
		}
		return decision{fields: map[string]any{"status": model.SignupApplicationExpired}}
	}); err != nil {
		return nil, err
	}
	app.Status = model.SignupApplicationExpired
	return app, nil
}

// Approve marks the application as approved.
//
// **ここでは registration_ticket を発行しない。** 発行を承認時にすると、登録
// までの数日間、利用者に渡していない bearer 相当の credential が DB に置き
// っぱなしになる。チケットは登録経路のうち**メール確認を挟むときだけ**
// (#2556 / #2813) 発行する。
func (s *Service) Approve(applicationID, moderatorID string) error {
	return s.review(applicationID, moderatorID, true)
}

// Reject marks the application as rejected.
func (s *Service) Reject(applicationID, moderatorID string) error {
	return s.review(applicationID, moderatorID, false)
}

// review is shared by Approve / Reject: the guards and the notification are the
// same shape, only the resulting status differs.
func (s *Service) review(applicationID, moderatorID string, approve bool) error {
	now := s.clock()
	status := model.SignupApplicationRejected
	if approve {
		status = model.SignupApplicationApproved
	}
	return s.transition(applicationID, now, func(cur *model.SignupApplication) decision {
		if cur.Status != model.SignupApplicationPending {
			return decision{err: ErrNotPending}
		}
		// 期限切れを審査できてしまうと、掃除の前後で結果が変わる。ついでに
		// 実態へ寄せておく (掃除は遅延反映なので pending のまま残っている)。
		// 期限切れは審査の結果ではないので processedBy / processedAt は立てない。
		if !now.Before(cur.ExpiresAt) {
			return expireDecision()
		}
		return decision{fields: map[string]any{
			"status":        status,
			"processedById": moderatorID,
			"processedAt":   now,
		}}
	})
}

// MarkCompleted records that the approved application produced a local account.
// ticketID は実際に消費した registration_ticket (監査用)。
//
// 登録経路 (#2556) から、ユーザー作成と同じ流れで呼ぶ。
func (s *Service) MarkCompleted(applicationID, userID, ticketID string) error {
	now := s.clock()
	return s.transition(applicationID, now, func(cur *model.SignupApplication) decision {
		if cur.Status != model.SignupApplicationApproved {
			return decision{err: ErrNotApproved}
		}
		if !now.Before(cur.ExpiresAt) {
			return expireDecision()
		}
		fields := map[string]any{
			"status":   model.SignupApplicationCompleted,
			"usedById": userID,
		}
		if ticketID != "" {
			fields["ticketId"] = ticketID
		}
		return decision{fields: fields}
	})
}

// decision is what a guarded transition wants to do with the locked row.
//
// **fields と err は同時に返せる。** 期限切れの検出は「expired に落とす」と
// 「呼び出し側にはエラーを返す」を同時に行う必要があるため。
type decision struct {
	fields map[string]any
	err    error
}

// transition applies a guarded state change under a row-level write lock.
//
// **ロックを取るのが要点。** 審査は管理者が、完了は登録経路が触るので、素の
// read-modify-write だと「却下と同時に登録が通る」ような取り違えが起きる。
// decide が nil の fields を返したら何もしない (競合して既に処理済み)。
// MarkTicket records the invitation ticket minted for an in-flight signup,
// leaving the application approved (#2571).
//
// メール確認を挟むとき、確認が終わるまでアカウントは無いので completed にできない。
// **その間に発行した ticket を申請に覚えさせておく。** 覚えていないと、申請者が
// 登録をやり直すたびに ticket と pending が積み上がり、届いたメールを両方確認
// すれば 1 つの承認から複数アカウントを作れてしまう。次の試行はここに記録された
// ticket を破棄してから新しく発行する。
// 置き換えた前の ticket ID を返す。**行ロックの中で読んだ値**なので、これを
// 破棄すれば「確認が先に通って消費された ticket を消してしまう」ことが無い
// (その場合はこの呼び出し自体が ErrNotApproved で落ちる)。
func (s *Service) MarkTicket(applicationID, ticketID string) (string, error) {
	now := s.clock()
	previous := ""
	err := s.transition(applicationID, now, func(cur *model.SignupApplication) decision {
		if cur.Status != model.SignupApplicationApproved {
			return decision{err: ErrNotApproved}
		}
		if !now.Before(cur.ExpiresAt) {
			return expireDecision()
		}
		if cur.TicketID != nil {
			previous = *cur.TicketID
		}
		return decision{fields: map[string]any{"ticketId": ticketID}}
	})
	if err != nil {
		return "", err
	}
	return previous, nil
}

func (s *Service) transition(
	applicationID string,
	now time.Time,
	decide func(cur *model.SignupApplication) decision,
) error {
	// **decide の err をトランザクションの外まで持ち越す。** gorm の
	// Transaction はコールバックがエラーを返すとロールバックするので、
	// 「期限切れに落としつつ ErrExpired を返す」を素直に書くと更新が巻き戻る。
	var outcome error
	if err := s.repo.DB().Transaction(func(tx *gorm.DB) error {
		cur, err := s.repo.FindByIDForUpdateTx(tx, applicationID)
		if err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrNotFound
			}
			return fmt.Errorf("signupapplication: lock: %w", err)
		}
		d := decide(cur)
		if len(d.fields) > 0 {
			d.fields["updatedAt"] = now
			if uerr := s.repo.UpdateFieldsTx(tx, applicationID, d.fields); uerr != nil {
				return fmt.Errorf("signupapplication: update: %w", uerr)
			}
		}
		outcome = d.err
		return nil
	}); err != nil {
		return err
	}
	return outcome
}

// expireDecision marks a live-but-overdue row as expired and reports ErrExpired.
// 期限切れは審査の結果ではないので、processedBy / processedAt は立てない。
func expireDecision() decision {
	return decision{
		fields: map[string]any{"status": model.SignupApplicationExpired},
		err:    ErrExpired,
	}
}

// Get returns a single application by ID.
func (s *Service) Get(applicationID string) (*model.SignupApplication, error) {
	app, err := s.repo.FindByID(applicationID)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("signupapplication: find: %w", err)
	}
	return app, nil
}

// List returns applications for the admin review screen, newest first.
func (s *Service) List(filter string, limit, offset int) ([]*model.SignupApplication, error) {
	rows, err := s.repo.List(filter, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("signupapplication: list: %w", err)
	}
	return rows, nil
}

// Count returns how many applications match the filter.
func (s *Service) Count(filter string) (int, error) {
	n, err := s.repo.Count(filter)
	if err != nil {
		return 0, fmt.Errorf("signupapplication: count: %w", err)
	}
	return n, nil
}

// ExpireStale flips every overdue live application to `expired`.
// 遅延反映 (expireIfStale) だけでも運用は回るが、管理画面の一覧を実態に
// 合わせるために一括でも掃除できるようにしておく。
func (s *Service) ExpireStale() (int, error) {
	n, err := s.repo.ExpireStale(s.clock())
	if err != nil {
		return 0, fmt.Errorf("signupapplication: expire stale: %w", err)
	}
	return n, nil
}

// claimCodeAttempts bounds how many times Apply retries on a hash collision.
const claimCodeAttempts = 4

// NewClaimCode returns a fresh 256-bit code in hex.
//
// **推測されると他人の申請を乗っ取れる** (状態確認と登録の両方の入口になる) ので
// crypto/rand から作る。
func NewClaimCode() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("signupapplication: generate claim code: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// HashClaimCode returns the value stored in the database for a claim code.
//
// **平文は保存しない。** 保存すると DB が漏れた時点で全申請が乗っ取れる。総当たり
// への耐性はコード自身のエントロピー (256bit) が担うので、伸長は要らない。
func HashClaimCode(code string) string {
	sum := sha256.Sum256([]byte(code))
	return hex.EncodeToString(sum[:])
}
