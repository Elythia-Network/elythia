package federation_test

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/core/federation"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// hookedPublickeyExtraRepo runs during inside the first ListByUserID, i.e.
// while the capability is being read, and can hold that read until release
// is closed.
type hookedPublickeyExtraRepo struct {
	stubPublickeyExtraRepo
	calls   atomic.Int32
	during  func()
	entered chan struct{}
	release chan struct{}
}

func (r *hookedPublickeyExtraRepo) ListByUserID(userID string) ([]model.UserPublickeyExtra, error) {
	n := r.calls.Add(1)
	rows, err := r.stubPublickeyExtraRepo.ListByUserID(userID)
	if n == 1 {
		if r.entered != nil {
			close(r.entered)
		}
		if r.release != nil {
			<-r.release
		}
		if r.during != nil {
			r.during()
		}
	}
	return rows, err
}

// hookedKeypairExtraRepo is hookedPublickeyExtraRepo for the signer keys: it
// runs during inside the first FindByUserID and can hold that read until
// release is closed.
type hookedKeypairExtraRepo struct {
	stubKeypairExtraRepo
	calls   atomic.Int32
	during  func()
	entered chan struct{}
	release chan struct{}
}

func (r *hookedKeypairExtraRepo) FindByUserID(userID string) (*model.UserKeypairExtra, error) {
	n := r.calls.Add(1)
	row, err := r.stubKeypairExtraRepo.FindByUserID(userID)
	if n == 1 {
		if r.entered != nil {
			close(r.entered)
		}
		if r.release != nil {
			<-r.release
		}
		if r.during != nil {
			r.during()
		}
	}
	return row, err
}

// deliverDropFixture is a DeliverService with a local signer "alice" whose
// Ed25519 key comes from a counting repo, and a remote recipient "bob".
func deliverDropFixture(t *testing.T, pkExtra *hookedPublickeyExtraRepo, kp *hookedKeypairExtraRepo) (svc *federation.DeliverService, deliver func() error) {
	t.Helper()
	svc, _, userRepo, _, keypairRepo := newDeliverService(t)
	installLocalSigner(t, userRepo, keypairRepo)
	kp.rows = map[string]*model.UserKeypairExtra{
		"alice": {UserID: "alice", Ed25519PublicKey: "P", Ed25519PrivateKey: "K"},
	}
	svc.SetKeypairExtraRepo(kp)
	require.NoError(t, pkExtra.Upsert(&model.UserPublickeyExtra{UserID: "bob", KeyID: "k", KeyPEM: "P", Alg: model.AlgEd25519}))
	svc.SetPublickeyExtraRepo(pkExtra)
	host := "remote.example"
	bob := &model.User{ID: "bob", Username: "bob", Host: &host, Inbox: strPtr("https://remote.example/users/bob/inbox")}
	return svc, func() error { return svc.DeliverToUser("alice", bob, []byte(`{}`)) }
}

// TestDeliverService_DropLocal pins that DropLocal drops both the recipient
// capabilities and the signer keys of this process (#3732).
func TestDeliverService_DropLocal(t *testing.T) {
	pkExtra, kp := &hookedPublickeyExtraRepo{}, &hookedKeypairExtraRepo{}
	svc, deliver := deliverDropFixture(t, pkExtra, kp)
	require.NoError(t, deliver())
	require.NoError(t, deliver())
	require.Equal(t, int32(1), pkExtra.calls.Load(), "前提: 対応の有無は cache から返る")
	require.Equal(t, int32(1), kp.calls.Load(), "前提: 署名者の鍵は cache から返る")

	svc.DropLocal()
	require.NoError(t, deliver())
	assert.Equal(t, int32(2), pkExtra.calls.Load(), "DropLocal did not drop the capabilities")
	assert.Equal(t, int32(2), kp.calls.Load(), "DropLocal did not drop the signer keys")
}

// TestDeliverService_DropLocalDuringReadIsNotCached pins, for the capability
// and the signer key, that a value read while DropLocal ran is not stored
// (#3732).
func TestDeliverService_DropLocalDuringReadIsNotCached(t *testing.T) {
	// signer-missing は署名者に Ed25519 の鍵が無い場合 (空の entry を積む経路)。
	for _, path := range []string{"capability", "signer", "signer-missing"} {
		t.Run(path, func(t *testing.T) {
			pkExtra, kp := &hookedPublickeyExtraRepo{}, &hookedKeypairExtraRepo{}
			svc, deliver := deliverDropFixture(t, pkExtra, kp)
			calls := &pkExtra.calls
			switch path {
			case "capability":
				pkExtra.during = svc.DropLocal
			case "signer-missing":
				delete(kp.rows, "alice")
				fallthrough
			default:
				kp.during = svc.DropLocal
				calls = &kp.calls
			}

			require.NoError(t, deliver())
			require.NoError(t, deliver())
			assert.Equal(t, int32(2), calls.Load(), "the value read during DropLocal was cached")
			require.NoError(t, deliver())
			assert.Equal(t, int32(2), calls.Load(), "a later read is cached again")
		})
	}
}

// TestDeliverService_DropLocalDoesNotJoinAnOlderRead pins, for the capability
// and the signer key, that a call made after DropLocal does not join a read
// that started before it (#3732): the singleflight key carries the generation.
func TestDeliverService_DropLocalDoesNotJoinAnOlderRead(t *testing.T) {
	for _, path := range []string{"capability", "signer"} {
		t.Run(path, func(t *testing.T) {
			pkExtra, kp := &hookedPublickeyExtraRepo{}, &hookedKeypairExtraRepo{}
			entered, release := make(chan struct{}), make(chan struct{})
			calls := &pkExtra.calls
			if path == "capability" {
				pkExtra.entered, pkExtra.release = entered, release
			} else {
				kp.entered, kp.release = entered, release
				calls = &kp.calls
			}
			svc, deliver := deliverDropFixture(t, pkExtra, kp)

			var once sync.Once
			releaseOnce := func() { once.Do(func() { close(release) }) }
			// 途中で落ちても、止めている読み込みを放してから抜ける。
			t.Cleanup(releaseOnce)
			first := make(chan error, 1)
			go func() { first <- deliver() }()
			<-entered
			svc.DropLocal()

			second := make(chan error, 1)
			go func() { second <- deliver() }()
			select {
			case err := <-second:
				require.NoError(t, err)
			case <-time.After(5 * time.Second):
				releaseOnce()
				<-first
				t.Fatal("the call after DropLocal joined the read that started before it")
			}
			assert.Equal(t, int32(2), calls.Load(), "the call after DropLocal did not read again")
			releaseOnce()
			require.NoError(t, <-first)
		})
	}
}
