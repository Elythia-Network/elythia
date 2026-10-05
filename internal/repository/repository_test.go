package repository

import (
	"os"
	"testing"

	"github.com/elythia-network/elythia/internal/testutil"
	"gorm.io/gorm"
)

var testDB *gorm.DB

func TestMain(m *testing.M) {
	db, err := testutil.OpenTestDB()
	if err != nil {
		panic("failed to open test DB: " + err.Error())
	}
	testDB = db

	// マイグレーション適用 (冪等)
	testutil.ApplyMigrations(testDB)

	os.Exit(m.Run())
}
