package inbox

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/elythia-network/elythia/internal/queue"
)

// #2682: 起動時の配線検査が見る述語。false 側を固定する。弱める変異
// (常に true) は e2e でも捕まらないので、ここで縛る。
func TestHandler_HasExpectedHost(t *testing.T) {
	assert.False(t, (&Handler{}).HasExpectedHost(), "未設定なら false")
	h := &Handler{}
	h.SetExpectedHost("example.test")
	assert.True(t, h.HasExpectedHost())
}

func TestHandler_HasEnqueuer(t *testing.T) {
	assert.False(t, (&Handler{}).HasEnqueuer(), "未配線なら false")

	h := &Handler{}
	h.SetEnqueuer(stubInboxEnqueuer{})
	assert.True(t, h.HasEnqueuer(), "配線したら true")
}

type stubInboxEnqueuer struct{}

func (stubInboxEnqueuer) EnqueueInbox(context.Context, queue.InboxPayload) error { return nil }

// #2683: host block checker も同じ扱い。未配線だと `federation: none` 設定でも
// inbox が受け付ける。
func TestHandler_HasHostBlockChecker(t *testing.T) {
	assert.False(t, (&Handler{}).HasHostBlockChecker(),
		"未配線なら false を返すこと (常に true だと検査が無意味になる)")

	// **配線したら true も固定する** (#2683 review LOW-1)。
	h := &Handler{}
	h.SetHostBlockChecker(stubInboxHostBlock{})
	assert.True(t, h.HasHostBlockChecker(), "配線したら true")
}

type stubInboxHostBlock struct{}

func (stubInboxHostBlock) IsBlocked(string) bool    { return false }
func (stubInboxHostBlock) IsAllowed(string) bool    { return true }
func (stubInboxHostBlock) FederationDisabled() bool { return false }
