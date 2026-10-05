// Package achievement provides server-side granting of user achievements,
// mirroring Misskey's AchievementService. Achievement type names live in
// internal/misc/achievement; this package is the write path that unlocks them
// and emits the achievementEarned notification.
//
// This is the path for server-triggered achievements (e.g. having your note
// favorited). The client-self-claim endpoint /api/i/claim-achievement writes
// the same profile.achievements column with equivalent semantics; the two
// coexist on the same storage.
package achievement

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/elythia-network/elythia/internal/core/notification"
	misc "github.com/elythia-network/elythia/internal/misc/achievement"
	"github.com/elythia-network/elythia/internal/model"
)

// Server-side granted achievement type names. Keep in sync with
// internal/misc/achievement ACHIEVEMENT_TYPES; using typed constants here keeps
// call sites typo-safe.
const (
	// MyNoteFavorited1 is granted to a local note's author when someone else
	// favorites the note (notes/favorites/create).
	MyNoteFavorited1 = "myNoteFavorited1"
)

// ProfileStore reads and updates the user profile holding the achievements
// JSON column. Satisfied by *user.Service (GetProfile / UpdateProfileFields).
type ProfileStore interface {
	GetProfile(userID string) *model.UserProfile
	UpdateProfileFields(userID string, fields map[string]any) error
}

// Notifier creates the achievementEarned notification. Satisfied by
// *notification.Service. Optional: when nil, grants are recorded without a
// notification.
type Notifier interface {
	Create(ctx context.Context, in notification.CreateInput) (*notification.Notification, error)
}

// Service grants achievements to users.
type Service struct {
	profiles ProfileStore
	notifier Notifier
}

// NewService creates an achievement Service. notifier may be nil to disable
// notification emission (grants are still persisted).
func NewService(profiles ProfileStore, notifier Notifier) *Service {
	return &Service{profiles: profiles, notifier: notifier}
}

// Grant unlocks achievement `name` for userID and reports whether a new
// achievement was recorded. It mirrors upstream AchievementService.create and
// is idempotent: it is a no-op (returns false, nil) when name is not a known
// achievement type, when the user's profile is missing, or when the user has
// already unlocked it. The achievementEarned notification is best-effort; a
// notification failure does not roll back the grant.
func (s *Service) Grant(ctx context.Context, userID, name string) (bool, error) {
	if !misc.IsValidType(name) {
		return false, nil
	}
	profile := s.profiles.GetProfile(userID)
	if profile == nil {
		return false, nil
	}

	var achievements []map[string]any
	if profile.Achievements != nil {
		_ = json.Unmarshal(profile.Achievements, &achievements)
	}
	for _, a := range achievements {
		if a["name"] == name {
			return false, nil
		}
	}

	achievements = append(achievements, map[string]any{
		"name":       name,
		"unlockedAt": time.Now().UnixMilli(),
	})
	data, _ := json.Marshal(achievements)
	if err := s.profiles.UpdateProfileFields(userID, map[string]any{"achievements": string(data)}); err != nil {
		return false, err
	}

	if s.notifier != nil {
		if _, err := s.notifier.Create(ctx, notification.CreateInput{
			NotifieeID: userID,
			Type:       notification.TypeAchievementEarned,
			Extra:      map[string]any{"achievement": name},
		}); err != nil {
			// 通知失敗は実績付与 (永続化済み) を巻き戻さない。upstream も
			// createNotification を await せず fire-and-forget している。
			slog.Warn("achievement grant: notification create failed", "user", userID, "achievement", name, "err", err)
		}
	}
	return true, nil
}
