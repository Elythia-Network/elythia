package notes

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/elythia-network/elythia/internal/core/timeline"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/elythia-network/elythia/internal/server/middleware"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/labstack/echo/v4"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

// BenchmarkHomeTimelineViewerRelations measures the per-request SQL count and
// latency of notes/timeline (database path) against a real PostgreSQL,
// focusing on the viewer's relation lists (#3607). It needs TEST_DB_* and is
// not run by CI (benchmarks only run with -bench).
//
// BENCH_RELATION_NOCACHE=1 leaves the relation cache unwired (only the
// per-request de-duplication applies); BENCH_RELATION_LOG_SQL=1 prints every
// statement.
//
//	go test ./internal/api/notes/ -run '^$' -bench BenchmarkHomeTimelineViewerRelations -benchtime 500x
func BenchmarkHomeTimelineViewerRelations(b *testing.B) {
	db, err := testutil.OpenTestDB()
	if err != nil {
		b.Skipf("test DB unavailable: %v", err)
	}
	testutil.ApplyMigrations(db)

	const prefix = "bvr3607"
	viewerID := prefix + "viewer"
	cleanup := func() {
		db.Exec(`DELETE FROM "user" WHERE id LIKE ?`, prefix+"%")
		db.Exec(`DELETE FROM "channel" WHERE id LIKE ?`, prefix+"%")
	}
	cleanup()
	b.Cleanup(cleanup)

	mkUser := func(uid string) {
		u := &model.User{ID: uid, Username: uid, UsernameLower: uid, AvatarDecorations: datatypes.JSON([]byte("[]"))}
		if err := db.Create(u).Error; err != nil {
			b.Fatal(err)
		}
	}
	mkUser(viewerID)
	for i := 0; i < 120; i++ {
		mkUser(fmt.Sprintf("%su%03d", prefix, i))
	}
	must := func(tx *gorm.DB) {
		if tx.Error != nil {
			b.Fatal(tx.Error)
		}
	}
	for i := 0; i < 100; i++ {
		must(db.Create(&model.Following{ID: fmt.Sprintf("%sf%03d", prefix, i), FollowerID: viewerID, FolloweeID: fmt.Sprintf("%su%03d", prefix, i)}))
	}
	for i := 100; i < 110; i++ {
		must(db.Create(&model.Muting{ID: fmt.Sprintf("%sm%03d", prefix, i), MuterID: viewerID, MuteeID: fmt.Sprintf("%su%03d", prefix, i)}))
	}
	for i := 110; i < 115; i++ {
		must(db.Create(&model.Blocking{ID: fmt.Sprintf("%sb%03d", prefix, i), BlockerID: fmt.Sprintf("%su%03d", prefix, i), BlockeeID: viewerID}))
	}
	for i := 115; i < 120; i++ {
		must(db.Create(&model.RenoteMuting{ID: fmt.Sprintf("%sr%03d", prefix, i), MuterID: viewerID, MuteeID: fmt.Sprintf("%su%03d", prefix, i)}))
	}
	for i := 0; i < 3; i++ {
		chID := fmt.Sprintf("%sch%d", prefix, i)
		must(db.Exec(`INSERT INTO "channel" (id, name, "userId") VALUES (?, ?, ?)`, chID, chID, viewerID))
		must(db.Create(&model.ChannelFollowing{ID: fmt.Sprintf("%scf%d", prefix, i), FollowerID: viewerID, FolloweeID: chID}))
	}
	must(db.Create(&model.ChannelMuting{ID: prefix + "cm0", UserID: viewerID, ChannelID: prefix + "ch0"}))

	var queries atomic.Int64
	logSQL := os.Getenv("BENCH_RELATION_LOG_SQL") != ""
	count := func(tx *gorm.DB) {
		queries.Add(1)
		if logSQL {
			fmt.Println("SQL:", tx.Statement.SQL.String())
		}
	}
	_ = db.Callback().Query().After("gorm:query").Register("bench3607:count", count)
	_ = db.Callback().Row().After("gorm:row").Register("bench3607:count", count)
	_ = db.Callback().Raw().After("gorm:raw").Register("bench3607:count", count)
	b.Cleanup(func() {
		_ = db.Callback().Query().Remove("bench3607:count")
		_ = db.Callback().Row().Remove("bench3607:count")
		_ = db.Callback().Raw().Remove("bench3607:count")
	})

	noteRepo := repository.NewNoteRepository(db)
	followingRepo := repository.NewFollowingRepository(db)
	userRepo := repository.NewCachedUserRepository(repository.NewUserRepository(db))
	tlService := timeline.NewService(nil, noteRepo, followingRepo)
	tlService.SetFanoutToggle(benchFanoutOff{})
	h := NewHandler(noteRepo, nil, nil, nil, tlService, nil, nil, nil, benchIDGen(b))
	h.SetUserRepo(userRepo)
	h.SetMetaRepo(repository.NewCachedMetaRepository(repository.NewMetaRepository(db)))
	h.SetMutingRepo(repository.NewMutingRepository(db))
	h.SetRenoteMutingRepo(repository.NewRenoteMutingRepository(db))
	h.SetBlockingRepo(repository.NewBlockingRepository(db))
	h.SetUserFollowingRepo(followingRepo)
	h.SetChannelMutingRepo(repository.NewChannelMutingRepository(db))
	h.SetChannelFollowingRepo(repository.NewChannelFollowingRepository(db))
	if os.Getenv("BENCH_RELATION_NOCACHE") == "" {
		h.SetRelationCache(repository.NewRelationCache(db))
	}

	viewer, err := userRepo.FindByID(viewerID)
	if err != nil {
		b.Fatal(err)
	}
	e := echo.New()
	call := func() {
		req := httptest.NewRequest(http.MethodPost, "/api/notes/timeline", strings.NewReader(`{"limit":10}`))
		req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
		rec := httptest.NewRecorder()
		c := e.NewContext(req, rec)
		c.Set(string(middleware.UserContextKey), viewer)
		if err := h.Timeline(c); err != nil || rec.Code != http.StatusOK {
			b.Fatalf("timeline: err=%v code=%d body=%s", err, rec.Code, rec.Body.String())
		}
	}
	call() // warm meta / user caches
	queries.Store(0)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		call()
	}
	b.StopTimer()
	b.ReportMetric(float64(queries.Load())/float64(b.N), "sql/op")
}

func benchIDGen(b *testing.B) id.Generator {
	g, err := id.NewGenerator("aidx")
	if err != nil {
		b.Fatal(err)
	}
	return g
}

type benchFanoutOff struct{}

func (benchFanoutOff) FanoutTimelineEnabled() bool { return false }
