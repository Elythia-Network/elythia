package stream

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/elythia-network/elythia/internal/model"
)

// readNotification over the stream needs write:notifications, the kind of
// notifications/mark-all-as-read; read:account only lets the token subscribe.
func TestDispatcher_ReadNotification_RequiresWriteNotifications(t *testing.T) {
	for _, tt := range []struct {
		name  string
		user  *model.User
		perms []string
		want  int
	}{
		{"read:account only does not mark read", &model.User{ID: "alice"}, []string{"read:account"}, 0},
		{"empty permission does not mark read", &model.User{ID: "alice"}, []string{}, 0},
		{"write:notifications marks read", &model.User{ID: "alice"}, []string{"read:account", "write:notifications"}, 1},
		{"native token marks read", &model.User{ID: "alice"}, nil, 1},
		{"anonymous does not mark read", nil, nil, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			conn := NewConnection("test", tt.user, newFakeConn())
			if tt.perms != nil {
				conn.SetPermissions(tt.perms)
			}
			d := NewDispatcher(conn, nil, nil)
			nr := &stubNotifReader{}
			d.SetNotificationReader(nr)
			d.HandleClientMessage("readNotification", nil)
			assert.Equal(t, tt.want, nr.called)
		})
	}
}

// channelContext.HasPermission is what channels use to gate client messages.
func TestChannelContext_HasPermission(t *testing.T) {
	appConn := NewConnection("test", &model.User{ID: "alice"}, newFakeConn())
	appConn.SetPermissions([]string{"read:chat"})
	app := &channelContext{dispatcher: NewDispatcher(appConn, nil, nil), id: "c"}
	assert.True(t, app.HasPermission("read:chat"))
	assert.False(t, app.HasPermission("write:chat"))

	native := &channelContext{dispatcher: NewDispatcher(NewConnection("test", &model.User{ID: "alice"}, newFakeConn()), nil, nil), id: "c"}
	assert.True(t, native.HasPermission("write:chat"))

	anon := &channelContext{dispatcher: NewDispatcher(NewConnection("test", nil, newFakeConn()), nil, nil), id: "c"}
	assert.False(t, anon.HasPermission("write:chat"), "anonymous connections have no permission")

	detached := &channelContext{dispatcher: NewDispatcher(nil, nil, nil), id: "c"}
	assert.False(t, detached.HasPermission("write:chat"))
}
