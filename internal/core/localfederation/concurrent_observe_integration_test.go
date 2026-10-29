package localfederation

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestConcurrentObserveExternalReferenceKeepsOneFirstReference(t *testing.T) {
	db, err := testutil.OpenTestDBSchema("localfederation_observe_race")
	require.NoError(t, err)
	testutil.ApplyMigrations(db)
	user := createFederationTestUserOn(t, db, model.FederationStatusOn)
	service := NewService(repository.NewLocalFederationRepository(db), false)

	const callers = 12
	start := make(chan struct{})
	type result struct {
		recorded bool
		reason   string
		host     string
		err      error
	}
	results := make(chan result, callers)
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			reason := fmt.Sprintf("reason-%d", i)
			host := fmt.Sprintf("peer-%d.example", i)
			<-start
			recorded, observeErr := service.ObserveExternalReference(user.ID, reason, host)
			results <- result{recorded: recorded, reason: reason, host: host, err: observeErr}
		}()
	}
	close(start)
	wg.Wait()
	close(results)

	var winner *result
	for got := range results {
		require.NoError(t, got.err)
		if got.recorded {
			assert.Nil(t, winner, "only one concurrent observation may establish the first reference")
			copy := got
			winner = &copy
		}
	}
	require.NotNil(t, winner)

	persisted, err := repository.NewUserRepository(db).FindByID(user.ID)
	require.NoError(t, err)
	require.NotNil(t, persisted.FirstExternalReferenceAt)
	require.NotNil(t, persisted.FirstExternalReferenceReason)
	require.NotNil(t, persisted.FirstExternalReferenceHost)
	assert.Equal(t, winner.reason, *persisted.FirstExternalReferenceReason)
	assert.Equal(t, winner.host, *persisted.FirstExternalReferenceHost)
	assert.True(t, time.Since(*persisted.FirstExternalReferenceAt) < time.Minute)
}
