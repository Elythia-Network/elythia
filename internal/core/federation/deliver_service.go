package federation

import (
	"errors"
	"fmt"
	"net/url"
	"sync"
	"time"

	"github.com/shiroha-a/mk/internal/activitypub"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/queue"
	"github.com/shiroha-a/mk/internal/queue/driver"
	"github.com/shiroha-a/mk/internal/repository"
	"golang.org/x/sync/singleflight"
)

// Errors returned by DeliverService.
var (
	// ErrSignerKeyMissing is returned when no keypair is found for the signer.
	ErrSignerKeyMissing = errors.New("signer keypair not found")
	// ErrNoLocalSigner is returned when DeliverService is asked to sign with a
	// remote user. リモートユーザーの代理署名は不可能。
	ErrNoLocalSigner = errors.New("cannot sign on behalf of a remote user")
)

// HostBlockChecker reports whether a host is on the blocked list and whether
// the local instance is willing to federate with it (allowlist + blocklist
// combined). federation package が core/instance に直接依存しないよう
// minimal interface だけ持つ (#536)。
type HostBlockChecker interface {
	IsBlocked(host string) bool
	// IsAllowed returns true when federation with host is permitted under
	// the current admin federation mode (none / specified / all) and the
	// host is not in blockedHosts.
	IsAllowed(host string) bool
}

// ed25519CacheTTL is how long recipient capability / sender Ed25519 keypair
// lookups stay in the in-memory cache. 5 分は actor refresh の actorTTL より
// 短く、Ed25519 鍵 rotation を検出する時間としては十分。capabilityCache に
// 入ったあとは同 recipient への deliver で DB を叩かない (#1080 review #2/#3)。
const ed25519CacheTTL = 5 * time.Minute

type ed25519CapEntry struct {
	capable   bool
	fetchedAt time.Time
}

type ed25519SignerEntry struct {
	keyID     string
	privPEM   string
	fetchedAt time.Time
}

// DeliverService computes recipient inboxes and enqueues HTTP-signed delivery
// jobs onto the deliver queue.
//
// 配信先計算と enqueue を分離するため、実際のHTTP送信は queue/processors の
// DeliverProcessor が担当する。
type DeliverService struct {
	enqueuer           queue.Enqueuer
	userRepo           repository.UserRepository
	followingRepo      repository.FollowingRepository
	keypairRepo        repository.UserKeypairRepository
	keypairExtraRepo   repository.UserKeypairExtraRepository   // optional: sender Ed25519 鍵
	publickeyExtraRepo repository.UserPublickeyExtraRepository // optional: recipient capability 判定
	urls               *activitypub.URLBuilder
	hostBlocker        HostBlockChecker
	// syncDeliverHook, when non-nil, replaces the queue enqueue with an
	// inline call to the hook. test 専用で federation deliver の queue 経路
	// を bypass し、sign + HTTP POST を同期実行する e2e_federation 用。
	// production code から SetSyncDeliverHook を呼ばないこと (#780)。
	syncDeliverHook func(payload queue.DeliverPayload) error
	// capCache / signerCache は recipient capability / sender Ed25519 鍵の
	// 短期 in-memory cache (#1080 review #2/#3)。多 deliver で DB を叩く
	// N+1 を抑えるため、TTL 5min で lookup を memoize する。actor 削除 /
	// 鍵 rotation 後は TTL 経過で自動的に refresh される。
	capCache    sync.Map         // string (userID) -> ed25519CapEntry
	signerCache sync.Map         // string (userID) -> ed25519SignerEntry
	clock       func() time.Time // テストで差し替え可能
	// capGroup / signerGroup は cache miss 時の thundering herd を collapse
	// する singleflight (#1080 review #3 follow-up)。並行 deliver の同 userID
	// に対する DB lookup を 1 回に集約。resolver の resolveActorGroup と同 pattern。
	capGroup    singleflight.Group
	signerGroup singleflight.Group
}

// NewDeliverService constructs a DeliverService.
func NewDeliverService(
	enqueuer queue.Enqueuer,
	userRepo repository.UserRepository,
	followingRepo repository.FollowingRepository,
	keypairRepo repository.UserKeypairRepository,
	urls *activitypub.URLBuilder,
) *DeliverService {
	return &DeliverService{
		enqueuer:      enqueuer,
		userRepo:      userRepo,
		followingRepo: followingRepo,
		keypairRepo:   keypairRepo,
		urls:          urls,
		clock:         time.Now,
	}
}

// SetHostBlockChecker attaches a HostBlockChecker. 配信時にこの checker が
// 呼ばれ、ブロック対象ホストの inbox にはエンキューしない。
func (s *DeliverService) SetHostBlockChecker(c HostBlockChecker) {
	s.hostBlocker = c
}

// SetClock replaces the clock used by the capability / signer cache TTL
// expiry checks. Intended for tests that need to deterministically advance
// time past the 5-minute TTL without sleeping.
func (s *DeliverService) SetClock(now func() time.Time) {
	if now != nil {
		s.clock = now
	}
}

// SetKeypairExtraRepo wires the Ed25519 keypair repository so sender が
// Ed25519 鍵を持つ場合に DeliverActivityWithRecipient 経路で Ed25519 sign
// 情報を payload に詰めることができる。未配線なら全配送 RSA fallback
// (= 旧 deployment と完全互換 / drop-in 維持) (#1067 / #1071)。
func (s *DeliverService) SetKeypairExtraRepo(r repository.UserKeypairExtraRepository) {
	s.keypairExtraRepo = r
}

// SetPublickeyExtraRepo wires the remote Multikey repository so recipient の
// Ed25519 capability を user_publickey_extra で判定できる。未配線なら全配送
// RSA fallback (#1067 / #1071)。
func (s *DeliverService) SetPublickeyExtraRepo(r repository.UserPublickeyExtraRepository) {
	s.publickeyExtraRepo = r
}

// SetSyncDeliverHookForTest replaces the queue enqueue with an inline
// synchronous call. Used by e2e_federation tests to bypass the queue layer
// and exercise the sign + POST + inbox handling path directly. Not for
// production use (#780).
//
// fn=nil restores the default (queue) path.
func (s *DeliverService) SetSyncDeliverHookForTest(fn func(payload queue.DeliverPayload) error) {
	s.syncDeliverHook = fn
}

// DeliverActivity enqueues a deliver job for each unique inbox in inboxes.
// signerUserID は署名に使うローカルユーザー。ブロック対象ホストの inbox は
// スキップする。受信者の actor 情報を caller が持たない経路 (= followers /
// inbox list ベース) は RSA only で配送される。受信者 actor URI が分かる
// 場合は DeliverActivityWithRecipient を使うと FEP-521a Multikey 対応サーバー
// 向けに Ed25519 sign が試行される (#1067 / #1071)。
func (s *DeliverService) DeliverActivity(signerUserID string, body []byte, inboxes []string) error {
	return s.deliverInternal(signerUserID, body, inboxes, nil, nil, time.Time{})
}

// DeliverActivityWithRecipient is DeliverActivity + recipient capability based
// Ed25519 sign 試行。recipient が user_publickey_extra に Ed25519 鍵を持ち、
// sender が user_keypair_extra に Ed25519 鍵を持つ場合、payload に Ed25519
// 鍵情報も詰める (DeliverProcessor 側で実際の sign 分岐 + degrade safeguard
// を担う)。recipient == nil なら DeliverActivity と等価動作 (#1067 / #1071)。
func (s *DeliverService) DeliverActivityWithRecipient(signerUserID string, body []byte, inboxes []string, recipient *model.User) error {
	return s.deliverInternal(signerUserID, body, inboxes, recipient, nil, time.Time{})
}

// deliverOnceDeadline is how long a DeliverToUserOnce job may wait before it
// is dropped instead of sent (#3238)。配送の queue で待つ時間も含むので、混んで
// いる間の分を見て少し長めにする。人が取り消すまでの時間より十分に短ければよい
// (保留中に重なって届いても害は無い)。
const deliverOnceDeadline = 5 * time.Minute

// DeliverToUserOnce is DeliverToUser without the deliver queue's retries and
// with a deadline (#3238)。届かなければ捨て、積んでから deliverOnceDeadline を
// 過ぎたら送らない。再送の判断を呼び出し側 (状態を確かめてから送る
// 定期処理) に任せるものに使う — 配送の queue は積んだ後に状態を見ないので、
// 何時間も後の再試行が、その間に変わった状態を上書きしうる。
func (s *DeliverService) DeliverToUserOnce(signerUserID string, recipient *model.User, body []byte) error {
	if recipient == nil || recipient.IsLocal() {
		return nil
	}
	inbox := preferredInbox(recipient)
	if inbox == "" {
		return nil
	}
	// 1 回きりでも、ブレーカーや流量の制限で後へ回されると (試行を消費しないので)
	// 何時間も後に届きうる。期限を付けて、過ぎたら送らない。
	return s.deliverInternal(signerUserID, body, []string{inbox}, recipient, nil, s.clock().Add(deliverOnceDeadline), driver.WithMaxRetry(0))
}

// deliverInternal enqueues one signed delivery per unique inbox. sharedInboxes
// (optional) marks which inbox URLs are shared inboxes so the processor can
// goneSuspend the instance on a 410 (#1811)。recipient の sharedInbox に一致する
// inbox も shared 扱いにする。
func (s *DeliverService) deliverInternal(signerUserID string, body []byte, inboxes []string, recipient *model.User, sharedInboxes map[string]bool, notAfter time.Time, opts ...driver.EnqueueOption) error {
	if len(inboxes) == 0 {
		return nil
	}
	keyID, keyPEM, err := s.signerCredentials(signerUserID)
	if err != nil {
		return err
	}
	// Ed25519 sign 経路を試行できるのは recipient が Ed25519 capable
	// (assertionMethod を持つ) + sender が Ed25519 keypair を持つ両方が
	// 真のときのみ。どちらかが false なら edKeyID/edPrivPEM は空のまま payload
	// に詰められ、DeliverProcessor は従来通り RSA で sign する。
	var edKeyID, edPrivPEM string
	if recipient != nil && s.recipientIsEd25519Capable(recipient) {
		edKeyID, edPrivPEM = s.signerEd25519Credentials(signerUserID)
	}
	seen := make(map[string]struct{}, len(inboxes))
	for _, inbox := range inboxes {
		if inbox == "" {
			continue
		}
		if _, dup := seen[inbox]; dup {
			continue
		}
		if s.isBlockedInbox(inbox) {
			continue
		}
		seen[inbox] = struct{}{}
		isShared := sharedInboxes[inbox] ||
			(recipient != nil && recipient.SharedInbox != nil && *recipient.SharedInbox != "" && inbox == *recipient.SharedInbox)
		// **署名鍵は payload に詰めない。** queue の job は
		// `admin/queue/jobs` が moderator へ返すので、詰めると鍵がそこから
		// 読める。worker は `SignerUserID` から配送時に引く。
		//
		// `keyID` / `edKeyID` は署名ヘッダに載る公開の識別子なので残す。
		// 鍵の存在確認は上の `signerCredentials` が enqueue 前に済ませて
		// いるので、鍵の無いユーザーの配送はここへ来ない。
		payload := queue.DeliverPayload{
			Inbox:         inbox,
			Body:          body,
			KeyID:         keyID,
			Ed25519KeyID:  edKeyID,
			IsSharedInbox: isShared,
			SignerUserID:  signerUserID,
		}
		if !notAfter.IsZero() {
			payload.NotAfter = notAfter.UnixMilli()
		}
		if s.syncDeliverHook != nil {
			// test 経路 (#780): queue を経由せず inline で sign + POST。
			// **こちらにだけ鍵を詰める** — queue を通らないので Redis にも
			// admin API にも出ない。hook 側が DB を引く必要も無くなる。
			inline := payload
			inline.KeyPEM = keyPEM
			inline.Ed25519PrivPEM = edPrivPEM
			if err := s.syncDeliverHook(inline); err != nil {
				return fmt.Errorf("sync deliver to %s: %w", inbox, err)
			}
			continue
		}
		if err := s.enqueuer.EnqueueDeliver(payload, opts...); err != nil {
			return fmt.Errorf("enqueue deliver to %s: %w", inbox, err)
		}
	}
	return nil
}

// recipientIsEd25519Capable reports whether the recipient actor has at least
// one Ed25519 entry in user_publickey_extra (= 受信側が FEP-521a Multikey で
// Ed25519 を expose している)。未配線時 / DB error / 行なしのいずれも false
// (= 安全側で RSA fallback)。capCache で TTL 5min 結果を memoize し、cache
// miss 時の thundering herd は singleflight で collapse する (#1080 review
// #2 / #3 follow-up)。
func (s *DeliverService) recipientIsEd25519Capable(recipient *model.User) bool {
	if s.publickeyExtraRepo == nil || recipient == nil {
		return false
	}
	if cached, ok := s.capCache.Load(recipient.ID); ok {
		entry := cached.(ed25519CapEntry)
		if s.clock().Sub(entry.fetchedAt) <= ed25519CacheTTL {
			return entry.capable
		}
	}
	val, _, _ := s.capGroup.Do(recipient.ID, func() (any, error) {
		rows, err := s.publickeyExtraRepo.ListByUserID(recipient.ID)
		if err != nil {
			// DB error は cache せず次回 retry させる (= 戻り値の bool 経由で
			// caller に伝播、cache には書き込まない)
			return false, nil
		}
		capable := len(rows) > 0
		s.capCache.Store(recipient.ID, ed25519CapEntry{
			capable:   capable,
			fetchedAt: s.clock(),
		})
		return capable, nil
	})
	return val.(bool)
}

// signerEd25519Credentials returns (keyId, PEM) for the local signer's
// Ed25519 keypair. 未配線時 / 鍵未発行 (= P5 backfill 前の旧 user) のいずれも
// 空文字列を返し、caller 側で RSA only にフォールバックする。signerCache で
// TTL 5min 結果を memoize し、cache miss 時の thundering herd は singleflight
// で collapse する (#1080 review #2 / #3 follow-up)。
func (s *DeliverService) signerEd25519Credentials(userID string) (string, string) {
	if s.keypairExtraRepo == nil {
		return "", ""
	}
	if cached, ok := s.signerCache.Load(userID); ok {
		entry := cached.(ed25519SignerEntry)
		if s.clock().Sub(entry.fetchedAt) <= ed25519CacheTTL {
			return entry.keyID, entry.privPEM
		}
	}
	val, _, _ := s.signerGroup.Do(userID, func() (any, error) {
		kp, err := s.keypairExtraRepo.FindByUserID(userID)
		if err != nil {
			// 鍵未発行は cache に空 entry を入れて再 query を抑える
			entry := ed25519SignerEntry{fetchedAt: s.clock()}
			s.signerCache.Store(userID, entry)
			return entry, nil
		}
		entry := ed25519SignerEntry{
			keyID:     s.urls.UserURI(userID) + "#ed25519-key",
			privPEM:   kp.Ed25519PrivateKey,
			fetchedAt: s.clock(),
		}
		s.signerCache.Store(userID, entry)
		return entry, nil
	})
	entry := val.(ed25519SignerEntry)
	return entry.keyID, entry.privPEM
}

// isBlockedInbox returns true when the inbox URL points at a host the local
// instance refuses to federate with — either because it is blocked or the
// federation mode (none / specified) excludes it (#536). blocker が未設定なら
// 常に false。
func (s *DeliverService) isBlockedInbox(inbox string) bool {
	if s.hostBlocker == nil {
		return false
	}
	// **既定ポートを剥がした形で判定する。** 生の `u.Host` を渡していた頃は
	// `https://blocked.example:443/inbox` が `blocked.example:443` として
	// `HostMatchesAny` の suffix 一致から外れ、defederation した相手への配送が
	// 続いていた。取り込み側 (`hostFromURI`) と同じ `punyHostPort` を通す。
	u, err := url.Parse(inbox)
	if err != nil || u.Host == "" {
		return false
	}
	host := punyHostPort(u)
	if s.hostBlocker.IsBlocked(host) {
		return true
	}
	return !s.hostBlocker.IsAllowed(host)
}

// DeliverToFollowers enqueues delivery to all remote followers of signerUserID.
// sharedInbox は repository 側で集約済み。
func (s *DeliverService) DeliverToFollowers(signerUserID string, body []byte) error {
	return s.DeliverToFollowersExcluding(signerUserID, body, nil)
}

// DeliverToFollowersExcluding enqueues delivery to the remote followers of
// signerUserID, skipping any inbox URL present in exclude. 除外は inbox URL の
// 完全一致で行い、exclude が nil / 空なら DeliverToFollowers と等価
// (#2567)。同一アクティビティを同一 inbox に重複 POST しないため、先に
// 別バッチ (DirectRecipe 等) で送った inbox をここで省く用途で使う。
func (s *DeliverService) DeliverToFollowersExcluding(signerUserID string, body []byte, exclude map[string]bool) error {
	rows, err := s.followingRepo.ListRemoteFollowerInboxes(signerUserID)
	if err != nil {
		return err
	}
	inboxes := make([]string, 0, len(rows))
	var sharedInboxes map[string]bool
	for _, row := range rows {
		if exclude[row.Inbox] {
			continue
		}
		inboxes = append(inboxes, row.Inbox)
		if row.Shared {
			if sharedInboxes == nil {
				sharedInboxes = make(map[string]bool)
			}
			sharedInboxes[row.Inbox] = true
		}
	}
	return s.deliverInternal(signerUserID, body, inboxes, nil, sharedInboxes, time.Time{})
}

// DeliverToInboxes enqueues delivery to an explicit set of remote inboxes,
// threading the shared-inbox flag (#2575).
//
// **フォロワー配送と同じ型を受け取るのが要点。** inbox URL の羅列を渡す形だと
// shared かどうかが落ち、410 Gone でインスタンス全体を suspend する判定
// (#1811) がこの経路だけ効かなくなる。
func (s *DeliverService) DeliverToInboxes(signerUserID string, body []byte, rows []model.RemoteInbox) error {
	inboxes := make([]string, 0, len(rows))
	var sharedInboxes map[string]bool
	for _, row := range rows {
		inboxes = append(inboxes, row.Inbox)
		if row.Shared {
			if sharedInboxes == nil {
				sharedInboxes = make(map[string]bool)
			}
			sharedInboxes[row.Inbox] = true
		}
	}
	return s.deliverInternal(signerUserID, body, inboxes, nil, sharedInboxes, time.Time{})
}

// DeliverToUser enqueues a delivery to a single recipient user. Local users
// are skipped (no AP delivery needed). リモートユーザーの sharedInbox があれば
// 優先する。recipient actor が既知なので FEP-521a Multikey による Ed25519
// sign を試行する経路 (DeliverActivityWithRecipient) を使う (#1067 / #1071)。
func (s *DeliverService) DeliverToUser(signerUserID string, recipient *model.User, body []byte) error {
	if recipient == nil || recipient.IsLocal() {
		return nil
	}
	inbox := preferredInbox(recipient)
	if inbox == "" {
		return nil
	}
	return s.DeliverActivityWithRecipient(signerUserID, body, []string{inbox}, recipient)
}

// signerCredentials returns the keyId URI and PEM private key for a local
// signer user.
func (s *DeliverService) signerCredentials(userID string) (string, string, error) {
	user, err := s.userRepo.FindByID(userID)
	if err != nil {
		return "", "", err
	}
	if !user.IsLocal() {
		return "", "", ErrNoLocalSigner
	}
	kp, err := s.keypairRepo.FindByUserID(userID)
	if err != nil {
		// **DB 障害を「鍵が無い」にしない** (#2799)。この経路は enqueue の
		// 前 (producer 側) なので job のリトライ判定は変わらないが、呼び出し元は
		// これを `slog.Warn` で握り潰すため、**接続断が「署名鍵が無い」として
		// ログに残る**。原因から遠い症状になるので種別は保つ。
		if !repository.IsNotFound(err) {
			return "", "", err
		}
		return "", "", ErrSignerKeyMissing
	}
	keyID := s.urls.UserKeyURI(userID)
	return keyID, kp.PrivateKey, nil
}

// fanoutInbox returns the inbox URL that the FOLLOWER fan-out would use for u.
//
// **フォロワー配信と重ねる経路はこちらを使う。** `ListRemoteFollowerInboxes` は
// 「sharedInbox が空なら inbox」を返すので、direct 側が個別 inbox を
// 使うと `DeliverToFollowersExcluding` の exclude (inbox URL の完全一致) が
// 効かず、**同じ activity が同じインスタンスへ 2 通届く** (#2567 / #2575 が
// 塞いだ形)。
//
// **exclude に両方入れる形では駄目。** sharedInbox はそのインスタンスの
// 全フォロワーを表す 1 エントリなので、そこを除外すると**同じインスタンスの
// 他のフォロワー全員に届かなくなる**。
//
// 1:1 だけで完結する経路 (`DeliverToUser`、specified なアンケートの
// Update(Question)) は `preferredInbox` を使う — あちらは exclude と揃える
// 必要が無く、個別 inbox のほうが確実に届く (`preferredInbox` の GoDoc 参照)。
func fanoutInbox(u *model.User) string {
	if u.SharedInbox != nil && *u.SharedInbox != "" {
		return *u.SharedInbox
	}
	if u.Inbox != nil {
		return *u.Inbox
	}
	return ""
}

// preferredInbox returns the inbox to use for a 1:1 (direct) delivery.
//
// **個別 inbox を優先する。** upstream の `ApDeliverManagerService.execute` は
// direct recipe に対して `inboxes.set(recipe.to.inbox, false)` と**必ず個別
// inbox** を使い、`sharedInbox` はフォロワー配信で既に積んだ inbox との重複
// 判定にしか使わない。sharedInbox へ送ると 2 つ壊れる:
//
//   - shared inbox で非公開 activity を扱わない実装では、DM や Follow が
//     **黙って落ちる** (エラーも返らない)
//   - 410 Gone が返ると `IsSharedInbox` 経由で host 単位の gone 判定
//     (`MarkGoneSuspended`) に届き、**DM 1 通の失敗でインスタンス全体を
//     suspend** しうる
//
// 個別 inbox を持たない行だけ sharedInbox へ倒す。upstream はその場合
// 配送を skip するが、送れるなら送る方が利用者の意図に近い。
func preferredInbox(u *model.User) string {
	if u.Inbox != nil && *u.Inbox != "" {
		return *u.Inbox
	}
	if u.SharedInbox != nil {
		return *u.SharedInbox
	}
	return ""
}

// HasHostBlockChecker reports whether the host block checker was wired.
//
// 未配線だと `isBlockedInbox` が常に false になり、ブロック済み host へも
// 配送する (outbound の defederation が効かない)。起動時検査に使う (#2683)。
func (s *DeliverService) HasHostBlockChecker() bool { return s.hostBlocker != nil }
