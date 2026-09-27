package federation

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/shiroha-a/mk/internal/activitypub/ld"
	"github.com/shiroha-a/mk/internal/repository"
)

// LDSignatureVerifier verifies the optional LD-Signature on an inbound
// activity. mk-go では従来 HTTP Signature のみで認証していたが、relay 経由
// 配送等で original creator を確認する経路で LD-Signature (RsaSignature2017)
// が必要になる。
//
// 本 verifier は activity body の `signature` field を gate に動作する:
//   - signature 無し → skip (nil 返却、HTTP Signature だけで処理続行)
//   - signature 有り + verify pass → nil
//   - signature 有り + verify fail → error (caller は activity を drop する)
//
// upstream Misskey TS 2026.5.4 の InboxProcessorService.process は、HTTP 署名者と
// activity.actor が一致しない (= 転送された) activity に対して
// `delete signature → compact → checkForForbiddenDirectives → freeze →
// verifyRsaSignature2017` を適用し、**compact 後の activity を以降の処理に使う**。
// mk-go も同じく compact 済みの文書を返し、呼び出し側 (InboxProcessor) はそれを
// dispatch する (VerifyAndCompact 参照)。
type LDSignatureVerifier struct {
	pubkeyRepo repository.UserPublickeyRepository
}

// VerifiedLDActivity is the result of a successful LD-Signature verification.
type VerifiedLDActivity struct {
	// Creator is the verified `signature.creator` key URI.
	Creator string
	// Body is the activity compacted into ld.InboxCompactContext with the
	// original `signature` re-attached. Only properties covered by the
	// signature survive under their short names, so callers that authenticate
	// an activity by its LD-Signature must process Body instead of the raw
	// request body.
	Body []byte
}

// ldSignatureMaxAge / ldSignatureMaxFuture bound `signature.created`.
//
// **upstream にこの検査は無い** (docs/divergence.md)。LD-Signature は本文だけを
// 縛り、HTTP 署名の Date のような鮮度を持たない。転送経路では HTTP 署名は
// 転送者 (= 攻撃者でもありうる) のものなので、一度受け取った署名付き activity は
// 何年後でも投げ直せる (削除済みノートの Create を再送して復活させる、古い
// Update で本文を巻き戻す等)。replay guard (inbox_replay.go) は 15 分しか覚えない。
//
// 窓の根拠: `created` は署名した時刻 (Mastodon の LinkedDataSignature#sign! も
// upstream の signRsaSignature2017 も配送用に render した時点の now) で、転送者は
// 元の body をそのまま流すので、届くまでの遅延は元サーバーの配送 retry と
// 転送者の retry の和になる。upstream の deliver は既定 12 回・間隔は倍々で最大
// 8 時間なので、retry を使い切るまでおよそ 1.4 日 (jitter 込みで 1.6 日、
// `httpRelatedBackoff`)。7 日を超えて届く転送活動は受信側が長く落ちていた場合
// くらいで、それを落とす損失より無期限の replay を塞ぐ利益を取る。未来側は
// 時計のずれだけを許す。窓の内側の replay は塞いでいない (docs/divergence.md)。
const (
	ldSignatureMaxAge    = 7 * 24 * time.Hour
	ldSignatureMaxFuture = time.Hour
)

// ErrLDSignatureExpired is returned when `signature.created` is outside the
// accepted window (see ldSignatureMaxAge).
var ErrLDSignatureExpired = errors.New("ld-sig: signature.created outside the accepted window")

// NewLDSignatureVerifier returns a verifier wired to the supplied
// user_publickey repository. signature.creator (key URI) から keyId 一致する
// row を引いて PEM を取り出す。
func NewLDSignatureVerifier(pubkeyRepo repository.UserPublickeyRepository) *LDSignatureVerifier {
	return &LDSignatureVerifier{pubkeyRepo: pubkeyRepo}
}

// VerifyIfPresent inspects the activity body and, if it carries a non-empty
// `signature` field, verifies it. Returns nil when:
//   - body has no `signature` field
//   - signature verifies successfully
//
// Returns error when:
//   - body has `signature` but creator / signatureValue missing or malformed
//   - `signature.created` is missing, ambiguous, unreadable or outside the
//     accepted window (read from the signed RDF, not the JSON key)
//   - public key cannot be resolved
//   - forbidden directive detected in the activity
//   - the activity cannot be compacted (e.g. non-preloaded context)
//   - RsaSignature2017 verify fails (signature mismatch / key mismatch /
//     unsupported algorithm)
func (v *LDSignatureVerifier) VerifyIfPresent(rawBody []byte) error {
	_, _, err := v.VerifyAndCompact(rawBody)
	return err
}

// VerifyAndCompact behaves like VerifyIfPresent but additionally reports
// whether the activity carried an LD-Signature (`present`) and, on success,
// the verified creator and the compacted activity. Callers use the creator to
// confirm the LD-Signature actually authenticates the activity's `actor`
// (forwarded-activity authentication, upstream
// `authUser.user.uri !== getApId(activity.actor)` gate), and must process the
// returned Body rather than rawBody.
//
//   - no `signature` field        -> (zero, false, nil)
//   - signature present + verify  -> (verified, true, nil)
//   - signature present + invalid -> (zero, true, err)
func (v *LDSignatureVerifier) VerifyAndCompact(rawBody []byte) (VerifiedLDActivity, bool, error) {
	if v == nil || v.pubkeyRepo == nil {
		return VerifiedLDActivity{}, false, nil
	}
	var act map[string]any
	if err := json.Unmarshal(rawBody, &act); err != nil {
		return VerifiedLDActivity{}, false, fmt.Errorf("ld-sig: body unmarshal: %w", err)
	}
	sigRaw, hasSig := act["signature"]
	if !hasSig || sigRaw == nil {
		return VerifiedLDActivity{}, false, nil
	}
	sig, ok := sigRaw.(map[string]any)
	if !ok {
		return VerifiedLDActivity{}, true, errors.New("ld-sig: signature field is not an object")
	}
	creator, _ := sig["creator"].(string)
	if creator == "" {
		return VerifiedLDActivity{}, true, errors.New("ld-sig: signature.creator missing")
	}
	// per-verify fresh processor (upstream JsonLd class instance と等価)。
	// cache cap / freeze は新規 instance ごとに reset される。
	proc := ld.NewProcessor()
	// 生の文書にも forbidden check を掛ける。upstream は compact 後にだけ見るが、
	// 入口で弾くほうが strict で、compact 前に `@reverse` 等を含む文書を
	// json-gold に渡さずに済む。compact 後にも下で改めて見る。
	if err := proc.CheckForForbiddenDirectives(act); err != nil {
		return VerifiedLDActivity{}, true, err
	}
	// **upstream と違い compact より前に Freeze する。** upstream は compact 中に
	// remote context を HTTP で取りに行くので、取り終えた後に freeze する。mk-go の
	// loader は preload 済みの 3 context しか返さず fetch 経路を持たないので、
	// 先に freeze しても今は結果が変わらない (preload は freeze の対象外、
	// ld.Processor.loadDocument / #2680)。先に置いておけば、将来 fetch fallback を
	// 足したときに compact が無防備に fetch する形にならない — その時は upstream と
	// 同じく compact の後へ動かし、forbidden check を compact 後にも掛けること。
	//
	// #2106 L49 (documented limitation): preload 外の custom context を参照する
	// activity は compact 段で ErrCacheFrozen になり、LD-Signature が唯一の
	// authenticator である転送経路では reject される。upstream は context を
	// fetch して受理する。標準 context のみの一般的な Misskey/Mastodon activity
	// (AS2 + security/v1 + インライン拡張オブジェクト) は preload だけで解決できる。
	proc.Freeze()

	// **署名された内容だけを処理に渡すために compact する** (upstream と同じ)。
	// URDNA2015 は RDF の triple しか見ないので、AS2 context の `@vocab: "_:"` で
	// blank node IRI に展開される未定義語 (`_misskey_content` を context で定義して
	// いない Mastodon の文書に足したもの等) は署名に含まれない。生の JSON を
	// そのまま handler に渡すと、転送者が署名済み activity にそういうキーを足して
	// 被害者名義のノート本文を差し替えられた。compact 後の文書では、署名された
	// 述語だけが InboxCompactContext の短い名前で現れ、署名外の述語は `_:<name>`
	// という handler が読まないキーになる。
	unsigned := make(map[string]any, len(act))
	for k, val := range act {
		if k == "signature" {
			continue
		}
		unsigned[k] = val
	}
	compacted, err := proc.Compact(unsigned, ld.InboxCompactContext())
	if err != nil {
		return VerifiedLDActivity{}, true, fmt.Errorf("ld-sig: %w", err)
	}
	if err := proc.CheckForForbiddenDirectives(compacted); err != nil {
		return VerifiedLDActivity{}, true, err
	}
	compacted["signature"] = sig

	// **created は署名された RDF から読む** (ld.Processor.SignedCreated)。JSON の
	// `created` キーだけを見ると、転送者がそれを `dc:created` (型付き) や完全 IRI
	// へ移すだけで「欠落」と読まれて窓を外せた — options は identity/v1 で正規化
	// されるので RDF は同じで、署名は通る。
	//
	// 鍵の lookup より先に見る。値は署名対象 (options 側) なので、改ざんされて
	// いれば後段の verify で落ちる。ここで先に弾くのは DB を読まずに済むからで、
	// 判定結果は変わらない。
	created, err := proc.SignedCreated(sig)
	if err != nil {
		return VerifiedLDActivity{}, true, fmt.Errorf("ld-sig: signature options: %w", err)
	}
	if err := checkLDSignatureCreated(created, time.Now()); err != nil {
		return VerifiedLDActivity{}, true, err
	}

	pubkey, err := v.pubkeyRepo.FindByKeyID(creator)
	if err != nil {
		if !repository.IsNotFound(err) {
			// **「鍵が無い」に潰さない** (#3121)。潰すと呼び出し側が
			// LD-Signature の検証失敗として activity を drop するので、
			// DB 障害のあいだ届いた転送 activity がまるごと失われる。
			return VerifiedLDActivity{}, true, fmt.Errorf("%w: ld-sig public key for keyId=%s: %v", ErrLookupUnavailable, creator, err)
		}
		return VerifiedLDActivity{}, true, fmt.Errorf("ld-sig: public key not found for keyId=%s: %w", creator, err)
	}
	// 検証も compact 後の文書に対して行う (upstream と同じ)。compact は RDF として
	// 同値な変形なので、正しい署名はそのまま通る。逆に json-gold の compact が
	// 何かを足したり変えたりしていれば、ここで署名が合わなくなって落ちる —
	// 返す Body が署名された内容そのものであることを、この verify が保証する。
	if err := proc.VerifyRsaSignature2017(compacted, pubkey.KeyPEM); err != nil {
		return VerifiedLDActivity{}, true, err
	}
	body, err := json.Marshal(compacted)
	if err != nil {
		return VerifiedLDActivity{}, true, fmt.Errorf("ld-sig: marshal compacted activity: %w", err)
	}
	return VerifiedLDActivity{Creator: creator, Body: body}, true, nil
}

// checkLDSignatureCreated enforces the `signature.created` window on the
// dc:created values read from the signed options (see
// ld.Processor.SignedCreated). Exactly one value is required.
//
// **欠落は拒否する。** この verifier を通るのは HTTP 署名者と actor が食い違う
// 転送経路 (と Headers の無い legacy 経路) だけで、そこでは LD-Signature が唯一の
// 認証になる。created が無い署名は鮮度を持たないので、一度受け取れば無期限に
// 再送できる。upstream (`JsonLdService.signRsaSignature2017`) も Mastodon
// (`LinkedDataSignature#sign!`) も署名時刻を必ず入れるので、正当な署名は落ちない
// (それ以外の実装は未確認。docs/divergence.md)。複数あるのも、どれで判定するか
// 決められないので拒否する。
func checkLDSignatureCreated(values []string, now time.Time) error {
	switch len(values) {
	case 0:
		return fmt.Errorf("%w: created missing", ErrLDSignatureExpired)
	case 1:
	default:
		return fmt.Errorf("%w: %d created values", ErrLDSignatureExpired, len(values))
	}
	created, err := time.Parse(time.RFC3339, values[0])
	if err != nil {
		return fmt.Errorf("%w: created %q: %v", ErrLDSignatureExpired, values[0], err)
	}
	if created.Before(now.Add(-ldSignatureMaxAge)) || created.After(now.Add(ldSignatureMaxFuture)) {
		return fmt.Errorf("%w: created=%s", ErrLDSignatureExpired, created.UTC().Format(time.RFC3339))
	}
	return nil
}

// CheckForbiddenDirectivesIfPresent runs only the forbidden-directive hardening of
// the LD-Signature pipeline (no public-key resolution, no RsaSignature2017 verify).
//
// Used by the signer==actor inbound path (#2106 N26) where upstream
// InboxProcessorService skips LD-Signature processing entirely. mk-go keeps the
// JSON-LD term-redefinition defense (CheckForForbiddenDirectives) but, unlike
// VerifyIfPresent, does NOT drop an HTTP-signature-authenticated activity merely
// because its LD-Signature fails to verify (uncached creator key / legacy or
// mismatched LD-sig / normalize 差異). Returns nil when the body carries no
// `signature` field (same scope as VerifyIfPresent).
func (v *LDSignatureVerifier) CheckForbiddenDirectivesIfPresent(rawBody []byte) error {
	if v == nil {
		return nil
	}
	var act map[string]any
	if err := json.Unmarshal(rawBody, &act); err != nil {
		return fmt.Errorf("ld-sig: body unmarshal: %w", err)
	}
	if sigRaw, hasSig := act["signature"]; !hasSig || sigRaw == nil {
		return nil
	}
	return ld.NewProcessor().CheckForForbiddenDirectives(act)
}
