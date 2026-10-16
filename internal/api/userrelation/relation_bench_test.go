package userrelation

import (
	"sync/atomic"
	"testing"

	"github.com/elythia-network/elythia/internal/entity"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/elythia-network/elythia/internal/testutil"
	"gorm.io/datatypes"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

// benchDB opens the package schema and seeds a viewer that follows the
// target and keeps a memo about it (the common users/show case).
func benchDB(b *testing.B) (*gorm.DB, string, *model.User) {
	b.Helper()
	dbOnce.Do(func() {
		testDB = testutil.MustOpenTestDB()
		testutil.ApplyMigrations(testDB)
	})
	db := testDB
	prefix := "urbench"
	for _, id := range []string{prefix + "v", prefix + "t"} {
		db.Exec(`DELETE FROM "user" WHERE id = ?`, id)
		if err := db.Create(&model.User{ID: id, Username: id, UsernameLower: id, AvatarDecorations: datatypes.JSON([]byte("[]"))}).Error; err != nil {
			b.Fatal(err)
		}
	}
	notify := "normal"
	for _, row := range []any{
		&model.Following{ID: prefix + "f", FollowerID: prefix + "v", FolloweeID: prefix + "t", Notify: &notify},
		&model.UserMemo{ID: prefix + "m", UserID: prefix + "v", TargetUserID: prefix + "t", Memo: "m"},
	} {
		if err := db.Create(row).Error; err != nil {
			b.Fatal(err)
		}
	}
	b.Cleanup(func() {
		db.Exec(`DELETE FROM "user" WHERE id LIKE ?`, prefix+"%")
	})
	// 本番の既定 (internal/db: maxOpen=25 / maxIdle=25) に揃える。テストの既定
	// (database/sql の maxIdle=2) のままだと、10 本を同時に投げる経路が毎回
	// 接続を張り直して、差が実際より大きく出る。
	sqlDB, err := db.DB()
	if err != nil {
		b.Fatal(err)
	}
	sqlDB.SetMaxOpenConns(25)
	sqlDB.SetMaxIdleConns(25)
	b.Cleanup(func() {
		sqlDB.SetMaxOpenConns(0)
		sqlDB.SetMaxIdleConns(2)
	})
	counted, err := gorm.Open(postgres.New(postgres.Config{Conn: sqlDB}), &gorm.Config{})
	if err != nil {
		b.Fatal(err)
	}
	count := func(*gorm.DB) { benchStatements.Add(1) }
	for _, err := range []error{
		counted.Callback().Query().After("gorm:query").Register("bench:count_query", count),
		counted.Callback().Row().After("gorm:row").Register("bench:count_row", count),
		counted.Callback().Raw().After("gorm:raw").Register("bench:count_raw", count),
	} {
		if err != nil {
			b.Fatal(err)
		}
	}
	return counted, prefix + "v", &model.User{ID: prefix + "t"}
}

var benchStatements atomic.Int64

// measure runs f b.N times and reports statements per op and the number of
// connections the pool had to open (= how many it used at once).
func measure(b *testing.B, db *gorm.DB, f func()) {
	b.Helper()
	sqlDB, err := db.DB()
	if err != nil {
		b.Fatal(err)
	}
	// 遊んでいる接続を閉じてから測り、測定中に開いた本数を数える。
	sqlDB.SetMaxIdleConns(0)
	sqlDB.SetMaxIdleConns(25)
	benchStatements.Store(0)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		f()
	}
	b.StopTimer()
	b.ReportMetric(float64(benchStatements.Load())/float64(b.N), "sql/op")
	b.ReportMetric(float64(sqlDB.Stats().OpenConnections), "conns")
}

// BenchmarkApply compares the per-relation queries with the one-statement
// read for one users/show relation block (#3608).
func BenchmarkApply(b *testing.B) {
	db, viewerID, target := benchDB(b)
	profile := &model.UserProfile{UserID: target.ID}
	each := reposOn(db)
	single := reposOn(db)
	single.Relation = repository.NewUserRelationRepository(db)
	for _, c := range []struct {
		name string
		r    Repos
	}{{"PerRelation", each}, {"OneStatement", single}} {
		b.Run(c.name, func(b *testing.B) {
			measure(b, db, func() {
				var d entity.UserDetailed
				c.r.Apply(&d, viewerID, target, profile)
			})
		})
	}
}

// BenchmarkMeUnreadFlags compares the five per-table /api/i unread queries
// with the one-statement read (#3608).
func BenchmarkMeUnreadFlags(b *testing.B) {
	db, viewerID, _ := benchDB(b)
	fr := repository.NewFollowRequestRepository(db)
	chat := repository.NewChatRepository(db)
	antenna := repository.NewAntennaNoteUnreadRepository(db)
	channel := repository.NewChannelNoteUnreadRepository(db)
	noteUnread := repository.NewNoteUnreadRepository(db)
	reader := repository.NewMeUnreadFlagsRepository(db)
	b.Run("PerTable", func(b *testing.B) {
		measure(b, db, func() {
			_, _ = fr.CountReceived(viewerID)
			_, _ = chat.CountUnread(viewerID)
			_, _ = antenna.HasAnyByUser(viewerID)
			_, _ = channel.HasAnyByUser(viewerID)
			_, _ = noteUnread.HasAnySpecified(viewerID)
		})
	})
	b.Run("OneStatement", func(b *testing.B) {
		measure(b, db, func() {
			_, _ = reader.ReadMeUnreadFlags(viewerID)
		})
	})
}
