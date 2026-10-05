package processors

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/queue"
	"github.com/elythia-network/elythia/internal/queue/driver"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/elythia-network/elythia/internal/testutil"
)

// failingKeypairRepo simulates a database outage (not a missing row).
type failingKeypairRepo struct {
	repository.UserKeypairRepository
	err error
}

func (r *failingKeypairRepo) FindByUserID(string) (*model.UserKeypair, error) {
	return nil, r.err
}

func TestRepoSigningKeySource_ReturnsRSAKey(t *testing.T) {
	kp := testutil.NewMockUserKeypairRepository()
	require.NoError(t, kp.Create(&model.UserKeypair{UserID: "u1", PrivateKey: "PEM-RSA"}))

	src := NewRepoSigningKeySource(kp, nil)
	got, err := src.SigningKeyPEM("u1", keyKindRSA)
	require.NoError(t, err)
	require.Equal(t, "PEM-RSA", got)
}

func TestRepoSigningKeySource_MissingRowIsSentinel(t *testing.T) {
	src := NewRepoSigningKeySource(testutil.NewMockUserKeypairRepository(), nil)
	_, err := src.SigningKeyPEM("ghost", keyKindRSA)
	require.ErrorIs(t, err, ErrSigningKeyMissing,
		"鍵が無いことは sentinel で表すこと (DB 障害と区別するため)")
}

// **DB 障害を sentinel にしない。** ここを取り違えると、呼び出し元が
// ErrSkipRetry に包んで配送が恒久的に消える。
func TestRepoSigningKeySource_OutageIsNotSentinel(t *testing.T) {
	outage := errors.New("dial tcp: connection refused")
	src := NewRepoSigningKeySource(&failingKeypairRepo{err: outage}, nil)

	_, err := src.SigningKeyPEM("u1", keyKindRSA)
	require.ErrorIs(t, err, outage)
	require.NotErrorIsf(t, err, ErrSigningKeyMissing,
		"DB 障害が「鍵が無い」に化けている。呼び出し元が job を捨てる")
}

func TestRepoSigningKeySource_Ed25519(t *testing.T) {
	extra := testutil.NewMockUserKeypairExtraRepository()
	require.NoError(t, extra.Upsert(&model.UserKeypairExtra{UserID: "u1", Ed25519PrivateKey: "PEM-ED"}))

	src := NewRepoSigningKeySource(testutil.NewMockUserKeypairRepository(), extra)
	got, err := src.SigningKeyPEM("u1", keyKindEd25519)
	require.NoError(t, err)
	require.Equal(t, "PEM-ED", got)

	// Ed25519 は任意。不在は sentinel で、呼び出し元が RSA へ落とす。
	_, err = src.SigningKeyPEM("ghost", keyKindEd25519)
	require.ErrorIs(t, err, ErrSigningKeyMissing)
}

// **repo 未配線は「鍵が無い」ではない。** 配線漏れは再起動で直るので、
// 恒久扱いにして job を捨てると、再起動しても配送が戻らない。RSA は必須なので
// ここを取り違えると全配送が 1 回で消える。
func TestRepoSigningKeySource_NilRepos(t *testing.T) {
	src := NewRepoSigningKeySource(nil, nil)

	_, err := src.SigningKeyPEM("u1", keyKindRSA)
	require.Error(t, err)
	require.NotErrorIsf(t, err, ErrSigningKeyMissing,
		"RSA repo の配線漏れが恒久扱いになっている。全配送が 1 回で捨てられる")

	// Ed25519 は任意なので、未配線は「鍵が無い」でよい (RSA へ落ちる)。
	_, err = src.SigningKeyPEM("u1", keyKindEd25519)
	require.ErrorIs(t, err, ErrSigningKeyMissing)
}

// **一時的な失敗を ErrSkipRetry に包まない。** これがこの変更で最も壊しやすい
// ところ。包むと、DB 障害の瞬間に配送中だった job が 1 回で failed 行きになる
// (本来は既定 12 回・約 32 時間かけて再送する)。
func TestSendOnce_TransientKeyErrorIsRetryable(t *testing.T) {
	outage := errors.New("dial tcp: connection refused")
	p := NewDeliverProcessor(nil)
	p.SetSigningKeySource(&failingKeypairSource{err: outage})

	_, _, err := p.sendOnce(queue.DeliverPayload{
		Inbox:        "https://remote.example/inbox",
		KeyID:        "https://example.com/users/u1#main-key",
		SignerUserID: "u1",
	}, false)
	require.Error(t, err)
	require.ErrorIs(t, err, outage)
	require.NotErrorIsf(t, err, driver.ErrSkipRetry,
		"一時的な失敗が ErrSkipRetry に包まれている。配送が恒久的に消える")
}

// 逆に、鍵が無いことは retry しても直らないので ErrSkipRetry でよい。
func TestSendOnce_MissingKeyIsPermanent(t *testing.T) {
	p := NewDeliverProcessor(nil)
	p.SetSigningKeySource(&failingKeypairSource{err: ErrSigningKeyMissing})

	_, _, err := p.sendOnce(queue.DeliverPayload{
		Inbox:        "https://remote.example/inbox",
		KeyID:        "https://example.com/users/u1#main-key",
		SignerUserID: "u1",
	}, false)
	require.ErrorIs(t, err, driver.ErrSkipRetry)
}

// 壊れた PEM も retry しても直らない。
func TestSendOnce_UnparsablePEMIsPermanent(t *testing.T) {
	p := NewDeliverProcessor(nil)
	p.SetSigningKeySource(&failingKeypairSource{pem: "not-a-pem"})

	_, _, err := p.sendOnce(queue.DeliverPayload{
		Inbox:        "https://remote.example/inbox",
		KeyID:        "https://example.com/users/u1#main-key",
		SignerUserID: "u1",
	}, false)
	require.ErrorIs(t, err, driver.ErrSkipRetry)
}

// **配線漏れは job を捨てない。** 再起動で直るので retry させる。
func TestSendOnce_UnwiredSourceIsRetryable(t *testing.T) {
	p := NewDeliverProcessor(nil)
	_, _, err := p.sendOnce(queue.DeliverPayload{
		Inbox:        "https://remote.example/inbox",
		KeyID:        "https://example.com/users/u1#main-key",
		SignerUserID: "u1",
	}, false)
	require.Error(t, err)
	require.NotErrorIsf(t, err, driver.ErrSkipRetry,
		"配線漏れで job を捨てると、再起動しても配送が戻らない")
}

// failingKeypairSource returns a fixed error or PEM for every lookup.
type failingKeypairSource struct {
	err error
	pem string
}

func (s *failingKeypairSource) SigningKeyPEM(string, string) (string, error) {
	if s.err != nil {
		return "", s.err
	}
	return s.pem, nil
}
