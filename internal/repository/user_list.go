package repository

import (
	"errors"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/jackc/pgx/v5/pgconn"
	"gorm.io/gorm"
)

// ErrUserListDuplicateMember is returned by UserListRepository.AddMember when
// the (userListId, userId) pair already exists. Misskey TS の `ALREADY_ADDED`
// error と対応する (#396)。
var ErrUserListDuplicateMember = errors.New("user is already a member of this list")

// UserListRepository provides data access for user lists.
type UserListRepository interface {
	Create(list *model.UserList) error
	FindByID(id string) (*model.UserList, error)
	ListByUser(userID string) ([]*model.UserList, error)
	// CountByUser returns the number of lists owned by userID. Used by
	// userListLimit policy gate (#1029 PR-1 follow-up).
	CountByUser(userID string) (int64, error)
	Delete(id string) error
	AddMember(m *model.UserListMembership) error
	RemoveMember(listID, userID string) error
	ListMembers(listID string) ([]*model.UserListMembership, error)
	// ListMembershipsByUser returns every membership row for the given member
	// user, across all lists (= which lists contain this user).
	//
	// アカウント移行のリスト張り替え (#2419) 用。既存の membership 系は listID
	// 起点しか無く、「このユーザーが入っているリスト」を引けない。upstream
	// updateLists の `find({ where: { userId: src.id } })` に対応する。
	// User は preload しない (移行処理は userListId しか使わない)。
	ListMembershipsByUser(userID string) ([]*model.UserListMembership, error)
	// ListMembershipsPage returns a list's memberships (User preloaded) with
	// cursor pagination by membership id. Used by users/lists/get-memberships
	// (#1550)。
	ListMembershipsPage(listID, sinceID, untilID string, limit int) ([]*model.UserListMembership, error)
	// CountMembers returns the number of members in the given list. Used by
	// userEachUserListsLimit policy gate (#1029 PR-1 follow-up).
	CountMembers(listID string) (int64, error)
	// ListMembersByListIDs returns userIDs grouped by listID in 1 query
	// (= users/lists/list の N+1 を消す batch fetch、#876)。
	ListMembersByListIDs(listIDs []string) (map[string][]string, error)
	UpdateList(id string, fields map[string]any) error
	UpdateMembership(listID, userID string, withReplies *bool) error
	// ListsContainingMember returns lists owned by ownerID that include
	// memberUserID as a member. Used by users/lists/get-memberships.
	ListsContainingMember(ownerID, memberUserID string) ([]*model.UserList, error)
	// ListIDsByMember returns all list IDs that contain userID as a member.
	// Used by timeline fanout to push notes to user list timelines.
	ListIDsByMember(userID string) ([]string, error)
	// ListIDsAndOwnersByMember returns a map of listID → ownerID for lists that
	// contain memberID. fanoutToUserLists が followers visibility note を per-list
	// owner の follow 関係で gate するために使う (#1465)。1 query で join
	// する。空 map の場合 memberID はどの list にも属していない。
	ListIDsAndOwnersByMember(memberID string) (map[string]string, error)
}

type userListRepository struct {
	db *gorm.DB
}

func NewUserListRepository(db *gorm.DB) UserListRepository {
	return &userListRepository{db: db}
}

func (r *userListRepository) Create(list *model.UserList) error {
	return r.db.Create(list).Error
}

func (r *userListRepository) FindByID(id string) (*model.UserList, error) {
	if !storable(id) {
		return nil, ErrNotFound
	}
	var list model.UserList
	if err := r.db.Where("id = ?", id).First(&list).Error; err != nil {
		return nil, err
	}
	return &list, nil
}

func (r *userListRepository) ListByUser(userID string) ([]*model.UserList, error) {
	var lists []*model.UserList
	if err := r.db.Where("\"userId\" = ?", userID).Order("id DESC").Find(&lists).Error; err != nil {
		return nil, err
	}
	return lists, nil
}

// CountByUser returns the number of lists owned by the given user.
func (r *userListRepository) CountByUser(userID string) (int64, error) {
	var count int64
	if err := r.db.Model(&model.UserList{}).
		Where(`"userId" = ?`, userID).
		Count(&count).Error; err != nil {
		return 0, err
	}
	return count, nil
}

func (r *userListRepository) Delete(id string) error {
	return r.db.Where("id = ?", id).Delete(&model.UserList{}).Error
}

func (r *userListRepository) AddMember(m *model.UserListMembership) error {
	if err := r.db.Create(m).Error; err != nil {
		// (userListId, userId) 一意制約違反 (23505) を ErrUserListDuplicateMember
		// に変換して、API 層で TS 互換の ALREADY_ADDED に翻訳する (#396)。
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return ErrUserListDuplicateMember
		}
		return err
	}
	return nil
}

func (r *userListRepository) RemoveMember(listID, userID string) error {
	return r.db.Where("\"userListId\" = ? AND \"userId\" = ?", listID, userID).
		Delete(&model.UserListMembership{}).Error
}

// CountMembers returns the number of members in the given list.
func (r *userListRepository) CountMembers(listID string) (int64, error) {
	var count int64
	if err := r.db.Model(&model.UserListMembership{}).
		Where(`"userListId" = ?`, listID).
		Count(&count).Error; err != nil {
		return 0, err
	}
	return count, nil
}

func (r *userListRepository) ListMembers(listID string) ([]*model.UserListMembership, error) {
	var members []*model.UserListMembership
	if err := r.db.Preload("User").Where("\"userListId\" = ?", listID).Find(&members).Error; err != nil {
		return nil, err
	}
	return members, nil
}

func (r *userListRepository) ListMembershipsByUser(userID string) ([]*model.UserListMembership, error) {
	if userID == "" {
		return nil, nil
	}
	var rows []*model.UserListMembership
	if err := r.db.Where(`"userId" = ?`, userID).Order("id").Find(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

func (r *userListRepository) ListMembershipsPage(listID, sinceID, untilID string, limit int) ([]*model.UserListMembership, error) {
	if limit <= 0 {
		limit = 30
	}
	q := r.db.Preload("User").Where(`"userListId" = ?`, listID)
	if sinceID != "" {
		q = q.Where("id > ?", sinceID)
	}
	if untilID != "" {
		q = q.Where("id < ?", untilID)
	}
	var members []*model.UserListMembership
	if err := q.Order(paginationOrder(sinceID, untilID, "id")).Limit(limit).Find(&members).Error; err != nil {
		return nil, err
	}
	return members, nil
}

func (r *userListRepository) UpdateList(id string, fields map[string]any) error {
	return r.db.Model(&model.UserList{}).Where("id = ?", id).Updates(fields).Error
}

func (r *userListRepository) UpdateMembership(listID, userID string, withReplies *bool) error {
	// withReplies が nil (= request で省略) の場合は既存値を維持する (旧 false
	// clobber bug の修正、#1948-15)。membership 不在時は handler の NO_SUCH_USER
	// 判定のため ErrRecordNotFound を返す必要があるので存在確認を行う。
	if withReplies == nil {
		var count int64
		if err := r.db.Model(&model.UserListMembership{}).
			Where("\"userListId\" = ? AND \"userId\" = ?", listID, userID).
			Count(&count).Error; err != nil {
			return err
		}
		if count == 0 {
			return gorm.ErrRecordNotFound
		}
		return nil
	}
	result := r.db.Model(&model.UserListMembership{}).
		Where("\"userListId\" = ? AND \"userId\" = ?", listID, userID).
		Update("withReplies", *withReplies)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return gorm.ErrRecordNotFound
	}
	return nil
}

func (r *userListRepository) ListIDsByMember(userID string) ([]string, error) {
	var ids []string
	err := r.db.Model(&model.UserListMembership{}).
		Where(`"userId" = ?`, userID).
		Pluck(`"userListId"`, &ids).Error
	if err != nil {
		return nil, err
	}
	return ids, nil
}

// ListIDsAndOwnersByMember returns {listID: ownerID} for every list that
// contains memberID. fanoutToUserLists が followers visibility note の per-list
// owner follow gate を 1 query で済ませるために導入 (#1465)。
func (r *userListRepository) ListIDsAndOwnersByMember(memberID string) (map[string]string, error) {
	out := make(map[string]string)
	type row struct {
		UserListID string `gorm:"column:userListId"`
		UserID     string `gorm:"column:userId"`
	}
	var rows []row
	err := r.db.Table(`"user_list_membership"`).
		Select(`"user_list_membership"."userListId" AS "userListId", "user_list"."userId" AS "userId"`).
		Joins(`JOIN "user_list" ON "user_list"."id" = "user_list_membership"."userListId"`).
		Where(`"user_list_membership"."userId" = ?`, memberID).
		Find(&rows).Error
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		out[r.UserListID] = r.UserID
	}
	return out, nil
}

// ListMembersByListIDs returns userIDs grouped by listID in a single SELECT.
// users/lists/list の per-list N+1 query を消すための batch fetch (#876)。
// listIDs が空なら空 map を返す。row 順は保証しないので caller 側で並び順
// に依存しないこと。
func (r *userListRepository) ListMembersByListIDs(listIDs []string) (map[string][]string, error) {
	out := make(map[string][]string)
	if len(listIDs) == 0 {
		return out, nil
	}
	type row struct {
		UserListID string `gorm:"column:userListId"`
		UserID     string `gorm:"column:userId"`
	}
	var rows []row
	if err := r.db.Model(&model.UserListMembership{}).
		Where(`"userListId" IN ?`, listIDs).
		Find(&rows).Error; err != nil {
		return nil, err
	}
	for _, r := range rows {
		out[r.UserListID] = append(out[r.UserListID], r.UserID)
	}
	return out, nil
}

func (r *userListRepository) ListsContainingMember(ownerID, memberUserID string) ([]*model.UserList, error) {
	var lists []*model.UserList
	err := r.db.
		Joins(`JOIN "user_list_membership" m ON m."userListId" = "user_list"."id"`).
		Where(`"user_list"."userId" = ? AND m."userId" = ?`, ownerID, memberUserID).
		Order(`"user_list"."id" DESC`).
		Find(&lists).Error
	if err != nil {
		return nil, err
	}
	return lists, nil
}
