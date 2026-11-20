package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/config"
	"github.com/elythia-network/elythia/internal/core/cache"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/plugin"
)

// The built server reads its own host from url, and the embed, the feed and
// notes/show for visitors judge with that registry (#4203). The handler tests
// build the registry themselves, so they keep passing when the server is wired
// with notedelivery.New() (no own host) or hands a handler no registry at all;
// this test is what fails then.
func TestNewServer_DeliveryRulesHideNotesFromVisitors(t *testing.T) {
	db := serverIntegrationDB
	if db == nil {
		t.Skip("PostgreSQL unavailable")
	}
	t.Setenv(config.EnvOnlyServer, "1")
	t.Setenv(config.EnvOnlyQueue, "")
	t.Chdir(t.TempDir())
	mr := miniredis.RunT(t)
	rc := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { require.NoError(t, rc.Close()) })
	clients := &cache.RedisClients{Default: rc, Pubsub: rc, JobQueue: rc, Timelines: rc, Reactions: rc}
	port, err := strconv.Atoi(mr.Port())
	require.NoError(t, err)
	ro := config.RedisOptions{Host: mr.Host(), Port: port}
	// 自分のホストは url から作る。大文字と既定のポートを含めて、そろえる処理も通す。
	cfg := &config.Config{
		URL: "http://Example.Test:80", Host: "example.test", Hostname: "example.test",
		Scheme: "http", WsScheme: "ws", ID: "aidx", TestMode: true,
		MediaProxySecret: []byte("test-secret"),
		Redis:            ro, RedisForPubsub: ro, RedisForJobQueue: ro, RedisForTimelines: ro, RedisForReactions: ro,
	}

	const (
		userID   = "ndvisuser4203aaa"
		username = "ndvis_user"
	)
	cleanupPluginAccountUsers(t, db, username)
	createLocalUser(t, db, userID, username, "ndvistoken4203xx", "ndvis-pass", false)
	idGen := testIDGen(t)
	now := time.Now()
	shownID, hiddenID := idGen.Generate(now), idGen.Generate(now.Add(time.Second))
	notes := map[string]string{shownID: "ndvis-shown-body", hiddenID: "ndvis-hidden-body"}
	for id, text := range notes {
		require.NoError(t, db.Create(&model.Note{ID: id, UserID: userID, Visibility: model.NoteVisibilityPublic, Text: &text}).Error)
	}
	t.Cleanup(func() { db.Exec(`DELETE FROM "note" WHERE "id" IN (?, ?)`, shownID, hiddenID) })

	def := noteDeliveryDef("ndvis", func(plugin.Context) (plugin.NoteDeliveryRegistration, error) {
		return validNoteDelivery(map[string]plugin.DeliveryTargets{
			hiddenID: {Mode: plugin.DeliveryTargetsExclude, Hosts: []string{"example.test"}},
		}), nil
	})
	srv, err := newServer(cfg, db, clients, []plugin.Definition{def}, noopStorage)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, srv.Shutdown(context.Background())) })

	serve := func(method, path, body string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		rec := httptest.NewRecorder()
		srv.echo.ServeHTTP(rec, req)
		return rec
	}

	t.Run("embed", func(t *testing.T) {
		shown := serve(http.MethodGet, "/embed/notes/"+shownID, "")
		require.Equal(t, http.StatusOK, shown.Code)
		assert.Contains(t, shown.Body.String(), notes[shownID])
		hidden := serve(http.MethodGet, "/embed/notes/"+hiddenID, "")
		assert.NotContains(t, hidden.Body.String(), notes[hiddenID])
	})

	t.Run("feed", func(t *testing.T) {
		rec := serve(http.MethodGet, "/@"+username+".atom", "")
		require.Equal(t, http.StatusOK, rec.Code)
		assert.Contains(t, rec.Body.String(), notes[shownID])
		assert.NotContains(t, rec.Body.String(), notes[hiddenID])
	})

	t.Run("notes/show", func(t *testing.T) {
		for id, wantHidden := range map[string]bool{shownID: false, hiddenID: true} {
			rec := serve(http.MethodPost, "/api/notes/show", `{"noteId":"`+id+`"}`)
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			var got struct {
				IsHidden bool    `json:"isHidden"`
				Text     *string `json:"text"`
			}
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
			assert.Equal(t, wantHidden, got.IsHidden, id)
			if wantHidden {
				assert.Nil(t, got.Text)
			} else {
				require.NotNil(t, got.Text)
				assert.Equal(t, notes[id], *got.Text)
			}
		}
	})
}
