package notifications

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/elythia-network/elythia/internal/core/notification"
	"github.com/elythia-network/elythia/internal/entity"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
)

// HasEntityPacker は mediaURL を持つ Packer を受け取ったときだけ true を返す
// (#3796)。criticalWiring がこれで入れ忘れを見る。
func TestHasEntityPacker(t *testing.T) {
	for name, h := range map[string]interface {
		SetEntityPacker(*entity.Packer)
		HasEntityPacker() bool
	}{"Handler": &Handler{}} {
		t.Run(name, func(t *testing.T) {
			if h.HasEntityPacker() {
				t.Error("an unwired component must report false")
			}
			h.SetEntityPacker(entity.NewPacker(entity.PackerDeps{}))
			if h.HasEntityPacker() {
				t.Error("a Packer without mediaURL must report false")
			}
			h.SetEntityPacker(entity.NewPacker(entity.PackerDeps{MediaURL: entity.NewMediaURLContext(
				"https://local.example", "", []byte("secret"), false, true)}))
			if !h.HasEntityPacker() {
				t.Error("a Packer with mediaURL must report true")
			}
		})
	}
}

// i/notifications の通知した人 (UserLite) は、SetEntityPacker で渡された Packer で
// 包む (#3796)。
// 渡した Packer を使わずに nil の Packer で包むと avatarUrl が生のリモートの
// URL になって落ちる。
func TestShow_PacksWithTheGivenPacker(t *testing.T) {
	h, svc := newTestHandler(t)
	userRepo := testutil.NewMockUserRepository()
	avatar := "https://remote.example/avatar.png"
	userRepo.Users["bob"] = &model.User{ID: "bob", Username: "bob", AvatarURL: &avatar}
	h.SetRepos(userRepo, testutil.NewMockNoteRepository())
	h.SetEntityPacker(entity.NewPacker(entity.PackerDeps{MediaURL: entity.NewMediaURLContext(
		"https://local.example", "https://local.example/proxy", []byte("secret"), false, true)}))
	_, err := svc.Create(context.Background(), notification.CreateInput{
		NotifieeID: "alice", NotifierID: "bob", Type: notification.TypeFollow,
	})
	if err != nil {
		t.Fatal(err)
	}

	c, rec := newJSONRequest(t, "/api/i/notifications", `{}`)
	setAuth(c, &model.User{ID: "alice"})
	if err := h.Show(c); err != nil {
		t.Fatal(err)
	}
	var resp []struct {
		User struct {
			AvatarURL string `json:"avatarUrl"`
		} `json:"user"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || len(resp) != 1 {
		t.Fatalf("unexpected response (%v): %s", err, rec.Body.String())
	}
	if got := resp[0].User.AvatarURL; !strings.HasPrefix(got, "https://local.example/proxy/avatar.webp?") {
		t.Errorf("avatarUrl must be proxied by the given Packer: %q", got)
	}
}
