package note_test

import (
	"sync"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/core/note"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

type pendingThenOnLocker struct {
	user         *model.User
	lockedStatus model.FederationStatus
}

func (l *pendingThenOnLocker) WithLockedUser(_ string, fn func(*gorm.DB, *model.User) error) error {
	locked := *l.user
	locked.FederationStatus = l.lockedStatus
	if locked.FederationStatus == "" {
		locked.FederationStatus = model.FederationStatusPending
	}
	if err := fn(nil, &locked); err != nil {
		return err
	}
	// Model the user becoming enabled immediately after the insertion lock is
	// released, before the asynchronous federation hook can inspect its inputs.
	l.user.FederationStatus = model.FederationStatusOn
	return nil
}

type signalFederationHook struct {
	called chan struct{}
	once   sync.Once
}

func (h *signalFederationHook) OnNoteCreated(*model.Note, *model.User) {
	h.once.Do(func() { close(h.called) })
}

func TestCreateService_PendingNoteDoesNotFederateAfterAccountEnabled(t *testing.T) {
	svc, repo, _ := newCreateService(t)
	user := &model.User{ID: "local", FederationStatus: model.FederationStatusPending}
	svc.SetLocalFederationLocker(&pendingThenOnLocker{user: user})
	hook := &signalFederationHook{called: make(chan struct{})}
	svc.SetFederationHook(hook)
	text := "pending post"

	created, err := svc.Create(note.CreateInput{User: user, Text: &text, Visibility: model.NoteVisibilityPublic})
	require.NoError(t, err)
	assert.False(t, created.LocalOnly, "pending accounts keep ordinary posts public")
	assert.False(t, repo.Notes[created.ID].LocalOnly)
	assert.Equal(t, model.FederationStatusOn, user.FederationStatus)

	select {
	case <-hook.called:
		t.Fatal("a post created while pending must not be federated after the account is enabled")
	case <-time.After(50 * time.Millisecond):
	}
}

func TestCreateService_AccountDisabledUnderLockForcesOrdinaryPostLocalOnly(t *testing.T) {
	svc, repo, _ := newCreateService(t)
	user := &model.User{ID: "local", FederationStatus: model.FederationStatusOn}
	svc.SetLocalFederationLocker(&pendingThenOnLocker{user: user, lockedStatus: model.FederationStatusOff})
	hook := &signalFederationHook{called: make(chan struct{})}
	svc.SetFederationHook(hook)
	text := "locked off post"

	created, err := svc.Create(note.CreateInput{User: user, Text: &text, Visibility: model.NoteVisibilityPublic})
	require.NoError(t, err)
	assert.True(t, created.LocalOnly, "the status read under the insertion lock determines federation")
	assert.True(t, repo.Notes[created.ID].LocalOnly)
	assert.Equal(t, model.FederationStatusOn, user.FederationStatus)

	select {
	case <-hook.called:
		t.Fatal("a post created while OFF must not be delivered after the account is enabled")
	case <-time.After(50 * time.Millisecond):
	}
}

type localFederationNoteObserver struct {
	available bool
	recorded  int
}

func (s *localFederationNoteObserver) IsExternallyAvailable(user *model.User) bool {
	return s.available && user.FederationStatus == model.FederationStatusOn
}
func (s *localFederationNoteObserver) ObserveExternalReference(string, string, string) (bool, error) {
	s.recorded++
	return true, nil
}

func TestCreateService_LocalOnlyAccountCannotReplyToRemoteNote(t *testing.T) {
	svc, repo, _ := newCreateService(t)
	host := "remote.example"
	repo.Notes["remote-note"] = &model.Note{ID: "remote-note", UserID: "remote-author", UserHost: &host, Visibility: model.NoteVisibilityPublic}
	svc.SetLocalFederationObserver(&localFederationNoteObserver{})
	text := "reply"
	replyID := "remote-note"
	_, err := svc.Create(note.CreateInput{User: &model.User{ID: "local", FederationStatus: model.FederationStatusOff}, Text: &text, ReplyID: &replyID, Visibility: model.NoteVisibilityPublic})
	require.ErrorIs(t, err, note.ErrFederationDisabled)
	assert.Len(t, repo.Notes, 1, "remote reply must be rejected before inserting a note")
}

func TestCreateService_FailedRemoteActionDoesNotRecordExternalReference(t *testing.T) {
	svc, repo, _ := newCreateService(t)
	host := "remote.example"
	inner := "original-note"
	repo.Notes["remote-pure-renote"] = &model.Note{
		ID: "remote-pure-renote", UserID: "remote-author", UserHost: &host,
		Visibility: model.NoteVisibilityPublic, RenoteID: &inner,
	}
	observer := &localFederationNoteObserver{available: true}
	svc.SetLocalFederationObserver(observer)
	text := "reply"
	user := &model.User{ID: "local", FederationStatus: model.FederationStatusOn}

	_, err := svc.Create(note.CreateInput{
		User: user, Text: &text, ReplyID: strPtr254("remote-pure-renote"),
		Visibility: model.NoteVisibilityPublic,
	})
	require.ErrorIs(t, err, note.ErrCannotReplyToAPureRenote)
	assert.Zero(t, observer.recorded, "validation failure must not make the local user permanently externally referenced")
	assert.Len(t, repo.Notes, 1)
}

func TestCreateService_RemoteActionChecksLockedAccountStatus(t *testing.T) {
	svc, repo, _ := newCreateService(t)
	host := "remote.example"
	repo.Notes["remote-target"] = &model.Note{ID: "remote-target", UserID: "remote-author", UserHost: &host, Visibility: model.NoteVisibilityPublic}
	user := &model.User{ID: "local", FederationStatus: model.FederationStatusOn}
	svc.SetLocalFederationLocker(&pendingThenOnLocker{user: user, lockedStatus: model.FederationStatusOff})
	observer := &localFederationNoteObserver{available: true}
	svc.SetLocalFederationObserver(observer)
	text := "reply"

	_, err := svc.Create(note.CreateInput{User: user, Text: &text, ReplyID: strPtr254("remote-target"), Visibility: model.NoteVisibilityPublic})
	require.ErrorIs(t, err, note.ErrFederationDisabled)
	assert.Len(t, repo.Notes, 1, "locked OFF state must reject before the local note insert")
	assert.Zero(t, observer.recorded)
}
