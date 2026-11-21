package notes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
	"gorm.io/gorm"

	"github.com/elythia-network/elythia/internal/bgwork"
	corenote "github.com/elythia-network/elythia/internal/core/note"
	"github.com/elythia-network/elythia/internal/core/notedelivery"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/elythia-network/elythia/plugin"
)

func postCreate(t *testing.T, h *Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/api/notes/create", strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	setAuthUser(c, &model.User{ID: "user1", Username: "alice", AvatarDecorations: datatypes.JSON([]byte("[]"))})
	require.NoError(t, h.Create(c))
	return rec
}

func registryWith(t *testing.T, prepare func(context.Context, plugin.NoteDeliveryPrepareRequest) error) *notedelivery.Registry {
	t.Helper()
	r := notedelivery.New()
	require.NoError(t, r.Add("targets", plugin.NoteDeliveryRegistration{
		Prepare: prepare,
		Targets: func(context.Context, []string) (map[string]plugin.DeliveryTargets, error) { return nil, nil },
	}))
	return r
}

// pluginOptions reaches the plugin with the stored note's ID (#4199).
func TestCreate_PluginOptions_PreparedWithNoteID(t *testing.T) {
	h, noteRepo := newTestHandler(t)
	var got plugin.NoteDeliveryPrepareRequest
	h.SetNoteDeliveryRules(registryWith(t, func(_ context.Context, req plugin.NoteDeliveryPrepareRequest) error {
		got = req
		return nil
	}))

	rec := postCreate(t, h, `{"text":"hi","pluginOptions":{"targets":{"mode":"include","hosts":["a.example"]}}}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var resp struct {
		CreatedNote struct {
			ID string `json:"id"`
		} `json:"createdNote"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, resp.CreatedNote.ID, got.NoteID)
	assert.Equal(t, "user1", got.UserID)
	assert.JSONEq(t, `{"mode":"include","hosts":["a.example"]}`, string(got.Option))
	assert.Len(t, noteRepo.Notes, 1)
}

func TestCreate_PluginOptions_Rejected(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		prepare  error
		wantCode int
	}{
		{name: "unknown plugin", body: `{"text":"hi","pluginOptions":{"nope":{}}}`, wantCode: http.StatusBadRequest},
		{name: "bad option", body: `{"text":"hi","pluginOptions":{"targets":{}}}`, prepare: fmt.Errorf("%w: empty", plugin.ErrBadNoteOption), wantCode: http.StatusBadRequest},
		{name: "plugin failure", body: `{"text":"hi","pluginOptions":{"targets":{}}}`, prepare: errors.New("db down"), wantCode: http.StatusInternalServerError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h, noteRepo := newTestHandler(t)
			h.SetNoteDeliveryRules(registryWith(t, func(context.Context, plugin.NoteDeliveryPrepareRequest) error { return tt.prepare }))
			rec := postCreate(t, h, tt.body)
			assert.Equal(t, tt.wantCode, rec.Code, rec.Body.String())
			assert.Empty(t, noteRepo.Notes, "no note may be created")
			assert.NotContains(t, rec.Body.String(), "db down", "the plugin's error must not leak")
		})
	}
}

// Without a NoteDelivery plugin, any pluginOptions entry is rejected rather
// than silently ignored: the client asked for a restriction nobody enforces.
func TestCreate_PluginOptions_NoRegistry(t *testing.T) {
	h, noteRepo := newTestHandler(t)
	rec := postCreate(t, h, `{"text":"hi","pluginOptions":{"targets":{"mode":"include","hosts":[]}}}`)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Empty(t, noteRepo.Notes)

	rec = postCreate(t, h, `{"text":"hi","pluginOptions":{}}`)
	assert.Equal(t, http.StatusOK, rec.Code, "an empty object asks for nothing")
}

// insertFailingNoteRepo fails to store notes, as the database would.
type insertFailingNoteRepo struct {
	*testutil.MockNoteRepository
}

func (*insertFailingNoteRepo) Create(*model.Note) error { return errors.New("insert failed") }

// laggingReplicaNoteRepo answers plain reads as a replica that has not copied
// the note yet; ExistingNoteIDsOnPrimary still reads the primary.
type laggingReplicaNoteRepo struct {
	*testutil.MockNoteRepository
}

func (*laggingReplicaNoteRepo) FindByID(string) (*model.Note, error) {
	return nil, gorm.ErrRecordNotFound
}

// insertFailingPollRepo fails after the note row is written.
type insertFailingPollRepo struct {
	*testutil.MockPollRepository
}

func (*insertFailingPollRepo) Create(*model.Poll) error { return errors.New("poll insert failed") }

// forgetLog records the IDs a plugin was told to forget. ForgetNotes is
// called on another goroutine.
type forgetLog struct {
	mu  sync.Mutex
	got []string
}

func (l *forgetLog) ids() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.got...)
}

// waitFor waits until the plugin was told to forget n IDs.
func (l *forgetLog) waitFor(t *testing.T, n int) []string {
	t.Helper()
	require.Eventually(t, func() bool { return len(l.ids()) >= n }, 2*time.Second, 5*time.Millisecond)
	return l.ids()
}

// never asserts that the plugin is not told to forget anything.
func (l *forgetLog) never(t *testing.T, msg string) {
	t.Helper()
	assert.Never(t, func() bool { return len(l.ids()) > 0 }, 100*time.Millisecond, 5*time.Millisecond, msg)
}

// forgetRegistry returns a registry whose plugin records the IDs it was told
// to forget, failing Prepare with prepareErr.
func forgetRegistry(t *testing.T, prepareErr error) (*notedelivery.Registry, *forgetLog) {
	t.Helper()
	log := &forgetLog{}
	r := notedelivery.New()
	require.NoError(t, r.Add("targets", plugin.NoteDeliveryRegistration{
		Prepare: func(context.Context, plugin.NoteDeliveryPrepareRequest) error { return prepareErr },
		Targets: func(context.Context, []string) (map[string]plugin.DeliveryTargets, error) { return nil, nil },
		ForgetNotes: func(_ context.Context, ids []string) error {
			log.mu.Lock()
			defer log.mu.Unlock()
			log.got = append(log.got, ids...)
			return nil
		},
	}))
	return r, log
}

func newHandlerWithRepos(t *testing.T, noteRepo repository.NoteRepository, pollRepo repository.PollRepository) *Handler {
	t.Helper()
	idGen, _ := id.NewGenerator("aidx")
	createSvc := corenote.NewCreateService(noteRepo, pollRepo, idGen, nil, nil, nil, nil, nil, nil, nil)
	return newHandlerNilDeps(noteRepo, createSvc, corenote.NewDeleteService(noteRepo), corenote.NewQueryService(noteRepo, nil), nil, nil, nil, nil, idGen)
}

// The rows Prepare wrote are dropped when the note is not stored (#4202).
func TestCreate_PluginOptions_ForgetWhenNotStored(t *testing.T) {
	t.Run("insert failed", func(t *testing.T) {
		h := newHandlerWithRepos(t, &insertFailingNoteRepo{testutil.NewMockNoteRepository()}, testutil.NewMockPollRepository())
		r, forgot := forgetRegistry(t, nil)
		h.SetNoteDeliveryRules(r)
		rec := postCreate(t, h, `{"text":"hi","pluginOptions":{"targets":{}}}`)
		assert.Equal(t, http.StatusInternalServerError, rec.Code)
		assert.Len(t, forgot.waitFor(t, 1), 1)
	})
	t.Run("prepare failed", func(t *testing.T) {
		h, noteRepo := newTestHandler(t)
		r, forgot := forgetRegistry(t, fmt.Errorf("%w: empty", plugin.ErrBadNoteOption))
		h.SetNoteDeliveryRules(r)
		rec := postCreate(t, h, `{"text":"hi","pluginOptions":{"targets":{}}}`)
		assert.Equal(t, http.StatusBadRequest, rec.Code)
		assert.Empty(t, noteRepo.Notes)
		assert.Len(t, forgot.waitFor(t, 1), 1, "another plugin may have stored rows before this one refused")
	})
}

// A note whose row was written before the failure can still be fetched over
// ActivityPub, so its conditions must stay (#4202).
func TestCreate_PluginOptions_KeepWhenStored(t *testing.T) {
	t.Run("failed after insert", func(t *testing.T) {
		noteRepo := testutil.NewMockNoteRepository()
		h := newHandlerWithRepos(t, noteRepo, &insertFailingPollRepo{testutil.NewMockPollRepository()})
		r, forgot := forgetRegistry(t, nil)
		h.SetNoteDeliveryRules(r)
		rec := postCreate(t, h, `{"text":"hi","poll":{"choices":["a","b"]},"pluginOptions":{"targets":{}}}`)
		assert.Equal(t, http.StatusInternalServerError, rec.Code)
		require.Len(t, noteRepo.Notes, 1, "the note row was written")
		forgot.never(t, "the stored note can still be fetched")
	})
	t.Run("cannot tell", func(t *testing.T) {
		noteRepo := testutil.NewMockNoteRepository()
		h := newHandlerWithRepos(t, &insertFailingNoteRepo{noteRepo}, testutil.NewMockPollRepository())
		r, forgot := forgetRegistry(t, nil)
		h.SetNoteDeliveryRules(r)
		noteRepo.ExistingOnPrimaryErr = errors.New("db down")
		postCreate(t, h, `{"text":"hi","pluginOptions":{"targets":{}}}`)
		forgot.never(t, "a lookup failure is not proof that the note is gone")
	})
	t.Run("replica has not caught up", func(t *testing.T) {
		noteRepo := testutil.NewMockNoteRepository()
		h := newHandlerWithRepos(t, &laggingReplicaNoteRepo{noteRepo}, &insertFailingPollRepo{testutil.NewMockPollRepository()})
		r, forgot := forgetRegistry(t, nil)
		h.SetNoteDeliveryRules(r)
		rec := postCreate(t, h, `{"text":"hi","poll":{"choices":["a","b"]},"pluginOptions":{"targets":{}}}`)
		assert.Equal(t, http.StatusInternalServerError, rec.Code)
		require.Len(t, noteRepo.Notes, 1, "the note row was written")
		forgot.never(t, "a replica that has not copied the row is not proof that the note is gone")
	})
	t.Run("federated note", func(t *testing.T) {
		h, _ := newTestHandler(t)
		r, forgot := forgetRegistry(t, nil)
		h.SetNoteDeliveryRules(r)
		rec := postCreate(t, h, `{"text":"hi","pluginOptions":{"targets":{}}}`)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		forgot.never(t, "a federated note keeps its conditions")
	})
}

// A local-only note keeps its conditions: they also hide the OGP of the note
// when /notes/:id is fetched as a web page (#4202).
func TestCreate_PluginOptions_KeepLocalOnly(t *testing.T) {
	h, noteRepo := newTestHandler(t)
	r, forgot := forgetRegistry(t, nil)
	h.SetNoteDeliveryRules(r)
	rec := postCreate(t, h, `{"text":"hi","localOnly":true,"pluginOptions":{"targets":{}}}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Len(t, noteRepo.Notes, 1)
	forgot.never(t, "the OGP of a local-only note is hidden by its conditions")
}

// Without pluginOptions nothing was prepared, so nothing is forgotten.
func TestCreate_PluginOptions_NothingPreparedNothingForgotten(t *testing.T) {
	h, _ := newTestHandler(t)
	r, forgot := forgetRegistry(t, nil)
	h.SetNoteDeliveryRules(r)
	rec := postCreate(t, h, `{"text":"hi","localOnly":true}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	forgot.never(t, "nothing was prepared")
}

// The cleanup does not hold up the answer: a plugin that just failed Prepare
// may be stuck, and waiting for it again would delay the error (#4202).
func TestCreate_PluginOptions_ForgetDoesNotDelayTheAnswer(t *testing.T) {
	h, _ := newTestHandler(t)
	release := make(chan struct{})
	called := make(chan struct{}, 1)
	r := notedelivery.New()
	require.NoError(t, r.Add("targets", plugin.NoteDeliveryRegistration{
		Prepare: func(context.Context, plugin.NoteDeliveryPrepareRequest) error {
			return fmt.Errorf("%w: empty", plugin.ErrBadNoteOption)
		},
		Targets: func(context.Context, []string) (map[string]plugin.DeliveryTargets, error) { return nil, nil },
		ForgetNotes: func(ctx context.Context, _ []string) error {
			called <- struct{}{}
			select {
			case <-release:
			case <-ctx.Done():
			}
			return nil
		},
	}))
	h.SetNoteDeliveryRules(r)
	t.Cleanup(func() { close(release) })

	start := time.Now()
	rec := postCreate(t, h, `{"text":"hi","pluginOptions":{"targets":{}}}`)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Less(t, time.Since(start), time.Second, "the answer must not wait for ForgetNotes")
	select {
	case <-called:
	case <-time.After(2 * time.Second):
		t.Fatal("ForgetNotes was not called")
	}
}

// The cleanup runs in the group of the server generation, so stopping the
// generation waits for it (#3661, #4202).
func TestCreate_PluginOptions_ForgetRunsInBackgroundGroup(t *testing.T) {
	h, _ := newTestHandler(t)
	release := make(chan struct{})
	r := notedelivery.New()
	require.NoError(t, r.Add("targets", plugin.NoteDeliveryRegistration{
		Prepare: func(context.Context, plugin.NoteDeliveryPrepareRequest) error {
			return fmt.Errorf("%w: empty", plugin.ErrBadNoteOption)
		},
		Targets: func(context.Context, []string) (map[string]plugin.DeliveryTargets, error) { return nil, nil },
		ForgetNotes: func(context.Context, []string) error {
			<-release
			return nil
		},
	}))
	h.SetNoteDeliveryRules(r)
	g := bgwork.New()
	h.SetBackground(g)
	t.Cleanup(func() { close(release) })

	rec := postCreate(t, h, `{"text":"hi","pluginOptions":{"targets":{}}}`)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	require.Eventually(t, func() bool { return g.Running() == 1 }, 2*time.Second, 5*time.Millisecond)
}
