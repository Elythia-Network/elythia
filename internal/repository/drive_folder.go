package repository

import (
	"github.com/elythia-network/elythia/internal/model"
	"gorm.io/gorm"
)

// DriveFolderRepository provides data access for the `drive_folder` table.
type DriveFolderRepository interface {
	Create(f *model.DriveFolder) error
	FindByID(id string) (*model.DriveFolder, error)
	// FindByIDs returns folders for the given ID set in a single query.
	// entity packing が folder embed を batch するために使う (timeline N+1 解消)。
	FindByIDs(ids []string) ([]*model.DriveFolder, error)
	Update(id string, fields map[string]any) error
	Delete(f *model.DriveFolder) error
	ListByUser(userID string, parentID *string, untilID, sinceID string, limit int) ([]*model.DriveFolder, error)
	HasChildren(folderID string) (bool, error)
	// CountChildFolders は detail mode pack 用に「直下の sub-folder 数」を
	// 返す (#845)。HasChildren は exists check で folder + file を兼ねるが、
	// 本 method は folder のみ厳密に count する。
	CountChildFolders(parentID string) (int, error)
	FindByName(userID, name string, parentID *string) ([]*model.DriveFolder, error)
}

type driveFolderRepository struct {
	db *gorm.DB
}

// NewDriveFolderRepository creates a new DriveFolderRepository.
func NewDriveFolderRepository(db *gorm.DB) DriveFolderRepository {
	return &driveFolderRepository{db: db}
}

func (r *driveFolderRepository) Create(f *model.DriveFolder) error {
	return r.db.Create(f).Error
}

func (r *driveFolderRepository) FindByID(id string) (*model.DriveFolder, error) {
	if !storable(id) {
		return nil, ErrNotFound
	}
	var f model.DriveFolder
	if err := r.db.First(&f, "id = ?", id).Error; err != nil {
		return nil, err
	}
	return &f, nil
}

func (r *driveFolderRepository) FindByIDs(ids []string) ([]*model.DriveFolder, error) {
	ids = storableIDs(ids)
	if len(ids) == 0 {
		return nil, nil
	}
	var folders []*model.DriveFolder
	if err := r.db.Where("id IN ?", ids).Find(&folders).Error; err != nil {
		return nil, err
	}
	return folders, nil
}

func (r *driveFolderRepository) Update(id string, fields map[string]any) error {
	if len(fields) == 0 {
		return nil
	}
	return r.db.Model(&model.DriveFolder{}).Where("id = ?", id).Updates(fields).Error
}

func (r *driveFolderRepository) Delete(f *model.DriveFolder) error {
	return r.db.Delete(f).Error
}

// ListByUser returns the user's folders. parentID=nil means root level
// (parentId IS NULL); otherwise restricts to that parent.
func (r *driveFolderRepository) ListByUser(userID string, parentID *string, untilID, sinceID string, limit int) ([]*model.DriveFolder, error) {
	var rows []*model.DriveFolder
	q := r.db.Where("\"userId\" = ?", userID)
	if parentID == nil {
		q = q.Where("\"parentId\" IS NULL")
	} else {
		q = q.Where("\"parentId\" = ?", *parentID)
	}
	if untilID != "" {
		q = q.Where("id < ?", untilID)
	}
	if sinceID != "" {
		q = q.Where("id > ?", sinceID)
	}
	// upstream drive/folders.ts は makePaginationQuery を使う (#2713)。
	if err := q.Order(paginationOrder(sinceID, untilID, "id")).Limit(limit).Find(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

// CountChildFolders returns the number of immediate sub-folders.
func (r *driveFolderRepository) CountChildFolders(parentID string) (int, error) {
	var n int64
	if err := r.db.Model(&model.DriveFolder{}).Where(`"parentId" = ?`, parentID).Count(&n).Error; err != nil {
		return 0, err
	}
	return int(n), nil
}

// HasChildren reports whether the folder has any nested folders or files.
// 削除前のチェックに使用する。folder/file 両方を 1 クエリで EXISTS チェックする。
func (r *driveFolderRepository) HasChildren(folderID string) (bool, error) {
	var exists bool
	err := r.db.Raw(`SELECT EXISTS (
		SELECT 1 FROM "drive_folder" WHERE "parentId" = ?
		UNION ALL
		SELECT 1 FROM "drive_file" WHERE "folderId" = ?
	)`, folderID, folderID).Scan(&exists).Error
	return exists, err
}

func (r *driveFolderRepository) FindByName(userID, name string, parentID *string) ([]*model.DriveFolder, error) {
	q := r.db.Where(`"userId" = ? AND "name" = ?`, userID, name)
	if parentID != nil {
		q = q.Where(`"parentId" = ?`, *parentID)
	} else {
		q = q.Where(`"parentId" IS NULL`)
	}
	var folders []*model.DriveFolder
	if err := q.Find(&folders).Error; err != nil {
		return nil, err
	}
	return folders, nil
}
