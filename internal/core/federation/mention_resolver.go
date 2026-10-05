package federation

import (
	"errors"

	"github.com/elythia-network/elythia/internal/activitypub"
	"github.com/elythia-network/elythia/internal/repository"
	"gorm.io/gorm"
)

// UserMentionResolver implements activitypub.MentionResolver by looking up
// users via UserRepository and building the AS Mention tag data (name + href).
// ローカルユーザーは urls.UserURI を返し、リモートユーザーは user.URI を返す。
type UserMentionResolver struct {
	userRepo repository.UserRepository
	urls     *activitypub.URLBuilder
}

// NewUserMentionResolver constructs a UserMentionResolver.
func NewUserMentionResolver(userRepo repository.UserRepository, urls *activitypub.URLBuilder) *UserMentionResolver {
	return &UserMentionResolver{userRepo: userRepo, urls: urls}
}

// ResolveMention looks up the user by ID and returns the mention name and URI.
//
// 戻り値:
//   - 成功: name, uri, nil
//   - ユーザー未存在 (削除済 / ID 不整合): "", "", activitypub.ErrMentionUserNotFound
//   - DB 障害: "", "", <生 error> (caller 側で log 出力)
//
// gorm.ErrRecordNotFound は明示的に not-found sentinel に変換し、それ以外の
// error は DB 障害として呼び出し元に伝える (#572 item 2)。
func (r *UserMentionResolver) ResolveMention(userID string) (name, uri string, err error) {
	u, err := r.userRepo.FindByID(userID)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return "", "", activitypub.ErrMentionUserNotFound
		}
		return "", "", err
	}
	if u == nil {
		return "", "", activitypub.ErrMentionUserNotFound
	}
	if u.IsLocal() {
		return "@" + u.Username, r.urls.UserURI(u.ID), nil
	}
	if u.URI == nil || *u.URI == "" {
		// リモートユーザーで URI が無いケース — 不整合として扱い、削除済 user
		// と同等の skip 動作にする。
		return "", "", activitypub.ErrMentionUserNotFound
	}
	// IsLocal() returns false here, so Host is non-nil and non-empty.
	return "@" + u.Username + "@" + *u.Host, *u.URI, nil
}
