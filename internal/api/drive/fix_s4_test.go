package drive

import (
	"net/http"
	"testing"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func onlyDriveFile(t *testing.T, repo *testutil.MockDriveFileRepository) *model.DriveFile {
	t.Helper()
	require.Len(t, repo.Files, 1)
	for _, f := range repo.Files {
		return f
	}
	return nil
}

// #2106 S4: drive/files/create は meta.enableIpLogging が有効なときだけ requestIp を保存する。
func TestFilesCreate_IPLoggingGate(t *testing.T) {
	run := func(enabled bool) *model.DriveFile {
		h, fileRepo, _ := newHandler(t)
		mr := testutil.NewMockMetaRepository()
		mr.Meta = &model.Meta{ID: "x", EnableIPLogging: enabled}
		h.SetMetaRepo(mr)
		c, rec := newMultipartReq(t, "raw.txt", "hello", nil)
		c.Request().RemoteAddr = "203.0.113.5:9999"
		setUser(c, "u1")
		require.NoError(t, h.FilesCreate(c))
		require.Equal(t, http.StatusOK, rec.Code)
		return onlyDriveFile(t, fileRepo)
	}

	t.Run("disabled: requestIp not stored", func(t *testing.T) {
		assert.Nil(t, run(false).RequestIP, "enableIpLogging=false must not store requestIp")
	})
	t.Run("enabled: requestIp stored", func(t *testing.T) {
		f := run(true)
		require.NotNil(t, f.RequestIP, "enableIpLogging=true stores requestIp")
		assert.Equal(t, "203.0.113.5", *f.RequestIP)
	})
}
