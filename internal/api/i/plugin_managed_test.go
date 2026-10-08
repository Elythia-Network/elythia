package i

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"

	"github.com/elythia-network/elythia/internal/model"
)

// i/update は、プラグインが管理するアカウント (#3468) の isBot を外す指定を
// 400 PLUGIN_MANAGED_ACCOUNT_MUST_BE_BOT で拒否する。
func TestUpdate_PluginManagedAccountMustStayBot(t *testing.T) {
	h, repo, _, _ := newTestHandler(t)
	name := "bot-plugin"
	bot := &model.User{ID: "bot1", Username: "bot1", IsBot: true, ManagedByPlugin: &name, AvatarDecorations: datatypes.JSON([]byte("[]"))}
	repo.Users["bot1"] = bot
	repo.Profiles["bot1"] = &model.UserProfile{UserID: "bot1", Fields: datatypes.JSON([]byte("[]"))}

	rec := post(h.Update, `{"isBot":false}`, bot)
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "PLUGIN_MANAGED_ACCOUNT_MUST_BE_BOT")
	assert.Contains(t, rec.Body.String(), "226cc907-c7b3-4e3d-aef4-f583967e3dd1")
	assert.True(t, repo.Users["bot1"].IsBot)
}
