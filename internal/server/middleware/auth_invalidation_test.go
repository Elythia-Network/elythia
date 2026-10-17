package middleware

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAuthMiddleware_InvalidationHook pins which invalidations are handed to
// the hook that tells the other processes (#3664): the user-level and the
// token-level ones, by user id only, and never the local-only ones.
func TestAuthMiddleware_InvalidationHook(t *testing.T) {
	auth := NewAuthMiddleware(testutil.NewMockUserRepository(), testutil.NewMockAccessTokenRepository())
	var hooked []string
	auth.SetInvalidationHook(func(userID string) {
		// hook の時点で、自プロセスの entry は捨て済みであること。
		_, _, _, _, ok := auth.tokenCache.get("tok-" + userID)
		assert.False(t, ok, "the local entry is dropped before the hook runs")
		hooked = append(hooked, userID)
	})
	fill := func() {
		auth.tokenCache.put("tok-u1", &model.User{ID: "u1"}, nil, "", false)
		auth.tokenCache.put("tok-u2", &model.User{ID: "u2"}, nil, "", false)
	}

	fill()
	auth.InvalidateTokensForUser("u1")
	auth.InvalidateUserToken("u2", "tok-u2")
	// 利用者の分からない token は自プロセスだけで捨てる。
	fill()
	auth.InvalidateUserToken("", "tok-u2")
	_, _, _, _, ok := auth.tokenCache.get("tok-u2")
	assert.False(t, ok)
	// 空の token / 利用者は何もしない。
	auth.InvalidateUserToken("u1", "")
	auth.InvalidateTokensForUser("")
	// 受けた側の口は伝えない。
	fill()
	auth.InvalidateTokensForUserLocally("u1")
	auth.InvalidateTokensForUserLocally("")
	_, _, _, _, ok = auth.tokenCache.get("tok-u1")
	assert.False(t, ok)
	auth.InvalidateAllLocally()
	_, _, _, _, ok = auth.tokenCache.get("tok-u2")
	assert.False(t, ok)

	assert.Equal(t, []string{"u1", "u2"}, hooked)
}

// hookedUserRepo runs during inside FindByToken, i.e. while the auth
// middleware's DB read is in flight.
type hookedUserRepo struct {
	*testutil.MockUserRepository
	during func()
	reads  int
}

func (r *hookedUserRepo) FindByToken(token string) (*model.User, error) {
	r.reads++
	u, err := r.MockUserRepository.FindByToken(token)
	if r.during != nil {
		d := r.during
		r.during = nil
		d()
	}
	return u, err
}

// hookedAccessTokenRepo is hookedUserRepo for app access tokens.
type hookedAccessTokenRepo struct {
	*testutil.MockAccessTokenRepository
	during func()
	reads  int
}

func (r *hookedAccessTokenRepo) FindByHashOrToken(hash, raw string) (*model.AccessToken, error) {
	r.reads++
	tok, err := r.MockAccessTokenRepository.FindByHashOrToken(hash, raw)
	if r.during != nil {
		d := r.during
		r.during = nil
		d()
	}
	return tok, err
}

// authenticateOnce runs one request with token through Authenticate and
// returns the user the handler saw.
func authenticateOnce(t *testing.T, auth *AuthMiddleware, token string) *model.User {
	t.Helper()
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	var seen *model.User
	require.NoError(t, auth.Authenticate()(func(c echo.Context) error {
		seen = GetUser(c)
		return c.NoContent(http.StatusOK)
	})(e.NewContext(req, rec)))
	return seen
}

// TestAuthMiddleware_InvalidationDuringReadIsNotStored pins that a user read
// from the DB while that user's entries were dropped (here by another
// process's notification) answers that one request but is not cached, so the
// next request reads the DB again instead of using the possibly stale row for
// the cache TTL (#3664). Native and app tokens, user-level and whole-cache
// invalidations.
func TestAuthMiddleware_InvalidationDuringReadIsNotStored(t *testing.T) {
	const native = "abcdef1234567890"
	const app = "app-token-value"
	for name, invalidate := range map[string]func(*AuthMiddleware){
		"user": func(a *AuthMiddleware) { a.InvalidateTokensForUserLocally("u1") },
		"all":  func(a *AuthMiddleware) { a.InvalidateAllLocally() },
		"token": func(a *AuthMiddleware) {
			a.InvalidateUserToken("u1", "some-other-token-of-u1")
		},
	} {
		t.Run(name, func(t *testing.T) {
			users := &hookedUserRepo{MockUserRepository: testutil.NewMockUserRepository()}
			tokens := &hookedAccessTokenRepo{MockAccessTokenRepository: testutil.NewMockAccessTokenRepository()}
			auth := NewAuthMiddleware(users, tokens)
			nativeToken := native
			users.Tokens[native] = &model.User{ID: "u1", Token: &nativeToken}

			users.during = func() { invalidate(auth) }
			require.NotNil(t, authenticateOnce(t, auth, native), "the in-flight read answers its request")
			require.NotNil(t, authenticateOnce(t, auth, native))
			assert.Equal(t, 2, users.reads, "the row read during the invalidation was not cached")
			require.NotNil(t, authenticateOnce(t, auth, native))
			assert.Equal(t, 2, users.reads, "a later read is cached again")

			tokens.Tokens[sha256Hash(app)] = &model.AccessToken{ID: "at1", UserID: "u1", User: &model.User{ID: "u1"}}
			tokens.during = func() { invalidate(auth) }
			require.NotNil(t, authenticateOnce(t, auth, app))
			require.NotNil(t, authenticateOnce(t, auth, app))
			assert.Equal(t, 2, tokens.reads, "the app token read during the invalidation was not cached")
		})
	}
}

// TestTokenCache_InvalidationMarks pins the marks behind the check: another
// user's mark does not block a store, marks older than the TTL are swept once
// there are many, and a whole-cache invalidation replaces them.
func TestTokenCache_InvalidationMarks(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	c := newTokenCache()
	c.now = func() time.Time { return now }

	readStart := now
	c.markInvalidated("u1")
	c.putIfFresh(readStart, "t2", &model.User{ID: "u2"}, nil, "", false)
	_, _, _, _, ok := c.get("t2")
	assert.True(t, ok, "another user's invalidation does not block the store")
	c.putIfFresh(readStart, "t1", &model.User{ID: "u1"}, nil, "", false)
	_, _, _, _, ok = c.get("t1")
	assert.False(t, ok)

	// 印が溜まったら、TTL より古いものを掃除する。
	for i := range invalidatedAtSweepAbove {
		c.markInvalidated(fmt.Sprintf("old-%d", i))
	}
	now = now.Add(c.ttl + time.Second)
	c.markInvalidated("fresh")
	assert.Len(t, c.invalidatedAt, 1, "marks older than the TTL are swept")

	c.invalidateAll()
	assert.Nil(t, c.invalidatedAt)
	c.putIfFresh(now, "t3", &model.User{ID: "u3"}, nil, "", false)
	_, _, _, _, ok = c.get("t3")
	assert.False(t, ok, "a read started before the whole-cache invalidation is not stored")
	c.putIfFresh(now.Add(time.Second), "t3", &model.User{ID: "u3"}, nil, "", false)
	_, _, _, _, ok = c.get("t3")
	assert.True(t, ok)
	// 利用者の無い entry (呼ばれない形) も判定できる。
	c.putIfFresh(now.Add(time.Second), "t4", nil, nil, "", false)
}

var _ repository.UserRepository = (*hookedUserRepo)(nil)

// TestTokenCache_MarksBeforeDropping pins the order inside the invalidations:
// the mark is set before the entries are deleted. A lookup that started
// before the invalidation and stores right after the deletion must then be
// rejected; with the order reversed it would store the stale user for the
// TTL (#3664).
func TestTokenCache_MarksBeforeDropping(t *testing.T) {
	for name, invalidate := range map[string]func(*tokenCache){
		"user": func(c *tokenCache) { c.invalidateByUserID("u1") },
		"all":  func(c *tokenCache) { c.invalidateAll() },
	} {
		t.Run(name, func(t *testing.T) {
			now := time.Unix(1_000_000, 0)
			c := newTokenCache()
			c.now = func() time.Time { return now }
			readStart := now
			c.put("old", &model.User{ID: "u1"}, nil, "", false)
			c.afterDrop = func() {
				c.afterDrop = nil
				// 捨てる前に読み始めた要求が、消し終えた直後に積もうとする。
				c.putIfFresh(readStart, "late", &model.User{ID: "u1"}, nil, "", false)
			}
			invalidate(c)
			_, _, _, _, ok := c.get("old")
			assert.False(t, ok)
			_, _, _, _, ok = c.get("late")
			assert.False(t, ok, "a lookup from before the invalidation must not be stored after it")
		})
	}
}
