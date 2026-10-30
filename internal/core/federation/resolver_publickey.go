package federation

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"log/slog"

	"github.com/elythia-network/elythia/internal/activitypub"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
)

// keyFetchBackoff bounds how often we re-fetch an actor document solely to
// refill the in-memory public key cache after a failure.
//
// この経路は TTL 内でもキャッシュが空なら毎回走る。鍵が入らない actor
// (document が恒久的に不正 / `publicKey` を持たない / 相手が落ちている) では
// 抑制しないと inbound activity 1 件につき outbound fetch が 1 回走り続ける。
//
// **エラーの種類で絞らない。** `refreshActor` 側は `ErrInvalidActor` に絞って
// いるが、あちらは「lastFetchedAt を進める = TTL を空振りさせる」ので一時的な
// 障害で進めると更新が遅れる。こちらは単なるレート制限で、5 分後には必ず
// 再挑戦する。一時障害でも 5 分待つが、その間は `PublicKeyForActor` の
// DB fallback が効く。
const keyFetchBackoff = 5 * time.Minute

// refreshPublicKey fetches the actor again and caches its public key.
// エラーは上に伝搬せず、keyFetchBackoff に載せて次の機会に回す。
func (r *Resolver) refreshPublicKey(userID, uri string) {
	r.keysMu.RLock()
	failedAt, failed := r.keyFetchFailures[userID]
	r.keysMu.RUnlock()
	if failed && r.clock().Sub(failedAt) < keyFetchBackoff {
		return
	}
	// background refresh は federation-loop 扱いで Strict。
	actor, err := r.fetchActor(uri, false)
	if err != nil {
		r.markKeyFetchFailed(userID)
		return
	}
	// **fetch は成功したが鍵が読めない場合も backoff に載せる。** この経路は
	// 「TTL 内かつ in-memory 鍵キャッシュが空」なら毎回走るので、キャッシュに
	// 何も入らないまま成功扱いにすると inbound activity 1 件につき outbound
	// fetch が 1 回走り続ける (#2662)。空 PEM をキャッシュして黙らせるのは
	// 論外 (既存の鍵を壊す)。
	pem := actor.PublicKey.PublicKeyPEM.String()
	if pem == "" {
		r.markKeyFetchFailed(userID)
		r.cacheAssertionMethods(userID, actor.ID, actor.AssertionMethod)
		return
	}
	r.keysMu.Lock()
	delete(r.keyFetchFailures, userID)
	r.keysMu.Unlock()
	r.cachePublicKey(userID, actor.PublicKey.ID, pem)
	r.cacheAssertionMethods(userID, actor.ID, actor.AssertionMethod)
}

// markKeyFetchFailed records a failed key refresh so refreshPublicKey backs off.
func (r *Resolver) markKeyFetchFailed(userID string) {
	r.keysMu.Lock()
	defer r.keysMu.Unlock()
	if r.keyFetchFailures == nil {
		r.keyFetchFailures = make(map[string]time.Time)
	}
	// backoff を過ぎた entry は用済みなので落とす。上限はリモート user 数
	// なので暴走はしないが、消えないと単調増加する。
	now := r.clock()
	for id, at := range r.keyFetchFailures {
		if now.Sub(at) >= keyFetchBackoff {
			delete(r.keyFetchFailures, id)
		}
	}
	r.keyFetchFailures[userID] = now
}

// cachePublicKey stores a PEM in the in-memory cache and optionally persists
// it to the user_publickey table.
func (r *Resolver) cachePublicKey(userID, keyID, pem string) {
	// **空の PEM で既存の鍵を上書きしない。** 空を書くと in-memory も DB も
	// 空になり、`PublicKeyForActor` が空文字を返して **その actor からの
	// inbound HTTP Signature がすべて verify 失敗する**。refresh のたびに
	// 同じ空が書き直されるので自然回復しない。
	//
	// `publicKeyPem` を寛容に読むようにした (#2662) ことで、`{"@value": ...}`
	// のような形の document が「通るが値は空」で到達するようになった。それ以前
	// から `publicKey` 欠落でも同じ経路には入る。鍵が読めないなら**何もしない**
	// のが正しい (既存の鍵で verify を続けられる)。
	if pem == "" {
		// upstream の validateActor は `if (x.publicKey)` で publicKey を任意
		// 扱いにするので、鍵を持たない actor は不正ではない (Ed25519 のみを
		// publish する実装など)。Warn だと出続けるので Debug にする。
		slog.Debug("federation: refusing to cache empty public key",
			"userID", userID, "keyID", keyID)
		return
	}
	r.keysMu.Lock()
	r.keys[userID] = publicKeyEntry{pem: pem, fetchedAt: r.clock()}
	r.keysMu.Unlock()
	if r.publickeyRepo != nil && keyID != "" {
		// **keyId / keyPem は行の身元。切ると別の鍵を指す**ので、収まらなければ
		// 行を作らない (#2726)。upsert に投げても 22001 / 22021 で落ちるだけで、
		// 結末は同じだが原因が DB error として残る。
		//
		// **in-memory cache は残す。** 上で入れた値は `PublicKeyForActor` の
		// 高速路で、再起動後は actor を引き直して同じ値が入る (= 永続化の有無で
		// 挙動が分かれない)。ここで消すと `refreshPublicKey` の backoff が
		// 効かず、inbound 1 件ごとに outbound fetch が走る。
		if !fitsColumn(keyID, publicKeyKeyIDMaxRunes) || !fitsColumn(pem, publicKeyPEMMaxRunes) {
			slog.Warn("federation: public key does not fit user_publickey; not persisted",
				"userID", userID, "keyID", truncateRunes(keyID, publicKeyKeyIDMaxRunes))
			return
		}
		if err := r.publickeyRepo.Upsert(&model.UserPublickey{
			UserID: userID,
			KeyID:  keyID,
			KeyPEM: pem,
		}); err != nil {
			slog.Warn("failed to persist public key", "userId", userID, "err", err)
		}
	}
}

// declareEd25519Capability records that actorURI's host publishes an Ed25519
// key. purge (key rotation で assertionMethod が消えた場合) では対になるクリアを
// 行わない: 値は host 単位で、同一 host の別 actor がまだ鍵を持っている可能性が
// あるため。「最後に確認した時刻」として保持し、古さの表現は UI 側に委ねる。
func (r *Resolver) declareEd25519Capability(actorURI string) {
	if r.capabilities == nil {
		return
	}
	// host は user.Host / instance.host と同じ導出 (hostFromURI) を使う。ここが
	// ずれると instance 一覧との突合が空振りしてラベルが出ない。
	host, err := hostFromURI(actorURI)
	if err != nil || host == "" {
		return
	}
	if err := r.capabilities.RecordEd25519Declared(host, time.Now()); err != nil {
		slog.Warn("ed25519 capability declaration persist failed",
			"host", host, "actorURI", actorURI, "error", err)
	}
}

// cacheAssertionMethods reconciles a remote actor's FEP-521a `assertionMethod[]`
// with our user_publickey_extra rows (#1067 / #1070):
//
//  1. 各 Multikey entry を decode して PEM 化 → user_publickey_extra に upsert
//  2. 既存 row のうち、今回の actor JSON に存在しない keyId は purge (delete)
//
// 2 が無いと remote が Ed25519 鍵を rotate した後も古い鍵が残り、攻撃者が
// compromised key で署名した activity が verify 通ってしまうので security
// 上必須。publickeyExtraRepo 未配線 (= 旧 deployment) のときは何もしない。
// 不正な entry (非 Multikey type / decode 失敗 / persist 失敗) は warn log
// + skip して RSA only にフォールバックする (fail-soft)。
func (r *Resolver) cacheAssertionMethods(userID, actorURI string, ams activitypub.MultikeyList) {
	if r.publickeyExtraRepo == nil {
		return
	}
	// **actor の文書を読み直したら「extra に鍵が無い」の記録を捨てる (#3610)。**
	// 相手が鍵を足した (回転した) なら、ここで行が入る。upsert の後で捨てるので
	// defer にする (先に捨てると、upsert までの間に引いた「無い」が残る)。
	defer r.forgetMissingExtraKeys(userID)
	// **読めなかったときは purge しない。** 下の purge は「actor が申告しなかった
	// keyId を消す」= key rotation 追従だが、それが成立するのは actor の申告を
	// 正しく読めた場合だけ。読めない形 (string や壊れた entry) で空リストを
	// 渡されて purge すると、キャッシュ済みの Ed25519 鍵を全消しし、Ed25519 のみを
	// publish する相手では **inbound の署名検証が恒久的に失敗する** (相手は同じ
	// 形を返し続けるので自然回復しない、#2662)。
	if ams.Unreadable {
		slog.Warn("assertionMethod partially unreadable; skipping stale-key purge",
			"userID", userID, "actorURI", actorURI, "readable", len(ams.Keys))
	}
	// 1. 新 entries の upsert + 受領 keyId set を構築
	receivedKeyIDs := make(map[string]bool, len(ams.Keys)+len(ams.Refs))
	// 参照形式 (bare IRI) は鍵素材を持たないので upsert はできないが、
	// 「actor がその keyId を申告している」ことは分かるので purge から守る。
	for _, ref := range ams.Refs {
		ref = trimWHATWGURL(ref)
		if sameDeliveryHost(ref, actorURI) {
			receivedKeyIDs[ref] = true
		}
	}
	upserted := 0
	for _, am := range ams.Keys {
		// **検査だけでなく保存値も正規化する。** 生値のまま入れると HTTP
		// ヘッダ由来の keyId と一致しないゴミ行になり、しかも自分自身を
		// purge から守ってしまう (#2662)。
		am.ID = trimWHATWGURL(am.ID)
		if am.Type != activitypub.MultikeyType || am.ID == "" || am.PublicKeyMultibase == "" {
			continue
		}
		// keyId (am.ID) の host を actor の host に縛る (Fix B, upstream validateActor の
		// publicKey.id host check と同型)。これが無いと攻撃者 actor が assertionMethod に
		// victim ドメインの keyId を載せ、自分の Ed25519 鍵を victim の keyId で植え込んで
		// victim を名乗る署名を verify 通過させられる (key confusion / 連合認証バイパス)。
		// 検証鍵の lookup key は am.ID 自身 (PublicKeyForKeyID) なので、am.ID の host を
		// actor に縛れば植え込み経路は塞がる (controller は lookup に使われないため追加検証
		// は不要)。不正 entry は既存の decode 失敗時と同じく warn + skip (fail-soft、
		// actor 自体は取り込む)。
		if !sameDeliveryHost(am.ID, actorURI) {
			slog.Warn("assertionMethod keyId host mismatch",
				"userID", userID, "actorURI", actorURI, "keyID", am.ID)
			continue
		}
		// keyId は `user_publickey_extra.keyId` (varchar(256)) の PK 構成要素。
		// 切ると別の鍵を指すので、収まらない entry は他の不正 entry と同じく
		// warn + skip する (fail-soft、actor 自体は取り込む、#2726)。
		// keyPem は Ed25519 の固定長 PEM なので溢れない。
		if !fitsColumn(am.ID, publicKeyKeyIDMaxRunes) {
			slog.Warn("assertionMethod keyId does not fit its column",
				"userID", userID, "actorURI", actorURI)
			continue
		}
		pub, err := activitypub.DecodeEd25519Multikey(am.PublicKeyMultibase)
		if err != nil {
			slog.Warn("assertionMethod decode failed",
				"userID", userID, "keyID", am.ID, "error", err)
			continue
		}
		pemStr, err := activitypub.MarshalEd25519PublicKeyPEM(pub)
		if err != nil {
			slog.Warn("assertionMethod PEM marshal failed",
				"userID", userID, "keyID", am.ID, "error", err)
			continue
		}
		if err := r.publickeyExtraRepo.Upsert(&model.UserPublickeyExtra{
			UserID: userID,
			KeyID:  am.ID,
			KeyPEM: pemStr,
			Alg:    model.AlgEd25519,
		}); err != nil {
			slog.Warn("assertionMethod persist failed",
				"userID", userID, "keyID", am.ID, "error", err)
			continue
		}
		receivedKeyIDs[am.ID] = true
		upserted++
	}
	// この host が Ed25519 を expose していることを instance 単位で記録する
	// (#2393)。鍵が複数あっても host 単位の事実は 1 つなので、ループ内ではなく
	// ここで 1 回だけ書く。actor resolve は inbound ほど高頻度ではないので buffer は
	// 挟まない。best-effort なので失敗しても actor の取り込みは続ける。
	// **参照形式 (bare IRI) では宣言しない。** ref は purge 保護のために
	// receivedKeyIDs へ入れるが、鍵素材が無いので Ed25519 対応の根拠にならない
	// (FEP-521a の bare IRI は alg も分からない)。ref だけで宣言すると
	// `SupportsEd25519()` が「対応」を返すのに鍵が 1 本も無い状態になる (#2662)。
	if upserted > 0 {
		r.declareEd25519Capability(actorURI)
	}
	// 2. actor JSON に無い既存 keyId を purge (key rotation 対応)
	if ams.Unreadable {
		return
	}
	existing, err := r.publickeyExtraRepo.ListByUserID(userID)
	if err != nil {
		slog.Warn("assertionMethod list failed", "userID", userID, "error", err)
		return
	}
	for _, row := range existing {
		if receivedKeyIDs[row.KeyID] {
			continue
		}
		if err := r.publickeyExtraRepo.DeleteByKeyID(userID, row.KeyID); err != nil {
			slog.Warn("stale assertionMethod delete failed",
				"userID", userID, "keyID", row.KeyID, "error", err)
		}
	}
}

// PublicKeyForKeyID returns the PEM-encoded public key whose keyId matches the
// given fragment URI. user_publickey_extra (Ed25519 / Multikey) を最初に探し、
// miss なら user_publickey の primary key (RSA) を返す fallback semantics。
// 受信した HTTP Signature の keyId fragment (e.g. `#main-key` / `#ed25519-key`)
// から正しい公開鍵を選ぶための path で、in-memory cache は介さない (= 永続層
// の最新値で verify する)。
//
// 鍵は必ず actorID-scope で引く (FindByUserAndKeyID)。keyID 単独の global lookup に
// すると、攻撃者 actor が assertionMethod に victim の keyId を載せて自分の鍵を
// 植え込み、victim を名乗る署名を verify 通過させる key confusion (連合認証
// バイパス) が成立する。actor (= keyId base から解決した signer) に紐づく鍵だけを
// 許すことで、植え込まれた cross-actor 鍵を読取段でも排除する (Fix C, 二重防御)。
//
// not-found (= keyId 一致なし = 通常状態) は silent fallback。それ以外の DB
// error は **fallback せず `ErrLookupUnavailable` で返す** (#3121) — 落とすと
// 「その keyId の鍵は無い」ことになり、呼び出し側が署名検証の失敗として ack
// するので、DB 障害のあいだ届いた activity が失われる。stale assertion key の
// 削除は cacheAssertionMethods 側で actor fetch 時に diff & delete するため、
// ここでは古い行が引っかかる可能性は最小化される。
func (r *Resolver) PublicKeyForKeyID(actorID, keyID string) (string, error) {
	// **列に入らない keyId は引くまでもなく無い (#3610)。** `user_publickey_extra.keyId`
	// は varchar(256) (migration/000050_user_publickey_extra.up.sql、
	// publicKeyKeyIDMaxRunes) で、cacheAssertionMethods は収まらない keyId を保存
	// しない。NUL や不正な UTF-8 を含む keyId は repository の FindByUserAndKeyID も
	// `storable` で弾いて not found を返すが、ここで先に弾けば列の長さも含めて
	// 1 か所で判定できる。署名を検証する前に呼ばれるので、長い keyId で DB を
	// 引かせ続ける経路と、否定キャッシュを埋める経路をここで閉じる (覚える必要も無い)。
	if r.publickeyExtraRepo != nil && keyID != "" && fitsColumn(keyID, publicKeyKeyIDMaxRunes) &&
		!r.missingExtra.known(actorID, keyID, r.clock()) {
		// 問い合わせの前に世代を読む。問い合わせの間に actor が読み直されたら
		// (= 鍵が足されたかもしれない)、見つからなかった結果を覚えない。
		gen := r.missingExtra.generation()
		row, err := r.publickeyExtraRepo.FindByUserAndKeyID(actorID, keyID)
		if err == nil {
			return row.KeyPEM, nil
		}
		if !repository.IsNotFound(err) {
			slog.Warn("publickeyExtra lookup failed",
				"actorID", actorID, "keyID", keyID, "error", err)
			// **fallback へ落とさない** (#3121)。落とすと「その keyId の鍵は
			// 無い」ことになり、呼び出し側が署名検証の失敗として ack する。
			// 障害の結果は覚えない (覚えると障害が明けても「無い」が残る)。
			return "", fmt.Errorf("%w: publickeyExtra %q/%q: %v", ErrLookupUnavailable, actorID, keyID, err)
		}
		r.missingExtra.remember(actorID, keyID, gen, r.clock())
	}
	return r.PublicKeyForActor(actorID)
}

// missingExtraKeyTTL is how long PublicKeyForKeyID remembers that
// user_publickey_extra has no row for an (actor, keyId) (#3610).
//
// Misskey / Mastodon の `#main-key` は RSA の主鍵で、この表には行が無い。覚えないと
// 受信 1 件ごとに必ず見つからない問い合わせが 1 回走る (ローカルの PostgreSQL で
// 中央値 約 240µs)。
//
// **短くし、actor を読み直したら捨てる。** この表に行を足すのは
// cacheAssertionMethods (actor の文書を読んだとき) だけで、そこで当該 actor の
// 記録を消し、世代を進める。問い合わせの間に世代が進んでいたら覚えない。残るのは
// 別のプロセスが行を足した場合で、その間は主鍵 (RSA) で検証される。相手が
// Ed25519 で署名していれば検証に失敗し、**その activity は失われる** (inbox は
// 202 を返しており、worker の署名の失敗は ack するので、相手は再送しない)。
// 期限を 1 分にしているのはこの窓を狭くするため。
const missingExtraKeyTTL = time.Minute

// 否定キャッシュの上限 (#3610)。
//
// PublicKeyForKeyID は**署名を検証する前に**呼ばれ、keyId は署名ヘッダの値なので、
// DB にある actor の URI に好きな fragment を付ければ、署名なしの要求で記録を
// 増やせる。上限に達したら覚えない (毎回 DB を引く今の動きに戻るだけ)。
const (
	// missingExtraKeyMax は全体の件数。
	missingExtraKeyMax = 10000
	// missingExtraKeysPerActor は actor ごとの件数。正当な相手の keyId は数個しかない。
	missingExtraKeysPerActor = 4
	// missingExtraKeyIDMaxBytes を超える keyId は覚えない。
	missingExtraKeyIDMaxBytes = 512
)

// missingExtraKeys remembers (userID, keyId) pairs that user_publickey_extra
// does not have. The zero value is ready to use.
type missingExtraKeys struct {
	mu     sync.Mutex
	byUser map[string]map[string]time.Time // userID → keyId → 見つからなかった時刻
	total  int
	// gen は forget のたびに進む。remember は問い合わせの前に読んだ値と一致する
	// ときだけ書く。
	gen uint64
	// lastSweep は全体を掃いた時刻。掃くのは missingExtraKeyTTL に 1 回まで
	// (満杯のまま毎回掃くと、mutex を握ったまま全件を走査し続ける)。
	lastSweep time.Time
}

func (m *missingExtraKeys) known(userID, keyID string, now time.Time) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	keys := m.byUser[userID]
	at, ok := keys[keyID]
	if !ok {
		return false
	}
	if now.Sub(at) >= missingExtraKeyTTL {
		m.deleteLocked(userID, keyID)
		return false
	}
	return true
}

func (m *missingExtraKeys) generation() uint64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.gen
}

func (m *missingExtraKeys) remember(userID, keyID string, gen uint64, now time.Time) {
	if len(keyID) > missingExtraKeyIDMaxBytes {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if gen != m.gen {
		return
	}
	keys := m.byUser[userID]
	if _, ok := keys[keyID]; ok {
		keys[keyID] = now
		return
	}
	if len(keys) >= missingExtraKeysPerActor {
		for k, at := range keys {
			if now.Sub(at) >= missingExtraKeyTTL {
				m.deleteLocked(userID, k)
			}
		}
		if len(keys) >= missingExtraKeysPerActor {
			return
		}
	}
	if m.total >= missingExtraKeyMax {
		if now.Sub(m.lastSweep) < missingExtraKeyTTL {
			return
		}
		m.lastSweep = now
		for uid, ks := range m.byUser {
			for k, at := range ks {
				if now.Sub(at) >= missingExtraKeyTTL {
					m.deleteLocked(uid, k)
				}
			}
		}
		if m.total >= missingExtraKeyMax {
			return
		}
	}
	if m.byUser == nil {
		m.byUser = make(map[string]map[string]time.Time)
	}
	keys = m.byUser[userID]
	if keys == nil {
		keys = make(map[string]time.Time)
		m.byUser[strings.Clone(userID)] = keys
	}
	// keyId は署名ヘッダの部分文字列なので、写さないとヘッダ全体を保持し続ける。
	keys[strings.Clone(keyID)] = now
	m.total++
}

// dropAll drops every record and advances the generation (#3732).
func (m *missingExtraKeys) dropAll() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.gen++
	clear(m.byUser)
	m.total = 0
}

// forget drops every record for userID and advances the generation.
func (m *missingExtraKeys) forget(userID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.gen++
	m.total -= len(m.byUser[userID])
	delete(m.byUser, userID)
}

func (m *missingExtraKeys) deleteLocked(userID, keyID string) {
	keys := m.byUser[userID]
	if _, ok := keys[keyID]; !ok {
		return
	}
	delete(keys, keyID)
	m.total--
	if len(keys) == 0 {
		delete(m.byUser, userID)
	}
}

// DropLocal drops the cached public keys of remote actors and the records of
// missing Ed25519 keys in this process (#3732). It is what the cache list of
// internal/server calls (docs/architecture.md 4.4). It tells no other process.
//
// 取得の失敗の backoff (keyFetchFailures) と mentionFetch は捨てない (#3729 と
// 同じ判断。値のキャッシュではなく、捨てると backoff が外れるだけ)。鍵を捨てた
// 直後は、ResolveActor が鍵の無い actor を相手から取り直すので、外への取得が
// keyFetchBackoff の範囲で増える。
func (r *Resolver) DropLocal() {
	r.keysMu.Lock()
	r.keysGen++
	clear(r.keys)
	r.keysMu.Unlock()
	r.missingExtra.dropAll()
}

// forgetMissingExtraKeys drops every "no extra key" record for userID.
func (r *Resolver) forgetMissingExtraKeys(userID string) {
	r.missingExtra.forget(userID)
}

// ResolveActorByKeyID resolves an actor based on the keyId fragment URI.
func (r *Resolver) ResolveActorByKeyID(keyID string) (*model.User, error) {
	return r.ResolveActor(activitypub.ResolveKeyURL(keyID))
}
