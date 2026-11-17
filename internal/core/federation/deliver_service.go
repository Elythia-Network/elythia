package federation

import (
	"errors"
	"fmt"
	"net/url"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/elythia-network/elythia/internal/activitypub"
	corelocalfederation "github.com/elythia-network/elythia/internal/core/localfederation"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/queue"
	"github.com/elythia-network/elythia/internal/queue/driver"
	"github.com/elythia-network/elythia/internal/repository"
	"golang.org/x/sync/singleflight"
)

// Errors returned by DeliverService.
var (
	// ErrSignerKeyMissing is returned when no keypair is found for the signer.
	ErrSignerKeyMissing = errors.New("signer keypair not found")
	// ErrNoLocalSigner is returned when DeliverService is asked to sign with a
	// remote user. リモートユーザーの代理署名は不可能。
	ErrNoLocalSigner = errors.New("cannot sign on behalf of a remote user")
	// ErrFederationDisabled is returned when a local-only or pending account
	// would otherwise be used as an external delivery signer.
	ErrFederationDisabled = corelocalfederation.ErrFederationDisabled
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

// rsaSignerEntry is a local signer's RSA keypair, cached only once found.
type rsaSignerEntry struct {
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
	// hostBlocker は配送先の host を federation 設定 (none / specified /
	// blockedHosts) で弾く判定。配信時に呼ばれ、ブロック対象ホストの inbox には
	// エンキューしない。**nil だと `isBlockedInbox` が常に false になり、ブロック済み
	// host へも配送する (outbound の defederation が効かない)。** 構築子の必須の
	// 引数 (#3833。以前は setter と起動時の検査、#2683)。テストは nil を渡せる。
	hostBlocker             HostBlockChecker
	localFederationObserver activitypub.LocalFederationObserver
	noteRepo                repository.NoteRepository
	localReferenceCache     localReferenceCache
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
	// rsaSignerCache は署名者の RSA 鍵 (#4221)。activity ごとに鍵の有無を DB へ
	// 聞いていた。見つかったものだけを持つ。queue の経路は値を使わず (worker が
	// SignerUserID から引き直す)、有無の確認にだけ使う。連合を Off から On に戻すと
	// 鍵は作り直されるが、Off の間の配送は preflightSigner が先に止める。
	// rsaSignerGroup は signerGroup と分ける。同じ group で鍵を取り違えると、
	// 戻り値の型が合わずに panic する。
	rsaSignerCache sync.Map // string (userID) -> rsaSignerEntry
	rsaSignerGroup singleflight.Group
	// cacheMu / cacheGen は DropLocal (#3732) のための世代。DropLocal は番号を進めてから
	// cache を消し、読み込みの結果は読む前に控えた番号と一致するときだけ積む。
	// singleflight の鍵にも番号を含め、捨てた後の呼び出しが捨てる前に始まった
	// 読み込みに相乗りしないようにする。
	cacheMu  sync.Mutex
	cacheGen uint64
}

func (s *DeliverService) SetLocalFederationObserver(observer activitypub.LocalFederationObserver) {
	s.localFederationObserver = observer
}

func (s *DeliverService) SetNoteRepo(repo repository.NoteRepository) { s.noteRepo = repo }

// NewDeliverService constructs a DeliverService.
func NewDeliverService(
	enqueuer queue.Enqueuer,
	userRepo repository.UserRepository,
	followingRepo repository.FollowingRepository,
	keypairRepo repository.UserKeypairRepository,
	urls *activitypub.URLBuilder,
	hostBlocker HostBlockChecker,
) *DeliverService {
	return &DeliverService{
		enqueuer:      enqueuer,
		userRepo:      userRepo,
		followingRepo: followingRepo,
		keypairRepo:   keypairRepo,
		urls:          urls,
		hostBlocker:   hostBlocker,
		clock:         time.Now,
	}
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
	_, err := s.deliverUsable(signerUserID, body, inboxes, recipient, sharedInboxes, notAfter, opts...)
	return err
}

// deliverUsable is deliverInternal that also returns the inboxes it enqueued
// to: inboxes without the empty, blocked and duplicate ones (nil on error).
func (s *DeliverService) deliverUsable(signerUserID string, body []byte, inboxes []string, recipient *model.User, sharedInboxes map[string]bool, notAfter time.Time, opts ...driver.EnqueueOption) ([]string, error) {
	if len(inboxes) == 0 {
		return nil, nil
	}
	usableInboxes := make([]string, 0, len(inboxes))
	usableSeen := make(map[string]struct{}, len(inboxes))
	for _, inbox := range inboxes {
		if inbox == "" || s.isBlockedInbox(inbox) {
			continue
		}
		if _, dup := usableSeen[inbox]; dup {
			continue
		}
		usableSeen[inbox] = struct{}{}
		usableInboxes = append(usableInboxes, inbox)
	}
	if len(usableInboxes) == 0 {
		return nil, nil
	}
	inboxes = usableInboxes
	var signer *model.User
	signerNeedsObservation := false
	var bodyReferences []referenceCandidate
	if s.localFederationObserver != nil {
		var err error
		signer, signerNeedsObservation, err = s.preflightSigner(signerUserID)
		if err != nil {
			return nil, err
		}
		knownUsers := make(map[string]*model.User, 1)
		if signer != nil {
			knownUsers[signer.ID] = signer
		}
		bodyReferences, err = s.preflightBodyReferences(body, knownUsers)
		if err != nil {
			return nil, err
		}
		// Both signer and every local actor/note reference have passed lookup and
		// availability checks. Only now can an outbound attempt record a first
		// external reference.
		host := firstDeliveryHost(inboxes, s)
		if signerNeedsObservation {
			if err := s.observeExternalReference(signer.ID, host); err != nil {
				return nil, err
			}
		}
		for _, candidate := range bodyReferences {
			if candidate.observe {
				if err := s.observeExternalReference(candidate.id, host); err != nil {
					return nil, err
				}
			}
			s.localReferenceCache.rememberUser(candidate.id, s.clock())
		}
		needsNoteRecheck := signerNeedsObservation
		for _, candidate := range bodyReferences {
			needsNoteRecheck = needsNoteRecheck || candidate.observe
		}
		if needsNoteRecheck {
			if err := s.confirmOutboundLocalNotes(body); err != nil {
				return nil, err
			}
		}
	}
	keyID, keyPEM, err := s.signerCredentials(signerUserID)
	if err != nil {
		return nil, err
	}
	// Ed25519 sign 経路を試行できるのは recipient が Ed25519 capable
	// (assertionMethod を持つ) + sender が Ed25519 keypair を持つ両方が
	// 真のときのみ。どちらかが false なら edKeyID/edPrivPEM は空のまま payload
	// に詰められ、DeliverProcessor は従来通り RSA で sign する。
	var edKeyID, edPrivPEM string
	if recipient != nil && s.recipientIsEd25519Capable(recipient) {
		edKeyID, edPrivPEM = s.signerEd25519Credentials(signerUserID)
	}
	// inboxes は冒頭で空・重複・ブロックを除いてある (#4221。以前はここでもう
	// 一度ブロックを照合していた)。
	// 本文の Digest は activity ごとに 1 回だけ計算し、job に持たせる (#4221)。
	digest := activitypub.SHA256Digest(body)
	payloads := make([]queue.DeliverPayload, 0, len(inboxes))
	for _, inbox := range inboxes {
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
			Digest:        digest,
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
				return nil, fmt.Errorf("sync deliver to %s: %w", inbox, err)
			}
			continue
		}
		payloads = append(payloads, payload)
	}
	if err := s.enqueueDelivers(payloads, opts); err != nil {
		return nil, err
	}
	return inboxes, nil
}

const (
	// deliverEnqueueParallelMin is the number of delivery jobs from which
	// enqueueDelivers enqueues them in parallel (#4219). 1件ずつの配送
	// (DeliverToUser など) では、並べても速くならず、goroutineの分だけ損をする。
	deliverEnqueueParallelMin = 8
	// deliverEnqueueParallelism is the maximum number of jobs enqueued at once
	// (#4219). 積む間、この本数だけRedisの接続を使う。16本で1件あたり152µsから
	// 73µsになり、それ以上はほとんど縮まなかった。
	deliverEnqueueParallelism = 16
)

// enqueueDelivers enqueues payloads onto the deliver queue and returns the
// first error. 件数が多ければ並べて積む (#4219)。失敗したら、まだ積んでいない
// ものは積まない (直列のときに、失敗したところで止めていたのと同じ)。積む処理の
// panic は、直列のときと同じく呼び出し元へ伝える。
func (s *DeliverService) enqueueDelivers(payloads []queue.DeliverPayload, opts []driver.EnqueueOption) error {
	if len(payloads) < deliverEnqueueParallelMin {
		for _, payload := range payloads {
			if err := s.enqueuer.EnqueueDeliver(payload, opts...); err != nil {
				return fmt.Errorf("enqueue deliver to %s: %w", payload.Inbox, err)
			}
		}
		return nil
	}
	var (
		next     atomic.Int64
		failed   atomic.Bool
		errOnce  sync.Once
		firstErr error
		wg       sync.WaitGroup
		// panicked は、worker の中で起きた最初の panic。
		panicked  any
		panicOnce sync.Once
	)
	fail := func(err error) {
		failed.Store(true)
		errOnce.Do(func() { firstErr = err })
	}
	for range min(deliverEnqueueParallelism, len(payloads)) {
		wg.Go(func() {
			var inbox string
			// 直列のときは、積む処理の panic を呼び出し元の recover (hook の
			// goroutine や echo の Recover) がスタック付きで記録していた。worker の
			// goroutine には受け手がいないので、ここで受け止めて、全部の worker が
			// 止まった後に呼び出し元の goroutine で投げ直す。元のスタックは、投げ直す
			// 値に含める。
			defer func() {
				if r := recover(); r != nil {
					failed.Store(true)
					panicOnce.Do(func() {
						panicked = fmt.Sprintf("enqueue deliver to %s: %v\n\n%s", inbox, r, debug.Stack())
					})
				}
			}()
			for !failed.Load() {
				i := int(next.Add(1)) - 1
				if i >= len(payloads) {
					return
				}
				inbox = payloads[i].Inbox
				if err := s.enqueuer.EnqueueDeliver(payloads[i], opts...); err != nil {
					fail(fmt.Errorf("enqueue deliver to %s: %w", inbox, err))
					return
				}
			}
		})
	}
	wg.Wait()
	if panicked != nil {
		panic(panicked)
	}
	return firstErr
}

func (s *DeliverService) confirmOutboundLocalNotes(body []byte) error {
	if s.urls == nil || s.noteRepo == nil {
		return nil
	}
	_, noteIDs := activitypub.ExtractLocalAPReferences(s.urls.BaseURL(), body)
	if len(noteIDs) == 0 {
		return nil
	}
	notes, err := s.noteRepo.FindManyLocalReferenceMetadataByIDs(noteIDs)
	if err != nil {
		return fmt.Errorf("confirm local note references before delivery: %w", err)
	}
	for _, note := range notes {
		if note != nil && note.UserHost == nil && note.LocalOnly {
			return ErrFederationDisabled
		}
		if note != nil && note.UserHost == nil && s.localReferenceCache.hasUser(note.UserID, s.clock()) {
			s.localReferenceCache.rememberNote(note.ID, note.UserID, s.clock())
		}
	}
	return nil
}

func firstDeliveryHost(inboxes []string, s *DeliverService) string {
	for _, inbox := range inboxes {
		if inbox == "" || s.isBlockedInbox(inbox) {
			continue
		}
		if u, err := url.Parse(inbox); err == nil {
			return u.Host
		}
	}
	return ""
}

type referenceCandidate struct {
	id      string
	observe bool
}

// preflightSigner checks policy without recording a reference. The returned
// user is memoized for body validation so the signer is looked up only once.
func (s *DeliverService) preflightSigner(userID string) (*model.User, bool, error) {
	if s.localReferenceCache.hasUser(userID, s.clock()) {
		return nil, false, nil
	}
	signer, err := s.userRepo.FindByID(userID)
	if err != nil {
		return nil, false, fmt.Errorf("load delivery signer: %w", err)
	}
	if !signer.IsLocal() {
		return signer, false, nil
	}
	if !s.localFederationObserver.IsExternallyAvailable(signer) {
		return nil, false, ErrFederationDisabled
	}
	if signer.FirstExternalReferenceAt != nil {
		s.localReferenceCache.rememberUser(signer.ID, s.clock())
		return signer, false, nil
	}
	return signer, true, nil
}

// preflightBodyReferences validates all account and note policy centrally so
// every activity kind is covered before any account is recorded as referenced.
func (s *DeliverService) preflightBodyReferences(body []byte, knownUsers map[string]*model.User) ([]referenceCandidate, error) {
	if s.urls == nil || s.userRepo == nil {
		return nil, nil
	}
	userIDs := map[string]struct{}{}
	ids, noteIDs := activitypub.ExtractLocalAPReferences(s.urls.BaseURL(), body)
	for _, id := range ids {
		userIDs[id] = struct{}{}
	}
	notesToConfirm := make(map[string]string, len(noteIDs))
	var coldNoteIDs []string
	for _, id := range noteIDs {
		if authorID, ok := s.localReferenceCache.noteAuthor(id, s.clock()); ok && s.externallyReferencedHas(authorID) {
			continue
		}
		coldNoteIDs = append(coldNoteIDs, id)
	}
	if len(coldNoteIDs) > 0 && s.noteRepo != nil {
		notes, err := s.noteRepo.FindManyLocalReferenceMetadataByIDs(coldNoteIDs)
		if err != nil {
			return nil, fmt.Errorf("lookup local note reference %s: %w", strings.Join(coldNoteIDs, ","), err)
		}
		for _, note := range notes {
			if note == nil || note.UserHost != nil {
				continue
			}
			if note.LocalOnly {
				return nil, ErrFederationDisabled
			}
			notesToConfirm[note.ID] = note.UserID
			userIDs[note.UserID] = struct{}{}
		}
	} else if len(coldNoteIDs) > 0 {
		return nil, fmt.Errorf("lookup local note references: note repository is unavailable")
	}
	userIDList := make([]string, 0, len(userIDs))
	for id := range userIDs {
		userIDList = append(userIDList, id)
	}
	sort.Strings(userIDList)
	toLookup := make([]string, 0, len(userIDList))
	for _, id := range userIDList {
		if s.localReferenceCache.hasUser(id, s.clock()) {
			continue
		}
		if _, ok := knownUsers[id]; ok {
			continue
		}
		toLookup = append(toLookup, id)
	}
	resolved := make(map[string]*model.User, len(toLookup))
	if len(toLookup) > 0 {
		users, err := s.userRepo.FindManyByIDs(toLookup)
		if err != nil {
			return nil, fmt.Errorf("lookup local user reference %s: %w", strings.Join(toLookup, ","), err)
		}
		for _, user := range users {
			if user != nil {
				resolved[user.ID] = user
			}
		}
	}
	candidates := make([]referenceCandidate, 0, len(userIDList))
	confirmedAuthors := make(map[string]struct{}, len(userIDList))
	for _, id := range userIDList {
		if s.localReferenceCache.hasUser(id, s.clock()) {
			confirmedAuthors[id] = struct{}{}
			continue
		}
		u, ok := knownUsers[id]
		if !ok {
			u = resolved[id]
		}
		if u == nil {
			continue
		}
		if !u.IsLocal() {
			continue
		}
		if u.FirstExternalReferenceAt != nil {
			confirmedAuthors[id] = struct{}{}
		}
		if !s.localFederationObserver.IsExternallyAvailable(u) {
			return nil, ErrFederationDisabled
		}
		candidates = append(candidates, referenceCandidate{id: u.ID, observe: u.FirstExternalReferenceAt == nil})
	}
	if len(notesToConfirm) > 0 {
		confirmIDs := make([]string, 0, len(notesToConfirm))
		for id := range notesToConfirm {
			confirmIDs = append(confirmIDs, id)
		}
		confirmed, err := s.noteRepo.FindManyLocalReferenceMetadataByIDs(confirmIDs)
		if err != nil {
			return nil, fmt.Errorf("confirm local note references: %w", err)
		}
		confirmedByID := make(map[string]*model.Note, len(confirmed))
		for _, note := range confirmed {
			if note != nil {
				confirmedByID[note.ID] = note
			}
		}
		for _, id := range confirmIDs {
			note := confirmedByID[id]
			if note == nil || note.LocalOnly || note.UserHost != nil || note.UserID != notesToConfirm[id] {
				return nil, ErrFederationDisabled
			}
			if _, ok := confirmedAuthors[note.UserID]; ok {
				s.localReferenceCache.rememberNote(id, note.UserID, s.clock())
			}
		}
	}
	return candidates, nil
}

func (s *DeliverService) externallyReferencedHas(userID string) bool {
	return s.localReferenceCache.hasUser(userID, s.clock())
}

func (s *DeliverService) observeExternalReference(userID, host string) error {
	if s.localReferenceCache.hasUser(userID, s.clock()) {
		return nil
	}
	if _, err := s.localFederationObserver.ObserveExternalReference(userID, "delivery", host); err != nil {
		return err
	}
	s.localReferenceCache.rememberUser(userID, s.clock())
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
	gen := s.cacheGeneration()
	val, _, _ := s.capGroup.Do(flightKey(recipient.ID, gen), func() (any, error) {
		rows, err := s.publickeyExtraRepo.ListByUserID(recipient.ID)
		if err != nil {
			// DB error は cache せず次回 retry させる (= 戻り値の bool 経由で
			// caller に伝播、cache には書き込まない)
			return false, nil
		}
		capable := len(rows) > 0
		s.storeIfCurrent(gen, func() {
			s.capCache.Store(recipient.ID, ed25519CapEntry{
				capable:   capable,
				fetchedAt: s.clock(),
			})
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
	gen := s.cacheGeneration()
	val, _, _ := s.signerGroup.Do(flightKey(userID, gen), func() (any, error) {
		kp, err := s.keypairExtraRepo.FindByUserID(userID)
		if err != nil {
			// 鍵未発行は cache に空 entry を入れて再 query を抑える
			entry := ed25519SignerEntry{fetchedAt: s.clock()}
			s.storeIfCurrent(gen, func() { s.signerCache.Store(userID, entry) })
			return entry, nil
		}
		entry := ed25519SignerEntry{
			keyID:     s.urls.UserURI(userID) + "#ed25519-key",
			privPEM:   kp.Ed25519PrivateKey,
			fetchedAt: s.clock(),
		}
		s.storeIfCurrent(gen, func() { s.signerCache.Store(userID, entry) })
		return entry, nil
	})
	entry := val.(ed25519SignerEntry)
	return entry.keyID, entry.privPEM
}

// DropLocal drops the cached Ed25519 capabilities of recipients and the
// cached Ed25519 and RSA keys of local signers in this process (#3732). It is what the
// cache list of internal/server calls (docs/architecture.md 4.4). It tells no
// other process.
func (s *DeliverService) DropLocal() {
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	s.cacheGen++
	s.capCache.Clear()
	s.signerCache.Clear()
	s.rsaSignerCache.Clear()
}

func (s *DeliverService) cacheGeneration() uint64 {
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	return s.cacheGen
}

// storeIfCurrent runs store only while no DropLocal has run since gen was
// read. 番号の比較と積むことを同じ lock の中で行い、比べた直後に捨てられて古い値が
// 残ることを防ぐ。
func (s *DeliverService) storeIfCurrent(gen uint64, store func()) {
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	if s.cacheGen == gen {
		store()
	}
}

// flightKey is the singleflight key of id in generation gen.
func flightKey(id string, gen uint64) string {
	return id + "\x00" + strconv.FormatUint(gen, 10)
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
	_, err := s.deliverToFollowers(signerUserID, body, exclude, nil)
	return err
}

// DeliverToFollowersAllowed is DeliverToFollowersExcluding limited to the
// followers whose host allow reports true (#4199).
//
// **判定に使うのはフォロワーの利用者の host。** inbox の URL のホストで
// 判定すると、inbox を actor とは別のホストに置くサーバー (利用者の host は
// `remote.example`、inbox は `inbox.remote.example`) で、外したはずのホストに
// 届くか、許したはずのホストに届かない。照会の判定 (署名した利用者の host)
// とも揃わなくなる。利用者の host は actor の URL のホストで、WebFinger の
// acct のドメインではない (#4202)。
//
// shared inbox を許すホストと許さないホストの利用者が共有しているときは送る
// (同じサーバーなので、送らなければ許したホストにも届かない)。
func (s *DeliverService) DeliverToFollowersAllowed(signerUserID string, body []byte, exclude map[string]bool, allow func(host string) bool) error {
	_, err := s.deliverToFollowers(signerUserID, body, exclude, allow)
	return err
}

// DeliverToFollowersCollect is DeliverToFollowersAllowed (allow == nil means
// every follower) that also returns the inboxes it enqueued to, so a caller
// can skip 1:1 deliveries those inboxes already cover (#4223). On error the
// set is nil.
func (s *DeliverService) DeliverToFollowersCollect(signerUserID string, body []byte, allow func(host string) bool) (map[string]bool, error) {
	return s.deliverToFollowers(signerUserID, body, nil, allow)
}

// deliverToFollowers enqueues delivery to signerUserID's remote followers,
// skipping inboxes in exclude and, when allow is non-nil, followers whose
// host allow rejects. Returns the inboxes enqueued to (blocked ones left out).
func (s *DeliverService) deliverToFollowers(signerUserID string, body []byte, exclude map[string]bool, allow func(host string) bool) (map[string]bool, error) {
	var rows []model.RemoteInbox
	if allow == nil {
		var err error
		rows, err = s.followingRepo.ListRemoteFollowerInboxes(signerUserID)
		if err != nil {
			return nil, err
		}
	} else {
		hostRows, err := s.followingRepo.ListRemoteFollowerInboxHosts(signerUserID)
		if err != nil {
			return nil, err
		}
		rows = make([]model.RemoteInbox, 0, len(hostRows))
		for _, row := range hostRows {
			if allow(row.Host) {
				rows = append(rows, row.RemoteInbox)
			}
		}
	}
	inboxes := make([]string, 0, len(rows))
	seen := make(map[string]bool, len(rows))
	var sharedInboxes map[string]bool
	for _, row := range rows {
		if exclude[row.Inbox] {
			continue
		}
		// shared の印は、同じ URL の行のどれか 1 つが shared なら付ける。行の順で
		// 決めると、ある利用者の sharedInbox と別の利用者の個別 inbox が同じ URL の
		// とき、410 をホスト単位の判定 (#1811) へ回せないことがある。
		if row.Shared {
			if sharedInboxes == nil {
				sharedInboxes = make(map[string]bool)
			}
			sharedInboxes[row.Inbox] = true
		}
		if seen[row.Inbox] {
			continue
		}
		seen[row.Inbox] = true
		inboxes = append(inboxes, row.Inbox)
	}
	// 返すのは実際に積んだ inbox だけ。除いた inbox (空、ブロックや許可リストの
	// 外) まで返すと、1:1 の配送を省いた相手がどこからも受け取らないことがある。
	// 判定は積むときの 1 回だけにする (もう一度判定すると、間に設定が変わったとき
	// 積まなかった inbox が入る)。
	usable, err := s.deliverUsable(signerUserID, body, inboxes, nil, sharedInboxes, time.Time{})
	if err != nil {
		return nil, err
	}
	enqueued := make(map[string]bool, len(usable))
	for _, inbox := range usable {
		enqueued[inbox] = true
	}
	return enqueued, nil
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
	keyID := s.urls.UserKeyURI(userID)
	if cached, ok := s.rsaSignerCache.Load(userID); ok {
		entry := cached.(rsaSignerEntry)
		if s.clock().Sub(entry.fetchedAt) <= ed25519CacheTTL {
			return keyID, entry.privPEM, nil
		}
	}
	gen := s.cacheGeneration()
	val, err, _ := s.rsaSignerGroup.Do(flightKey(userID, gen), func() (any, error) {
		kp, err := s.keypairRepo.FindByUserID(userID)
		if err != nil {
			return nil, err
		}
		// 無いときと読めなかったときは持たない。鍵を作った直後や DB が戻った
		// 直後に、TTL の間だけ配送が止まらないようにする。
		entry := rsaSignerEntry{privPEM: kp.PrivateKey, fetchedAt: s.clock()}
		s.storeIfCurrent(gen, func() { s.rsaSignerCache.Store(userID, entry) })
		return entry, nil
	})
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
	return keyID, val.(rsaSignerEntry).privPEM, nil
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
//
// ただし、ノートの作成と SendNoteUpdate の Update (引用の承認を付けたものと
// 外したもの) では、相手の sharedInbox へフォロワーとして既に積んでいれば、
// 1:1 では送らない (#4223、本家と同じ)。上の
// 「shared inbox で非公開 activity を扱わない実装」の相手には、フォロワー限定や
// ホームのノートが、フォローしていないメンション先へ届かないことがある。本家と
// 同じ振る舞いなので受け入れる。
func preferredInbox(u *model.User) string {
	if u.Inbox != nil && *u.Inbox != "" {
		return *u.Inbox
	}
	if u.SharedInbox != nil {
		return *u.SharedInbox
	}
	return ""
}
