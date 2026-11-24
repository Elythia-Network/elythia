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

// CheckDB runs first on every newly opened database. When it refuses, the
// pool is closed and no generation is built, at start and on a rebuild.
func TestSupervisor_BuildGenerationChecksTheDatabase(t *testing.T) {
	f := newFakeGenerations(t)
	deps := f.deps(0)
	var steps []string
	open := deps.OpenDB
	deps.OpenDB = func(cfg *config.Config) (*gorm.DB, error) {
		steps = append(steps, "open")
		return open(cfg)
	}
	build := deps.NewServer
	deps.NewServer = func(cfg *config.Config, db *gorm.DB, r *cache.RedisClients) (*Server, error) {
		steps = append(steps, "build")
		return build(cfg, db, r)
	}
	deps.PrepareDB = func(*gorm.DB) { steps = append(steps, "prepare") }
	var refuse error
	deps.CheckDB = func(*gorm.DB) error {
		steps = append(steps, "check")
		return refuse
	}

	s, err := NewSupervisor(&config.Config{}, nil, deps)
	require.NoError(t, err)
	assert.Equal(t, []string{"open", "check", "prepare", "build"}, steps)

	steps = nil
	refuse = errors.New("unconverted")
	closed := f.closed.Load()
	_, _, err = s.buildGeneration()
	require.ErrorIs(t, err, refuse)
	assert.Equal(t, []string{"open", "check"}, steps, "nothing is prepared or built on a refused database")
	assert.Equal(t, closed+1, f.closed.Load(), "the refused pool is closed")

	steps = nil
	_, err = NewSupervisor(&config.Config{}, nil, deps)
	require.ErrorIs(t, err, refuse, "a refused database fails the start")
	assert.Equal(t, []string{"open", "check"}, steps)
}
