package admin

// Internal (package admin) test for modlog helpers. Lets us hit the
// nil-target defensive branch of logUserActionWithUser directly without
// going through a handler. Phase 1 review iter 2 #1 follow-up.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/elythia-network/elythia/internal/core/moderationlog"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/labstack/echo/v4"
)

// newCtx builds a minimal echo.Context for helper invocation. The
// helpers only touch c.Request().Context() so a stub request is enough.
func newCtx() echo.Context {
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	return e.NewContext(req, rec)
}

func TestLogUserActionWithUser_NilTargetIsNoop(t *testing.T) {
	repo := testutil.NewMockModerationLogRepository()
	gen, err := id.NewGenerator("aidx")
	if err != nil {
		t.Fatalf("idgen: %v", err)
	}
	h := &Handler{modLogService: moderationlog.New(repo, gen)}

	// nil target — must early-return without writing anything.
	h.logUserActionWithUser(newCtx(), moderationlog.LogSuspend, nil)

	if got := repo.Snapshot(); len(got) != 0 {
		t.Errorf("expected no log writes, got %d", len(got))
	}
}

// logUserAction が targetUserID を FindByID で引けない (= 削除済 / bogus id) 場合は
// debug ログだけ残して何も書かないことを直接 guard する。reset-password が
// 存在チェックを前段に持つようになり (#1862) このパスを踏まなくなったので
// helper 単体で覆う。
func TestLogUserAction_MissingTargetIsNoop(t *testing.T) {
	repo := testutil.NewMockModerationLogRepository()
	gen, err := id.NewGenerator("aidx")
	if err != nil {
		t.Fatalf("idgen: %v", err)
	}
	// userRepo は空 → FindByID は ErrNotFound を返す。
	h := &Handler{userRepo: testutil.NewMockUserRepository(), modLogService: moderationlog.New(repo, gen)}

	h.logUserAction(newCtx(), moderationlog.LogResetPassword, "ghost")

	if got := repo.Snapshot(); len(got) != 0 {
		t.Errorf("expected no log writes for a missing target, got %d", len(got))
	}
}
