package auth

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/bgwork"
	"github.com/elythia-network/elythia/internal/model"
)

// The createToken notification starts in the group of the server generation
// (#3661): after the group has stopped, it does not run.
func TestGenToken_NotifierRunsInTheBackgroundGroup(t *testing.T) {
	h, _ := newTestHandler()
	notifier := &chanTokenNotifier{ch: make(chan string, 1)}
	h.SetTokenNotifier(notifier)
	g := bgwork.New()
	h.SetBackground(g)
	assert.True(t, h.HasBackground())
	require.Zero(t, g.Stop(context.Background()))

	rec := post(h.GenToken, `{"permission":["read:account"]}`, &model.User{ID: "u1", Username: "testuser"})
	require.Equal(t, http.StatusOK, rec.Code)
	select {
	case <-notifier.ch:
		t.Fatal("世代を止めた後に token の作成を通知した")
	case <-time.After(100 * time.Millisecond):
	}
	assert.EqualValues(t, 1, g.Dropped())
}
