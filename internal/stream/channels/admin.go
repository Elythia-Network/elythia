package channels

import (
	"encoding/json"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/stream"
)

// AdminRoleChecker checks whether a user has moderator/admin privileges.
//
// upstream の admin stream channel は streaming layer で role check せず、
// requireCredential=true (native session) または kind='read:admin:stream'
// permission (OAuth token) のみで gate する。`read:admin:stream` は moderator
// 用 scope なので moderator が購読できるのが意図。mk-go は native session に
// role/permission 概念を持たないため、その scope 意図を server-side の
// moderator check で代替する (= upstream より厳格だが abuse report 等の event
// を漏らさず moderator にも届ける)。以前は IsAdministrator で admin 限定にして
// おり、moderator を不当に弾いていた (#1948-20)。IsModerator は administrator
// も含む。
type AdminRoleChecker interface {
	IsModerator(userID string) bool
}

// AdminChannel forwards admin-only events. Requires authenticated admin user.
type AdminChannel struct {
	ctx         stream.ChannelContext
	roleChecker AdminRoleChecker
	topic       string
	connected   bool
}

// AdminFactory holds the role checker dependency for admin channel creation.
type AdminFactory struct {
	roleChecker AdminRoleChecker
}

// NewAdminFactory constructs an AdminFactory with the given role checker.
func NewAdminFactory(roleChecker AdminRoleChecker) *AdminFactory {
	return &AdminFactory{roleChecker: roleChecker}
}

// New builds a new AdminChannel. Usable as a stream.ChannelFactory.
func (f *AdminFactory) New(ctx stream.ChannelContext) stream.Channel {
	return &AdminChannel{ctx: ctx, roleChecker: f.roleChecker}
}

func (c *AdminChannel) Init(_ json.RawMessage) error {
	user, ok := c.ctx.User().(*model.User)
	if !ok || user == nil {
		return stream.ErrInvalidParams
	}
	// moderator/admin 権限チェック (read:admin:stream scope 相当、AdminRoleChecker
	// の docstring 参照)。moderator も abuse report 等を受信できる (#1948-20。以前は
	// IsAdministrator で admin 限定だった)。
	if c.roleChecker == nil || !c.roleChecker.IsModerator(user.ID) {
		return stream.ErrInvalidParams
	}
	c.connected = true
	// upstream admin.ts は per-user topic `adminStream:${userId}` を購読する
	// (#1549)。各 moderator/admin は自分宛の newAbuseUserReport 等のみ受信する。
	c.topic = "adminStream:" + user.ID
	c.ctx.Subscribe(c.topic)
	return nil
}

func (c *AdminChannel) OnRedisEvent(payload []byte) {
	// {type, body}エンベロープの展開
	var envelope struct {
		Type string          `json:"type"`
		Body json.RawMessage `json:"body"`
	}
	if err := json.Unmarshal(payload, &envelope); err == nil && envelope.Type != "" {
		_ = c.ctx.Send(envelope.Type, envelope.Body)
		return
	}
	_ = c.ctx.Send("adminEvent", json.RawMessage(payload))
}

func (c *AdminChannel) OnClientMessage(string, json.RawMessage) {}

// ShouldShare implements stream.ShareableChannel.
func (c *AdminChannel) ShouldShare() bool { return true }

// RequiredPermission implements stream.PermittedChannel.
func (c *AdminChannel) RequiredPermission() string { return "read:admin:stream" }

func (c *AdminChannel) Dispose() {
	if c.connected && c.topic != "" {
		c.ctx.Unsubscribe(c.topic)
	}
}
