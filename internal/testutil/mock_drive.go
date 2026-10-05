package testutil

import (
	sortpkg "sort"
	"strings"

	"github.com/elythia-network/elythia/internal/model"
)

// MockDriveFileRepository is a test double for repository.DriveFileRepository.
type MockDriveFileRepository struct {
	Files map[string]*model.DriveFile // keyed by ID
	// EmojiReferencedURLs は DeleteOrphans / ListOrphans の guard を mock する
	// ための「emoji が参照している drive_file.url 集合」。production の SQL は
	// `NOT EXISTS (SELECT 1 FROM emoji WHERE originalUrl = url OR publicUrl = url)`
	// で、**2 列のどちらか**が一致すれば除外する (#722)。本フィールドはその
	// 和集合を表すので、片方の列だけを模倣しているわけではない。
	//
	// 空なら emoji guard は効かない。ただしそれは「userId NULL を全削除」を
	// 意味しない — userHost 条件が別にあるため、リモートの owner 無し行は
	// emoji 参照が無くても残る (#2721)。
	EmojiReferencedURLs map[string]bool

	// NoteReferencedFileIDs は ListOrphanRemoteAttachmentCandidates の guard を
	// mock するための「いずれかの note.fileIds に載っている drive_file.id 集合」
	// (#2722)。production の SQL は
	// `NOT EXISTS (SELECT 1 FROM note WHERE "fileIds" @> ARRAY[id])`。
	NoteReferencedFileIDs map[string]bool

	// BulkFolderUserID / BulkFolderFileIDs record the last UpdateBulkFolder
	// call so handler tests can assert the owning user is scoped (IDOR guard).
	BulkFolderUserID  string
	BulkFolderFileIDs []string

	// 以下は bulk cleanup (clean-remote-files / delete-all-files) のエラー経路を
	// テストするための注入フィールド。nil なら通常動作。
	ListRemoteCacheErr error
	ListByUserAllErr   error
	ListOrphansErr     error
	DeleteByIDsErr     error
	// DeleteByIDsNoOp を true にすると DeleteByIDs が err=nil で 0 件削除する
	// (= 行が縮小しない degenerate ケース。maxBatches 安全弁の終了性検証用)。
	DeleteByIDsNoOp bool
}

func NewMockDriveFileRepository() *MockDriveFileRepository {
	return &MockDriveFileRepository{Files: make(map[string]*model.DriveFile)}
}

func (m *MockDriveFileRepository) Create(f *model.DriveFile) error {
	m.Files[f.ID] = f
	return nil
}

func (m *MockDriveFileRepository) FindByID(id string) (*model.DriveFile, error) {
	f, ok := m.Files[id]
	if !ok {
		return nil, ErrNotFound
	}
	return f, nil
}

// FindByIDs mirrors production's `id IN ?`: **重複した id を渡しても行は 1 度
// しか返らない** (#2755)。
//
// 旧実装は入力の重複をそのまま重複行で返していた。漏れる先は **AP renderer の
// `addAttachments`** で、あそこは戻り行をそのまま `attachment` に並べるので
// 重複 id で Document が二重になる。到達可能な乖離だったので production に揃える。
//
// (entity の pack は `n.FileIDs` を回すので、`note.fileIds` の重複は production
// でも二重に pack される。#2755 の issue 本文にあった「pack が二重になる」は誤り。)
func (m *MockDriveFileRepository) FindByIDs(ids []string) ([]*model.DriveFile, error) {
	var result []*model.DriveFile
	seen := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		if f, ok := m.Files[id]; ok {
			result = append(result, f)
		}
	}
	return result, nil
}

func (m *MockDriveFileRepository) FindByMD5(userID, md5 string) (*model.DriveFile, error) {
	var match *model.DriveFile
	for _, f := range m.Files {
		if f.UserID != nil && *f.UserID == userID && f.MD5 == md5 {
			if match == nil || f.ID > match.ID {
				match = f
			}
		}
	}
	if match == nil {
		return nil, ErrNotFound
	}
	return match, nil
}

// FindAllByMD5 returns every md5-matching file of the user, id ASC
// (production 実装と同じ決定的順序)。
func (m *MockDriveFileRepository) FindAllByMD5(userID, md5 string) ([]*model.DriveFile, error) {
	var result []*model.DriveFile
	for _, f := range m.Files {
		if f.UserID != nil && *f.UserID == userID && f.MD5 == md5 {
			result = append(result, f)
		}
	}
	sortpkg.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result, nil
}

func (m *MockDriveFileRepository) FindByAnyURL(url string) (*model.DriveFile, error) {
	if url == "" {
		return nil, ErrNotFound
	}
	var match *model.DriveFile
	for _, f := range m.Files {
		if f.URL == url ||
			(f.WebpublicURL != nil && *f.WebpublicURL == url) ||
			(f.ThumbnailURL != nil && *f.ThumbnailURL == url) {
			if match == nil || f.ID < match.ID {
				match = f
			}
		}
	}
	if match == nil {
		return nil, ErrNotFound
	}
	return match, nil
}

func (m *MockDriveFileRepository) FindByURI(uri string) (*model.DriveFile, error) {
	if uri == "" {
		return nil, ErrNotFound
	}
	var match *model.DriveFile
	for _, f := range m.Files {
		if f.URI != nil && *f.URI == uri {
			if match == nil || f.ID < match.ID {
				match = f
			}
		}
	}
	if match == nil {
		return nil, ErrNotFound
	}
	return match, nil
}

func (m *MockDriveFileRepository) FindByAccessKey(accessKey string) (*model.DriveFile, error) {
	if accessKey == "" {
		return nil, ErrNotFound
	}
	for _, f := range m.Files {
		if f.AccessKey != nil && *f.AccessKey == accessKey {
			return f, nil
		}
	}
	return nil, ErrNotFound
}

func (m *MockDriveFileRepository) FindByAnyAccessKey(accessKey string) (*model.DriveFile, error) {
	if accessKey == "" {
		return nil, ErrNotFound
	}
	var match *model.DriveFile
	for _, f := range m.Files {
		if (f.AccessKey != nil && *f.AccessKey == accessKey) ||
			(f.ThumbnailAccessKey != nil && *f.ThumbnailAccessKey == accessKey) ||
			(f.WebpublicAccessKey != nil && *f.WebpublicAccessKey == accessKey) {
			if match == nil || f.ID < match.ID {
				match = f
			}
		}
	}
	if match == nil {
		return nil, ErrNotFound
	}
	return match, nil
}

func (m *MockDriveFileRepository) Update(id string, fields map[string]any) error {
	f, ok := m.Files[id]
	if !ok {
		return ErrNotFound
	}
	applyDriveFileFields(f, fields)
	return nil
}

func (m *MockDriveFileRepository) FindByName(userID, name string, folderID *string) ([]*model.DriveFile, error) {
	var result []*model.DriveFile
	for _, f := range m.Files {
		if f.UserID == nil || *f.UserID != userID || f.Name != name {
			continue
		}
		if !matchesDriveFolder(f.FolderID, folderID) {
			continue
		}
		result = append(result, f)
	}
	return result, nil
}

func (m *MockDriveFileRepository) ExistsByMD5(userID, md5 string) (bool, error) {
	for _, f := range m.Files {
		if f.UserID != nil && *f.UserID == userID && f.MD5 == md5 {
			return true, nil
		}
	}
	return false, nil
}

func (m *MockDriveFileRepository) CountByFolder(folderID string) (int, error) {
	n := 0
	for _, f := range m.Files {
		if f.FolderID != nil && *f.FolderID == folderID {
			n++
		}
	}
	return n, nil
}

func (m *MockDriveFileRepository) ListByFileIDs(ids []string) ([]*model.DriveFile, error) {
	return m.FindByIDs(ids)
}

func (m *MockDriveFileRepository) UsageByUser(userID string) (int64, error) {
	var total int64
	for _, f := range m.Files {
		// 本番 repo 同様 isLink=false の行のみ合算する (#1831)。
		if f.IsLink {
			continue
		}
		if f.UserID != nil && *f.UserID == userID {
			total += int64(f.Size)
		}
	}
	return total, nil
}

// UpdateBulkFolder records the call **and applies it** (#2755)。
//
// 旧実装は呼び出しを記録するだけで Files を変更しなかったため、「移動後に読み
// 直す」テストが書けなかった。production と同じく **userID で絞る** ので、
// 他人の file を指定しても動かない。
//
// **記録 (BulkFolderUserID) は今も要る。** handler が自分の user.ID を渡して
// いるかを確かめられるのは記録だけで、この述語は production の guard を
// 模しているだけ。層が違う。
func (m *MockDriveFileRepository) UpdateBulkFolder(userID string, fileIDs []string, folderID *string) error {
	m.BulkFolderUserID = userID
	m.BulkFolderFileIDs = fileIDs
	for _, id := range fileIDs {
		f, ok := m.Files[id]
		if !ok || f.UserID == nil || *f.UserID != userID {
			continue
		}
		f.FolderID = folderID
	}
	return nil
}

func (m *MockDriveFileRepository) Delete(f *model.DriveFile) error {
	delete(m.Files, f.ID)
	return nil
}

// matchesDriveFileType mirrors the production fileType predicate: a `/*`
// suffix means prefix match, anything else is an exact match (#1772).
//
// 無条件 prefix にすると `image/*` がどこにも当たらず (`image/*` で始まる
// MIME は存在しない)、逆に完全一致のつもりの `image/heic` が
// `image/heic-sequence` まで拾う。production
// (internal/repository/drive_file.go の ListByUser / ListForAdmin /
// ListSystemFiles) は 3 経路ともこの形。
func matchesDriveFileType(actual, fileType string) bool {
	if fileType == "" {
		return true
	}
	if strings.HasSuffix(fileType, "/*") {
		return strings.HasPrefix(actual, strings.TrimSuffix(fileType, "*"))
	}
	return actual == fileType
}

// matchesDriveFolder mirrors the production folder predicate used by both
// drive_file."folderId" and drive_folder."parentId": a nil want means
// `IS NULL`, not "any folder".
func matchesDriveFolder(actual, want *string) bool {
	if want == nil {
		return actual == nil
	}
	return actual != nil && *actual == *want
}

// ListByUser mirrors the production predicates and ordering. caller が知って
// おくべき差は 3 つで、うち 1 つは**意図的な簡略化**:
//
//   - `limit <= 0` を「無制限」として扱う。**乖離するのは `limit == 0` だけ** —
//     production は生の値を GORM に渡し、GORM は負値では LIMIT 句自体を出さない
//     ので `limit < 0` は production も無制限になる (#2755 のレビューで発見)。
//     handler は `pagination.ResolveLimit` が 1 未満を 400 で弾くので endpoint
//     経由では到達しない。**mock を直に叩くときは実値を渡すこと**
//
// 残り 2 つは関数本体のコメントにある: name 比較の collation 差と、tie が
// id 昇順に落ちること (production は tie 順を保証しない)。
func (m *MockDriveFileRepository) ListByUser(userID string, folderID *string, anyFolder bool, fileType, sort, untilID, sinceID string, limit int) ([]*model.DriveFile, error) {
	var rows []*model.DriveFile
	for _, f := range m.Files {
		if f.UserID == nil || *f.UserID != userID {
			continue
		}
		if !anyFolder && !matchesDriveFolder(f.FolderID, folderID) {
			continue
		}
		if !matchesDriveFileType(f.Type, fileType) {
			continue
		}
		if untilID != "" && f.ID >= untilID {
			continue
		}
		if sinceID != "" && f.ID <= sinceID {
			continue
		}
		rows = append(rows, f)
	}
	// **name の比較は Go のバイト順で、production は DB の collation に従う。**
	// テスト DB (en_US.UTF-8) では `'B' < 'a'` が偽なので、大小文字が混ざる
	// name では**別の順序になる** (逆順ではない。実測で PostgreSQL は
	// `[a a b b B c C]`、Go のバイト順は `[B C a a b b c]`)。collation は配備
	// 依存なので揃えない — 大小文字混在の name で順序を assert しないこと。
	//
	// **入力順を固定してから安定ソートする** (#2755)。map 反復は毎回順序が
	// 違うので、unstable sort と組むと同値行 (同名 / 同サイズ) の並びが run ごとに
	// 変わり、テストの flake 源になる。
	//
	// **production は同値行の順序を保証しない** — upstream files.ts と同じく
	// sort 指定時は `name` / `size` だけで order するので tiebreak が無い。
	// mock を決定的にするのは再現性のためで、**同値行の相対順序に依存する
	// アサーションを書いてよいという意味ではない**。
	// pre-sort で入力順を固定し、安定ソートで tie を id 昇順に落とす。
	// **どちらも要る** — pre-sort だけだと map 反復の非決定は消えるが、
	// unstable sort は tie グループが 2 つ以上あると並びを崩す (13 件から
	// 実測で差が出る)。
	sortpkg.Slice(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })
	// **sort を渡したかどうかで分かれる。** production (と upstream files.ts) は
	// sort 指定時だけ order を上書きし、それ以外は paginationOrder に落とす
	// (`internal/repository/drive_file.go` の switch)。未知の値も default 側なので、
	// ここも列挙した 6 つ以外は全て SortMockPage に倒す (#2766)。
	switch sort {
	case "+createdAt", "-createdAt", "+name", "-name", "+size", "-size":
		sortpkg.SliceStable(rows, func(i, j int) bool {
			switch sort {
			case "-createdAt":
				return rows[i].ID < rows[j].ID
			case "+name":
				return rows[i].Name > rows[j].Name
			case "-name":
				return rows[i].Name < rows[j].Name
			case "+size":
				return rows[i].Size > rows[j].Size
			case "-size":
				return rows[i].Size < rows[j].Size
			default: // "+createdAt"
				return rows[i].ID > rows[j].ID
			}
		})
	default:
		SortMockPage(rows, sinceID, untilID, func(f *model.DriveFile) string { return f.ID })
	}
	if limit > 0 && len(rows) > limit {
		rows = rows[:limit]
	}
	return rows, nil
}

func applyDriveFileFields(f *model.DriveFile, fields map[string]any) {
	for k, v := range fields {
		switch k {
		case "name":
			if s, ok := v.(string); ok {
				f.Name = s
			}
		case "comment":
			if s, ok := v.(*string); ok {
				f.Comment = s
			}
		case "isSensitive":
			if b, ok := v.(bool); ok {
				f.IsSensitive = b
			}
		case "maybeSensitive":
			if b, ok := v.(bool); ok {
				f.MaybeSensitive = b
			}
		case "folderId":
			if s, ok := v.(*string); ok {
				f.FolderID = s
			}
		}
	}
}

// MockDriveFolderRepository is a test double for repository.DriveFolderRepository.
type MockDriveFolderRepository struct {
	Folders map[string]*model.DriveFolder
	// FilesRef は HasChildren が参照するファイルストア。テストで紐づける。
	FilesRef *MockDriveFileRepository
}

func NewMockDriveFolderRepository() *MockDriveFolderRepository {
	return &MockDriveFolderRepository{Folders: make(map[string]*model.DriveFolder)}
}

func (m *MockDriveFolderRepository) Create(f *model.DriveFolder) error {
	m.Folders[f.ID] = f
	return nil
}

func (m *MockDriveFolderRepository) FindByID(id string) (*model.DriveFolder, error) {
	f, ok := m.Folders[id]
	if !ok {
		return nil, ErrNotFound
	}
	return f, nil
}

// FindByIDs returns the folders present in the mock for the given ID set
// (missing IDs are skipped, mirroring an IN query).
//
// file 側と同じく**重複した id は 1 度しか返さない** (#2755)。production の
// `Where("id IN ?")` (repository/drive_folder.go) がそうで、doc も「IN を模す」と
// 言っている以上ここだけ違うと主張が偽になる。
func (m *MockDriveFolderRepository) FindByIDs(ids []string) ([]*model.DriveFolder, error) {
	out := make([]*model.DriveFolder, 0, len(ids))
	seen := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		if f, ok := m.Folders[id]; ok {
			out = append(out, f)
		}
	}
	return out, nil
}

func (m *MockDriveFolderRepository) Update(id string, fields map[string]any) error {
	f, ok := m.Folders[id]
	if !ok {
		return ErrNotFound
	}
	for k, v := range fields {
		switch k {
		case "name":
			if s, ok := v.(string); ok {
				f.Name = s
			}
		case "parentId":
			if s, ok := v.(*string); ok {
				f.ParentID = s
			}
		}
	}
	return nil
}

func (m *MockDriveFolderRepository) Delete(f *model.DriveFolder) error {
	delete(m.Folders, f.ID)
	return nil
}

func (m *MockDriveFolderRepository) ListByUser(userID string, parentID *string, untilID, sinceID string, limit int) ([]*model.DriveFolder, error) {
	var rows []*model.DriveFolder
	for _, f := range m.Folders {
		if f.UserID == nil || *f.UserID != userID {
			continue
		}
		if !matchesDriveFolder(f.ParentID, parentID) {
			continue
		}
		if untilID != "" && f.ID >= untilID {
			continue
		}
		if sinceID != "" && f.ID <= sinceID {
			continue
		}
		rows = append(rows, f)
	}
	SortMockPage(rows, sinceID, untilID, func(f *model.DriveFolder) string { return f.ID })
	if limit > 0 && len(rows) > limit {
		rows = rows[:limit]
	}
	return rows, nil
}

func (m *MockDriveFolderRepository) FindByName(userID, name string, parentID *string) ([]*model.DriveFolder, error) {
	var result []*model.DriveFolder
	for _, f := range m.Folders {
		if f.UserID == nil || *f.UserID != userID || f.Name != name {
			continue
		}
		// production は parentID が nil なら `"parentId" IS NULL`。file 側の
		// FindByName と同形 (#2747)。
		if !matchesDriveFolder(f.ParentID, parentID) {
			continue
		}
		result = append(result, f)
	}
	return result, nil
}

func (m *MockDriveFolderRepository) HasChildren(folderID string) (bool, error) {
	for _, f := range m.Folders {
		if f.ParentID != nil && *f.ParentID == folderID {
			return true, nil
		}
	}
	if m.FilesRef != nil {
		for _, f := range m.FilesRef.Files {
			if f.FolderID != nil && *f.FolderID == folderID {
				return true, nil
			}
		}
	}
	return false, nil
}

func (m *MockDriveFolderRepository) CountChildFolders(parentID string) (int, error) {
	n := 0
	for _, f := range m.Folders {
		if f.ParentID != nil && *f.ParentID == parentID {
			n++
		}
	}
	return n, nil
}

func (m *MockDriveFileRepository) ListForAdmin(userID, origin, host, fileType, untilID, sinceID string, limit int) ([]*model.DriveFile, error) {
	rows := make([]*model.DriveFile, 0, len(m.Files))
	for _, f := range m.Files {
		if userID != "" {
			if f.UserID == nil || *f.UserID != userID {
				continue
			}
		} else {
			switch origin {
			case "local":
				if f.UserHost != nil {
					continue
				}
			case "remote":
				if f.UserHost == nil {
					continue
				}
			}
			if host != "" {
				if f.UserHost == nil || *f.UserHost != host {
					continue
				}
			}
		}
		if !matchesDriveFileType(f.Type, fileType) {
			continue
		}
		if untilID != "" && f.ID >= untilID {
			continue
		}
		if sinceID != "" && f.ID <= sinceID {
			continue
		}
		rows = append(rows, f)
	}
	SortMockPage(rows, sinceID, untilID, func(f *model.DriveFile) string { return f.ID })
	if limit <= 0 {
		limit = 30
	}
	// production は 100 で頭打ちにする (#2755)。handler は
	// pagination.ResolveLimit が 100 超を **400 で弾く** (丸めない) ので
	// endpoint 経由では repo に届かないが、mock を直に叩くテストが production
	// では返らない件数を受け取れてしまう。
	if limit > 100 {
		limit = 100
	}
	if limit > len(rows) {
		limit = len(rows)
	}
	return rows[:limit], nil
}

func (m *MockDriveFileRepository) ListSystemFiles(fileType, untilID, sinceID string, limit int) ([]*model.DriveFile, error) {
	rows := make([]*model.DriveFile, 0, len(m.Files))
	for _, f := range m.Files {
		if f.UserID != nil {
			continue
		}
		// production は userHost IS NULL も見る (#2753)。
		if f.UserHost != nil {
			continue
		}
		if !matchesDriveFileType(f.Type, fileType) {
			continue
		}
		if untilID != "" && f.ID >= untilID {
			continue
		}
		if sinceID != "" && f.ID <= sinceID {
			continue
		}
		rows = append(rows, f)
	}
	SortMockPage(rows, sinceID, untilID, func(f *model.DriveFile) string { return f.ID })
	if limit <= 0 {
		limit = 30
	}
	// 100 clamp の理由は ListForAdmin 側のコメントを参照 (#2755)。
	if limit > 100 {
		limit = 100
	}
	if limit > len(rows) {
		limit = len(rows)
	}
	return rows[:limit], nil
}

// DeleteOrphans の mock は production SQL の semantics を model 化する:
// userId NULL かつ userHost NULL かつ EmojiReferencedURLs に URL が
// 含まれない drive file を削除する (#722 / #2721)。
//
// `userHost` を見る条件は後から入ったもので、mock 側が追随していない間は
// 「リモートの owner 無し行が消える」という production では起きない挙動を
// 通していた。production の条件は internal/repository/drive_file.go の
// orphanWhere を参照。
func (m *MockDriveFileRepository) DeleteOrphans() (int64, error) {
	n := int64(0)
	for id, f := range m.Files {
		if f.UserID != nil {
			continue
		}
		if f.UserHost != nil {
			continue
		}
		if m.EmojiReferencedURLs[f.URL] {
			continue
		}
		delete(m.Files, id)
		n++
	}
	return n, nil
}

// ListOrphanRemoteAttachmentCandidates mirrors the production candidate query
// for owner-less remote link attachments (#2722).
//
// **参照チェックは NoteReferencedFileIDs で模倣する。** production は
// `note.fileIds @> ARRAY[id]` で「どの note からも参照されていない」ことを
// 確かめており、この述語が materialize 済み / dedup 再利用された添付を守る。
// 空にすると「参照されている行も候補に出る」という production では起きない
// 挙動を通してしまう。
func (m *MockDriveFileRepository) ListOrphanRemoteAttachmentCandidates(cutoffID, afterID string, limit int) ([]string, error) {
	if cutoffID == "" {
		return nil, nil
	}
	if limit <= 0 {
		limit = 100
	}
	ids := make([]string, 0, len(m.Files))
	for id, f := range m.Files {
		if f.UserID != nil || f.UserHost == nil || !f.IsLink {
			continue
		}
		if id >= cutoffID || id <= afterID {
			continue
		}
		if m.NoteReferencedFileIDs[id] {
			continue
		}
		ids = append(ids, id)
	}
	// production は `ORDER BY id LIMIT ?` で切る。map 走査のままだと keyset
	// cursor が飛び飛びになり、取りこぼした行が二度と候補にならない。
	sortpkg.Strings(ids)
	if len(ids) > limit {
		ids = ids[:limit]
	}
	return ids, nil
}

// ListOrphans mirrors DeleteOrphans's selection without deleting (#1724).
func (m *MockDriveFileRepository) ListOrphans(limit int) ([]*model.DriveFile, error) {
	if m.ListOrphansErr != nil {
		return nil, m.ListOrphansErr
	}
	if limit <= 0 {
		limit = 100
	}
	var out []*model.DriveFile
	for _, f := range m.Files {
		if f.UserID != nil {
			continue
		}
		// userHost 条件は #2721。理由は repository の orphanWhere の doc を参照。
		if f.UserHost != nil {
			continue
		}
		if m.EmojiReferencedURLs[f.URL] {
			continue
		}
		out = append(out, f)
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

// ExpireRemoteCache mirrors the repository: cached remote rows become link rows
// instead of being deleted (#3102)。`uri` を持たない行だけ消す。
func (m *MockDriveFileRepository) ExpireRemoteCache() (int64, error) {
	ids := make([]string, 0, len(m.Files))
	for id, f := range m.Files {
		if !f.IsLink && f.UserHost != nil {
			ids = append(ids, id)
		}
	}
	return m.ExpireByIDs(ids)
}

// ExpireByIDs turns the given rows into link rows (#3102)。
func (m *MockDriveFileRepository) ExpireByIDs(ids []string) (int64, error) {
	n := int64(0)
	for _, id := range ids {
		f, ok := m.Files[id]
		if !ok {
			continue
		}
		if f.URI == nil {
			// uri が無ければ倒せないので消す (repository と同じ)。
			delete(m.Files, id)
			n++
			continue
		}
		f.IsLink = true
		f.URL = *f.URI
		f.ThumbnailURL = nil
		f.WebpublicURL = nil
		f.StoredInternal = false
		f.Size = 0
		f.AccessKey = nil
		f.ThumbnailAccessKey = nil
		f.WebpublicAccessKey = nil
		n++
	}
	return n, nil
}

func (m *MockDriveFileRepository) ListRemoteCache(limit int) ([]*model.DriveFile, error) {
	if m.ListRemoteCacheErr != nil {
		return nil, m.ListRemoteCacheErr
	}
	if limit <= 0 {
		limit = 100
	}
	out := make([]*model.DriveFile, 0, limit)
	for _, f := range m.Files {
		if !f.IsLink && f.UserHost != nil {
			out = append(out, f)
			if len(out) >= limit {
				break
			}
		}
	}
	return out, nil
}

func (m *MockDriveFileRepository) ListByUserAll(userID string, limit int) ([]*model.DriveFile, error) {
	if m.ListByUserAllErr != nil {
		return nil, m.ListByUserAllErr
	}
	if userID == "" {
		return nil, nil
	}
	if limit <= 0 {
		limit = 100
	}
	out := make([]*model.DriveFile, 0, limit)
	for _, f := range m.Files {
		if f.UserID != nil && *f.UserID == userID {
			out = append(out, f)
			if len(out) >= limit {
				break
			}
		}
	}
	return out, nil
}

func (m *MockDriveFileRepository) DeleteByIDs(ids []string) (int64, error) {
	if m.DeleteByIDsErr != nil {
		return 0, m.DeleteByIDsErr
	}
	if m.DeleteByIDsNoOp {
		// 行を消さず成功扱い (= list が縮小しない degenerate ケース)。
		return 0, nil
	}
	n := int64(0)
	for _, id := range ids {
		if _, ok := m.Files[id]; ok {
			delete(m.Files, id)
			n++
		}
	}
	return n, nil
}

func (m *MockDriveFileRepository) DeleteByUser(userID string) (int64, error) {
	if userID == "" {
		return 0, nil
	}
	n := int64(0)
	for id, f := range m.Files {
		if f.UserID != nil && *f.UserID == userID {
			delete(m.Files, id)
			n++
		}
	}
	return n, nil
}

func (m *MockDriveFileRepository) DeleteByHost(host string) (int64, error) {
	if host == "" {
		return 0, nil
	}
	n := int64(0)
	for id, f := range m.Files {
		if f.UserHost != nil && *f.UserHost == host {
			delete(m.Files, id)
			n++
		}
	}
	return n, nil
}

func (m *MockDriveFileRepository) ListByHost(host string) ([]*model.DriveFile, error) {
	if host == "" {
		return nil, nil
	}
	var rows []*model.DriveFile
	for _, f := range m.Files {
		if f.UserHost != nil && *f.UserHost == host {
			rows = append(rows, f)
		}
	}
	return rows, nil
}
