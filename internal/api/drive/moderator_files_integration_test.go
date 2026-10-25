package drive

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/bgwork"
	coredrive "github.com/elythia-network/elythia/internal/core/drive"
	"github.com/elythia-network/elythia/internal/core/moderationlog"
	"github.com/elythia-network/elythia/internal/core/role"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/elythia-network/elythia/internal/server/middleware"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// Error ids of drive/files/update and drive/files/delete (upstream meta.errors).
const (
	updateAccessDeniedID     = "01a53b27-82fc-445b-a0c1-b558465a8ed2"
	deleteAccessDeniedID     = "5eb8d909-2540-4970-90b8-dd6f86088121"
	updateRestrictedByRoleID = "7f59dccb-f465-75ab-5cf4-3ce44e3282f7"
)

// recordingDrivePublisher records drive stream events as "<userID>:<type>".
type recordingDrivePublisher struct {
	mu     sync.Mutex
	events []string
}

func (p *recordingDrivePublisher) PublishDriveEvent(userID, eventType string, _ any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.events = append(p.events, userID+":"+eventType)
}

func (p *recordingDrivePublisher) snapshot() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.events...)
}

// moderatorFilesFixture wires drive/files/update and drive/files/delete to
// the real PostgreSQL repositories, the real role service and the real
// moderation log, the same way the router does (#3678).
type moderatorFilesFixture struct {
	t        *testing.T
	db       *gorm.DB
	idGen    id.Generator
	prefix   string
	h        *Handler
	svc      *coredrive.Service
	storeDir string
	pub      *recordingDrivePublisher
	// logs runs the asynchronous moderation log writes; stopping it waits
	// for them before the table is read.
	logs  *bgwork.Group
	owner *model.User
	mod   *model.User
	plain *model.User
	// modRole is the moderator role assigned to mod.
	modRole *model.Role
	// ownerless holds the ids of files without an owner, which cleanup cannot
	// find by the prefix of userId.
	ownerless []string
	// uploads numbers the uploads so each has its own body.
	uploads int
}

func newModeratorFilesFixture(t *testing.T) *moderatorFilesFixture {
	t.Helper()
	db, err := testutil.OpenTestDB()
	if err != nil {
		t.Skipf("PostgreSQL test DB unavailable: %v", err)
	}
	testutil.ApplyMigrations(db)

	idGen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	fx := &moderatorFilesFixture{
		t: t, db: db, idGen: idGen,
		prefix:   "m3678" + idGen.Generate(time.Now()),
		storeDir: t.TempDir(),
		pub:      &recordingDrivePublisher{},
		logs:     bgwork.New(),
	}
	fx.owner = fx.createUser("owner")
	fx.mod = fx.createUser("mod")
	fx.plain = fx.createUser("plain")
	fx.modRole = fx.createRole("mod", true, nil)
	fx.assignRole(fx.mod, fx.modRole)
	t.Cleanup(fx.cleanup)

	meta := testutil.NewMockMetaRepository()
	meta.Meta = &model.Meta{ID: "x"}
	roles := role.NewService(repository.NewRoleRepository(db), repository.NewRoleAssignmentRepository(db), meta, idGen)
	modLog := moderationlog.New(repository.NewModerationLogRepository(db), idGen, fx.logs)

	svc := coredrive.NewService(repository.NewDriveFileRepository(db), repository.NewDriveFolderRepository(db),
		coredrive.NewLocalStorage(fx.storeDir, "https://example.com/files"), idGen,
		nil, roles, modLog, repository.NewUserRepository(db))
	svc.SetStreamingPublisher(fx.pub)
	fx.svc = svc
	fx.h = NewHandler(svc, idGen, nil, nil, nil)
	return fx
}

func (fx *moderatorFilesFixture) createUser(name string) *model.User {
	fx.t.Helper()
	u := &model.User{ID: fx.prefix + name, Username: fx.prefix + name, UsernameLower: fx.prefix + name}
	require.NoError(fx.t, fx.db.Create(u).Error)
	return u
}

func (fx *moderatorFilesFixture) createRole(name string, moderator bool, policies map[string]any) *model.Role {
	fx.t.Helper()
	raw, err := json.Marshal(policies)
	require.NoError(fx.t, err)
	if policies == nil {
		raw = []byte(`{}`)
	}
	now := time.Now()
	r := &model.Role{
		ID: fx.prefix + name, Name: fx.prefix + name, IsModerator: moderator,
		UpdatedAt: now, LastUsedAt: now, Policies: raw,
	}
	require.NoError(fx.t, fx.db.Create(r).Error)
	return r
}

func (fx *moderatorFilesFixture) assignRole(u *model.User, r *model.Role) {
	fx.t.Helper()
	require.NoError(fx.t, fx.db.Create(&model.RoleAssignment{
		ID: fx.idGen.Generate(time.Now()), UserID: u.ID, RoleID: r.ID,
	}).Error)
}

func (fx *moderatorFilesFixture) cleanup() {
	fx.logs.StopWithin(bgwork.MaxWait)
	like := fx.prefix + "%"
	fx.db.Exec(`DELETE FROM "moderation_log" WHERE "userId" LIKE ?`, like)
	fx.db.Exec(`DELETE FROM "drive_file" WHERE "userId" LIKE ?`, like)
	if len(fx.ownerless) > 0 {
		fx.db.Exec(`DELETE FROM "drive_file" WHERE id IN ?`, fx.ownerless)
	}
	fx.db.Exec(`DELETE FROM "drive_folder" WHERE "userId" LIKE ?`, like)
	fx.db.Exec(`DELETE FROM "role_assignment" WHERE "userId" LIKE ?`, like)
	fx.db.Exec(`DELETE FROM "role" WHERE id LIKE ?`, like)
	fx.db.Exec(`DELETE FROM "user" WHERE id LIKE ?`, like)
}

// body returns a body no other upload of the fixture has.
//
// Upload は同じ利用者の同じ md5 のファイルを作り直さずに返すので、本体を
// 変えないと 2 回目以降が同じ行になる。
func (fx *moderatorFilesFixture) body() []byte {
	fx.uploads++
	return []byte(fmt.Sprintf("hello %s %d", fx.prefix, fx.uploads))
}

// upload stores a file owned by u through the real upload path, so it has a
// stored object to delete.
func (fx *moderatorFilesFixture) upload(u *model.User) *model.DriveFile {
	fx.t.Helper()
	f, err := fx.svc.Upload(context.Background(), coredrive.UploadInput{User: u, Body: fx.body(), Name: "a.txt"})
	require.NoError(fx.t, err)
	return f
}

// uploadOwnerless stores a file without an owner: system-owned when host is
// nil, otherwise a remote attachment whose author is not materialized yet.
func (fx *moderatorFilesFixture) uploadOwnerless(host *string) *model.DriveFile {
	fx.t.Helper()
	f, err := fx.svc.Upload(context.Background(), coredrive.UploadInput{Body: fx.body(), Name: "a.txt"})
	require.NoError(fx.t, err)
	fx.ownerless = append(fx.ownerless, f.ID)
	require.Nil(fx.t, f.UserID)
	if host != nil {
		require.NoError(fx.t, fx.db.Model(&model.DriveFile{}).Where("id = ?", f.ID).Update("userHost", *host).Error)
	}
	return fx.load(f.ID)
}

func (fx *moderatorFilesFixture) setSensitive(f *model.DriveFile, v bool) {
	fx.t.Helper()
	require.NoError(fx.t, fx.db.Model(&model.DriveFile{}).Where("id = ?", f.ID).Update("isSensitive", v).Error)
}

func (fx *moderatorFilesFixture) load(fileID string) *model.DriveFile {
	fx.t.Helper()
	var f model.DriveFile
	require.NoError(fx.t, fx.db.Where("id = ?", fileID).First(&f).Error)
	return &f
}

func (fx *moderatorFilesFixture) exists(fileID string) bool {
	fx.t.Helper()
	var n int64
	require.NoError(fx.t, fx.db.Model(&model.DriveFile{}).Where("id = ?", fileID).Count(&n).Error)
	return n > 0
}

func (fx *moderatorFilesFixture) call(fn func(*Handler, echo.Context) error, u *model.User, body string) *httptest.ResponseRecorder {
	fx.t.Helper()
	c, rec := newJSONReq(fx.t, body)
	c.Set(string(middleware.UserContextKey), u)
	require.NoError(fx.t, fn(fx.h, c))
	return rec
}

func (fx *moderatorFilesFixture) update(u *model.User, body string) *httptest.ResponseRecorder {
	fx.t.Helper()
	return fx.call((*Handler).FilesUpdate, u, body)
}

func (fx *moderatorFilesFixture) delete(u *model.User, fileID string) *httptest.ResponseRecorder {
	fx.t.Helper()
	return fx.call((*Handler).FilesDelete, u, `{"fileId":"`+fileID+`"}`)
}

// modLogEntry is a moderation_log row with its info decoded.
type modLogEntry struct {
	UserID string
	Type   string
	Info   map[string]any
}

// moderationLogs waits for the pending writes and returns every row the
// fixture's users wrote, oldest first. Writes after this call are not run.
func (fx *moderatorFilesFixture) moderationLogs() []modLogEntry {
	fx.t.Helper()
	require.Zero(fx.t, fx.logs.StopWithin(bgwork.MaxWait), "moderation log writes did not finish")
	var rows []model.ModerationLog
	require.NoError(fx.t, fx.db.Where(`"userId" LIKE ?`, fx.prefix+"%").Order("id").Find(&rows).Error)
	out := make([]modLogEntry, 0, len(rows))
	for _, r := range rows {
		var info map[string]any
		require.NoError(fx.t, json.Unmarshal(r.Info, &info))
		out = append(out, modLogEntry{UserID: r.UserID, Type: r.Type, Info: info})
	}
	return out
}

// fileLogInfo is the info upstream DriveService writes for a local owner.
func (fx *moderatorFilesFixture) fileLogInfo(fileID string) map[string]any {
	return map[string]any{
		"fileId":           fileID,
		"fileUserId":       fx.owner.ID,
		"fileUserUsername": fx.owner.Username,
		"fileUserHost":     nil,
	}
}

func assertAPIError(t *testing.T, rec *httptest.ResponseRecorder, wantID string) {
	t.Helper()
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	var body struct {
		Error struct {
			ID string `json:"id"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, wantID, body.Error.ID)
}

// TestModeratorFiles_MarkAndUnmarkSensitive_DB: moderator は他人のファイルの
// センシティブを付け外しでき、本家と同じ型と info でログが残る (#3678)。
// 値が変わらない更新はログを残さない。
func TestModeratorFiles_MarkAndUnmarkSensitive_DB(t *testing.T) {
	fx := newModeratorFilesFixture(t)
	f := fx.upload(fx.owner)
	require.False(t, f.IsSensitive)

	rec := fx.update(fx.mod, `{"fileId":"`+f.ID+`","isSensitive":true}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.True(t, fx.load(f.ID).IsSensitive)

	// 変わらない値を送ってもログは増えない。
	rec = fx.update(fx.mod, `{"fileId":"`+f.ID+`","isSensitive":true}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	rec = fx.update(fx.mod, `{"fileId":"`+f.ID+`","isSensitive":false}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.False(t, fx.load(f.ID).IsSensitive)

	rec = fx.update(fx.mod, `{"fileId":"`+f.ID+`","isSensitive":false}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	// fileUpdated は操作した moderator ではなく所有者へ送る。
	ownerUpdated := fx.owner.ID + ":fileUpdated"
	assert.Equal(t, []string{
		fx.owner.ID + ":fileCreated", ownerUpdated, ownerUpdated, ownerUpdated, ownerUpdated,
	}, fx.pub.snapshot())

	assert.Equal(t, []modLogEntry{
		{UserID: fx.mod.ID, Type: "markSensitiveDriveFile", Info: fx.fileLogInfo(f.ID)},
		{UserID: fx.mod.ID, Type: "unmarkSensitiveDriveFile", Info: fx.fileLogInfo(f.ID)},
	}, fx.moderationLogs())
}

// TestModeratorFiles_Delete_DB: moderator は他人のファイルを削除でき、行と
// 保存した実体が消え、所有者の drive stream に fileDeleted が届き、
// deleteDriveFile のログが残る (#3678)。
func TestModeratorFiles_Delete_DB(t *testing.T) {
	fx := newModeratorFilesFixture(t)
	f := fx.upload(fx.owner)
	require.NotNil(t, f.AccessKey)
	stored := filepath.Join(fx.storeDir, *f.AccessKey)
	require.FileExists(t, stored)

	rec := fx.delete(fx.mod, f.ID)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	assert.False(t, fx.exists(f.ID))
	_, err := os.Stat(stored)
	assert.True(t, os.IsNotExist(err), "stored object must be removed: %v", err)
	assert.Equal(t, []string{fx.owner.ID + ":fileCreated", fx.owner.ID + ":fileDeleted"}, fx.pub.snapshot())

	assert.Equal(t, []modLogEntry{
		{UserID: fx.mod.ID, Type: "deleteDriveFile", Info: fx.fileLogInfo(f.ID)},
	}, fx.moderationLogs())
}

// TestModeratorFiles_OwnerLeavesNoLog_DB: 所有者自身の操作はモデレーションの
// 操作ではないのでログを残さない。所有者が moderator でも同じ。
func TestModeratorFiles_OwnerLeavesNoLog_DB(t *testing.T) {
	fx := newModeratorFilesFixture(t)
	f := fx.upload(fx.mod)

	rec := fx.update(fx.mod, `{"fileId":"`+f.ID+`","isSensitive":true,"name":"b.txt"}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	rec = fx.delete(fx.mod, f.ID)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())

	assert.Empty(t, fx.moderationLogs())
}

// TestModeratorFiles_NonModeratorDenied_DB: moderator でない利用者は、他人の
// ファイルを更新も削除もできない (#499 から変わらない)。
func TestModeratorFiles_NonModeratorDenied_DB(t *testing.T) {
	fx := newModeratorFilesFixture(t)
	f := fx.upload(fx.owner)

	assertAPIError(t, fx.update(fx.plain, `{"fileId":"`+f.ID+`","isSensitive":true}`), updateAccessDeniedID)
	assertAPIError(t, fx.update(fx.plain, `{"fileId":"`+f.ID+`","name":"b.txt"}`), updateAccessDeniedID)
	assertAPIError(t, fx.delete(fx.plain, f.ID), deleteAccessDeniedID)

	got := fx.load(f.ID)
	assert.False(t, got.IsSensitive)
	assert.Equal(t, "a.txt", got.Name)
	assert.Empty(t, fx.moderationLogs())
}

// TestModeratorFiles_OtherFieldsDenied_DB: moderator でも、他人のファイルの
// 名前・フォルダ・説明文は変えられない。isSensitive と一緒に指定しても拒否し、
// センシティブも変えない (#3678)。
func TestModeratorFiles_OtherFieldsDenied_DB(t *testing.T) {
	fx := newModeratorFilesFixture(t)
	f := fx.upload(fx.owner)
	folder := &model.DriveFolder{ID: fx.prefix + "folder", Name: "x", UserID: &fx.owner.ID}
	require.NoError(t, fx.db.Create(folder).Error)

	cases := []struct {
		name string
		body string
	}{
		{"name", `"name":"b.txt"`},
		{"empty name", `"name":""`},
		{"comment", `"comment":"c"`},
		{"comment null", `"comment":null`},
		{"folderId", `"folderId":"` + folder.ID + `"`},
		{"folderId null", `"folderId":null`},
		{"name with isSensitive", `"name":"b.txt","isSensitive":true`},
		{"comment with isSensitive", `"comment":"c","isSensitive":true`},
		{"folderId with isSensitive", `"folderId":"` + folder.ID + `","isSensitive":true`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assertAPIError(t, fx.update(fx.mod, `{"fileId":"`+f.ID+`",`+tc.body+`}`), updateAccessDeniedID)
			got := fx.load(f.ID)
			assert.Equal(t, "a.txt", got.Name)
			assert.Nil(t, got.Comment)
			assert.Nil(t, got.FolderID)
			assert.False(t, got.IsSensitive)
		})
	}
	assert.Equal(t, []string{fx.owner.ID + ":fileCreated"}, fx.pub.snapshot())
	assert.Empty(t, fx.moderationLogs())
}

// TestModeratorFiles_AlwaysMarkNsfwOfOwner_DB: alwaysMarkNsfw は操作した
// moderator ではなく、ファイルの所有者の policy で判定する (本家と同じ)。
func TestModeratorFiles_AlwaysMarkNsfwOfOwner_DB(t *testing.T) {
	fx := newModeratorFilesFixture(t)
	nsfw := map[string]any{"alwaysMarkNsfw": map[string]any{"useDefault": false, "priority": 0, "value": true}}
	fx.assignRole(fx.owner, fx.createRole("nsfw", false, nsfw))
	fx.assignRole(fx.mod, fx.createRole("modnsfw", false, nsfw))

	// 所有者が alwaysMarkNsfw なら、moderator でも外せない。
	forced := fx.upload(fx.owner)
	require.True(t, forced.IsSensitive, "alwaysMarkNsfw of the owner marks the upload")
	assertAPIError(t, fx.update(fx.mod, `{"fileId":"`+forced.ID+`","isSensitive":false}`), updateRestrictedByRoleID)
	assert.True(t, fx.load(forced.ID).IsSensitive)

	// 本家と同じく、もともと非センシティブなら false を送っても policy に
	// 掛からない (値が変わらないのでログも残らない)。
	fx.setSensitive(forced, false)
	rec := fx.update(fx.mod, `{"fileId":"`+forced.ID+`","isSensitive":false}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.False(t, fx.load(forced.ID).IsSensitive)

	// moderator 自身が alwaysMarkNsfw でも、所有者が違えば外せる。
	free := fx.upload(fx.plain)
	fx.setSensitive(free, true)
	rec = fx.update(fx.mod, `{"fileId":"`+free.ID+`","isSensitive":false}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.False(t, fx.load(free.ID).IsSensitive)

	logs := fx.moderationLogs()
	require.Len(t, logs, 1)
	assert.Equal(t, "unmarkSensitiveDriveFile", logs[0].Type)
	assert.Equal(t, free.ID, logs[0].Info["fileId"])
}

// createEmoji makes a local custom emoji whose image is at originalURL /
// publicURL.
func (fx *moderatorFilesFixture) createEmoji(name, originalURL, publicURL string) {
	fx.t.Helper()
	fx.createEmojiOn(nil, name, originalURL, publicURL)
}

// createEmojiOn makes a custom emoji of host (nil for a local one).
func (fx *moderatorFilesFixture) createEmojiOn(host *string, name, originalURL, publicURL string) {
	fx.t.Helper()
	e := &model.Emoji{ID: fx.prefix + name, Name: fx.prefix + name, Host: host, OriginalURL: originalURL, PublicURL: publicURL}
	require.NoError(fx.t, fx.db.Create(e).Error)
	fx.t.Cleanup(func() { fx.db.Exec(`DELETE FROM "emoji" WHERE id = ?`, e.ID) })
}

// ownerlessLogInfo is the info upstream DriveService writes for a file
// without an owner.
func ownerlessLogInfo(fileID string) map[string]any {
	return map[string]any{
		"fileId":           fileID,
		"fileUserId":       nil,
		"fileUserUsername": nil,
		"fileUserHost":     nil,
	}
}

// TestModeratorFiles_SystemOwnedFile_DB: system 所有のファイル (絵文字の画像
// など) は、moderator でもセンシティブの付け外しだけができ、削除はできない
// (#3678、本家との差)。moderator でなければどちらもできない。
func TestModeratorFiles_SystemOwnedFile_DB(t *testing.T) {
	fx := newModeratorFilesFixture(t)
	// 所有者の居ないファイルの alwaysMarkNsfw は本家の getUserPolicies(null) と
	// 同じく base の policy で判定する。操作した moderator が alwaysMarkNsfw でも
	// 外せることを確かめるため、moderator に付けておく。
	nsfw := map[string]any{"alwaysMarkNsfw": map[string]any{"useDefault": false, "priority": 0, "value": true}}
	fx.assignRole(fx.mod, fx.createRole("modnsfw", false, nsfw))
	f := fx.uploadOwnerless(nil)

	assertAPIError(t, fx.update(fx.plain, `{"fileId":"`+f.ID+`","isSensitive":true}`), updateAccessDeniedID)
	assertAPIError(t, fx.delete(fx.plain, f.ID), deleteAccessDeniedID)
	assert.False(t, fx.load(f.ID).IsSensitive)

	rec := fx.update(fx.mod, `{"fileId":"`+f.ID+`","isSensitive":true}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.True(t, fx.load(f.ID).IsSensitive)
	rec = fx.update(fx.mod, `{"fileId":"`+f.ID+`","isSensitive":false}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.False(t, fx.load(f.ID).IsSensitive)

	assertAPIError(t, fx.delete(fx.mod, f.ID), deleteAccessDeniedID)
	assert.True(t, fx.exists(f.ID))
	require.NotNil(t, f.AccessKey)
	assert.FileExists(t, filepath.Join(fx.storeDir, *f.AccessKey))

	// 所有者が居ないのでイベントは誰にも送らない。
	assert.Empty(t, fx.pub.snapshot())
	assert.Equal(t, []modLogEntry{
		{UserID: fx.mod.ID, Type: "markSensitiveDriveFile", Info: ownerlessLogInfo(f.ID)},
		{UserID: fx.mod.ID, Type: "unmarkSensitiveDriveFile", Info: ownerlessLogInfo(f.ID)},
	}, fx.moderationLogs())
}

// TestModeratorFiles_OwnerlessRemoteFile_DB: 著者を materialize していない
// リモートの添付 (userId は NULL、userHost 付き) は system 所有ではないので、
// 本家と同じく moderator は削除できる。moderator でなければできない。
func TestModeratorFiles_OwnerlessRemoteFile_DB(t *testing.T) {
	fx := newModeratorFilesFixture(t)
	host := "remote.example"
	f := fx.uploadOwnerless(&host)

	assertAPIError(t, fx.update(fx.plain, `{"fileId":"`+f.ID+`","isSensitive":true}`), updateAccessDeniedID)
	assertAPIError(t, fx.delete(fx.plain, f.ID), deleteAccessDeniedID)
	assert.True(t, fx.exists(f.ID))

	rec := fx.update(fx.mod, `{"fileId":"`+f.ID+`","isSensitive":true}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	rec = fx.delete(fx.mod, f.ID)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	assert.False(t, fx.exists(f.ID))

	assert.Empty(t, fx.pub.snapshot())
	// 本家は fileUserHost に所有者の user.host を入れるので、所有者が居なければ
	// ファイルの userHost があっても null になる。
	assert.Equal(t, []modLogEntry{
		{UserID: fx.mod.ID, Type: "markSensitiveDriveFile", Info: ownerlessLogInfo(f.ID)},
		{UserID: fx.mod.ID, Type: "deleteDriveFile", Info: ownerlessLogInfo(f.ID)},
	}, fx.moderationLogs())
}

// TestModeratorFiles_EmojiImage_DB: 絵文字が画像として使っている利用者の
// ファイル (TS 版から移行した絵文字や、#2999 より前に登録した絵文字の形) は、
// system 所有でなくても moderator には消させない (#3678)。originalUrl と
// publicUrl のどちらで参照されていても同じ。所有者本人は消せる。絵文字が参照
// していないファイルは moderator が消せる。
func TestModeratorFiles_EmojiImage_DB(t *testing.T) {
	fx := newModeratorFilesFixture(t)
	byOriginal := fx.upload(fx.owner)
	fx.createEmoji("byorig", byOriginal.URL, "https://example.com/files/other1-"+fx.prefix)
	byPublic := fx.upload(fx.owner)
	fx.createEmoji("bypub", "https://example.com/files/other2-"+fx.prefix, byPublic.URL)
	unused := fx.upload(fx.owner)

	for _, f := range []*model.DriveFile{byOriginal, byPublic} {
		assertAPIError(t, fx.delete(fx.mod, f.ID), deleteAccessDeniedID)
		assert.True(t, fx.exists(f.ID))
		assert.FileExists(t, filepath.Join(fx.storeDir, *f.AccessKey))
		// moderator でない利用者は、今までどおり拒否される。
		assertAPIError(t, fx.delete(fx.plain, f.ID), deleteAccessDeniedID)
	}

	// 絵文字が参照していないファイルは、moderator が消せる。
	rec := fx.delete(fx.mod, unused.ID)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	assert.False(t, fx.exists(unused.ID))

	// 所有者本人は、絵文字が使っていても消せる (#3678 より前と同じ)。
	rec = fx.delete(fx.owner, byOriginal.ID)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	assert.False(t, fx.exists(byOriginal.ID))

	// 所有者が moderator でも同じ。TS 時代に管理者が自分の drive から絵文字を
	// 登録した形で、モデレーションの操作ではないのでログも残らない。
	ownByMod := fx.upload(fx.mod)
	fx.createEmoji("bymod", ownByMod.URL, ownByMod.URL)
	rec = fx.delete(fx.mod, ownByMod.ID)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	assert.False(t, fx.exists(ownByMod.ID))

	// 所有者の居ないリモートの添付も、ローカルの絵文字が使っていれば消させない。
	host := "remote.example"
	remoteAttachment := fx.uploadOwnerless(&host)
	fx.createEmoji("byremoteatt", remoteAttachment.URL, remoteAttachment.URL)
	assertAPIError(t, fx.delete(fx.mod, remoteAttachment.ID), deleteAccessDeniedID)
	assert.True(t, fx.exists(remoteAttachment.ID))

	assert.Equal(t, []modLogEntry{
		{UserID: fx.mod.ID, Type: "deleteDriveFile", Info: fx.fileLogInfo(unused.ID)},
	}, fx.moderationLogs())
}

// TestModeratorFiles_RemoteEmojiDoesNotProtect_DB: リモートの絵文字が参照して
// いるだけのファイルは、moderator が消せる (#3678)。リモートの絵文字の URL は
// 受信した Emoji タグの icon.url そのままなので、数えるとリモートのアカウントが
// ローカルのファイルを moderator の削除から守れてしまう。
func TestModeratorFiles_RemoteEmojiDoesNotProtect_DB(t *testing.T) {
	fx := newModeratorFilesFixture(t)
	host := "remote.example"
	f := fx.upload(fx.owner)
	fx.createEmojiOn(&host, "remote", f.URL, f.URL)
	remoteAttachment := fx.uploadOwnerless(&host)
	fx.createEmojiOn(&host, "remoteatt", remoteAttachment.URL, remoteAttachment.URL)

	rec := fx.delete(fx.mod, f.ID)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	assert.False(t, fx.exists(f.ID))
	rec = fx.delete(fx.mod, remoteAttachment.ID)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	assert.False(t, fx.exists(remoteAttachment.ID))

	assert.Equal(t, []modLogEntry{
		{UserID: fx.mod.ID, Type: "deleteDriveFile", Info: fx.fileLogInfo(f.ID)},
		{UserID: fx.mod.ID, Type: "deleteDriveFile", Info: ownerlessLogInfo(remoteAttachment.ID)},
	}, fx.moderationLogs())
}
