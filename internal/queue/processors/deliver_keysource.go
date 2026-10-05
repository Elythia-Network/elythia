package processors

import (
	"errors"

	"github.com/elythia-network/elythia/internal/repository"
)

// ErrSigningKeyMissing reports that the signer has no key of the requested kind.
//
// **DB 障害と区別するために要る。** 呼び出し元はこれを `ErrSkipRetry` に包むが、
// それ以外の error (接続断・failover・pool 枯渇) は retry させる。区別しないと、
// **一時的な DB 障害の瞬間に配送中だった job が恒久的に消える** (deliver は
// 既定 12 回・約 32 時間かけて再送する設計なのに、1 回で failed 行きになる)。
// しかも配送が消えたことは相手サーバー側でしか分からない。
var ErrSigningKeyMissing = errors.New("signing key not found")

// repoSigningKeySource resolves signing keys from the database at delivery time.
//
// **これがある理由**: deliver job の payload に署名鍵を載せると、
// `admin/queue/jobs` が job を moderator へ返すので鍵がそこから読める。
// 取得できれば任意のローカルユーザーとして署名付き連合リクエストを偽造できる
// ため、payload には `SignerUserID` だけを載せ、worker が配送時に引く
// (upstream Misskey も配送時に引く設計)。
type repoSigningKeySource struct {
	keypair      repository.UserKeypairRepository
	keypairExtra repository.UserKeypairExtraRepository
}

// NewRepoSigningKeySource builds a SigningKeySource backed by the repositories.
// keypairExtra may be nil (Ed25519 は任意)。
func NewRepoSigningKeySource(kp repository.UserKeypairRepository, extra repository.UserKeypairExtraRepository) SigningKeySource {
	return &repoSigningKeySource{keypair: kp, keypairExtra: extra}
}

func (s *repoSigningKeySource) SigningKeyPEM(userID, kind string) (string, error) {
	if kind == keyKindEd25519 {
		if s.keypairExtra == nil {
			// Ed25519 は任意。未配線は「鍵が無い」で、RSA へ落ちる。
			return "", ErrSigningKeyMissing
		}
		kp, err := s.keypairExtra.FindByUserID(userID)
		if err != nil {
			// **DB 障害を「鍵が無い」にしない** (#2792)。not-found だけを
			// 不在として扱い、それ以外は error のまま返して job を retry させる。
			if repository.IsNotFound(err) {
				return "", ErrSigningKeyMissing
			}
			return "", err
		}
		if kp.Ed25519PrivateKey == "" {
			return "", ErrSigningKeyMissing
		}
		return kp.Ed25519PrivateKey, nil
	}
	if s.keypair == nil {
		// **恒久扱いにしない。** 配線漏れは再起動で直るので、job を捨てるより
		// retry させる (`resolveSigningKey` の keySource==nil と同じ判断)。
		// RSA は必須なので、ここを missing にすると全配送が 1 回で消える。
		return "", errors.New("user keypair repository not configured")
	}
	kp, err := s.keypair.FindByUserID(userID)
	if err != nil {
		// **not-found と DB 障害を分ける。** 前者は retry しても直らないので
		// ErrSkipRetry に包まれてよいが、後者を包むと配送が恒久的に消える。
		if repository.IsNotFound(err) {
			return "", ErrSigningKeyMissing
		}
		return "", err
	}
	if kp.PrivateKey == "" {
		return "", ErrSigningKeyMissing
	}
	return kp.PrivateKey, nil
}
