package federation_test

import (
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/activitypub"
	"github.com/elythia-network/elythia/internal/core/federation"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/queue"
	"github.com/elythia-network/elythia/internal/testutil"
)

// countingKeypairRepo counts FindByUserID calls and can fail them.
type countingKeypairRepo struct {
	*testutil.MockUserKeypairRepository
	finds atomic.Int32
	fail  error
}

func (c *countingKeypairRepo) FindByUserID(userID string) (*model.UserKeypair, error) {
	c.finds.Add(1)
	if c.fail != nil {
		return nil, c.fail
	}
	return c.MockUserKeypairRepository.FindByUserID(userID)
}

// newSignerKeyCacheService returns a service whose deliveries run through a
// sync hook that records the RSA key each one was signed with.
func newSignerKeyCacheService(t *testing.T) (*federation.DeliverService, *countingKeypairRepo, *[]string) {
	t.Helper()
	userRepo := testutil.NewMockUserRepository()
	userRepo.Users["alice"] = &model.User{ID: "alice", Username: "alice"}
	kp := &countingKeypairRepo{MockUserKeypairRepository: testutil.NewMockUserKeypairRepository()}
	svc := federation.NewDeliverService(&stubEnqueuer{}, userRepo, testutil.NewMockFollowingRepository(), kp,
		activitypub.NewURLBuilder("https://example.com"), nil)
	var keys []string
	svc.SetSyncDeliverHookForTest(func(p queue.DeliverPayload) error {
		keys = append(keys, p.KeyPEM)
		return nil
	})
	return svc, kp, &keys
}

func deliverOnce(svc *federation.DeliverService) error {
	return svc.DeliverActivity("alice", []byte(`{}`), []string{"https://remote.example/inbox"})
}

// TestDeliverActivity_SignerKeyCachedAcrossActivities pins #4221: the signer's
// RSA key is read from the DB once, not once per activity, until the TTL
// passes or DropLocal runs.
func TestDeliverActivity_SignerKeyCachedAcrossActivities(t *testing.T) {
	svc, kp, keys := newSignerKeyCacheService(t)
	now := time.Unix(1_700_000_000, 0)
	svc.SetClock(func() time.Time { return now })
	kp.Keypairs["alice"] = &model.UserKeypair{UserID: "alice", PrivateKey: "PEM-1"}

	for range 3 {
		require.NoError(t, deliverOnce(svc))
	}
	assert.Equal(t, int32(1), kp.finds.Load(), "the key is read once for three activities")

	// 鍵が DB で変わっても、捨てるまでは持っている方で署名する。
	kp.Keypairs["alice"] = &model.UserKeypair{UserID: "alice", PrivateKey: "PEM-2"}
	require.NoError(t, deliverOnce(svc))
	svc.DropLocal()
	require.NoError(t, deliverOnce(svc))
	assert.Equal(t, int32(2), kp.finds.Load(), "DropLocal makes the next activity read the key again")

	kp.Keypairs["alice"] = &model.UserKeypair{UserID: "alice", PrivateKey: "PEM-3"}
	now = now.Add(5*time.Minute + time.Second)
	require.NoError(t, deliverOnce(svc))
	assert.Equal(t, int32(3), kp.finds.Load(), "an entry past the TTL is read again")

	assert.Equal(t, []string{"PEM-1", "PEM-1", "PEM-1", "PEM-1", "PEM-2", "PEM-3"}, *keys)
}

// TestDeliverActivity_SignerKeyMissIsNotCached pins that a missing key and a
// failed read are not remembered: the next activity asks the DB again.
func TestDeliverActivity_SignerKeyMissIsNotCached(t *testing.T) {
	svc, kp, keys := newSignerKeyCacheService(t)

	require.ErrorIs(t, deliverOnce(svc), federation.ErrSignerKeyMissing)

	kp.fail = errors.New("db down")
	err := deliverOnce(svc)
	require.Error(t, err)
	assert.NotErrorIs(t, err, federation.ErrSignerKeyMissing, "a failed read is not reported as a missing key")

	kp.fail = nil
	kp.Keypairs["alice"] = &model.UserKeypair{UserID: "alice", PrivateKey: "PEM-1"}
	require.NoError(t, deliverOnce(svc))
	assert.Equal(t, int32(3), kp.finds.Load())
	assert.Equal(t, []string{"PEM-1"}, *keys)
}

// TestDeliverActivity_JobsCarryBodyDigest pins #4221: every job of one
// activity carries the Digest of its body, so the worker does not hash it.
func TestDeliverActivity_JobsCarryBodyDigest(t *testing.T) {
	svc, enq, userRepo, _, keypairRepo := newDeliverService(t)
	installLocalSigner(t, userRepo, keypairRepo)
	body := []byte(`{"type":"Create","id":"https://example.com/notes/1"}`)

	require.NoError(t, svc.DeliverActivity("alice", body, []string{"https://a.example/inbox", "https://b.example/inbox"}))
	require.Len(t, enq.calls, 2)
	for _, p := range enq.calls {
		assert.Equal(t, activitypub.SHA256Digest(body), p.Digest, p.Inbox)
	}
}
