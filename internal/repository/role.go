package repository

import (
	"time"

	"github.com/elythia-network/elythia/internal/model"
	"gorm.io/gorm"
)

// RoleRepository provides data access for the `role` table.
type RoleRepository interface {
	Create(role *model.Role) error
	FindByID(id string) (*model.Role, error)
	List() ([]*model.Role, error)
	// ListByLastUsed returns roles ordered by lastUsedAt DESC for admin/roles/list
	// (upstream `order: { lastUsedAt: 'DESC' }`、#2061)。eval 用の List() (cached,
	// displayOrder 順) とは別に毎回 fresh で引く。
	ListByLastUsed() ([]*model.Role, error)
	UpdateFields(id string, fields map[string]any) error
	Delete(id string) error
}

type roleRepository struct {
	db *gorm.DB
}

// NewRoleRepository creates a new RoleRepository.
func NewRoleRepository(db *gorm.DB) RoleRepository {
	return &roleRepository{db: db}
}

func (r *roleRepository) Create(role *model.Role) error {
	return r.db.Create(role).Error
}

func (r *roleRepository) FindByID(id string) (*model.Role, error) {
	if !storable(id) {
		return nil, ErrNotFound
	}
	var role model.Role
	if err := r.db.Where("id = ?", id).First(&role).Error; err != nil {
		return nil, err
	}
	return &role, nil
}

func (r *roleRepository) List() ([]*model.Role, error) {
	var roles []*model.Role
	if err := r.db.Order("\"displayOrder\" DESC, id ASC").Find(&roles).Error; err != nil {
		return nil, err
	}
	return roles, nil
}

func (r *roleRepository) ListByLastUsed() ([]*model.Role, error) {
	var roles []*model.Role
	// upstream admin/roles/list.ts: `order: { lastUsedAt: 'DESC' }` (#2061)。
	// 同 lastUsedAt の安定順のため id ASC を tie-breaker に足す。
	if err := r.db.Order("\"lastUsedAt\" DESC, id ASC").Find(&roles).Error; err != nil {
		return nil, err
	}
	return roles, nil
}

func (r *roleRepository) UpdateFields(id string, fields map[string]any) error {
	return r.db.Model(&model.Role{}).Where("id = ?", id).Updates(fields).Error
}

func (r *roleRepository) Delete(id string) error {
	return r.db.Where("id = ?", id).Delete(&model.Role{}).Error
}

// RoleAssignmentRepository provides data access for the `role_assignment` table.
type RoleAssignmentRepository interface {
	Create(a *model.RoleAssignment) error
	Delete(userID, roleID string) error
	// FindActive returns the matching assignment when ExpiresAt is nil or
	// strictly after at. Absence and expiry at the boundary return (nil, nil);
	// persistence failures are returned unchanged.
	FindActive(userID, roleID string, at time.Time) (*model.RoleAssignment, error)
	ListByUser(userID string) ([]*model.RoleAssignment, error)
	ListByRole(roleID string, untilID, sinceID string, limit int) ([]*model.RoleAssignment, error)
	// ListVisibleByRole is ListByRole restricted to assignees that
	// non-moderators may see: not suspended, and not a deleted local user
	// (#3999).
	ListVisibleByRole(roleID string, untilID, sinceID string, limit int) ([]*model.RoleAssignment, error)
	Exists(userID, roleID string) (bool, error)
	// CountActiveByRole returns the number of non-expired assignments for a
	// role (expiresAt IS NULL OR > now). Used for the Role packer's usersCount.
	CountActiveByRole(roleID string) (int, error)
	// CountVisibleByRole is CountActiveByRole restricted to the assignees
	// ListVisibleByRole returns (#3999).
	CountVisibleByRole(roleID string) (int, error)
	// DeleteExpired physically removes assignments whose expiresAt has passed.
	// Read paths already filter them out; this is the daily clean cron prune
	// (#1563). Returns rows deleted.
	DeleteExpired(now time.Time) (int64, error)
}

type roleAssignmentRepository struct {
	db *gorm.DB
}

// NewRoleAssignmentRepository creates a new RoleAssignmentRepository.
func NewRoleAssignmentRepository(db *gorm.DB) RoleAssignmentRepository {
	return &roleAssignmentRepository{db: db}
}

func (r *roleAssignmentRepository) Create(a *model.RoleAssignment) error {
	return r.db.Create(a).Error
}

func (r *roleAssignmentRepository) Delete(userID, roleID string) error {
	return r.db.Where("\"userId\" = ? AND \"roleId\" = ?", userID, roleID).
		Delete(&model.RoleAssignment{}).Error
}

func (r *roleAssignmentRepository) DeleteExpired(now time.Time) (int64, error) {
	res := r.db.Where(`"expiresAt" IS NOT NULL AND "expiresAt" < ?`, now).Delete(&model.RoleAssignment{})
	return res.RowsAffected, res.Error
}

// FindActive returns the active exact assignment, or (nil, nil) when no row is
// active. A nil expiry is active; an expiry equal to at is inactive.
func (r *roleAssignmentRepository) FindActive(userID, roleID string, at time.Time) (*model.RoleAssignment, error) {
	if !storable(userID) || !storable(roleID) {
		// **この関数の契約に合わせる (#3025)。** 「無い」を `(nil, nil)` で表す
		// ので、ここだけ `ErrNotFound` を返すと呼び出し側が err として扱い、
		// `roles/assignment-show` が 500 に倒れる。
		return nil, nil
	}
	var assignment model.RoleAssignment
	result := r.db.Where(`"userId" = ? AND "roleId" = ? AND ("expiresAt" IS NULL OR "expiresAt" > ?)`, userID, roleID, at).
		Limit(1).
		Find(&assignment)
	if result.Error != nil {
		return nil, result.Error
	}
	if result.RowsAffected == 0 {
		return nil, nil
	}
	return &assignment, nil
}

func (r *roleAssignmentRepository) ListByUser(userID string) ([]*model.RoleAssignment, error) {
	var assignments []*model.RoleAssignment
	now := time.Now()
	if err := r.db.Preload("Role").
		Where("\"userId\" = ? AND (\"expiresAt\" IS NULL OR \"expiresAt\" > ?)", userID, now).
		Find(&assignments).Error; err != nil {
		return nil, err
	}
	return assignments, nil
}

// visibleAssigneeCond は、モデレーターでない人に見せてよい割り当てだけを残す
// 条件 (#3999)。users/show が隠す利用者 (凍結中と、ローカルの削除済み) に揃える。
// リモートの削除済みは users/show が出しているので、ここでも残す。ローカルの
// 判定は model.User.IsLocal と同じく、host が空文字の行もローカルとみなす。
const visibleAssigneeCond = `EXISTS (SELECT 1 FROM "user" u WHERE u.id = "role_assignment"."userId" AND u."isSuspended" = false AND (COALESCE(u.host, '') <> '' OR u."isDeleted" = false))`

func (r *roleAssignmentRepository) ListByRole(roleID string, untilID, sinceID string, limit int) ([]*model.RoleAssignment, error) {
	return r.listByRole(roleID, untilID, sinceID, limit, false)
}

func (r *roleAssignmentRepository) ListVisibleByRole(roleID string, untilID, sinceID string, limit int) ([]*model.RoleAssignment, error) {
	return r.listByRole(roleID, untilID, sinceID, limit, true)
}

func (r *roleAssignmentRepository) listByRole(roleID string, untilID, sinceID string, limit int, visibleOnly bool) ([]*model.RoleAssignment, error) {
	var assignments []*model.RoleAssignment
	now := time.Now()
	q := r.db.Preload("User").
		Where("\"roleId\" = ? AND (\"expiresAt\" IS NULL OR \"expiresAt\" > ?)", roleID, now)
	if visibleOnly {
		// 取ってから外すとページが limit より短くなり、空のページも出るので、SQL で絞る。
		q = q.Where(visibleAssigneeCond)
	}
	if untilID != "" {
		q = q.Where("id < ?", untilID)
	}
	if sinceID != "" {
		q = q.Where("id > ?", sinceID)
	}
	// sinceID 単独指定時は ASC で keyset cursor を進ませる Misskey TS 互換。
	// 規則をここで手書きすると片側だけ直す事故になるので helper を呼ぶ (#2713)。
	q = q.Order(paginationOrder(sinceID, untilID, "id"))
	if limit > 0 {
		q = q.Limit(limit)
	}
	if err := q.Find(&assignments).Error; err != nil {
		return nil, err
	}
	return assignments, nil
}

func (r *roleAssignmentRepository) CountActiveByRole(roleID string) (int, error) {
	return r.countByRole(roleID, false)
}

func (r *roleAssignmentRepository) CountVisibleByRole(roleID string) (int, error) {
	return r.countByRole(roleID, true)
}

func (r *roleAssignmentRepository) countByRole(roleID string, visibleOnly bool) (int, error) {
	var count int64
	now := time.Now()
	q := r.db.Model(&model.RoleAssignment{}).
		Where("\"roleId\" = ? AND (\"expiresAt\" IS NULL OR \"expiresAt\" > ?)", roleID, now)
	if visibleOnly {
		q = q.Where(visibleAssigneeCond)
	}
	err := q.Count(&count).Error
	return int(count), err
}

func (r *roleAssignmentRepository) Exists(userID, roleID string) (bool, error) {
	var count int64
	now := time.Now()
	err := r.db.Model(&model.RoleAssignment{}).
		Where("\"userId\" = ? AND \"roleId\" = ? AND (\"expiresAt\" IS NULL OR \"expiresAt\" > ?)", userID, roleID, now).
		Count(&count).Error
	return count > 0, err
}
