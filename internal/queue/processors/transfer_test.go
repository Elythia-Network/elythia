package processors_test

import (
	"context"
	"testing"
	"time"

	"github.com/shiroha-a/mk/internal/core/drive"
	"github.com/shiroha-a/mk/internal/core/notification"
	"github.com/shiroha-a/mk/internal/core/transfer"
	"github.com/shiroha-a/mk/internal/misc/id"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/queue"
	"github.com/shiroha-a/mk/internal/queue/driver"
	"github.com/shiroha-a/mk/internal/queue/processors"
	"github.com/shiroha-a/mk/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- stubs ---

type stubTransferDriveSaver struct{}

func (stubTransferDriveSaver) Upload(_ context.Context, in drive.UploadInput) (*model.DriveFile, error) {
	return &model.DriveFile{ID: "f_" + in.Name, Name: in.Name}, nil
}

type stubTransferNotifier struct{}

func (stubTransferNotifier) Create(_ context.Context, _ notification.CreateInput) (*notification.Notification, error) {
	return &notification.Notification{ID: "n"}, nil
}

type stubTransferDriveReader struct{ body []byte }

func (s stubTransferDriveReader) Fetch(_ string) (*model.DriveFile, []byte, error) {
	return &model.DriveFile{ID: "f"}, s.body, nil
}

type stubBlockingSvc struct{}

func (stubBlockingSvc) BlockSilent(_, blockeeID string) (*model.Blocking, error) {
	return &model.Blocking{ID: "b_" + blockeeID}, nil
}

type stubMutingSvc struct{}

func (stubMutingSvc) Mute(_, muteeID string, _ *time.Time) (*model.Muting, error) {
	return &model.Muting{ID: "m_" + muteeID}, nil
}

type stubFollowingSvc struct{}

func (stubFollowingSvc) Follow(_, _ string, _ transfer.FollowOptions) (any, error) {
	return nil, nil
}

// --- Builders ---

func newTestExporter(t *testing.T) *transfer.Exporter {
	t.Helper()
	userRepo := testutil.NewMockUserRepository()
	userRepo.Users["u1"] = &model.User{ID: "u1", Username: "u1", UsernameLower: "u1"}
	return transfer.NewExporter(transfer.ExporterDeps{
		UserRepo:         userRepo,
		NoteRepo:         testutil.NewMockNoteRepository(),
		PollRepo:         testutil.NewMockPollRepository(),
		FollowingRepo:    testutil.NewMockFollowingRepository(),
		BlockingRepo:     testutil.NewMockBlockingRepository(),
		MutingRepo:       testutil.NewMockMutingRepository(),
		NoteFavoriteRepo: testutil.NewMockNoteFavoriteRepository(),
		AntennaRepo:      testutil.NewMockAntennaRepository(),
		ClipRepo:         testutil.NewMockClipRepository(),
		ClipNoteRepo:     testutil.NewMockClipNoteRepository(),
		UserListRepo:     testutil.NewMockUserListRepository(),
		Drive:            stubTransferDriveSaver{},
		Notifier:         stubTransferNotifier{},
	})
}

func newTestImporter(t *testing.T, body []byte) *transfer.Importer {
	t.Helper()
	idGen, _ := id.NewGenerator("aidx")
	userRepo := testutil.NewMockUserRepository()
	userRepo.Users["u1"] = &model.User{ID: "u1", Username: "u1", UsernameLower: "u1"}
	userRepo.Users["bob"] = &model.User{ID: "bob", Username: "bob", UsernameLower: "bob"}
	return transfer.NewImporter(transfer.ImporterDeps{
		UserRepo:     userRepo,
		UserListRepo: testutil.NewMockUserListRepository(),
		AntennaRepo:  testutil.NewMockAntennaRepository(),
		Drive:        stubTransferDriveReader{body: body},
		Blocking:     stubBlockingSvc{},
		Muting:       stubMutingSvc{},
		Following:    stubFollowingSvc{},
		Notifier:     stubTransferNotifier{},
		IDGen:        idGen,
		SelfHost:     "local.example",
	})
}

// --- Tests: ExportProcessor ---

func TestExportProcessor_Handle_Success(t *testing.T) {
	exporter := newTestExporter(t)
	p := processors.NewExportProcessor(exporter)
	task := queue.NewExportTask(queue.ExportPayload{UserID: "u1", Type: transfer.ExportNotes})
	err := p.Handle(context.Background(), task)
	require.NoError(t, err)
}

func TestExportProcessor_Handle_NilExporter(t *testing.T) {
	p := processors.NewExportProcessor(nil)
	task := queue.NewExportTask(queue.ExportPayload{UserID: "u1", Type: transfer.ExportNotes})
	err := p.Handle(context.Background(), task)
	require.Error(t, err)
	assert.ErrorIs(t, err, driver.ErrSkipRetry)
}

func TestExportProcessor_Handle_InvalidPayload(t *testing.T) {
	exporter := newTestExporter(t)
	p := processors.NewExportProcessor(exporter)
	task := driver.RawTask{TypeName: queue.TaskTypeExport, Body: []byte("not json")}
	err := p.Handle(context.Background(), task)
	require.Error(t, err)
	assert.ErrorIs(t, err, driver.ErrSkipRetry)
}

func TestExportProcessor_Handle_MissingFields(t *testing.T) {
	exporter := newTestExporter(t)
	p := processors.NewExportProcessor(exporter)
	task := queue.NewExportTask(queue.ExportPayload{UserID: "", Type: ""})
	err := p.Handle(context.Background(), task)
	require.Error(t, err)
	assert.ErrorIs(t, err, driver.ErrSkipRetry)
}

func TestExportProcessor_Handle_UnsupportedType(t *testing.T) {
	exporter := newTestExporter(t)
	p := processors.NewExportProcessor(exporter)
	task := queue.NewExportTask(queue.ExportPayload{UserID: "u1", Type: "nope"})
	err := p.Handle(context.Background(), task)
	require.Error(t, err)
	assert.ErrorIs(t, err, driver.ErrSkipRetry)
}

func TestExportProcessor_Handle_ExporterError(t *testing.T) {
	exporter := newTestExporter(t)
	p := processors.NewExportProcessor(exporter)
	// Unknown user -> internal error from Exporter (NOT ErrUnsupportedType)
	task := queue.NewExportTask(queue.ExportPayload{UserID: "ghost", Type: transfer.ExportNotes})
	err := p.Handle(context.Background(), task)
	require.Error(t, err)
	// ghost user lookup error propagates unchanged (no ErrSkipRetry wrap)
}

// --- Tests: ImportProcessor ---

func TestImportProcessor_Handle_Success(t *testing.T) {
	imp := newTestImporter(t, []byte("bob\n"))
	p := processors.NewImportProcessor(imp)
	task := queue.NewImportTask(queue.ImportPayload{UserID: "u1", Type: transfer.ImportBlocking, FileID: "f"})
	err := p.Handle(context.Background(), task)
	require.NoError(t, err)
}

func TestImportProcessor_Handle_NilImporter(t *testing.T) {
	p := processors.NewImportProcessor(nil)
	task := queue.NewImportTask(queue.ImportPayload{UserID: "u1", Type: transfer.ImportBlocking, FileID: "f"})
	err := p.Handle(context.Background(), task)
	require.Error(t, err)
	assert.ErrorIs(t, err, driver.ErrSkipRetry)
}

func TestImportProcessor_Handle_InvalidPayload(t *testing.T) {
	imp := newTestImporter(t, nil)
	p := processors.NewImportProcessor(imp)
	task := driver.RawTask{TypeName: queue.TaskTypeImport, Body: []byte("not json")}
	err := p.Handle(context.Background(), task)
	require.Error(t, err)
	assert.ErrorIs(t, err, driver.ErrSkipRetry)
}

func TestImportProcessor_Handle_MissingFields(t *testing.T) {
	imp := newTestImporter(t, nil)
	p := processors.NewImportProcessor(imp)
	task := queue.NewImportTask(queue.ImportPayload{UserID: "u1", Type: "", FileID: ""})
	err := p.Handle(context.Background(), task)
	require.Error(t, err)
	assert.ErrorIs(t, err, driver.ErrSkipRetry)
}

func TestImportProcessor_Handle_UnsupportedType(t *testing.T) {
	imp := newTestImporter(t, nil)
	p := processors.NewImportProcessor(imp)
	task := queue.NewImportTask(queue.ImportPayload{UserID: "u1", Type: "nope", FileID: "f"})
	err := p.Handle(context.Background(), task)
	require.Error(t, err)
	assert.ErrorIs(t, err, driver.ErrSkipRetry)
}

func TestImportProcessor_Handle_ImporterError(t *testing.T) {
	imp := newTestImporter(t, nil)
	p := processors.NewImportProcessor(imp)
	task := queue.NewImportTask(queue.ImportPayload{UserID: "ghost", Type: transfer.ImportBlocking, FileID: "f"})
	err := p.Handle(context.Background(), task)
	require.Error(t, err)
}
