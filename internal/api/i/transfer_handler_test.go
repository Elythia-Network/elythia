package i

import (
	"errors"
	"math"
	"net/http"
	"testing"

	"github.com/elythia-network/elythia/internal/core/transfer"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/queue"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubImportFileReader returns canned drive content for i/import-antennas
// limit enforcement tests (#1667)。
type stubImportFileReader struct {
	body []byte
	err  error
}

func (s *stubImportFileReader) Fetch(_ string) (*model.DriveFile, []byte, error) {
	return nil, s.body, s.err
}

// stubAntennaCounter returns a canned current antenna count (#1667)。
type stubAntennaCounter struct {
	count int64
	err   error
}

func (s *stubAntennaCounter) CountByUser(_ string) (int64, error) { return s.count, s.err }

// stubTransferEnqueuer records calls and optionally returns an error.
type stubTransferEnqueuer struct {
	exportCalls []queue.ExportPayload
	importCalls []queue.ImportPayload
	exportErr   error
	importErr   error
}

func (s *stubTransferEnqueuer) EnqueueExport(p queue.ExportPayload) error {
	if s.exportErr != nil {
		return s.exportErr
	}
	s.exportCalls = append(s.exportCalls, p)
	return nil
}

func (s *stubTransferEnqueuer) EnqueueImport(p queue.ImportPayload) error {
	if s.importErr != nil {
		return s.importErr
	}
	s.importCalls = append(s.importCalls, p)
	return nil
}

// ownedFileID / ownerID は import 系テストが使う「u1 が所有する drive file」。
const (
	ownedFileID = "f1"
	ownerID     = "u1"
)

func newTransferHandler() (*Handler, *stubTransferEnqueuer) {
	h, enq, _ := newTransferHandlerWithDrive()
	return h, enq
}

// newTransferHandlerWithDrive wires a drive file repo seeded with a file owned
// by ownerID (ownedFileID) so that the import-* ownership gate (#1555) passes
// for the happy path. Negative cases insert/lookup other files via the returned
// mock.
func newTransferHandlerWithDrive() (*Handler, *stubTransferEnqueuer, *testutil.MockDriveFileRepository) {
	h, _, _, _ := newTestHandler(&testing.T{})
	enq := &stubTransferEnqueuer{}
	h.SetTransferEnqueuer(enq)
	driveRepo := testutil.NewMockDriveFileRepository()
	uid := ownerID
	// Size > 0 でないと EMPTY_FILE で弾かれる (#1555)。happy path 用に非空にする。
	driveRepo.Files[ownedFileID] = &model.DriveFile{ID: ownedFileID, UserID: &uid, Size: 16}
	h.SetDriveFileRepo(driveRepo)
	return h, enq, driveRepo
}

// --- Export handlers ---

func TestExport_NotesNoAuthIfEnqueuerMissing(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	rec := post(h.ExportNotes, `{}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

func TestExport_AllTypesEnqueue(t *testing.T) {
	h, enq := newTransferHandler()
	user := &model.User{ID: "u1"}

	// Use a simple test invocation approach: call handler via post helper.
	assertOK := func(recCode int) {
		assert.Equal(t, http.StatusNoContent, recCode)
	}

	// notes
	assertOK(post(h.ExportNotes, `{}`, user).Code)
	// following
	assertOK(post(h.ExportFollowing, `{}`, user).Code)
	// blocking
	assertOK(post(h.ExportBlocking, `{}`, user).Code)
	// mute
	assertOK(post(h.ExportMute, `{}`, user).Code)
	// favorites
	assertOK(post(h.ExportFavorites, `{}`, user).Code)
	// user-lists
	assertOK(post(h.ExportUserLists, `{}`, user).Code)
	// antennas
	assertOK(post(h.ExportAntennas, `{}`, user).Code)
	// clips
	assertOK(post(h.ExportClips, `{}`, user).Code)

	assert.Len(t, enq.exportCalls, 8)
	// verify the types are distinct
	seen := make(map[string]bool)
	for _, c := range enq.exportCalls {
		seen[c.Type] = true
		assert.Equal(t, "u1", c.UserID)
	}
	assert.Len(t, seen, 8)
}

// #1555 export-following の excludeMuting / excludeInactive が ExportPayload に
// 伝わる。
func TestExport_Following_ExcludeParams(t *testing.T) {
	h, enq := newTransferHandler()
	rec := post(h.ExportFollowing, `{"excludeMuting":true,"excludeInactive":true}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusNoContent, rec.Code)
	require.Len(t, enq.exportCalls, 1)
	assert.True(t, enq.exportCalls[0].ExcludeMuting)
	assert.True(t, enq.exportCalls[0].ExcludeInactive)
}

func TestExport_EnqueuerFailure(t *testing.T) {
	h, enq := newTransferHandler()
	enq.exportErr = errors.New("enq boom")
	rec := post(h.ExportNotes, `{}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

// --- Import handlers ---

func TestImport_AllTypesEnqueue(t *testing.T) {
	h, enq := newTransferHandler()
	user := &model.User{ID: "u1"}

	assertOK := func(recCode int) {
		assert.Equal(t, http.StatusNoContent, recCode)
	}

	assertOK(post(h.ImportFollowing, `{"fileId":"f1"}`, user).Code)
	assertOK(post(h.ImportBlocking, `{"fileId":"f1"}`, user).Code)
	assertOK(post(h.ImportMuting, `{"fileId":"f1"}`, user).Code)
	assertOK(post(h.ImportUserLists, `{"fileId":"f1"}`, user).Code)
	assertOK(post(h.ImportAntennas, `{"fileId":"f1"}`, user).Code)

	assert.Len(t, enq.importCalls, 5)
	for _, c := range enq.importCalls {
		assert.Equal(t, "u1", c.UserID)
		assert.Equal(t, "f1", c.FileID)
	}
}

func TestImport_MissingFileID(t *testing.T) {
	h, _ := newTransferHandler()
	rec := post(h.ImportFollowing, `{}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestImport_NoEnqueuer(t *testing.T) {
	// drive repo は所有 file を保持させ、ownership gate を通過させたうえで
	// enqueuer 未配線の 500 経路を踏ませる。
	h, _, _, _ := newTestHandler(t)
	driveRepo := testutil.NewMockDriveFileRepository()
	uid := ownerID
	driveRepo.Files[ownedFileID] = &model.DriveFile{ID: ownedFileID, UserID: &uid, Size: 16}
	h.SetDriveFileRepo(driveRepo)
	rec := post(h.ImportFollowing, `{"fileId":"f1"}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

func TestImport_EnqueuerError(t *testing.T) {
	h, enq := newTransferHandler()
	enq.importErr = errors.New("enq boom")
	rec := post(h.ImportFollowing, `{"fileId":"f1"}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

func TestImport_InvalidJSON(t *testing.T) {
	h, _ := newTransferHandler()
	rec := post(h.ImportFollowing, `not json`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// 他人所有の drive file を指定すると NO_SUCH_FILE で弾かれ、enqueue されない
// (cross-user import ownership bypass #1555 の回帰防止)。全 import handler で
// 同じ fail-closed 挙動を確認する。
func TestImport_OtherUsersFileRejected(t *testing.T) {
	for name, makeFn := range map[string]func(*Handler) func(c echo.Context) error{
		"following":  func(h *Handler) func(c echo.Context) error { return h.ImportFollowing },
		"blocking":   func(h *Handler) func(c echo.Context) error { return h.ImportBlocking },
		"muting":     func(h *Handler) func(c echo.Context) error { return h.ImportMuting },
		"user-lists": func(h *Handler) func(c echo.Context) error { return h.ImportUserLists },
		"antennas":   func(h *Handler) func(c echo.Context) error { return h.ImportAntennas },
	} {
		t.Run(name, func(t *testing.T) {
			h, enq, driveRepo := newTransferHandlerWithDrive()
			// otherID 所有の file を別 ID で seed する。
			otherID := "u2"
			driveRepo.Files["other"] = &model.DriveFile{ID: "other", UserID: &otherID}
			rec := post(makeFn(h), `{"fileId":"other"}`, &model.User{ID: ownerID})
			assert.Equal(t, http.StatusBadRequest, rec.Code)
			assert.Contains(t, rec.Body.String(), "NO_SUCH_FILE")
			// 本家は import endpoint ごとに固有の noSuchFile UUID を持つ (#1555)。
			wantID := map[string]string{
				"following":  "b98644cf-a5ac-4277-a502-0b8054a709a3",
				"blocking":   "ebb53e5f-6574-9c0c-0b92-7ca6def56d7e",
				"muting":     "e674141e-bd2a-ba85-e616-aefb187c9c2a",
				"user-lists": "ea9cc34f-c415-4bc6-a6fe-28ac40357049",
				"antennas":   "3b71d086-c3fa-431c-b01d-ded65a777172",
			}[name]
			assert.Contains(t, rec.Body.String(), wantID, "per-endpoint noSuchFile UUID must match upstream")
			assert.Empty(t, enq.importCalls)
		})
	}
}

// 不在 fileId は NO_SUCH_FILE で弾かれる (存在判定 oracle を作らないため
// 他人所有と同一 shape)。
func TestImport_MissingFileRejected(t *testing.T) {
	h, enq, _ := newTransferHandlerWithDrive()
	rec := post(h.ImportFollowing, `{"fileId":"ghost"}`, &model.User{ID: ownerID})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "NO_SUCH_FILE")
	assert.Empty(t, enq.importCalls)
}

// userId NULL の system file (emoji copy / import zip 保管先等) も所有者では
// ないので NO_SUCH_FILE。NULL handling を欠くと nil-deref か誤許可になる。
func TestImport_SystemFileRejected(t *testing.T) {
	h, enq, driveRepo := newTransferHandlerWithDrive()
	driveRepo.Files["sys"] = &model.DriveFile{ID: "sys", UserID: nil}
	rec := post(h.ImportFollowing, `{"fileId":"sys"}`, &model.User{ID: ownerID})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "NO_SUCH_FILE")
	assert.Empty(t, enq.importCalls)
}

// drive repo 未配線時は fail-closed (NO_SUCH_FILE)。所有権検証ができない以上
// import を通すと cross-user read が成立するため。
func TestImport_NoDriveRepoFailsClosed(t *testing.T) {
	h, _, _ := newTransferHandlerWithDrive()
	h.SetDriveFileRepo(nil)
	rec := post(h.ImportFollowing, `{"fileId":"f1"}`, &model.User{ID: ownerID})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "NO_SUCH_FILE")
}

// 自分所有の file は import が enqueue される (happy path / 回帰防止)。
func TestImport_OwnedFileAccepted(t *testing.T) {
	h, enq, _ := newTransferHandlerWithDrive()
	rec := post(h.ImportFollowing, `{"fileId":"f1"}`, &model.User{ID: ownerID})
	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.Len(t, enq.importCalls, 1)
	assert.Equal(t, ownedFileID, enq.importCalls[0].FileID)
	assert.Equal(t, ownerID, enq.importCalls[0].UserID)
}

// 空 file (size==0) は EMPTY_FILE で弾かれ、enqueue されない (#1555)。
// 全 import endpoint で per-endpoint UUID を確認する。
func TestImport_EmptyFileRejected(t *testing.T) {
	for name, makeFn := range map[string]func(*Handler) func(c echo.Context) error{
		"following":  func(h *Handler) func(c echo.Context) error { return h.ImportFollowing },
		"blocking":   func(h *Handler) func(c echo.Context) error { return h.ImportBlocking },
		"muting":     func(h *Handler) func(c echo.Context) error { return h.ImportMuting },
		"user-lists": func(h *Handler) func(c echo.Context) error { return h.ImportUserLists },
		"antennas":   func(h *Handler) func(c echo.Context) error { return h.ImportAntennas },
	} {
		t.Run(name, func(t *testing.T) {
			h, enq, driveRepo := newTransferHandlerWithDrive()
			uid := ownerID
			driveRepo.Files["empty"] = &model.DriveFile{ID: "empty", UserID: &uid, Size: 0}
			rec := post(makeFn(h), `{"fileId":"empty"}`, &model.User{ID: ownerID})
			assert.Equal(t, http.StatusBadRequest, rec.Code)
			assert.Contains(t, rec.Body.String(), "EMPTY_FILE")
			wantID := map[string]string{
				"following":  "31a1b42c-06f7-42ae-8a38-a661c5c9f691",
				"blocking":   "6f3a4dcc-f060-a707-4950-806fbdbe60d6",
				"muting":     "d2f12af1-e7b4-feac-86a3-519548f2728e",
				"user-lists": "99efe367-ce6e-4d44-93f8-5fae7b040356",
				"antennas":   "7f60115d-8d93-4b0f-bd0e-3815dcbb389f",
			}[name]
			assert.Contains(t, rec.Body.String(), wantID)
			assert.Empty(t, enq.importCalls)
		})
	}
}

// 64KB 超の file は TOO_BIG_FILE で弾かれる (import-antennas を除く)。
func TestImport_TooBigFileRejected(t *testing.T) {
	for name, makeFn := range map[string]func(*Handler) func(c echo.Context) error{
		"following":  func(h *Handler) func(c echo.Context) error { return h.ImportFollowing },
		"blocking":   func(h *Handler) func(c echo.Context) error { return h.ImportBlocking },
		"muting":     func(h *Handler) func(c echo.Context) error { return h.ImportMuting },
		"user-lists": func(h *Handler) func(c echo.Context) error { return h.ImportUserLists },
	} {
		t.Run(name, func(t *testing.T) {
			h, enq, driveRepo := newTransferHandlerWithDrive()
			uid := ownerID
			driveRepo.Files["big"] = &model.DriveFile{ID: "big", UserID: &uid, Size: 64*1024 + 1}
			rec := post(makeFn(h), `{"fileId":"big"}`, &model.User{ID: ownerID})
			assert.Equal(t, http.StatusBadRequest, rec.Code)
			assert.Contains(t, rec.Body.String(), "TOO_BIG_FILE")
			assert.Empty(t, enq.importCalls)
		})
	}
}

// import-antennas は upstream に tooBigFile が無いので 64KB 超でも通る。
func TestImport_Antennas_NoTooBigCheck(t *testing.T) {
	h, enq, driveRepo := newTransferHandlerWithDrive()
	uid := ownerID
	driveRepo.Files["big"] = &model.DriveFile{ID: "big", UserID: &uid, Size: 64*1024 + 1}
	rec := post(h.ImportAntennas, `{"fileId":"big"}`, &model.User{ID: ownerID})
	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.Len(t, enq.importCalls, 1)
}

// #1667 現 antenna 件数 + import 件数 >= antennaLimit なら TOO_MANY_ANTENNAS
// (upstream import-antennas.ts、import-antennas 固有 UUID)。
func TestImportAntennas_TooMany(t *testing.T) {
	h, enq, driveRepo := newTransferHandlerWithDrive()
	uid := ownerID
	driveRepo.Files["ant"] = &model.DriveFile{ID: "ant", UserID: &uid, Size: 100}
	h.SetImportDriveReader(&stubImportFileReader{body: []byte(`[{"name":"a"},{"name":"b"}]`)})
	h.SetAntennaCounter(&stubAntennaCounter{count: 4})
	h.SetRoleProvider(&stubRoleProvider{policies: map[string]any{"antennaLimit": 5}})
	// 現 4 + import 2 = 6 >= 5 → TOO_MANY_ANTENNAS。
	rec := post(h.ImportAntennas, `{"fileId":"ant"}`, &model.User{ID: ownerID})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "TOO_MANY_ANTENNAS")
	assert.Contains(t, rec.Body.String(), "600917d4-a4cb-4cc5-8ba8-7ac8ea3c7779")
	assert.Empty(t, enq.importCalls)
}

func TestImportAntennas_MaxCountCannotWrapBelowLimit(t *testing.T) {
	h, enq, driveRepo := newTransferHandlerWithDrive()
	uid := ownerID
	driveRepo.Files["ant"] = &model.DriveFile{ID: "ant", UserID: &uid, Size: 100}
	h.SetImportDriveReader(&stubImportFileReader{body: []byte(`[{"name":"a"}]`)})
	h.SetAntennaCounter(&stubAntennaCounter{count: math.MaxInt64})
	h.SetRoleProvider(&stubRoleProvider{policies: map[string]any{"antennaLimit": math.MaxInt}})

	rec := post(h.ImportAntennas, `{"fileId":"ant"}`, &model.User{ID: ownerID})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "TOO_MANY_ANTENNAS")
	assert.Empty(t, enq.importCalls)
}

// 上限未満なら enqueue される。
func TestImportAntennas_UnderLimit(t *testing.T) {
	h, enq, driveRepo := newTransferHandlerWithDrive()
	uid := ownerID
	driveRepo.Files["ant"] = &model.DriveFile{ID: "ant", UserID: &uid, Size: 100}
	h.SetImportDriveReader(&stubImportFileReader{body: []byte(`[{"name":"a"}]`)})
	h.SetAntennaCounter(&stubAntennaCounter{count: 1})
	h.SetRoleProvider(&stubRoleProvider{policies: map[string]any{"antennaLimit": 5}})
	// 現 1 + import 1 = 2 < 5 → enqueue。
	rec := post(h.ImportAntennas, `{"fileId":"ant"}`, &model.User{ID: ownerID})
	assert.Equal(t, http.StatusNoContent, rec.Code)
	require.Len(t, enq.importCalls, 1)
	assert.Equal(t, transfer.ImportAntennas, enq.importCalls[0].Type)
}

// 依存 (driveReader / antennaCounter / roleProvider) 未配線時は limit check を
// skip して enqueue する (degrade、worker に委ねる)。
func TestImportAntennas_NoDepsDegrades(t *testing.T) {
	h, enq, driveRepo := newTransferHandlerWithDrive()
	uid := ownerID
	driveRepo.Files["ant"] = &model.DriveFile{ID: "ant", UserID: &uid, Size: 100}
	rec := post(h.ImportAntennas, `{"fileId":"ant"}`, &model.User{ID: ownerID})
	assert.Equal(t, http.StatusNoContent, rec.Code)
	require.Len(t, enq.importCalls, 1)
}

// import-following の job-level withReplies が ImportPayload に伝わる (#1555)。
func TestImport_Following_JobWithReplies(t *testing.T) {
	h, enq, _ := newTransferHandlerWithDrive()
	rec := post(h.ImportFollowing, `{"fileId":"f1","withReplies":true}`, &model.User{ID: ownerID})
	assert.Equal(t, http.StatusNoContent, rec.Code)
	require.Len(t, enq.importCalls, 1)
	assert.True(t, enq.importCalls[0].WithReplies)
}

// stubMoveInValidator は移行直後判定を固定する。
type stubMoveInValidator struct{ confirmed bool }

func (s stubMoveInValidator) HasRecentConfirmedMoveIn(*model.User) bool { return s.confirmed }

// 相互確認の取れた移行直後は 32MB まで許可する (#2415)。
func TestImport_MovedInRelaxesSizeLimit(t *testing.T) {
	makers := map[string]func(*Handler) func(c echo.Context) error{
		"following":  func(h *Handler) func(c echo.Context) error { return h.ImportFollowing },
		"blocking":   func(h *Handler) func(c echo.Context) error { return h.ImportBlocking },
		"muting":     func(h *Handler) func(c echo.Context) error { return h.ImportMuting },
		"user-lists": func(h *Handler) func(c echo.Context) error { return h.ImportUserLists },
	}
	for name, makeFn := range makers {
		t.Run(name+"/移行直後は64KB超を通す", func(t *testing.T) {
			h, enq, driveRepo := newTransferHandlerWithDrive()
			h.SetMoveInValidator(stubMoveInValidator{confirmed: true})
			uid := ownerID
			driveRepo.Files["big"] = &model.DriveFile{ID: "big", UserID: &uid, Size: 64*1024 + 1}
			rec := post(makeFn(h), `{"fileId":"big"}`, &model.User{ID: ownerID})
			assert.Equal(t, http.StatusNoContent, rec.Code)
			assert.Len(t, enq.importCalls, 1)
		})
		t.Run(name+"/移行直後でも32MB超は弾く", func(t *testing.T) {
			h, enq, driveRepo := newTransferHandlerWithDrive()
			h.SetMoveInValidator(stubMoveInValidator{confirmed: true})
			uid := ownerID
			driveRepo.Files["huge"] = &model.DriveFile{ID: "huge", UserID: &uid, Size: 32*1024*1024 + 1}
			rec := post(makeFn(h), `{"fileId":"huge"}`, &model.User{ID: ownerID})
			assert.Equal(t, http.StatusBadRequest, rec.Code)
			assert.Contains(t, rec.Body.String(), "TOO_BIG_FILE")
			assert.Empty(t, enq.importCalls)
		})
		t.Run(name+"/移行が確認できなければ64KBのまま", func(t *testing.T) {
			h, enq, driveRepo := newTransferHandlerWithDrive()
			h.SetMoveInValidator(stubMoveInValidator{confirmed: false})
			uid := ownerID
			driveRepo.Files["big"] = &model.DriveFile{ID: "big", UserID: &uid, Size: 64*1024 + 1}
			rec := post(makeFn(h), `{"fileId":"big"}`, &model.User{ID: ownerID})
			assert.Equal(t, http.StatusBadRequest, rec.Code)
			assert.Contains(t, rec.Body.String(), "TOO_BIG_FILE")
			assert.Empty(t, enq.importCalls)
		})
	}
}

// validator 未配線なら通常上限に倒す。判定できないときは絞る側が安全。
func TestImport_WithoutMoveInValidatorKeepsDefaultLimit(t *testing.T) {
	h, enq, driveRepo := newTransferHandlerWithDrive()
	uid := ownerID
	driveRepo.Files["big"] = &model.DriveFile{ID: "big", UserID: &uid, Size: 64*1024 + 1}
	rec := post(h.ImportFollowing, `{"fileId":"big"}`, &model.User{ID: ownerID})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "TOO_BIG_FILE")
	assert.Empty(t, enq.importCalls)
}

// failingDriveRepo makes every drive file lookup look like a database failure.
type failingDriveRepo struct {
	*testutil.MockDriveFileRepository
	err error
}

func (r *failingDriveRepo) FindByID(string) (*model.DriveFile, error) { return nil, r.err }

// **DB 障害を「そんなファイルは無い」にしない** (#2792)。
//
// import は利用者がドライブに上げたファイルを指定する。障害を 400 で返すと
// 「アップロードが失敗した」と読めてしまい、上げ直しを繰り返す。
func TestImport_DBFailureIsNot4xx(t *testing.T) {
	h, enq, _ := newTransferHandlerWithDrive()
	h.SetDriveFileRepo(&failingDriveRepo{
		MockDriveFileRepository: testutil.NewMockDriveFileRepository(),
		err:                     errors.New("dial tcp 127.0.0.1:5432: connect: connection refused"),
	})

	rec := post(h.ImportFollowing, `{"fileId":"`+ownedFileID+`"}`, &model.User{ID: ownerID})
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.Empty(t, enq.importCalls, "障害時に job を積んでいる")
}
