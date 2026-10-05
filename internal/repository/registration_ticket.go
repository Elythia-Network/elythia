package repository

import (
	"time"

	"github.com/elythia-network/elythia/internal/model"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Registration ticket filter values accepted by
// RegistrationTicketRepository.List. Declared as plain string constants so
// callers (admin handlers, testutil mocks) can pass literals without taking a
// dependency on this package's type system — avoiding an import cycle with
// testutil.
const (
	// RegistrationTicketAll returns every ticket.
	RegistrationTicketAll = "all"
	// RegistrationTicketUnused returns tickets that have not been redeemed.
	RegistrationTicketUnused = "unused"
	// RegistrationTicketUsed returns tickets that have been redeemed.
	RegistrationTicketUsed = "used"
	// RegistrationTicketExpired returns tickets past their expiresAt.
	RegistrationTicketExpired = "expired"
)

// RegistrationTicketRepository handles persistence for the
// `registration_ticket` table that backs admin invite code management.
type RegistrationTicketRepository interface {
	Create(t *model.RegistrationTicket) error
	// FindByCode returns the ticket matching the human-readable invite code.
	FindByCode(code string) (*model.RegistrationTicket, error)
	// FindByIDForUpdateTx returns the ticket by ID with a row-level write
	// lock (`SELECT ... FOR UPDATE`). 必ず caller が渡した tx 内で実行される
	// 想定で、PromotePending のような "ticket 消費を atomic に行う" 経路で
	// concurrent burst を直列化するために使う (#604 race fix)。
	FindByIDForUpdateTx(tx *gorm.DB, id string) (*model.RegistrationTicket, error)
	// MarkUsed records ticket consumption via the bare repo connection.
	// Signup 直接 path (handler が PromotePending 経由ではなく Signup を呼ぶ
	// 経路) で使う best-effort path。
	MarkUsed(ticketID, userID string) error
	// ClaimForSignup atomically reserves an unused ticket for a signup by
	// setting usedAt, and reports whether this call got it. With
	// emailRequired, a ticket whose confirmation mail was sent more than
	// SignupConfirmationResendWindow ago can be claimed again.
	ClaimForSignup(ticketID string, emailRequired bool) (bool, error)
	// ReleaseClaim undoes ClaimForSignup when the signup failed. A ticket
	// already bound to an account (usedById set) is left alone.
	ReleaseClaim(ticketID string) error
	// MarkUsedTx records ticket consumption inside the given transaction.
	// PromotePending tx 経路で SELECT FOR UPDATE ロック中に使うので、
	// commit までロックを保持して race を完全に閉じる。
	MarkUsedTx(tx *gorm.DB, ticketID, userID string) error
	// MarkPending records that a ticket has been used to create a pending
	// signup (email 確認待ち): usedAt + pendingUserId をセットする (usedById は
	// 立てない)。upstream SignupApiService の email path で確認メール送信時に
	// `update({usedAt, pendingUserId})` する挙動と一致 (#2083)。確認完了時に
	// MarkUsed で usedById が立つ。
	MarkPending(ticketID, pendingID string) error
	// List returns tickets matching the filter, paginated with limit/offset.
	// Unknown filter values are treated as "all".
	// `now` is passed by callers so tests can supply a deterministic clock
	// when evaluating the `expired` filter.
	List(filter string, limit, offset int, now time.Time) ([]*model.RegistrationTicket, error)
	// ListSorted is List plus the upstream admin/invite/list sort enum
	// (+createdAt/-createdAt/+usedAt/-usedAt、#1545)。空 sort は id DESC。
	ListSorted(filter, sort string, limit, offset int, now time.Time) ([]*model.RegistrationTicket, error)
	// CountByCreatorSince returns the number of tickets created by creatorID
	// whose ID compares strictly greater than sinceID. Used by
	// invite/create + invite/limit endpoints to enforce the
	// inviteLimit + inviteLimitCycle role policy (#1029 PR-2). aidx 型 ID
	// は時刻 prefix を持つので id 比較で「特定時刻以降に作成された」レコード
	// を絞り込める (upstream は typeorm `MoreThan(idService.gen(time))` で
	// 同じセマンティクスを実現している)。
	CountByCreatorSince(creatorID, sinceID string) (int64, error)
	// FindByID returns the ticket with the given primary key, or
	// gorm.ErrRecordNotFound when no row matches.
	FindByID(id string) (*model.RegistrationTicket, error)
	// ListByCreator returns tickets owned by creatorID, paginated by id
	// cursor (sinceID < id < untilID, empty cursors disable each bound).
	// Used by invite/list (= 自分が発行した invite の一覧)。
	ListByCreator(creatorID, sinceID, untilID string, limit int) ([]*model.RegistrationTicket, error)
	Delete(id string) error
}

type registrationTicketRepository struct {
	db *gorm.DB
}

// NewRegistrationTicketRepository returns a GORM-backed RegistrationTicketRepository.
func NewRegistrationTicketRepository(db *gorm.DB) RegistrationTicketRepository {
	return &registrationTicketRepository{db: db}
}

func (r *registrationTicketRepository) Create(t *model.RegistrationTicket) error {
	return r.db.Create(t).Error
}

func (r *registrationTicketRepository) FindByCode(code string) (*model.RegistrationTicket, error) {
	if !storable(code) {
		return nil, ErrNotFound
	}
	var ticket model.RegistrationTicket
	if err := r.db.Where(`"code" = ?`, code).First(&ticket).Error; err != nil {
		return nil, err
	}
	return &ticket, nil
}

func (r *registrationTicketRepository) FindByIDForUpdateTx(tx *gorm.DB, id string) (*model.RegistrationTicket, error) {
	if !storable(id) {
		return nil, ErrNotFound
	}
	var ticket model.RegistrationTicket
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
		Where(`"id" = ?`, id).First(&ticket).Error; err != nil {
		return nil, err
	}
	return &ticket, nil
}

func (r *registrationTicketRepository) MarkUsed(ticketID, userID string) error {
	return r.markUsedOn(r.db, ticketID, userID)
}

// SignupConfirmationResendWindow is how long an email-confirmation signup
// holds its invitation ticket before the code can be used again.
const SignupConfirmationResendWindow = 30 * time.Minute

// ClaimForSignup reserves the ticket with a conditional UPDATE.
//
// **検証 (FindByCode) と消費の間を詰める。** 読んでから作る形だと、同じコードで
// 並行に来たリクエストが全部検証を通り、1 枚の招待コードで複数のアカウントが
// できる。作成の前にこの条件付き UPDATE で先に確保し、影響行数で 1 件だけを
// 通す (upstream SignupApiService の claimRegistrationTicket と同じ条件)。
func (r *registrationTicketRepository) ClaimForSignup(ticketID string, emailRequired bool) (bool, error) {
	now := time.Now()
	q := r.db.Model(&model.RegistrationTicket{}).Where(`"id" = ? AND "usedById" IS NULL`, ticketID)
	if emailRequired {
		q = q.Where(`("usedAt" IS NULL OR "usedAt" <= ?)`, now.Add(-SignupConfirmationResendWindow))
	} else {
		q = q.Where(`"usedAt" IS NULL`)
	}
	res := q.Update("usedAt", now)
	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected == 1, nil
}

// ReleaseClaim clears the reservation made by ClaimForSignup unless the
// ticket is already bound to an account.
func (r *registrationTicketRepository) ReleaseClaim(ticketID string) error {
	return r.db.Model(&model.RegistrationTicket{}).
		Where(`"id" = ? AND "usedById" IS NULL`, ticketID).
		Updates(map[string]any{"usedAt": nil, "pendingUserId": nil}).Error
}

func (r *registrationTicketRepository) MarkUsedTx(tx *gorm.DB, ticketID, userID string) error {
	return r.markUsedOn(tx, ticketID, userID)
}

func (r *registrationTicketRepository) markUsedOn(db *gorm.DB, ticketID, userID string) error {
	now := time.Now()
	return db.Model(&model.RegistrationTicket{}).Where(`"id" = ?`, ticketID).Updates(map[string]any{
		"usedById": userID,
		"usedAt":   now,
	}).Error
}

func (r *registrationTicketRepository) MarkPending(ticketID, pendingID string) error {
	// usedAt + pendingUserId のみ更新 (usedById は確認完了時に MarkUsed が立てる)。
	return r.db.Model(&model.RegistrationTicket{}).Where(`"id" = ?`, ticketID).Updates(map[string]any{
		"usedAt":        time.Now(),
		"pendingUserId": pendingID,
	}).Error
}

func (r *registrationTicketRepository) List(filter string, limit, offset int, now time.Time) ([]*model.RegistrationTicket, error) {
	return r.ListSorted(filter, "", limit, offset, now)
}

func (r *registrationTicketRepository) ListSorted(filter, sort string, limit, offset int, now time.Time) ([]*model.RegistrationTicket, error) {
	if limit <= 0 {
		limit = 30
	}
	if offset < 0 {
		offset = 0
	}
	q := r.db.Model(&model.RegistrationTicket{})
	switch filter {
	case RegistrationTicketUnused:
		q = q.Where(`"usedById" IS NULL`)
	case RegistrationTicketUsed:
		q = q.Where(`"usedById" IS NOT NULL`)
	case RegistrationTicketExpired:
		q = q.Where(`"expiresAt" IS NOT NULL AND "expiresAt" < ?`, now)
	}
	// upstream admin/invite/list sort enum (#1545)。createdAt は aidx id に対応
	// (+ が DESC、- が ASC という upstream の符号付け)。usedAt は NULLS 順も合わせる。
	var order string
	switch sort {
	case "-createdAt":
		order = `"id" ASC`
	case "+usedAt":
		order = `"usedAt" DESC NULLS LAST`
	case "-usedAt":
		order = `"usedAt" ASC NULLS FIRST`
	default: // "+createdAt" / 空 / 不正値
		order = `"id" DESC`
	}
	var rows []*model.RegistrationTicket
	if err := q.Order(order).Limit(limit).Offset(offset).Find(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

// CountByCreatorSince counts tickets created by creatorID with id > sinceID.
func (r *registrationTicketRepository) CountByCreatorSince(creatorID, sinceID string) (int64, error) {
	var count int64
	if err := r.db.Model(&model.RegistrationTicket{}).
		Where(`"createdById" = ? AND "id" > ?`, creatorID, sinceID).
		Count(&count).Error; err != nil {
		return 0, err
	}
	return count, nil
}

// FindByID returns the ticket by primary key.
func (r *registrationTicketRepository) FindByID(id string) (*model.RegistrationTicket, error) {
	if !storable(id) {
		return nil, ErrNotFound
	}
	var ticket model.RegistrationTicket
	if err := r.db.Where(`"id" = ?`, id).First(&ticket).Error; err != nil {
		return nil, err
	}
	return &ticket, nil
}

// ListByCreator returns the invites owned by creatorID with id cursor
// pagination, limited to limit rows. limit <= 0 falls back to 30, > 100 is
// clamped to 100 (= upstream paramDef.limit のデフォルトと max)。
func (r *registrationTicketRepository) ListByCreator(creatorID, sinceID, untilID string, limit int) ([]*model.RegistrationTicket, error) {
	if limit <= 0 {
		limit = 30
	}
	if limit > 100 {
		limit = 100
	}
	q := r.db.Model(&model.RegistrationTicket{}).
		Where(`"createdById" = ?`, creatorID).
		Limit(limit)
	if sinceID != "" {
		q = q.Where(`"id" > ?`, sinceID)
	}
	if untilID != "" {
		q = q.Where(`"id" < ?`, untilID)
	}
	var rows []*model.RegistrationTicket
	// upstream invite/list.ts は makePaginationQuery を使う (#2713)。
	if err := q.Order(paginationOrder(sinceID, untilID, `"id"`)).Find(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

func (r *registrationTicketRepository) Delete(id string) error {
	return r.db.Where("id = ?", id).Delete(&model.RegistrationTicket{}).Error
}
