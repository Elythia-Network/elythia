package server

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/elythia-network/elythia/internal/config"
	"github.com/elythia-network/elythia/internal/core/cache"
)

// PrepareDB runs on every newly opened database, before the server generation
// is built on it (#3909), and not when the database could not be opened.
func TestSupervisor_BuildGenerationPreparesTheDatabase(t *testing.T) {
	f := newFakeGenerations(t)
	deps := f.deps(0)
	var steps []string
	var prepared *gorm.DB
	open := deps.OpenDB
	deps.OpenDB = func(cfg *config.Config) (*gorm.DB, error) {
		steps = append(steps, "open")
		return open(cfg)
	}
	build := deps.NewServer
	deps.NewServer = func(cfg *config.Config, db *gorm.DB, r *cache.RedisClients) (*Server, error) {
		steps = append(steps, "build")
		assert.Same(t, prepared, db, "the server is built on the prepared database")
		return build(cfg, db, r)
	}
	deps.PrepareDB = func(db *gorm.DB) {
		steps = append(steps, "prepare")
		prepared = db
	}
	// NewSupervisor が最初の世代を作る。
	s, err := NewSupervisor(&config.Config{}, nil, deps)
	require.NoError(t, err)
	assert.Equal(t, []string{"open", "prepare", "build"}, steps)

	// 作り直すときも同じ順。
	steps = nil
	gen, _, err := s.buildGeneration()
	require.NoError(t, err)
	require.NotNil(t, gen)
	assert.Equal(t, []string{"open", "prepare", "build"}, steps)

	steps = nil
	f.openErr = errors.New("refused")
	_, _, err = s.buildGeneration()
	require.Error(t, err)
	assert.Equal(t, []string{"open"}, steps, "nothing is prepared without a database")
}
