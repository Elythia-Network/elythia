package model

import (
	"github.com/elythia-network/elythia/internal/config"
	"github.com/elythia-network/elythia/internal/db"
	"gorm.io/gorm"
)

// NewDatabase opens the application's GORM database connection. Thin wrapper
// around internal/db.New retained so existing callers (internal/cli/serve) need no
// import path change. New code should depend on internal/db directly.
func NewDatabase(cfg *config.Config) (*gorm.DB, error) {
	return db.New(cfg)
}
