package signup_test

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/core/signup"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

// integrationDB は本ファイル専用 (testcontainers / 既存 PostgreSQL に接続)。
// 他の core/signup mock test と並走しても DSN は共有なので問題なし。
func integrationDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := testutil.OpenTestDB()
	if err != nil {
		t.Skipf("PostgreSQL test DB unavailable: %v", err)
	}
	testutil.ApplyMigrations(db)
	return db
}

// cleanupSignupRows は本ファイルで作った行を毎テスト後にきれいに掃く。
// 互いの test が前ステップの pending / user / ticket を引き継がないように
// suffix prefix で絞り込んでいる (it_ で始まる行を全削除)。
func cleanupSignupRows(t *testing.T, db *gorm.DB, prefix string) {
	t.Helper()
	db.Exec(`DELETE FROM "user_pending" WHERE id LIKE ? OR username LIKE ?`, prefix+"%", prefix+"%")
	db.Exec(`DELETE FROM "user_profile" WHERE "userId" IN (SELECT id FROM "user" WHERE id LIKE ? OR "usernameLower" LIKE ?)`, prefix+"%", prefix+"%")
	db.Exec(`DELETE FROM "user_keypair" WHERE "userId" LIKE ?`, prefix+"%")
	db.Exec(`DELETE FROM "user_keypair_extra" WHERE "userId" LIKE ?`, prefix+"%")
	db.Exec(`DELETE FROM "user" WHERE id LIKE ? OR "usernameLower" LIKE ?`, prefix+"%", prefix+"%")
	db.Exec(`DELETE FROM "registration_ticket" WHERE id LIKE ? OR code LIKE ?`, prefix+"%", prefix+"%")
	// #2106 N23: promotePendingTx は used_username に必ず insert するため、idempotent な
	// 再実行のために prefix 付き行を掃除する (used_username は account 削除後も残る設計)。
	db.Exec(`DELETE FROM "used_username" WHERE "username" LIKE ?`, prefix+"%")
}

func newTxService(t *testing.T, db *gorm.DB) *signup.Service {
	t.Helper()
	idGen, _ := id.NewGenerator("aidx")

	// real repos (mock ではなく本物の GORM 経由で挙動確認)
	userRepo := repository.NewUserRepository(db)
	pendingRepo := repository.NewUserPendingRepository(db)
	ticketRepo := repository.NewRegistrationTicketRepository(db)

	// Meta は seed が無いと FindByUsernameLower で参照される PreservedUsernames
	// が空 default になり問題ないので、空 meta repo を mock で渡す。
	metaRepo := testutil.NewMockMetaRepository()
	metaRepo.Meta = &model.Meta{ID: "x"}

	svc := signup.NewService(userRepo, metaRepo, idGen)
	svc.SetUserPendingRepo(pendingRepo)
	svc.SetTicketRepo(ticketRepo)
	svc.SetSignupApplicationRepo(repository.NewSignupApplicationRepository(db))
	svc.SetDB(db)
	return svc
}

// invitation 経由で pending を作成し、PromotePending が ticket を消費する
// 一連の流れを real DB 上で検証する (#600 item 2 + #604 happy path)。
func TestPromotePending_TxConsumesInvitationTicket(t *testing.T) {
	db := integrationDB(t)
	const prefix = "itinv_"
	defer cleanupSignupRows(t, db, prefix)

	svc := newTxService(t, db)

	// 招待 ticket seed
	ticket := &model.RegistrationTicket{
		ID:   prefix + "tkt1",
		Code: prefix + "code1",
	}
	require.NoError(t, db.Create(ticket).Error)

	row, err := svc.CreatePending(prefix+"alice", "alice@it.example", "pw123", &ticket.ID)
	require.NoError(t, err)

	result, err := svc.PromotePending(row.Code)
	require.NoError(t, err)
	require.NotNil(t, result.User)
	assert.True(t, result.InvitationTicketConsumed, "tx 経路では Service 側で MarkUsed 済")

	// ticket が消費済 (usedById = 新 user, usedAt が set)
	var consumed model.RegistrationTicket
	require.NoError(t, db.Where("id = ?", ticket.ID).First(&consumed).Error)
	require.NotNil(t, consumed.UsedByID)
	assert.Equal(t, result.User.ID, *consumed.UsedByID)
	require.NotNil(t, consumed.UsedAt)

	// pending row が消えている
	var lingering model.UserPending
	err = db.Where("id = ?", row.ID).First(&lingering).Error
	assert.ErrorIs(t, err, gorm.ErrRecordNotFound)
}

// 同じ pending code で並列 2 PromotePending を試行すると、片方は user 作成
// 成功 / もう片方は ErrInvitationAlreadyUsed (もしくは ErrUsernameAlreadyExists)
// で拒否される (#604 race fix)。
func TestPromotePending_ConcurrentPromotesYieldOneUser(t *testing.T) {
	db := integrationDB(t)
	const prefix = "itrace_"
	defer cleanupSignupRows(t, db, prefix)

	// 同一 pending を 2 つ作って同じ ticket を共有させる concurrent シナリオ
	svcA := newTxService(t, db)
	svcB := newTxService(t, db)

	ticket := &model.RegistrationTicket{
		ID:   prefix + "tkt",
		Code: prefix + "code",
	}
	require.NoError(t, db.Create(ticket).Error)

	rowA, err := svcA.CreatePending(prefix+"userA", "a@it.example", "pw", &ticket.ID)
	require.NoError(t, err)
	rowB, err := svcB.CreatePending(prefix+"userB", "b@it.example", "pw", &ticket.ID)
	require.NoError(t, err)

	var wg sync.WaitGroup
	var errA, errB error
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, errA = svcA.PromotePending(rowA.Code)
	}()
	go func() {
		defer wg.Done()
		_, errB = svcB.PromotePending(rowB.Code)
	}()
	wg.Wait()

	// 1 件だけ成功、もう片方は ErrInvitationAlreadyUsed
	successes := 0
	if errA == nil {
		successes++
	}
	if errB == nil {
		successes++
	}
	assert.Equal(t, 1, successes, "ticket lock により 1 件だけ成功する")

	failed := errA
	if errB != nil {
		failed = errB
	}
	assert.ErrorIs(t, failed, signup.ErrInvitationAlreadyUsed)

	// user は 1 件だけ作成されている
	var users []model.User
	require.NoError(t, db.Where(`"usernameLower" LIKE ?`, prefix+"%").Find(&users).Error)
	assert.Len(t, users, 1)
}

// username 衝突で tx が rollback すると user / profile が一切残らないこと。
// (#600 item 2 partial failure rollback)。
func TestPromotePending_UsernameConflictRollsBack(t *testing.T) {
	db := integrationDB(t)
	const prefix = "itroll_"
	defer cleanupSignupRows(t, db, prefix)

	svc := newTxService(t, db)
	idGen, _ := id.NewGenerator("aidx")

	// 既存 user を seed (同名で衝突)
	existing := &model.User{
		ID:                idGen.Generate(time.Now()),
		Username:          prefix + "dup",
		UsernameLower:     prefix + "dup",
		AvatarDecorations: datatypes.JSON([]byte("[]")),
	}
	require.NoError(t, db.Create(existing).Error)

	// pending 作成 (CreatePending の事前チェックを回避するため直接 INSERT)
	pending := &model.UserPending{
		ID:       idGen.Generate(time.Now()),
		Code:     prefix + "code",
		Username: prefix + "dup",
		Email:    "dup@it.example",
		Password: "$2a$10$fakehash",
	}
	require.NoError(t, db.Create(pending).Error)

	_, err := svc.PromotePending(pending.Code)
	require.ErrorIs(t, err, signup.ErrUsernameAlreadyExists)

	// 失敗後、新 user 行は作成されていない (existing 1 件のみ)
	var users []model.User
	require.NoError(t, db.Where(`"usernameLower" = ?`, prefix+"dup").Find(&users).Error)
	assert.Len(t, users, 1, "rollback で新 user は作成されていない")
	assert.Equal(t, existing.ID, users[0].ID)

	// pending row も消えていない (rollback)
	var lingering model.UserPending
	require.NoError(t, db.Where("id = ?", pending.ID).First(&lingering).Error)
}

// 非招待 pending を tx 経路で promote する場合、ticket 経路はスキップされ
// InvitationTicketConsumed = false で返る。
func TestPromotePending_TxNoInvitation(t *testing.T) {
	db := integrationDB(t)
	const prefix = "itnoinv_"
	defer cleanupSignupRows(t, db, prefix)

	svc := newTxService(t, db)

	row, err := svc.CreatePending(prefix+"plain", "p@it.example", "pw", nil)
	require.NoError(t, err)

	result, err := svc.PromotePending(row.Code)
	require.NoError(t, err)
	assert.Nil(t, result.InvitationTicketID)
	assert.False(t, result.InvitationTicketConsumed, "非招待では consumed flag は false")
}

// tx 経路で keypair repo が wire されていれば federation 用 keypair も
// 同 tx で作成される (RSA は遅いので別テスト)。
func TestPromotePending_TxCreatesKeypair(t *testing.T) {
	db := integrationDB(t)
	const prefix = "itkp_"
	defer cleanupSignupRows(t, db, prefix)

	svc := newTxService(t, db)
	keypairRepo := repository.NewUserKeypairRepository(db)
	svc.SetKeypairRepo(keypairRepo)

	row, err := svc.CreatePending(prefix+"kp", "kp@it.example", "pw", nil)
	require.NoError(t, err)
	result, err := svc.PromotePending(row.Code)
	require.NoError(t, err)

	kp, err := keypairRepo.FindByUserID(result.User.ID)
	require.NoError(t, err)
	assert.NotEmpty(t, kp.PublicKey)
	assert.NotEmpty(t, kp.PrivateKey)
}

// tx 経路で keypairExtraRepo が wire されていれば Ed25519 鍵対も同 tx で作成される。
// FEP-521a Multikey 対応 (#1067 / #1068)。
func TestPromotePending_TxCreatesKeypairExtra(t *testing.T) {
	db := integrationDB(t)
	const prefix = "itkpx_"
	defer cleanupSignupRows(t, db, prefix)

	svc := newTxService(t, db)
	keypairRepo := repository.NewUserKeypairRepository(db)
	keypairExtraRepo := repository.NewUserKeypairExtraRepository(db)
	svc.SetKeypairRepo(keypairRepo)
	svc.SetKeypairExtraRepo(keypairExtraRepo)

	row, err := svc.CreatePending(prefix+"kpx", "kpx@it.example", "pw", nil)
	require.NoError(t, err)
	result, err := svc.PromotePending(row.Code)
	require.NoError(t, err)

	kx, err := keypairExtraRepo.FindByUserID(result.User.ID)
	require.NoError(t, err)
	assert.Contains(t, kx.Ed25519PublicKey, "PUBLIC KEY")
	assert.Contains(t, kx.Ed25519PrivateKey, "PRIVATE KEY")
}

// tx 経路で webhook hook が wire されていれば commit 後に発火する。
func TestPromotePending_TxFiresWebhook(t *testing.T) {
	db := integrationDB(t)
	const prefix = "itwh_"
	defer cleanupSignupRows(t, db, prefix)

	svc := newTxService(t, db)
	hook := &integrationHook{}
	svc.SetWebhookHook(hook)

	row, err := svc.CreatePending(prefix+"wh", "wh@it.example", "pw", nil)
	require.NoError(t, err)
	_, err = svc.PromotePending(row.Code)
	require.NoError(t, err)
	assert.Equal(t, 1, hook.calls)
}

type integrationHook struct{ calls int }

func (h *integrationHook) OnUserCreated(_ *model.User) { h.calls++ }

// pending row 作成後に ticket を delete すると、PromotePending tx 内の
// FindByIDForUpdateTx が NotFound になり ErrInvitationRevoked として返る
// (#610 item 2: AlreadyUsed と区別)。
func TestPromotePending_TxTicketRevoked(t *testing.T) {
	db := integrationDB(t)
	const prefix = "itrev_"
	defer cleanupSignupRows(t, db, prefix)

	svc := newTxService(t, db)

	ticket := &model.RegistrationTicket{
		ID:   prefix + "tkt",
		Code: prefix + "code",
	}
	require.NoError(t, db.Create(ticket).Error)

	row, err := svc.CreatePending(prefix+"u", "u@it.example", "pw", &ticket.ID)
	require.NoError(t, err)

	// admin が ticket を revoke する状況を再現
	require.NoError(t, db.Where("id = ?", ticket.ID).Delete(&model.RegistrationTicket{}).Error)

	_, err = svc.PromotePending(row.Code)
	assert.ErrorIs(t, err, signup.ErrInvitationRevoked)
	// AlreadyUsed と混同しないこと (#610 item 2)
	assert.NotErrorIs(t, err, signup.ErrInvitationAlreadyUsed)
}

// FindByIDForUpdateTx が NotFound 以外の DB error を返した場合は、Service が
// ErrInvitationRevoked にすり替えず生 error を返して handler が 500 を出せる
// ようにする (#610 item 2)。
func TestPromotePending_TxTicketRepoGenericError(t *testing.T) {
	db := integrationDB(t)
	const prefix = "iterr_"
	defer cleanupSignupRows(t, db, prefix)

	// real DB + 故意に error を返す ticket repo を組み合わせる
	idGen, _ := id.NewGenerator("aidx")
	userRepo := repository.NewUserRepository(db)
	pendingRepo := repository.NewUserPendingRepository(db)
	metaRepo := testutil.NewMockMetaRepository()
	metaRepo.Meta = &model.Meta{ID: "x"}

	svc := signup.NewService(userRepo, metaRepo, idGen)
	svc.SetUserPendingRepo(pendingRepo)
	svc.SetTicketRepo(&errorTicketRepo{
		err: errors.New("simulated DB error"),
	})
	svc.SetDB(db)

	tid := prefix + "tid"
	row, err := svc.CreatePending(prefix+"err", "err@it.example", "pw", &tid)
	require.NoError(t, err)

	_, err = svc.PromotePending(row.Code)
	require.Error(t, err)
	// NotFound 以外の error は ErrInvitationRevoked にすり替えない
	assert.NotErrorIs(t, err, signup.ErrInvitationRevoked)
	assert.NotErrorIs(t, err, signup.ErrInvitationAlreadyUsed)
}

// errorTicketRepo は FindByIDForUpdateTx で常に指定 error を返す test double。
// Create / List / Delete 等は本テストで使わないので最小実装。
type errorTicketRepo struct {
	repository.RegistrationTicketRepository
	err error
}

func (r *errorTicketRepo) FindByIDForUpdateTx(_ *gorm.DB, _ string) (*model.RegistrationTicket, error) {
	return nil, r.err
}

// insertApprovedApplication seeds an approved application for the tx tests.
func insertApprovedApplication(t *testing.T, db *gorm.DB, id string) *model.SignupApplication {
	t.Helper()
	now := time.Now()
	app := &model.SignupApplication{
		ID:            id,
		ClaimCodeHash: id + "-hash",
		Status:        model.SignupApplicationApproved,
		Answers:       []byte(`[]`),
		CreatedAt:     now,
		UpdatedAt:     now,
		ExpiresAt:     now.Add(24 * time.Hour),
	}
	require.NoError(t, db.Create(app).Error)
	return app
}

// **1 つの承認から作れるアカウントは 1 つ。** 申請者が確認リンクを踏むのと登録
// フォームの再送が重なると、2 つの pending が別々の ticket を持って両方生き残る。
// ticket ロック (#604) は ticket が違うので直列化できず、申請行を掴んで初めて
// 閉じる (#2576)。
func TestPromotePending_SecondPendingCannotCreateAnotherAccount(t *testing.T) {
	db := integrationDB(t)
	const prefix = "itapp_"
	defer cleanupSignupRows(t, db, prefix)
	defer db.Exec(`DELETE FROM "signup_application" WHERE id LIKE ?`, prefix+"%")

	svc := newTxService(t, db)
	app := insertApprovedApplication(t, db, prefix+"app1")

	// 同じ申請から 2 つの pending を作る (やり直しの残骸)。
	appID := app.ID
	p1, err := svc.CreatePendingForApplication(prefix+"one", "one@example.com", "hunter22", nil, &appID)
	require.NoError(t, err)
	p2, err := svc.CreatePendingForApplication(prefix+"two", "two@example.com", "hunter22", nil, &appID)
	require.NoError(t, err)

	// 1 つ目は通り、申請が同じ tx で completed になる。
	res, err := svc.PromotePending(p1.Code)
	require.NoError(t, err)
	require.NotNil(t, res.SignupApplicationID)
	assert.Equal(t, appID, *res.SignupApplicationID)
	assert.True(t, res.SignupApplicationCompleted, "tx 内で確定させること")

	var stored model.SignupApplication
	require.NoError(t, db.Where("id = ?", appID).First(&stored).Error)
	assert.Equal(t, model.SignupApplicationCompleted, stored.Status)
	require.NotNil(t, stored.UsedByID)
	assert.Equal(t, res.User.ID, *stored.UsedByID)

	// 2 つ目は弾かれ、**ユーザーごと巻き戻る**。
	_, err = svc.PromotePending(p2.Code)
	require.ErrorIs(t, err, signup.ErrApplicationNotApproved)

	var count int64
	require.NoError(t, db.Model(&model.User{}).
		Where(`"usernameLower" = ?`, prefix+"two").Count(&count).Error)
	assert.Equal(t, int64(0), count, "2 つ目のアカウントは作られない")

	// pending も消えていない (巻き戻っている) ので、状況が追える。
	var pendingCount int64
	require.NoError(t, db.Model(&model.UserPending{}).
		Where("id = ?", p2.ID).Count(&pendingCount).Error)
	assert.Equal(t, int64(1), pendingCount)
}

// 期限切れの申請は承認済みでも通さない。
func TestPromotePending_ExpiredApplicationIsRejected(t *testing.T) {
	db := integrationDB(t)
	const prefix = "itexp_"
	defer cleanupSignupRows(t, db, prefix)
	defer db.Exec(`DELETE FROM "signup_application" WHERE id LIKE ?`, prefix+"%")

	svc := newTxService(t, db)
	app := insertApprovedApplication(t, db, prefix+"app1")
	require.NoError(t, db.Model(app).Update("expiresAt", time.Now().Add(-time.Hour)).Error)

	appID := app.ID
	pending, err := svc.CreatePendingForApplication(prefix+"late", "late@example.com", "hunter22", nil, &appID)
	require.NoError(t, err)

	_, err = svc.PromotePending(pending.Code)
	require.ErrorIs(t, err, signup.ErrApplicationNotApproved)

	var count int64
	require.NoError(t, db.Model(&model.User{}).
		Where(`"usernameLower" = ?`, prefix+"late").Count(&count).Error)
	assert.Equal(t, int64(0), count, "期限切れの申請からアカウントを作らない")
}

// 申請と無関係の pending は従来どおり通る。
func TestPromotePending_WithoutApplicationUnaffected(t *testing.T) {
	db := integrationDB(t)
	const prefix = "itnoapp_"
	defer cleanupSignupRows(t, db, prefix)

	svc := newTxService(t, db)
	pending, err := svc.CreatePending(prefix+"plain", "plain@example.com", "hunter22", nil)
	require.NoError(t, err)

	res, err := svc.PromotePending(pending.Code)
	require.NoError(t, err)
	assert.Nil(t, res.SignupApplicationID)
	assert.False(t, res.SignupApplicationCompleted)
}

// 承認制が有効なら、申請に紐付かない pending は昇格させない (#2804)。実 DB 経路。
//
// `PendingSignupTTL` は 30 分なので、承認制へ切り替える直前 30 分に発行された確認
// メールがここに来る。申請 ID を持たないので settleApplicationTx (#2576) を通らず、
// ゲートが無いと**承認を経ていないローカルアカウント**ができる。
//
// ゲートは tx に入る前に返るので**巻き戻すものは無い**。それでも user 行を数えるのは、
// ゲートを消したり後ろへ動かしたりすると行ができることを検出するため。
func TestPromotePending_ApprovalRequiredRejectsUnappliedPendingTx(t *testing.T) {
	db := integrationDB(t)
	const prefix = "itgate_"
	defer cleanupSignupRows(t, db, prefix)

	// meta は pointer で渡るので、pending を作った後にフラグを立てられる。
	meta := &model.Meta{ID: "x"}
	svc := newTxServiceWithMeta(t, db, meta)

	// 切り替え前に発行された確認メールを再現する (承認制 OFF のうちに作る)。
	pending, err := svc.CreatePending(prefix+"late", "gate-late@example.com", "hunter22", nil)
	require.NoError(t, err)

	meta.ApprovalRequiredForSignup = true

	_, err = svc.PromotePending(pending.Code)
	require.ErrorIs(t, err, signup.ErrApplicationNotApproved)

	var count int64
	require.NoError(t, db.Model(&model.User{}).
		Where(`"usernameLower" = ?`, prefix+"late").Count(&count).Error)
	assert.Equal(t, int64(0), count, "承認を経ないアカウントを作らない")

	// pending は残す。承認制を戻した運用者が状況を追えるようにする。
	var pendingCount int64
	require.NoError(t, db.Model(&model.UserPending{}).
		Where("id = ?", pending.ID).Count(&pendingCount).Error)
	assert.Equal(t, int64(1), pendingCount)
}

// 承認制が有効でも、承認済みの申請に紐付く pending は従来どおり通る (#2804)。
func TestPromotePending_ApprovalRequiredStillAllowsApprovedApplication(t *testing.T) {
	db := integrationDB(t)
	const prefix = "itgateok_"
	defer cleanupSignupRows(t, db, prefix)
	defer db.Exec(`DELETE FROM "signup_application" WHERE id LIKE ?`, prefix+"%")

	svc := newTxServiceWithMeta(t, db, &model.Meta{ID: "x", ApprovalRequiredForSignup: true})

	app := insertApprovedApplication(t, db, prefix+"app1")
	appID := app.ID
	pending, err := svc.CreatePendingForApplication(prefix+"ok", "gate-ok@example.com", "hunter22", nil, &appID)
	require.NoError(t, err)

	res, err := svc.PromotePending(pending.Code)
	require.NoError(t, err)
	assert.True(t, res.SignupApplicationCompleted)

	var count int64
	require.NoError(t, db.Model(&model.User{}).
		Where(`"usernameLower" = ?`, prefix+"ok").Count(&count).Error)
	assert.Equal(t, int64(1), count)
}

// 申請行が消えていたら通さない。**「見つからない = 承認されていない」に倒す** —
// 承認の裏付けが無いままアカウントを作らない。
func TestPromotePending_MissingApplicationIsRejected(t *testing.T) {
	db := integrationDB(t)
	const prefix = "itgone_"
	defer cleanupSignupRows(t, db, prefix)
	defer db.Exec(`DELETE FROM "signup_application" WHERE id LIKE ?`, prefix+"%")

	svc := newTxService(t, db)
	app := insertApprovedApplication(t, db, prefix+"app1")
	appID := app.ID
	pending, err := svc.CreatePendingForApplication(prefix+"gone", "gone@example.com", "hunter22", nil, &appID)
	require.NoError(t, err)

	require.NoError(t, db.Where("id = ?", appID).Delete(&model.SignupApplication{}).Error)

	_, err = svc.PromotePending(pending.Code)
	require.ErrorIs(t, err, signup.ErrApplicationNotApproved)

	var count int64
	require.NoError(t, db.Model(&model.User{}).
		Where(`"usernameLower" = ?`, prefix+"gone").Count(&count).Error)
	assert.Equal(t, int64(0), count)
}

// 却下済みの申請も通さない。
func TestPromotePending_RejectedApplicationIsRejected(t *testing.T) {
	db := integrationDB(t)
	const prefix = "itrej_"
	defer cleanupSignupRows(t, db, prefix)
	defer db.Exec(`DELETE FROM "signup_application" WHERE id LIKE ?`, prefix+"%")

	svc := newTxService(t, db)
	app := insertApprovedApplication(t, db, prefix+"app1")
	appID := app.ID
	pending, err := svc.CreatePendingForApplication(prefix+"rej", "rej@example.com", "hunter22", nil, &appID)
	require.NoError(t, err)

	require.NoError(t, db.Model(app).Update("status", model.SignupApplicationRejected).Error)

	_, err = svc.PromotePending(pending.Code)
	require.ErrorIs(t, err, signup.ErrApplicationNotApproved)
}

// 招待 ticket 経由でも申請が確定し、ticket ID が申請に記録される。
func TestPromotePending_ApplicationRecordsConsumedTicket(t *testing.T) {
	db := integrationDB(t)
	const prefix = "ittkt_"
	defer cleanupSignupRows(t, db, prefix)
	defer db.Exec(`DELETE FROM "signup_application" WHERE id LIKE ?`, prefix+"%")

	svc := newTxService(t, db)
	app := insertApprovedApplication(t, db, prefix+"app1")
	appID := app.ID

	expires := time.Now().Add(time.Hour)
	ticket := &model.RegistrationTicket{
		ID: prefix + "tkt1", Code: prefix + "code1", ExpiresAt: &expires,
	}
	require.NoError(t, db.Create(ticket).Error)

	ticketID := ticket.ID
	pending, err := svc.CreatePendingForApplication(prefix+"tk", "tk@example.com", "hunter22", &ticketID, &appID)
	require.NoError(t, err)

	res, err := svc.PromotePending(pending.Code)
	require.NoError(t, err)
	assert.True(t, res.SignupApplicationCompleted)

	var stored model.SignupApplication
	require.NoError(t, db.Where("id = ?", appID).First(&stored).Error)
	require.NotNil(t, stored.TicketID)
	assert.Equal(t, ticketID, *stored.TicketID, "消費した ticket を申請に残す")
}

// **1 つの承認から作れるアカウントは 1 つ。** 承認済みの申請者が別々の username で
// 同時に登録すると、状態確認とアカウント作成が別々なら両方が「承認済み」を読んで
// 両方作れてしまう。申請行を掴んだまま作れば負けた側は巻き戻る (#2580)。
func TestSignupForApplication_SecondCallCannotCreateAnotherAccount(t *testing.T) {
	db := integrationDB(t)
	const prefix = "itimm_"
	defer cleanupSignupRows(t, db, prefix)
	defer db.Exec(`DELETE FROM "signup_application" WHERE id LIKE ?`, prefix+"%")

	svc := newTxService(t, db)
	app := insertApprovedApplication(t, db, prefix+"app1")

	res, err := svc.SignupForApplication(prefix+"first", "hunter22", app.ID, "")
	require.NoError(t, err)
	assert.True(t, res.SignupApplicationCompleted, "同じ tx で確定させること")
	require.NotNil(t, res.Profile, "レスポンスの packer に profile が要る")
	assert.Nil(t, res.Profile.Email, "メール確認を通っていないのでメールは持たせない")

	var stored model.SignupApplication
	require.NoError(t, db.Where("id = ?", app.ID).First(&stored).Error)
	assert.Equal(t, model.SignupApplicationCompleted, stored.Status)
	require.NotNil(t, stored.UsedByID)
	assert.Equal(t, res.User.ID, *stored.UsedByID)

	// 2 回目は別 username でも通らない。
	_, err = svc.SignupForApplication(prefix+"second", "hunter22", app.ID, "")
	require.ErrorIs(t, err, signup.ErrApplicationNotApproved)

	var count int64
	require.NoError(t, db.Model(&model.User{}).
		Where(`"usernameLower" = ?`, prefix+"second").Count(&count).Error)
	assert.Equal(t, int64(0), count, "2 つ目のアカウントは作られない")
}

// 同時に走らせても 1 つしか通らない。**行ロックが効いていることを直接見る。**
func TestSignupForApplication_ConcurrentCallsCreateOneAccount(t *testing.T) {
	db := integrationDB(t)
	const prefix = "itconc_"
	defer cleanupSignupRows(t, db, prefix)
	defer db.Exec(`DELETE FROM "signup_application" WHERE id LIKE ?`, prefix+"%")

	svc := newTxService(t, db)
	app := insertApprovedApplication(t, db, prefix+"app1")

	const n = 4
	start := make(chan struct{})
	results := make(chan error, n)
	for i := range n {
		go func(i int) {
			<-start
			_, err := svc.SignupForApplication(
				fmt.Sprintf("%su%d", prefix, i), "hunter22", app.ID, "")
			results <- err
		}(i)
	}
	close(start)

	succeeded := 0
	for range n {
		if err := <-results; err == nil {
			succeeded++
		}
	}
	assert.Equal(t, 1, succeeded, "通るのは 1 つだけ")

	var count int64
	require.NoError(t, db.Model(&model.User{}).
		Where(`"usernameLower" LIKE ?`, prefix+"u%").Count(&count).Error)
	assert.Equal(t, int64(1), count, "作られたアカウントも 1 つだけ")
}

// 期限切れ / 却下済みは通らず、アカウントも残らない。
func TestSignupForApplication_RejectsUnusableApplication(t *testing.T) {
	db := integrationDB(t)
	const prefix = "itimmbad_"
	defer cleanupSignupRows(t, db, prefix)
	defer db.Exec(`DELETE FROM "signup_application" WHERE id LIKE ?`, prefix+"%")

	svc := newTxService(t, db)

	t.Run("expired", func(t *testing.T) {
		app := insertApprovedApplication(t, db, prefix+"exp")
		require.NoError(t, db.Model(app).Update("expiresAt", time.Now().Add(-time.Hour)).Error)
		_, err := svc.SignupForApplication(prefix+"a", "hunter22", app.ID, "")
		assert.ErrorIs(t, err, signup.ErrApplicationNotApproved)
	})

	t.Run("rejected", func(t *testing.T) {
		app := insertApprovedApplication(t, db, prefix+"rej")
		require.NoError(t, db.Model(app).Update("status", model.SignupApplicationRejected).Error)
		_, err := svc.SignupForApplication(prefix+"b", "hunter22", app.ID, "")
		assert.ErrorIs(t, err, signup.ErrApplicationNotApproved)
	})

	var count int64
	require.NoError(t, db.Model(&model.User{}).
		Where(`"usernameLower" LIKE ?`, prefix+"%").Count(&count).Error)
	assert.Equal(t, int64(0), count)
}

// ticket を渡すと申請に記録される。
//
// **#2813 以降、この引数に非空を渡す本番の呼び出しは無い** (唯一の呼び出し元である
// 即時作成が `""` を渡す)。メール確認の経路は `SignupForApplication` を通らず、
// `promotePendingTx` → `settleApplicationTx` が `lockedTicketID` で同じ記録をする。
// 引数の意味自体は生きているのでここで固定しておく。
func TestSignupForApplication_RecordsTicket(t *testing.T) {
	db := integrationDB(t)
	const prefix = "itimmtkt_"
	defer cleanupSignupRows(t, db, prefix)
	defer db.Exec(`DELETE FROM "signup_application" WHERE id LIKE ?`, prefix+"%")

	svc := newTxService(t, db)
	app := insertApprovedApplication(t, db, prefix+"app1")

	_, err := svc.SignupForApplication(prefix+"u", "hunter22", app.ID, prefix+"tkt")
	require.NoError(t, err)

	var stored model.SignupApplication
	require.NoError(t, db.Where("id = ?", app.ID).First(&stored).Error)
	require.NotNil(t, stored.TicketID)
	assert.Equal(t, prefix+"tkt", *stored.TicketID)
}

// db / 申請 repo が未配線なら通常の Signup に落ちる (完了記録は呼び出し側に委ねる)。
func TestSignupForApplication_FallsBackWithoutTxWiring(t *testing.T) {
	db := integrationDB(t)
	const prefix = "itimmfb_"
	defer cleanupSignupRows(t, db, prefix)

	userRepo := repository.NewUserRepository(db)
	metaRepo := testutil.NewMockMetaRepository()
	metaRepo.Meta = &model.Meta{ID: "x"}
	idGen, _ := id.NewGenerator("aidx")
	svc := signup.NewService(userRepo, metaRepo, idGen)

	res, err := svc.SignupForApplication(prefix+"u", "hunter22", "no-such-app", "")
	require.NoError(t, err)
	assert.False(t, res.SignupApplicationCompleted)
	assert.Nil(t, res.SignupApplicationID)
	require.NotNil(t, res.Profile)
}

// 事前検査は tx に入る前に弾く。**メールも鍵も作る前に落とす。**
func TestSignupForApplication_PreChecks(t *testing.T) {
	db := integrationDB(t)
	const prefix = "itimmpre_"
	defer cleanupSignupRows(t, db, prefix)
	defer db.Exec(`DELETE FROM "signup_application" WHERE id LIKE ?`, prefix+"%")

	svc := newTxService(t, db)

	t.Run("invalid username", func(t *testing.T) {
		app := insertApprovedApplication(t, db, prefix+"a1")
		_, err := svc.SignupForApplication("!!!bad!!!", "hunter22", app.ID, "")
		assert.ErrorIs(t, err, signup.ErrInvalidUsername)
	})

	t.Run("username already exists", func(t *testing.T) {
		app := insertApprovedApplication(t, db, prefix+"a2")
		_, err := svc.SignupForApplication(prefix+"dup", "hunter22", app.ID, "")
		require.NoError(t, err)

		other := insertApprovedApplication(t, db, prefix+"a3")
		_, err = svc.SignupForApplication(prefix+"dup", "hunter22", other.ID, "")
		assert.ErrorIs(t, err, signup.ErrUsernameAlreadyExists)

		// **弾いた側の申請は消費されない。**
		var stored model.SignupApplication
		require.NoError(t, db.Where("id = ?", other.ID).First(&stored).Error)
		assert.Equal(t, model.SignupApplicationApproved, stored.Status)
	})

	t.Run("password too long", func(t *testing.T) {
		app := insertApprovedApplication(t, db, prefix+"a4")
		_, err := svc.SignupForApplication(prefix+"long", strings.Repeat("a", 80), app.ID, "")
		assert.ErrorIs(t, err, signup.ErrPasswordTooLong)
	})

	t.Run("reserved username", func(t *testing.T) {
		app := insertApprovedApplication(t, db, prefix+"a5")
		svc2 := newTxServiceWithMeta(t, db, &model.Meta{
			ID: "x", PreservedUsernames: []string{prefix + "res"},
		})
		_, err := svc2.SignupForApplication(prefix+"res", "hunter22", app.ID, "")
		assert.ErrorIs(t, err, signup.ErrUsernameReserved)
	})

	t.Run("prohibited word", func(t *testing.T) {
		app := insertApprovedApplication(t, db, prefix+"a6")
		svc2 := newTxServiceWithMeta(t, db, &model.Meta{
			ID: "x", ProhibitedWordsForNameOfUser: []string{prefix + "ban"},
		})
		_, err := svc2.SignupForApplication(prefix+"ban", "hunter22", app.ID, "")
		assert.ErrorIs(t, err, signup.ErrUsernameUsed)
	})
}

// newTxServiceWithMeta is newTxService with a caller-supplied meta.
func newTxServiceWithMeta(t *testing.T, db *gorm.DB, meta *model.Meta) *signup.Service {
	t.Helper()
	idGen, _ := id.NewGenerator("aidx")
	metaRepo := testutil.NewMockMetaRepository()
	metaRepo.Meta = meta
	svc := signup.NewService(repository.NewUserRepository(db), metaRepo, idGen)
	svc.SetUserPendingRepo(repository.NewUserPendingRepository(db))
	svc.SetTicketRepo(repository.NewRegistrationTicketRepository(db))
	svc.SetSignupApplicationRepo(repository.NewSignupApplicationRepository(db))
	svc.SetDB(db)
	return svc
}

// 申請経由の tx 経路も最小文字数を受ける (#3015)。
//
// **mock だけでは踏めない。** db / appRepo が未配線だと
// `SignupForApplication` は `Signup` へ委譲する fallback に入るので、
// tx 経路に置いた guard を外しても unit test は緑のまま通る (変異検証で実測)。
func TestSignupForApplication_RejectsUsernameShorterThanMinimum(t *testing.T) {
	db := integrationDB(t)
	const prefix = "itmul_"
	defer cleanupSignupRows(t, db, prefix)
	defer db.Exec(`DELETE FROM "signup_application" WHERE id LIKE ?`, prefix+"%")

	svc := newTxServiceWithMeta(t, db, &model.Meta{ID: "x", MinimumUsernameLength: 12})
	app := insertApprovedApplication(t, db, prefix+"app1")

	// prefix (6) + "short" = 11 文字で n-1。
	short := prefix + "short"
	require.Len(t, short, 11)
	_, err := svc.SignupForApplication(short, "hunter22", app.ID, "")
	require.ErrorIs(t, err, signup.ErrUsernameTooShort)

	var count int64
	require.NoError(t, db.Model(&model.User{}).
		Where(`"usernameLower" = ?`, short).Count(&count).Error)
	assert.Equal(t, int64(0), count, "弾いたのにアカウントを作っている")

	// **申請は消費されていないこと。** 弾いた時点で tx が始まっていれば
	// 巻き戻るはずで、消費されていたら承認が 1 回無駄になる。
	var stored model.SignupApplication
	require.NoError(t, db.Where("id = ?", app.ID).First(&stored).Error)
	assert.Equal(t, model.SignupApplicationApproved, stored.Status)

	// 境界の上側は通る。**弾きすぎていないことを見る。**
	long := prefix + "longenough"
	require.Len(t, long, 16)
	res, err := svc.SignupForApplication(long, "hunter22", app.ID, "")
	require.NoError(t, err)
	assert.Equal(t, long, res.User.Username)
}
