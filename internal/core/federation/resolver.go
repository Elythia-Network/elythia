// Package federation provides services for processing inbound and outbound
// ActivityPub traffic.
package federation

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"log/slog"

	"github.com/elythia-network/elythia/internal/activitypub"
	"github.com/elythia-network/elythia/internal/activitypub/mfm"
	"github.com/elythia-network/elythia/internal/bgwork"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/misc/keyword"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
)

// HTTPFetcher abstracts the HTTP client used for fetching remote AP objects.
// 実装は activitypub.Client か互換のテストダブル。署名なし fetch でも互換性は
// 保たれるため、ここでは accept ヘッダ指定だけ受け取る。
type HTTPFetcher interface {
	FetchObject(uri string) ([]byte, error)
}

// InstanceTracker is an interface used by Resolver to register hosts as soon
// as a remote user is fetched. パッケージ間の循環依存を避けるため interface で
// 受け取る (実装は core/instance.Service)。
type InstanceTracker interface {
	RegisterFromHost(host string) (*model.Instance, error)
}

// ChartHook is invoked after a remote user has been freshly created so
// the chart subsystem can record the new-user event into UsersChart and
// InstanceChart. パッケージ間の循環依存を避けるため interface で受け取る
// (実装は core/chart/charthook)。
type ChartHook interface {
	OnRemoteUserCreated(user *model.User)
}

// HashtagHook is invoked after a remote note has been freshly persisted
// (or updated) so the hashtag subsystem can record per-tag mentioned
// counts. パッケージ間の循環依存を避けるため interface で受け取る (実装は
// core/hashtag.Service)。#680。
//
// 実装契約 (#719): OnNoteCreated は **non-blocking** でなければならず、
// repo 書き込みが必要な場合は実装側で goroutine を起こすこと。IngestNote /
// UpdateRemoteNote は本 hook 結果を待たずに即時 return することで inbox
// processor の drain time が tag 数に比例しないようにする。panic recovery
// も実装側の責務。
type HashtagHook interface {
	OnNoteCreated(note *model.Note, author *model.User)
	// UpdateUsertags reconciles the hashtag attach aggregate when a remote
	// user's person.tag derived tags change (#1362)。OnNoteCreated と同じく
	// non-blocking であること。
	UpdateUsertags(userID string, isLocal bool, oldTags, newTags []string)
}

// Errors returned by Resolver.
var (
	// ErrInvalidActor is returned when the fetched JSON cannot be parsed.
	ErrInvalidActor = errors.New("invalid actor document")
	// ErrLocalActor is returned when an actor URI points at this instance.
	//
	// upstream ApPersonService.createPerson の
	// `throw new StatusError('cannot resolve local user', 400, ...)` に対応する。
	// **ErrInvalidActor を包む** ので、既存の `isPermanentSkipError` (= ack して
	// retry しない) と呼び出し側の分岐がそのまま効く。
	ErrLocalActor = fmt.Errorf("%w: cannot resolve local user", ErrInvalidActor)
	// ErrInvalidNote is returned when a fetched Note cannot be parsed.
	ErrInvalidNote = errors.New("invalid note document")
	// ErrHostNotAllowed is returned when the resolver is asked to fetch /
	// ingest content from a host that the local instance's federation policy
	// (federation: none / specified, blockedHosts) does not allow.
	ErrHostNotAllowed = errors.New("host not allowed by federation policy")
	// ErrObjectHostMismatch is returned when a fetched AP object's id host does
	// not match the host that actually served it (after redirects). Mirrors
	// upstream assertActivityMatchesUrl の hard requirement で object-spoofing を
	// 防ぐ (#1820)。
	ErrObjectHostMismatch = errors.New("fetched object id host does not match response host")
	// ErrNoteAttributionMismatch is returned when an inbound Note's id host does
	// not match its attributedTo host, or its attributedTo does not match the
	// delivering actor. Mirrors upstream ApNoteService.validateNote で note 偽装
	// (なりすまし) を防ぐ (#1839)。malformed note (ErrInvalidNote) と違い、これは
	// retry しても解消しないため caller は ack して drop する。
	ErrNoteAttributionMismatch = errors.New("note attribution does not match delivering actor or id host")
	// ErrResolveFragment is returned when a resolve target URL carries a `#`
	// fragment. Fragments are not transmitted over HTTP(S) so such URLs cannot
	// be dereferenced; mirrors upstream Resolver の b94fd5b1 guard (#1828)。
	ErrResolveFragment = errors.New("cannot resolve URL with fragment")
	// ErrRecursionLimit is returned when a single resolve operation's note/quote
	// chain exceeds resolveRecursionLimit, mirroring upstream Resolver の
	// d592da9f guard。悪意ある quote チェーンによる無限再帰・amplification を防ぐ。
	ErrRecursionLimit = errors.New("hit recursion limit")
	// ErrResolveWouldDeadlock is returned when joining an in-flight resolve
	// would close a wait cycle across goroutines. 呼び出し側は既存行に落とすか、
	// その回の解決を諦める (#2685 review HIGH-1)。
	ErrResolveWouldDeadlock = errors.New("resolving would deadlock: wait cycle")
	// ErrResolveJoinTimeout is returned when this resolve tree used up its
	// total wait budget (resolveJoinTimeout) waiting on other chains.
	//
	// 循環検出の**保険**。検出はモデル化した待ちしか見ないので、載っていない
	// 待ちが増えれば見逃す。見逃しても永久には止まらないようにする。
	ErrResolveJoinTimeout = errors.New("resolving timed out waiting for an in-flight resolve")
	// ErrNoteAncestorIngesting is what a best-effort resolve returns when the
	// document it fetched is already being ingested by an ancestor of the same
	// resolve chain, and no row exists yet to fall back to (#2695).
	//
	// **相乗りした別チェーンにも届きうる** (group は先頭の返り値を配る)。それは
	// 受け取った側の話ではないので、下記の印を見て 1 度だけやり直させる。
	//
	// **取り込みを横取りしない**ためのもの。横取りすると外側の `Create` が
	// UNIQUE に当たって `created=false` になり、呼び出し側が通知とチャートの
	// フックを飛ばす (#2686 と同じ形)。
	//
	// **`errChainLocalVerdict` を包んである。** これは先頭のチェーンの状態から
	// 出した答えで、相乗りしただけの別チェーンには当てはまらない。素で返すと
	// group がそれを追従側へ配り、**引用の renoteId を恒久的に落とす**。
	ErrNoteAncestorIngesting = fmt.Errorf("note is already being ingested by an ancestor of this resolve chain: %w", errChainLocalVerdict)
	// ErrResolveWouldBlock is returned to a best-effort caller that would have
	// had to wait for another chain's in-flight resolve. 待たずに既存行へ落とす。
	ErrResolveWouldBlock = errors.New("resolving would block on another in-flight resolve")
	// errResolveAborted wakes followers when the leader unwound without a
	// result (panic 等)。起こさないと鍵が残り、以降の追従側が上限まで待たされる。
	errResolveAborted = errors.New("in-flight resolve aborted")
)

// resolveJoinTimeout bounds how long **one resolve tree** may spend waiting for
// other chains' in-flight resolves, in total. 先頭側には掛けない (自分の HTTP
// timeout で終わる)。
//
// **join ごとではなく木ごと。** 1 回の解決は著者・返信・引用と何度も待ちうるし、
// 引用チェーンは resolveRecursionLimit まで入れ子になれるので、join ごとに
// 掛けると回数分積み上がる (#2685 review HIGH-2)。予算は木の根で作って枝が
// 共有する (resolveChain.budget)。**時刻の期限ではなく、待ちに費やした時間で
// 減らす** — 理由は resolveChain.budget のコメント。
//
// AP fetch は 1 本 30s で、先頭は著者・返信・引用を順に引くので分単位まで
// 伸びうる。短くすると #2685 が直した「別 worker が取り込み中の引用先を待てずに
// renoteId を落とす」が戻るため、実際の解決より十分長い側に倒してある。ここに
// 掛かるのは循環検出の見落としか、相手方が極端に遅い場合。
//
// var なのはテストから縮めるため。実行時に書き換える経路は無い。
var resolveJoinTimeout = 5 * time.Minute

// resolveRecursionLimit bounds how deep a single resolve operation may recurse
// through note quote chains, matching upstream Resolver.recursionLimit (256)。
const resolveRecursionLimit = 256

// EphemeralSink receives relay-delivered notes and their authors instead of
// the database (#2332)。実装は core/ephemeral.Store。
//
// リレー経由でしか観測しない投稿は、ローカルの誰とも関係が無いまま流れていく
// だけなので DB に入れる価値が無い。それでも保存すると流量に比例して INSERT と
// DELETE が両方発生し、autovacuum まで含めた I/O が二重に掛かる。
type EphemeralSink interface {
	// Enabled は取り込みを ephemeral 経路に倒すかどうか。false なら従来どおり
	// DB へ保存する。sink が配線されているだけで機能が入ってしまわないよう、
	// 既定無効の担保をここで行う。
	Enabled() bool
	PutNote(ctx context.Context, n *model.Note, author *model.User) error
	UserIDByURI(ctx context.Context, uri string) (string, error)
	GetUser(ctx context.Context, id string) (*model.User, error)
	// NoteIDByURI / DropNote は重複解消に使う。同じ投稿が後から直接配送で
	// DB に入ったとき、ephemeral 側を落とさないとタイムラインに 2 回出る。
	NoteIDByURI(ctx context.Context, uri string) (string, error)
	DropNote(ctx context.Context, id, uri string) error
	// GetNote は取り込み時の dedup で既存の ephemeral ノートを返すために使う
	// (#2397)。ID だけでは呼び出し側が note を返せない。
	GetNote(ctx context.Context, id string) (*model.Note, error)
}

// EphemeralTimelineRemover drops a superseded ephemeral note ID from the
// Redis timeline lists (#2332)。実装は core/timeline.FanoutHook。
type EphemeralTimelineRemover interface {
	RemoveNoteID(noteID string, author *model.User, visibility string, host *string)
}

// MaterializeActor creates (or returns) the database row for a remote actor,
// reusing preassignedID when the row does not exist yet (#2332).
//
// ephemeral store から著者を起こすときの入口。ID を据え置くのは、変えると
// Redis に残っている既存ノートが古い ID を指したままになり、ミュートが効かなく
// なるため。鍵ごと正規に作るため actor は fetch し直す。
func (r *Resolver) MaterializeActor(uri, preassignedID string) (*model.User, error) {
	return r.resolveActorWithID(uri, preassignedID)
}

// RelayObservedMarker records that a remote user was first seen via a relay
// (#2340)。実装は repository.RelayObservedUserRepository。
type RelayObservedMarker interface {
	MarkObserved(userID string) error
}

// SetRelayObservedMarker wires the relay-observed recorder. Optional.
func (r *Resolver) SetRelayObservedMarker(m RelayObservedMarker) { r.relayObserved = m }

// SetEphemeralSink wires the ephemeral store. Optional — nil keeps the
// database-only behaviour.
func (r *Resolver) SetEphemeralSink(s EphemeralSink) { r.ephemeralSink = s }

// SetEphemeralTimelineRemover wires the timeline list cleaner used when a
// database row supersedes an ephemeral entry. Optional.
func (r *Resolver) SetEphemeralTimelineRemover(t EphemeralTimelineRemover) { r.ephemeralTimeline = t }

// ephemeralNoteByURI returns the ephemeral note already stored for uri, or nil.
//
// ephemeral 経路は DB に行を作らないので、`noteRepo.FindByURI` の miss は
// 「まだ取り込んでいない」を意味しない。URI 逆引きを併せて引かないと、2 つ目の
// リレーから同じ投稿が届くたびに別 ID を採番してタイムラインに重複する (#2397)。
// 著者側は resolveNoteAuthor が同じ逆引きを既に行っている。
func (r *Resolver) ephemeralNoteByURI(uri string) *model.Note {
	if r.ephemeralSink == nil || uri == "" {
		return nil
	}
	ctx := context.Background()
	id, err := r.ephemeralSink.NoteIDByURI(ctx, uri)
	if err != nil || id == "" {
		return nil
	}
	n, err := r.ephemeralSink.GetNote(ctx, id)
	if err != nil || n == nil {
		return nil
	}
	return n
}

// dropSupersededEphemeral removes an ephemeral entry that the freshly created
// database row supersedes, along with its now-stale ID in the timeline lists.
//
// ephemeral 側の ID と DB 行の ID は別物 (先に ephemeral として採番したものは
// materialize 時にのみ引き継がれる) なので、FTT に残った旧 ID も除かないと
// hydrate で ephemeral 側が拾われ続ける。
//
// It reports whether an ephemeral entry for uri existed.
//
// 戻り値は「ephemeral に同じ投稿があったか」(#3330)。ephemeral に入れた時点で
// note の chart と instance の notesCount は数えてあるので、呼び出し側はこれが
// true なら作成として数え直さない (materialize の EnsureNote が数えないのと
// 同じ)。DropNote が失敗しても true を返す — 数えたのは ephemeral に入れた
// ときで、消せたかどうかとは関係ない。
func (r *Resolver) dropSupersededEphemeral(uri, visibility string, author *model.User) bool {
	if r.ephemeralSink == nil || uri == "" {
		return false
	}
	ctx := context.Background()
	oldID, err := r.ephemeralSink.NoteIDByURI(ctx, uri)
	if err != nil || oldID == "" {
		return false
	}
	if derr := r.ephemeralSink.DropNote(ctx, oldID, uri); derr != nil {
		slog.Warn("federation: failed to drop superseded ephemeral note", "uri", uri, "err", derr)
	}
	if r.ephemeralTimeline != nil && author != nil {
		r.ephemeralTimeline.RemoveNoteID(oldID, author, visibility, author.Host)
	}
	return true
}

// resolveNoteAuthor resolves a note's author, keeping relay-only authors out
// of the database (#2332).
//
// 解決順が重要:
//
//  1. DB を先に引く。ミュート済み / フォロー済み / 過去に materialize 済みの
//     著者は実 ID を使う。ここを飛ばすと、ミュートしたのに ephemeral 側の
//     別 ID を持つ投稿がタイムラインに残り続ける
//  2. ephemeral store の URI 逆引き。同じ著者の 2 件目以降で ID を再利用する。
//     これが無いと投稿ごとに別 ID を採番して同一人物が別人として並ぶ
//  3. どちらにも無ければ通常の ResolveActor で解決する。DB 行は作られるが、
//     ephemeral な著者を作る経路は Phase 2 (materialize) で扱う
func (r *Resolver) resolveNoteAuthor(uri string, ephemeral bool, depth int, chain *resolveChain) (*model.User, error) {
	if !ephemeral || r.ephemeralSink == nil {
		// depth > 0 は「既にノート解決の内側に居る」ことを意味する。ここで
		// featured を引くと、ピン留め → 引用先 → その著者 → その featured …
		// と入れ子になり、1 段ごとに 5 分岐する取得の連鎖になる (#2552)。
		// 入口が depth 0 なので、通常の配送で著者を初めて観測する経路は
		// これまでどおり featured を引く。
		return r.resolveActor(uri, false, depth > 0, chain)
	}
	existing, err := r.userRepo.FindByURI(uri)
	switch {
	case err == nil && existing != nil:
		return existing, nil
	case err != nil && !repository.IsNotFound(err):
		// **引けなかったら ephemeral 側へ落とさない** (#3121)。落とすと、上の
		// 手順 1 が守っている「ミュート済み / materialize 済みの著者は実 ID を
		// 使う」が DB 障害中だけ崩れ、**ミュートした相手の投稿が別 ID の
		// ephemeral 行としてタイムラインに戻る**。retry に倒すほうが安全側。
		return nil, fmt.Errorf("%w: note author %q: %v", ErrLookupUnavailable, truncateRunes(uri, userURIMaxRunes), err)
	}
	ctx := context.Background()
	if id, err := r.ephemeralSink.UserIDByURI(ctx, uri); err == nil && id != "" {
		if u, uerr := r.ephemeralSink.GetUser(ctx, id); uerr == nil && u != nil {
			return u, nil
		}
	}
	// 3. どこにも居なければ fetch して **DB に入れずに** 組み立てる。
	//
	// ここを ResolveActor に任せると、ノートを Redis に逃がしても著者だけが
	// DB に積み上がる。実測でリレー購読後は note と user がほぼ 1:1 で増える
	// ため、著者を止めないと肥大化を抑える目的が半分しか達成できない。
	return r.resolveActorEphemeral(uri, chain)
}

// resolveActorEphemeral fetches an actor and builds the row **without
// persisting it** (#2332)。
//
// singleflight key を通常経路と分けるのは、同一 URI に対する DB 経路の解決と
// 混ざると片方が意図しない層へ書かれるため (note 側と同じ理由)。
func (r *Resolver) resolveActorEphemeral(uri string, chain *resolveChain) (*model.User, error) {
	key := "eph\x00" + crossHostKey(uri, false)
	chain = chain.ensureTree()
	v, err := r.joinResolve(&r.resolveActorGroup, chain, actorWaitKey(key), key, func() (any, error) {
		// ephemeral な行は DB に載らないので featured の取り込み対象外。
		return r.resolveActorOnceWithID(uri, false, "", true, false, true, chain)
	})
	if err != nil {
		return nil, err
	}
	if v == nil {
		return nil, ErrInvalidActor
	}
	return v.(*model.User), nil
}

// DefaultActorTTL is the default duration after which a cached actor (and its
// public key) is considered stale and refetched on next access.
const DefaultActorTTL = 24 * time.Hour

// publicKeyEntry stores a cached PEM together with the time it was fetched so
// the resolver can detect TTL expiry.
type publicKeyEntry struct {
	pem       string
	fetchedAt time.Time
}

// PublickeyStore abstracts persistence of remote actor public keys. Resolver
// uses this to fall back to the database when the in-memory cache misses.
type PublickeyStore interface {
	Upsert(pk *model.UserPublickey) error
	FindByUserID(userID string) (*model.UserPublickey, error)
}

// PublickeyExtraStore abstracts persistence of *additional* remote actor
// public keys (Ed25519 / Multikey) for FEP-521a compliant peers. Resolver
// upserts here when an inbound actor JSON includes `assertionMethod[]`
// (#1067 / #1070). `user_publickey_extra` table backs this in production;
// tests use an in-memory mock.
//
// DeleteByKeyID は resolver が cacheAssertionMethods で actor JSON から消えた
// 旧 keyId を purge するために使う。これが無いと remote が Ed25519 鍵を
// rotate した後も古い鍵が user_publickey_extra に残り、compromised key で
// の rogue verify を許してしまう (#1070 follow-up)。
type PublickeyExtraStore interface {
	Upsert(pk *model.UserPublickeyExtra) error
	// FindByUserAndKeyID は (userID, keyID) で actor-scope に鍵を引く。keyID 単独の
	// global lookup (FindByKeyID) は、攻撃者 actor が assertionMethod に victim の
	// keyId を載せて鍵を植え込み victim を verify する key confusion を許すため、
	// 意図的に interface へ公開しない (#security: HTTP-sig key confusion)。
	FindByUserAndKeyID(userID, keyID string) (*model.UserPublickeyExtra, error)
	ListByUserID(userID string) ([]model.UserPublickeyExtra, error)
	DeleteByKeyID(userID, keyID string) error
}

// Resolver fetches remote actors / notes and persists them in the local
// user / note tables.
//
// 公開鍵は in-memory + DB (user_publickey テーブル) の二段キャッシュで管理
// する。エントリは actorTTL を超えると miss として扱い、次回 ResolveActor 時
// にリフレッシュされる。
type Resolver struct {
	// background は新しく作ったノートの chart hook を起動する箱 (#3661)。nil なら素の
	// goroutine で起動する (テスト)。**本番で nil だと、世代を止めても新しいノートの
	// chart hook の goroutine を待たず、閉じた DB へ書きに行く。** 構築子の必須の引数
	// (#3833。以前は setter と起動時の検査)。
	background *bgwork.Group
	// rules は連合のルール (#3090)。nil なら評価しない。
	rules                   RuleEvaluator
	userRepo                repository.UserRepository
	noteRepo                repository.NoteRepository
	localFederationObserver activitypub.LocalFederationObserver
	localReferenceCache     localReferenceCache
	urls                    *activitypub.URLBuilder
	fetcher                 HTTPFetcher
	idGen                   id.Generator
	// keysMu は keys map (userID → publicKey + fetchedAt) の concurrent
	// access を保護する。queue worker や inbox handler は別 actor を並行
	// 処理するため、ロック無しの map read/write は runtime panic を起こす
	// (Devin review #555 FLAG-1)。
	keysMu sync.RWMutex
	keys   map[string]publicKeyEntry // userID → publicKey + fetchedAt
	// keysGen は DropLocal が keys を捨てるたびに進める (#3732)。keysMu で保護する。
	// DB から読んでいる間に捨てられたとき、読んだ鍵を捨てた後に積まないために使う。
	keysGen uint64
	// keyFetchFailures は refreshPublicKey が失敗した時刻 (userID → 時刻)。
	// keysMu で保護する。keyFetchBackoff の説明を参照。
	keyFetchFailures map[string]time.Time
	// missingExtra は user_publickey_extra に行が無かった (userID, keyId) の記録
	// (#3610)。missingExtraKeyTTL の説明を参照。
	missingExtra missingExtraKeys
	clock        func() time.Time // テストで差し替える時計
	actorTTL     time.Duration    // アクター情報の最大寿命
	// mentionFetch は受信ノートの未知の actor の取得の同時実行数と失敗の記憶
	// (inbox の worker 全体で共有する)。
	mentionFetch *mentionFetchGuard
	// moveProcessor はリモートアカウント移行の検知時に呼ぶ引き継ぎ処理
	// (#2414)。実体は core/move.Service。nil なら移行を検知しても何もしない。
	moveProcessor      RemoteMoveProcessor
	instanceTracker    InstanceTracker             // optional: ホスト発見を通知
	chartHook          ChartHook                   // optional: 新規 remote user の集計
	noteChartHook      NoteChartHook               // optional: 新しく取り込んだ note の集計 (#3330)
	hashtagHook        HashtagHook                 // optional: per-tag mentionedUsersCount 集計 (#680)
	publickeyRepo      PublickeyStore              // optional: 公開鍵の永続化 (RSA)
	publickeyExtraRepo PublickeyExtraStore         // optional: 追加公開鍵 (Ed25519 / Multikey) の永続化
	capabilities       SignatureCapabilityDeclarer // optional: Ed25519 対応宣言の記録 (#2393)
	pollRepo           repository.PollRepository   // optional: Question(投票)のPoll作成
	// suspensionOriginRepo は凍結の由来 (local / remote) の記録先 (#2973)。
	// **nil なら `toot:suspended` を読まない** — 由来を持てない状態で自動
	// 凍結すると、モデレーターが `admin/unsuspend-user` で解除しても次の actor
	// refresh で無言で戻る (#2951 の既知の制約そのもの)。本番で nil だと、リモート
	// actor の凍結を読まなくなる。構築子の必須の引数 (#3833。以前は setter と起動時の検査)。
	suspensionOriginRepo repository.UserSuspensionOriginRepository
	// pinningRepo / pinningIDGen はリモート actor の featured コレクション
	// (ピン留め投稿) の取り込み先 (#2552)。未配線なら取り込まない。
	pinningRepo   repository.UserNotePiningRepository
	pinningIDGen  id.Generator
	pollVoter     PollVoter                      // optional: AP vote (Note.name) の投票記録
	emojiRepo     repository.EmojiRepository     // optional: リモート絵文字の永続化
	driveFileRepo repository.DriveFileRepository // optional: リモート添付の link 化
	// ephemeralSink はリレー経由でしか観測しない投稿の置き場 (#2332)。
	// 設定が有効かつ配線されているときだけ、DB ではなくこちらへ書く。
	ephemeralSink EphemeralSink
	// relayObserved はリレー経由で初めて観測した remote user の記録先 (#2340)。
	relayObserved RelayObservedMarker
	// ephemeralTimeline は DB 行が ephemeral を上書きしたときに FTT から旧 ID
	// を除くためのもの。残すと hydrate で ephemeral 側が拾われ二重表示になる。
	ephemeralTimeline EphemeralTimelineRemover
	// attachmentProbeClient は添付の先頭取得 (#461 / #3243) で使う outbound
	// HTTP client。SSRF-safe transport (router.go で safehttp の transport を
	// 適用したもの) を渡す前提で、未設定なら取得自体をスキップする (安全側に
	// 倒す: SSRF リスクを起こすくらいなら申告と推測だけで登録する)。
	attachmentProbeClient *http.Client
	// probeBudget は 1 document 分の先頭取得に許す合計時間。0 なら
	// attachmentProbeBudget を使う (テストだけが縮める)。
	probeBudget time.Duration
	// hostBlocker は federation 設定 (none / specified / blockedHosts) を
	// 評価する gate。fetchActor / resolveNoteOnce / IngestNoteWithCreated
	// の入口で URI の host / attributedTo の host を判定して、ホワイト
	// リスト外 host からの取り込みを拒否する。meta.prohibitedWords と
	// meta.sensitiveWords もここから読む (ResolverHostBlocker)。
	//
	// **nil なら legacy 挙動 (全 host 許可) にフォールバックし、禁止語と
	// センシティブワードの判定も skip する** (テスト)。本番で nil だと、federation
	// 設定を無視してリモートを解決し、リモートから届くノートに禁止語のフィルタが
	// 掛からず、センシティブワードに当たる public のノートが home にならない
	// (どれもローカル投稿にだけ効く状態)。構築子の必須の引数 (#3833。以前は setter と
	// 起動時の検査、#2683)。
	hostBlocker ResolverHostBlocker
	// softwareBlocker は受信を拒否するソフトウェア (meta.inboxSuspendedSoftware、
	// #3362) の判定。hostAllowed でドメインブロックと同じ入口に当てる (#3563)。
	// inbox だけで判定していた頃は、拒否していないサーバーのリノートや返信の
	// 連鎖、ap/show で、拒否中のホストの投稿を取り込めた。nil なら判定しない
	// (本番で nil だと、その形で取り込む)。構築子の必須の引数 (#3833)。
	softwareBlocker SoftwareSuspensionChecker

	// mediaSilencedChecker は meta.mediaSilencedHosts の判定 (#3218)。未配線なら
	// 添付をセンシティブにしない。
	mediaSilencedChecker MediaSilencedHostChecker
	// silencedChecker は remote note ingest 時に meta.silencedHosts 該当 host の
	// public note を home に降格する判定に使う (#2106 N14)。nil なら降格しない
	// (本番で nil だと、silenced instance の remote public note が public timeline に
	// 出る)。構築子の必須の引数 (#3833。以前は setter と起動時の検査、#2708)。
	silencedChecker SilencedHostChecker
	// rolePolicyProvider は受信した note の mentionLimit を投稿者の role policy
	// から引くのに使う (#3330)。nil なら corenote.DefaultMentionLimit (本番で nil
	// だと、base policy やリモートに当たるロールで絞った上限が効かない)。構築子の
	// 必須の引数 (#3833)。
	rolePolicyProvider RolePolicyProvider

	// resolveActorGroup / resolveNoteGroup は同一 URI への並行 ResolveActor /
	// ResolveNote 呼び出しを 1 度の DB lookup + HTTP fetch に collapse する
	// (#300 3-7)。inbox 受信時に同じ remote actor / note を参照する activity
	// が連続して届く現実的なケースで thundering herd を抑える。
	resolveActorGroup resolveGroup
	resolveNoteGroup  resolveGroup
	// waits は cross-goroutine の待ち循環を検出する (#2685 review HIGH-1)。
	// チェーンローカルの判定は自分の祖先しか見ないので、これが無いと相互引用で
	// worker が永久に止まる。
	waits resolveWaits
	// 解決の in-flight は resolveChain が持つ (#2685)。以前は Resolver に
	// sync.Map を 2 つ (ingesting / resolvingNotes) 置いていたが、プロセス全体で
	// 共有されるため「自分の祖先が握っている」と「無関係な goroutine が握って
	// いる」を区別できなかった。
}

// NewResolver constructs a Resolver.
// noteRepo / urls はリモート Note の解決と取り込みに使用する。リモート Note
// 機能を使わない呼び出し側 (テスト等) では nil を渡してよい。
//
// hostBlocker 以降の依存もテストでは nil を渡せる (それぞれのフィールドの説明の
// とおりに判定が外れる)。本番は hostBlocker と silencedChecker に
// core/instance.Service を渡す。
func NewResolver(
	userRepo repository.UserRepository,
	noteRepo repository.NoteRepository,
	urls *activitypub.URLBuilder,
	fetcher HTTPFetcher,
	idGen id.Generator,
	hostBlocker ResolverHostBlocker,
	softwareBlocker SoftwareSuspensionChecker,
	silencedChecker SilencedHostChecker,
	rolePolicyProvider RolePolicyProvider,
	suspensionOriginRepo repository.UserSuspensionOriginRepository,
	background *bgwork.Group,
) *Resolver {
	return &Resolver{
		userRepo: userRepo,
		noteRepo: noteRepo,
		urls:     urls,
		fetcher:  fetcher,
		idGen:    idGen,
		keys:     map[string]publicKeyEntry{},
		clock:    time.Now,
		actorTTL: DefaultActorTTL,

		hostBlocker:          hostBlocker,
		softwareBlocker:      softwareBlocker,
		silencedChecker:      silencedChecker,
		rolePolicyProvider:   rolePolicyProvider,
		suspensionOriginRepo: suspensionOriginRepo,
		background:           background,

		mentionFetch: newMentionFetchGuard(inboundMentionFetchSlots),
	}
}

// SetClock replaces the clock used for TTL checks. Intended for tests.
func (r *Resolver) SetClock(now func() time.Time) {
	if now != nil {
		r.clock = now
	}
}

// SetActorTTL overrides the actor TTL used for cache freshness checks.
// 0 以下を渡した場合は変更しない。
func (r *Resolver) SetActorTTL(d time.Duration) {
	if d > 0 {
		r.actorTTL = d
	}
}

// SetInstanceTracker attaches an InstanceTracker that will be notified each
// time a remote actor is fetched (created or refreshed). nil 渡しは無効化と
// 同義。
func (r *Resolver) SetInstanceTracker(t InstanceTracker) {
	r.instanceTracker = t
}

// SetChartHook attaches a ChartHook invoked after a remote user has
// been freshly created. nil 渡しは無効化と同義。
func (r *Resolver) SetChartHook(h ChartHook) {
	r.chartHook = h
}

// SetNoteChartHook attaches a NoteChartHook invoked for every note this
// resolver newly creates (inbound Create, and notes fetched while resolving
// replies, quotes, Announce targets or featured collections). nil disables it.
func (r *Resolver) SetNoteChartHook(h NoteChartHook) {
	r.noteChartHook = h
}

// fireNoteCreated runs the note chart hook for a freshly created note.
//
// 本家は ApNoteService.createNote → NoteCreateService.create を通る全経路で
// notes chart と instance の notesCount を数える。以前の mk-go は inbox の
// Create を処理する processor だけが hook を呼んでいたので、返信元・引用・
// Announce の対象などを解決のついでに取り込んだ投稿は数えず、その削除
// (DeleteService) では引いていた (#3330)。作成の数え方をここ 1 か所に寄せ、
// 削除と経路を揃える。dedup hit (created=false) では呼ばない。
func (r *Resolver) fireNoteCreated(note *model.Note) {
	if r.noteChartHook == nil || note == nil {
		return
	}
	safeGoFedHook(r.background, func() { r.noteChartHook.OnNoteCreated(note) })
}

// incrementAuthorNotesCount adds one to the author's notesCount after a
// remote note has been stored in the database.
func (r *Resolver) incrementAuthorNotesCount(userID string) {
	if r.userRepo == nil {
		return
	}
	if err := r.userRepo.IncrementNotesCount(userID, 1); err != nil {
		slog.Warn("federation: failed to increment notesCount", "user", userID, "err", err)
	}
}

// SetHashtagHook attaches a HashtagHook invoked after a remote note has
// been ingested or updated, so the hashtag table mentionedUsersCount /
// mentionedUserIds arrays stay in sync with /api/hashtags/list ranking
// (#680)。nil 渡しは無効化と同義。
func (r *Resolver) SetHashtagHook(h HashtagHook) {
	r.hashtagHook = h
}

// SetPublickeyRepo attaches a PublickeyStore for persistent public key
// storage. nil 渡しは無効化と同義 (in-memory only に戻る)。
func (r *Resolver) SetPublickeyRepo(repo PublickeyStore) {
	r.publickeyRepo = repo
}

// SignatureCapabilityDeclarer records that a remote host declared Ed25519
// support by publishing an assertionMethod Multikey (#2393). 実装は
// repository.InstanceSignatureCapabilityRepository。
type SignatureCapabilityDeclarer interface {
	RecordEd25519Declared(host string, at time.Time) error
}

// SetSignatureCapabilityDeclarer attaches the store that records per-host
// Ed25519 capability declarations (#2393). 未配線なら宣言は記録されない。
func (r *Resolver) SetSignatureCapabilityDeclarer(d SignatureCapabilityDeclarer) {
	r.capabilities = d
}

// SetPublickeyExtraRepo attaches a PublickeyExtraStore for persistent
// additional public key storage (FEP-521a Multikey / Ed25519). nil 渡しは
// 無効化と同義: ResolveActor は actor の assertionMethod[] を parse せず、
// PublicKeyForKeyID は user_publickey (RSA) のみを返す (#1067 / #1070)。
func (r *Resolver) SetPublickeyExtraRepo(repo PublickeyExtraStore) {
	r.publickeyExtraRepo = repo
}

// SetPollRepo attaches a PollRepository for ingesting Question (poll) objects.
func (r *Resolver) SetPollRepo(repo repository.PollRepository) {
	r.pollRepo = repo
}

// SetUserRepo replaces the user repository used by resolver lookups.
func (r *Resolver) SetUserRepo(repo repository.UserRepository) {
	r.userRepo = repo
}

// SetNoteRepo replaces the note repository used by resolver lookups.
func (r *Resolver) SetNoteRepo(repo repository.NoteRepository) {
	r.noteRepo = repo
}

// resolveSuspensionChange decides whether to follow the actor's `suspended`.
//
// **ローカルの判断には触らない。** Mastodon の `ProcessAccountService#set_suspension!`
// と同じ形で、最初に由来を見て local なら何もしない:
//
//	return if @account.suspended? && @account.suspension_origin_local?
//
// 由来が remote (= こちらが actor を見て凍結した) なら、発信元の解除にも追従する。
// 記録が無い行 (この表より前から凍結されている / まだ一度も判断していない) は
// **local 扱い**にする — モデレーターが凍結した可能性があるものをリモートに
// 解除させないため。安全側に倒しても、誤って local にした行はモデレーターが
// 手で戻せる。
//
// 2 つ目の戻り値は「変更するかどうか」。false なら呼び出し元は触らない。
func (r *Resolver) resolveSuspensionChange(existing *model.User, actorSuspended bool) (bool, bool) {
	if r.suspensionOriginRepo == nil {
		// 由来を記録できないなら読まない (#2951 の制約を再生産しない)。
		return false, false
	}
	if existing.IsSuspended == actorSuspended {
		return false, false
	}
	// **削除済みの行には触らない** (#2973)。`admin/accounts/delete` は
	// `isSuspended` と `isDeleted` を同時に立てるので、発信元由来の凍結が
	// 残っている tombstone を発信元が解除できてしまう。inbound の gate は
	// `isSuspended` と `isDeleted` の両方を見る (`processor.go` の dispatch 前
	// チェック) が、ここで凍結を外させてはいけない。読み取り側 (packer や
	// timeline) は `isSuspended` を見て隠すので、外れると削除したアカウントが
	// 見えるようになる。
	if existing.IsDeleted {
		return false, false
	}
	origin, err := r.suspensionOriginRepo.Origin(existing.ID)
	if err != nil {
		// **DB 障害では判断しない。** 「記録が無い」と取り違えると、
		// 接続断の瞬間にモデレーターの判断を上書きしうる。
		slog.Warn("federation: failed to read suspension origin; leaving state untouched",
			"userId", existing.ID, "err", err)
		return false, false
	}
	if actorSuspended {
		// 凍結する側: 記録が無い = まだ誰も判断していないので従う。
		// local なら「モデレーターが解除した」ので従わない。
		if origin == model.SuspensionOriginLocal {
			return false, false
		}
		return true, true
	}
	// 解除する側: **remote 由来のときだけ従う。** 記録が無い行を解除すると、
	// この表より前にモデレーターが凍結したものをリモートが解除できてしまう。
	if origin != model.SuspensionOriginRemote {
		return false, false
	}
	return false, true
}

// SetPinningRepo wires the pinned-notes store used to import a remote actor's
// featured collection (#2552). 未配線なら featured の取り込み自体を行わない。
func (r *Resolver) SetPinningRepo(repo repository.UserNotePiningRepository, idGen id.Generator) {
	r.pinningRepo = repo
	r.pinningIDGen = idGen
}

// PollVoter records a poll vote on behalf of a remote actor when an
// inbound AP Note carries `name` + `inReplyTo` to a poll-bearing note
// (Misskey TS の vote AP wire format)。実装は core/poll.Service。循環依存
// 回避のため interface で受け取る。nil 配線時は federation 経由 vote が
// 無視される (legacy / 元バグ挙動)。
type PollVoter interface {
	Vote(user *model.User, noteID string, choice int) error
}

// SetPollVoter attaches a PollVoter so AP votes are routed to the local
// poll service instead of being mistakenly persisted as reply notes (#690)。
func (r *Resolver) SetPollVoter(v PollVoter) {
	r.pollVoter = v
}

// SetEmojiRepo attaches an EmojiRepository for extracting and upserting
// remote custom emoji from AP Person/Note Tag arrays (#330).
func (r *Resolver) SetEmojiRepo(repo repository.EmojiRepository) {
	r.emojiRepo = repo
}

// SetAttachmentProbeClient attaches an SSRF-safe *http.Client used to read
// the leading bytes of remote attachments (type, filename and image
// dimensions; #461 / #3243). The supplied client must wrap a transport with
// safehttp.NewSSRFSafeTransport(...) — otherwise a malicious remote can point
// a Document URL at internal addresses (cloud metadata, localhost services).
// nil 渡しは無効化と同義で、その場合は AP の申告と URL からの推測だけで登録する。
func (r *Resolver) SetAttachmentProbeClient(client *http.Client) {
	r.attachmentProbeClient = client
}

// SetDriveFileRepo attaches a DriveFileRepository for ingesting AP
// `attachment` arrays into drive_file rows (#378). 未設定なら attachment
// は無視される (旧挙動)。
func (r *Resolver) SetDriveFileRepo(repo repository.DriveFileRepository) {
	r.driveFileRepo = repo
}

// ResolverHostBlocker is what the resolver reads from the instance settings:
// the federation policy (federation: none / specified、blockedHosts) and
// meta.prohibitedWords / meta.sensitiveWords. *instance.Service implements it.
//
// **禁止語とセンシティブワードも、引数の型で求める (#3833)。** 以前は
// HostBlockChecker を受けて optional interface で拾い、持たない型を渡すと
// 静かに無検査へ落ちていた (レビュー L2)。起動時の検査で見ていたのを、
// 持たない型を渡すとコンパイルで落ちる形にした。
type ResolverHostBlocker interface {
	HostBlockChecker
	ProhibitedWordsProvider
	SensitiveWordsProvider
}

// SoftwareSuspensionChecker reports whether content from a host must be
// refused because the instance runs software listed in
// meta.inboxSuspendedSoftware (Elythia 独自、#3362)。実装は
// NewInboxSuspendedChecker。
type SoftwareSuspensionChecker interface {
	IsSuspended(host string) bool
}

// SilencedHostChecker reports whether a remote host is in meta.silencedHosts.
// *instance.Service implements it (IsSilenced).
type SilencedHostChecker interface {
	IsSilenced(host string) bool
}

// MediaSilencedHostChecker reports whether a host is in meta.mediaSilencedHosts.
type MediaSilencedHostChecker interface {
	IsMediaSilenced(host string) bool
}

// SetMediaSilencedHostChecker attaches the meta.mediaSilencedHosts checker
// used to mark remote attachments sensitive (#3218).
func (r *Resolver) SetMediaSilencedHostChecker(c MediaSilencedHostChecker) {
	r.mediaSilencedChecker = c
}

// isMediaSilencedHost reports whether host is in meta.mediaSilencedHosts
// (未配線 / host 無しは false)。
func (r *Resolver) isMediaSilencedHost(host *string) bool {
	return host != nil && *host != "" && r.mediaSilencedChecker != nil && r.mediaSilencedChecker.IsMediaSilenced(*host)
}

// noteEmojisFor returns the custom emojis a note may use.
//
// upstream の NoteCreateService は、投稿者のホストがメディアサイレンス対象なら
// 投稿の emojis を空にする (#3220。`NoteCreateService.ts:607`)。絵文字の行は
// その前に ApNoteService.extractEmojis が作るので、`upsertEmojis` は呼んだまま
// 投稿に載せる名前だけを落とす。**空は nil ではなく空配列** (note.emojis は
// NOT NULL で、Updates の map 経由だと nil が NULL になる)。
func (r *Resolver) noteEmojisFor(emojis model.StringArray, host *string) model.StringArray {
	if r.isMediaSilencedHost(host) {
		return model.StringArray{}
	}
	return emojis
}

// markMediaSilencedFiles marks the attachments of a note by a media-silenced
// host sensitive.
//
// upstream は `DriveService.addFile` で、リンクだけのリモートファイルにも
// `isMediaSilencedHost` を当てて `isSensitive = true` にする (#3218)。mk-go の
// 添付は `upsertAttachments` が drive を通さずに作るので、ここで同じことをする。
// `maybeSensitive` は upstream でも検出の結果なので触らない。
//
// 書き換えるのは投稿者自身の行と、**持ち主のホストもメディアサイレンス対象の行**
// (本来センシティブであるべき行なので、書き換えを悪用される経路にならない)。
// upstream は投稿者ごとに行を作る (`addFile` の dedup は md5 + userId) ので必ず
// 投稿者の行に当たるが、mk-go は URL で再利用するので他人の行を指しうる。他人の
// 行は書き換えない (#3090 と同じ理由。URL を指すだけで他人のファイルを書き換え
// られる)。**書き換えられない添付が残ったら false を返す** — 呼び出し側は投稿ごと
// 空の CW で畳む (そのままだと他人の URL を指すだけでメディアサイレンスを
// すり抜けられる)。再利用する投稿者自身の行にも当てるのは upstream に無い
// 挙動で、設定する前に取り込んだ画像を同じ人が添付し直すとそのまま表示される
// のを防ぐ。
func (r *Resolver) markMediaSilencedFiles(ids model.StringArray, authorID string, host *string) bool {
	if !r.isMediaSilencedHost(host) {
		return true
	}
	ownerSilenced := func(f *model.DriveFile) bool {
		return f.UserHost != nil && *f.UserHost != "" && r.mediaSilencedChecker.IsMediaSilenced(*f.UserHost)
	}
	return r.markAuthorFiles(ids, authorID, ownerSilenced, map[string]any{"isSensitive": true})
}

// ProhibitedWordsProvider exposes meta.prohibitedWords to the inbound note
// ingest path. *instance.Service implements it.
type ProhibitedWordsProvider interface {
	ProhibitedWords() []string
}

// SensitiveWordsProvider exposes meta.sensitiveWords to the inbound note
// ingest path. *instance.Service implements it.
type SensitiveWordsProvider interface {
	SensitiveWords() []string
}

// sensitiveWords returns the configured meta.sensitiveWords, or nil when no
// hostBlocker is wired.
func (r *Resolver) sensitiveWords() []string {
	if r.hostBlocker == nil {
		return nil
	}
	return r.hostBlocker.SensitiveWords()
}

// containsSensitiveWords reports whether cw or text hits meta.sensitiveWords.
//
// ローカル投稿 (`core/note.matchesSensitiveWords`) と同じく、cw と text を別々に
// 判定し (#1106)、欄の中の URL を表示される形に直したもの (mfm.KeywordCheckTarget)
// も同じ欄の一部として見る。
func (r *Resolver) containsSensitiveWords(text, cw *string) bool {
	words := r.sensitiveWords()
	if len(words) == 0 {
		return false
	}
	if cw != nil && *cw != "" && keyword.IsKeyWordIncluded(mfm.KeywordCheckTarget(*cw), words) {
		return true
	}
	return text != nil && *text != "" && keyword.IsKeyWordIncluded(mfm.KeywordCheckTarget(*text), words)
}

// prohibitedWords returns the configured meta.prohibitedWords, or nil when no
// hostBlocker is wired / meta is unreadable (= 判定を skip、他の meta ゲートと同じ
// ベストエフォート)。
func (r *Resolver) prohibitedWords() []string {
	if r.hostBlocker == nil {
		return nil
	}
	return r.hostBlocker.ProhibitedWords()
}

// containsProhibitedWords reports whether cw / text / poll choices hit
// meta.prohibitedWords.
//
// 判定そのものは `internal/misc/keyword` (= `core/note.checkProhibitedWords` と
// `matchesSensitiveWords` が使うのと同じ実装) に委ねる。`/regex/flags` と
// スペース区切り AND、case-sensitive の扱いをローカル投稿経路と揃えるため。
// 検査対象の 3 つ (cw / text / poll choices) も upstream の
// `checkProhibitedWordsContain` と同じ。cw と text は、欄の中の URL を表示される形に
// 直したもの (mfm.KeywordCheckTarget) も同じ欄の一部として判定する (本家と同じ)。
func (r *Resolver) containsProhibitedWords(text, cw *string, pollChoices []string) bool {
	words := r.prohibitedWords()
	if len(words) == 0 {
		return false
	}
	if cw != nil && *cw != "" && keyword.IsKeyWordIncluded(mfm.KeywordCheckTarget(*cw), words) {
		return true
	}
	if text != nil && *text != "" && keyword.IsKeyWordIncluded(mfm.KeywordCheckTarget(*text), words) {
		return true
	}
	for _, choice := range pollChoices {
		if choice != "" && keyword.IsKeyWordIncluded(choice, words) {
			return true
		}
	}
	return false
}

// apPollChoices returns the AP Question's choice labels in the same normalized
// form createPollFromQuestion would store, so the prohibited-word check sees
// what the row would hold.
func apPollChoices(apNote *activitypub.Note) []string {
	choices := apNote.OneOf
	if len(apNote.AnyOf) > 0 {
		choices = apNote.AnyOf
	}
	if len(choices) == 0 {
		return nil
	}
	out := make([]string, 0, len(choices))
	for _, c := range choices {
		out = append(out, remotePollChoice(c.Name))
	}
	return out
}

// selfHost returns this instance's own host in the same normalized form
// hostFromURI produces, or "" when the URL builder is unwired.
func (r *Resolver) selfHost() string {
	if r.urls == nil {
		return ""
	}
	// UserURI("") = `<baseURL>/users/`。URLBuilder は baseURL を公開しないので
	// 既存の組み立てから取り出す。正規化は host を作る唯一の規則を通す。
	return NormalizeGateHost(r.urls.UserURI(""))
}

// isSelfHost reports whether host is this instance's own host.
//
// **host で判定する (URI の接頭辞ではない)。** upstream も
// `host === toPuny(config.host)` で見る。接頭辞一致 (`urls.IsLocalURI`) だと
// scheme 違い (`http://` を名乗る) で素通りし、その後の host binding は
// 通ってしまうので shadow 行が作られる。
func (r *Resolver) isSelfHost(host string) bool {
	self := r.selfHost()
	return self != "" && host != "" && host == self
}

// isSelfHostURI reports whether uri is served by this instance.
func (r *Resolver) isSelfHostURI(uri string) bool {
	host, err := hostFromURI(uri)
	if err != nil {
		return false
	}
	return r.isSelfHost(host)
}

// hostAllowed reports whether the resolver may talk to / persist content
// authored on host: the federation policy (blockedHosts / federation mode)
// and meta.inboxSuspendedSoftware. host == "" (= local) は常に true。未配線時も true。
func (r *Resolver) hostAllowed(host string) bool {
	return r.hostPolicyAllowed(host) && r.hostSoftwareAllowed(host)
}

// hostPolicyAllowed applies only the federation policy (blockedHosts /
// federation mode). 判定はメモリ上で済む。
func (r *Resolver) hostPolicyAllowed(host string) bool {
	if host == "" || r.hostBlocker == nil {
		return true
	}
	return !r.hostBlocker.IsBlocked(host) && r.hostBlocker.IsAllowed(host)
}

// hostSoftwareAllowed applies only meta.inboxSuspendedSoftware.
//
// 受信を拒否するソフトウェアのホストも、ドメインブロックと同じく取り込まない
// (#3563)。既に DB にある行は、ドメインブロックと同じく呼び出し側で素通しになる。
// こちらは instance の行を引く (meta はキャッシュ) ので、DB の照合で済む呼び出し側
// (fetchUnknownMentionActors) は既知の行を除いてから呼ぶ。
func (r *Resolver) hostSoftwareAllowed(host string) bool {
	return host == "" || r.softwareBlocker == nil || !r.softwareBlocker.IsSuspended(host)
}

// hostAllowedForURI parses uri and applies hostAllowed. URI 解析不可は別
// 経路で reject される前提で許容 (= true) する。
func (r *Resolver) hostAllowedForURI(uri string) bool {
	host, err := hostFromURI(uri)
	if err != nil {
		return true
	}
	return r.hostAllowed(host)
}

// RemoteMoveProcessor carries a moved account's followers and relationships
// over to its destination. Implemented by core/move.Service.
//
// federation → core/move の一方向依存なので循環しない。interface で受けるのは
// テストで差し替えるため。
type RemoteMoveProcessor interface {
	PostMoveProcess(src, dst *model.User)
}

// SetMoveProcessor wires the handler invoked when a remote account is observed
// to have moved.
func (r *Resolver) SetMoveProcessor(p RemoteMoveProcessor) { r.moveProcessor = p }
