package drive

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	coredrive "github.com/elythia-network/elythia/internal/core/drive"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
)

// failingDriveFileRepo makes every file lookup look like a database failure.
type failingDriveFileRepo struct {
	*testutil.MockDriveFileRepository
	err error
}

func (r *failingDriveFileRepo) FindByID(string) (*model.DriveFile, error) { return nil, r.err }

// failingDriveFolderRepo makes every folder lookup look like a database failure.
type failingDriveFolderRepo struct {
	*testutil.MockDriveFolderRepository
	err error
}

func (r *failingDriveFolderRepo) FindByID(string) (*model.DriveFolder, error) { return nil, r.err }

// **DB 障害を「そんなファイルは無い」にしない** (#2792)。
func TestDrive_DBFailureIsNot4xx(t *testing.T) {
	dbErr := errors.New("dial tcp 127.0.0.1:5432: connect: connection refused")

	newFailing := func(t *testing.T) *Handler {
		t.Helper()
		fileRepo := &failingDriveFileRepo{
			MockDriveFileRepository: testutil.NewMockDriveFileRepository(),
			err:                     dbErr,
		}
		folderRepo := &failingDriveFolderRepo{
			MockDriveFolderRepository: testutil.NewMockDriveFolderRepository(),
			err:                       dbErr,
		}
		storage := coredrive.NewLocalStorage(t.TempDir(), "https://example.com/files")
		idGen, _ := id.NewGenerator("aidx")
		h := NewHandler(coredrive.NewService(fileRepo, folderRepo, storage, idGen), idGen)
		h.SetRepos(fileRepo, folderRepo, testutil.NewMockNoteRepository())
		return h
	}

	call := func(t *testing.T, fn func(echo.Context) error, body string) int {
		t.Helper()
		e := echo.New()
		req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
		req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
		rec := httptest.NewRecorder()
		c := e.NewContext(req, rec)
		setUser(c, "u1")
		require.NoError(t, fn(c))
		return rec.Code
	}

	t.Run("drive/files/attached-notes", func(t *testing.T) {
		assert.Equal(t, http.StatusInternalServerError,
			call(t, newFailing(t).FilesAttachedNotes, `{"fileId":"f1"}`),
			"DB 障害が 4xx に化けている (#2792)")
	})

	t.Run("drive/files/move-bulk", func(t *testing.T) {
		// **folderId を指定する。** 移動先の所有権検証で folder を引くので、
		// 指定しないと lookup に到達せず 204 で終わる。
		assert.Equal(t, http.StatusInternalServerError,
			call(t, newFailing(t).FilesMoveBulk, `{"fileIds":["f1"],"folderId":"fo1"}`),
			"DB 障害が 4xx に化けている (#2792)")
	})
}
