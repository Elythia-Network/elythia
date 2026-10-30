package i

import (
	"encoding/json"
	"net/http"

	"github.com/elythia-network/elythia/internal/api/apierr"
	"github.com/elythia-network/elythia/internal/core/avatardecoration"
	"github.com/elythia-network/elythia/internal/core/role"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
)

// avatarDecorationAPIError carries a (status, body) pair from
// normalizeAvatarDecorations back to the i/update handler so callers can emit
// the precise Misskey-compatible error payload.
type avatarDecorationAPIError struct {
	status int
	body   map[string]any
}

// normalizeAvatarDecorations validates the requested decoration array and
// returns the JSON bytes ready for `user.avatarDecorations` jsonb storage.
//
// 検証順:
//  1. policies.avatarDecorationLimit を超える長さは TOO_MANY_AVATAR_DECORATIONS で 400
//     (**絵文字由来も合算して数える**、#2975)
//  2. 絵文字由来のエントリ (emojiName 指定) は canUseEmojiAsAvatarDecoration を見て
//     RESTRICTED_BY_ROLE、ローカルに無ければ NO_SUCH_EMOJI、センシティブなら
//     SENSITIVE_EMOJI_NOT_ALLOWED で 400
//  3. 各 id が avatar_decoration テーブルに存在しなければ NO_SUCH_AVATAR_DECORATION で 400
//  4. decoration.roleIdsThatCanBeUsedThisDecoration が空でない場合、
//     ユーザーが許可ロールを持っていなければ RESTRICTED_BY_ROLE で 400
//
// avatarDecorationRepo / roleProvider が未配線でも処理は継続する (length
// チェックは default 1、catalog / role 検証は repo / provider が無い場合
// skip する)。これは Update ハンドラを最小依存で test できるようにする
// ための fallback。
//
// 戻り値の JSON は `[{id,angle,flipH,offsetX,offsetY}, ...]` 形式。空配列
// なら `[]` を返してデコレーション全外しを表現する。
func (h *Handler) normalizeAvatarDecorations(userID string, in []AvatarDecorationInput) ([]byte, *avatarDecorationAPIError) {
	limit := 1
	// 絵文字由来を許すか (#2975)。**個数はここでは見ない** — 上の limit に
	// 合算するので、専用の int policy は持たない。roleProvider 未配線では
	// policy の既定 (true) を使う (catalog / role 検証と同じ fallback)。
	emojiAllowed := true
	if h.roleProvider != nil {
		policies := h.roleProvider.GetUserPolicies(userID)
		// role override 経路は role.Policies の jsonb を json.Unmarshal で
		// any に展開するので数値は float64 で入ってくる。一方 default 経路
		// (DefaultPolicies()) は Go の int リテラル。両方を受けないと role
		// 上書きが silent ignore される (#524 review)。
		switch v := policies["avatarDecorationLimit"].(type) {
		case int:
			limit = v
		case float64:
			limit = int(v)
		}
		// **bool でなければ拒否する (fail-closed)。** `admin/roles/update-default-policies`
		// は受け取った map を型検証せず `meta.policies` へ書き、`coerceToBaseType`
		// には bool の枝が無いので、文字列 "false" のような値がそのまま
		// policy map に届きうる。型アサートが外れたときに既定 (true) を
		// 残すと、**制限が黙って効かなくなる**。既定 true の兄弟 policy
		// (canUseChunkedUpload / canUseTranslator) も `!ok || !v` で拒否する。
		allowed, ok := policies[role.PolicyCanUseEmojiAsAvatarDecoration].(bool)
		emojiAllowed = ok && allowed
	}
	// **upstream の paramDef が持つ範囲検証を mk-go も掛ける。**
	// `i/update` の schema は `maxItems: 16` と angle ±0.5 / offsetX,offsetY
	// ±0.25 を強制しているが (`endpoints/i/update.ts:149-159`)、mk-go は
	// これを持っておらず `offsetX: -1e9` が 200 で保存できた (実測)。値は
	// packer 経由で**その利用者を見る全員**の `avatarDecorations[]` に出る。
	// #2975 以前は `avatar_decoration` に管理者が登録した行が無ければ 1 件も
	// 保存できなかったが、これ以降はローカル絵文字があるだけで全利用者が
	// 到達するので、到達性が変わったこの PR で揃える。
	if len(in) > avatarDecorationMaxItems {
		return nil, &avatarDecorationAPIError{status: http.StatusBadRequest, body: apierr.InvalidParam()}
	}
	for _, d := range in {
		if !avatarDecorationPlacementInRange(d) {
			return nil, &avatarDecorationAPIError{status: http.StatusBadRequest, body: apierr.InvalidParam()}
		}
	}
	if len(in) > limit {
		return nil, &avatarDecorationAPIError{
			status: http.StatusBadRequest,
			body: apierr.Error(
				"TOO_MANY_AVATAR_DECORATIONS",
				"You cannot apply more avatar decorations than the limit allows.",
				"b449d1cf-d840-4a0a-8e25-f0b0c1782132",
			),
		}
	}
	// 許可ロール検証用の lookup set。provider 未配線時は空 set でフォールスルーし、
	// roleIds 制限のあるデコレーションは catalog 側で 1 件目で弾かれる。
	allowedRoleIDs := map[string]struct{}{}
	if h.roleProvider != nil {
		if roles, err := h.roleProvider.GetUserRoles(userID); err == nil {
			for _, r := range roles {
				allowedRoleIDs[r.ID] = struct{}{}
			}
		}
	}
	out := make([]map[string]any, 0, len(in))
	for _, d := range in {
		// 絵文字由来 (#2975) は catalog を引かない。**判別子は emojiName の
		// 有無**で、指定があれば `id` は読まない (入力の食い違いを持ち込まない)。
		if d.EmojiName != nil && *d.EmojiName != "" {
			emoji, apiErr := h.decorationEmojiByName(*d.EmojiName, emojiAllowed, allowedRoleIDs)
			if apiErr != nil {
				return nil, apiErr
			}
			out = append(out, emojiDecorationEntry(d, emoji))
			continue
		}
		if d.ID == "" {
			return nil, &avatarDecorationAPIError{
				status: http.StatusBadRequest,
				body:   apierr.InvalidParam(),
			}
		}
		if h.avatarDecorationRepo != nil {
			deco, err := h.avatarDecorationRepo.FindByID(d.ID)
			if err != nil && !repository.IsNotFound(err) {
				// **DB 障害を not-found に丸めない** (#2792)。この関数は
				// 専用の error 型を返すので、呼び出し側が 500 に変換する。
				return nil, &avatarDecorationAPIError{status: http.StatusInternalServerError, body: apierr.InternalError()}
			}
			if err != nil || deco == nil {
				// **`emojiName` を落とすクライアントのための fallback** (#2975)。
				// 型付きクライアント (misskey_dart 等) は既知の field だけを
				// 持つ構造体へ読んで送り返すので、装着済みの絵文字エントリが
				// `{id, angle, flipH, offsetX, offsetY}` になって戻ってくる。
				// catalog に無い id をそのまま弾くと、**絵文字を 1 つ着けた
				// 時点でそのクライアントからは装飾を一切編集できなくなる**
				// (無関係な装飾を外すだけでも配列ごと送り直すため)。
				// id が `emoji` 行を指していれば絵文字として扱う。検証は
				// name 経由と同じものを通す。
				emoji, emojiErr := h.decorationEmojiByID(d.ID, emojiAllowed, allowedRoleIDs)
				if emojiErr != nil {
					return nil, emojiErr
				}
				if emoji != nil {
					out = append(out, emojiDecorationEntry(d, emoji))
					continue
				}
				return nil, &avatarDecorationAPIError{
					status: http.StatusBadRequest,
					body: apierr.Error(
						"NO_SUCH_AVATAR_DECORATION",
						"No such avatar decoration.",
						"c0fa7a0d-a32a-4cb1-b657-ae8a90eaa3a8",
					),
				}
			}
			if len(deco.RoleIDs) > 0 {
				ok := false
				for _, rid := range deco.RoleIDs {
					if _, has := allowedRoleIDs[rid]; has {
						ok = true
						break
					}
				}
				if !ok {
					return nil, &avatarDecorationAPIError{
						status: http.StatusBadRequest,
						body: apierr.Error(
							"RESTRICTED_BY_ROLE",
							"This feature is restricted by your role.",
							"8feff0ba-5ab5-585b-31f4-4df816663fad",
						),
					}
				}
			}
		}
		row := avatarDecorationRow(d)
		row["id"] = d.ID
		out = append(out, row)
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return nil, &avatarDecorationAPIError{
			status: http.StatusInternalServerError,
			body:   apierr.InternalError(),
		}
	}
	return raw, nil
}

// UUIDSensitiveEmojiNotAllowed is the error id for SENSITIVE_EMOJI_NOT_ALLOWED
// (#2975). mk-go 固有の code なので upstream に対応する id は無い。
//
// **同梱 frontend がこの値を持っている。** `os.apiWithDialog` の `customErrors`
// は `err.id` で引くので、ここを変えると向こうの文面が黙って出なくなる。
// `TestEmojiDecorationErrorIDsMatchFrontend` が突き合わせる。
const UUIDSensitiveEmojiNotAllowed = "6e5caaca-f206-4822-a565-e3beb0b60a25"

// upstream `i/update` の paramDef が avatarDecorations に掛けている範囲
// (`endpoints/i/update.ts:149-159`)。**値そのものを upstream と揃える。**
const (
	avatarDecorationMaxItems  = 16
	avatarDecorationMaxAngle  = 0.5
	avatarDecorationMaxOffset = 0.25
	// scale は mk-go 独自 (#2975)。**上限は 1 = 現状の描画**で、拡大は許さない
	// (`.decoration` は既にアバターの 2 倍の枠なので、超えるとアバターの外へ
	// はみ出して周囲の UI を覆える)。下限は「見えなくなるだけ」なので緩め。
	avatarDecorationDefaultScale = 1.0
	avatarDecorationMinScale     = 0.1
	avatarDecorationMaxScale     = 1.0
)

// avatarDecorationPlacementInRange reports whether one entry's placement values
// are within the upstream schema's bounds.
//
// **clamp ではなく拒否する。** upstream は schema 違反を 400 で返すので、
// 黙って丸めると「送った値と保存された値が違う」という upstream に無い挙動に
// なる。省略 (nil) は既定値が入るので常に範囲内。
func avatarDecorationPlacementInRange(d AvatarDecorationInput) bool {
	inRange := func(v *float64, max float64) bool {
		return v == nil || (*v >= -max && *v <= max)
	}
	if d.Scale != nil && (*d.Scale < avatarDecorationMinScale || *d.Scale > avatarDecorationMaxScale) {
		return false
	}
	return inRange(d.Angle, avatarDecorationMaxAngle) &&
		inRange(d.OffsetX, avatarDecorationMaxOffset) &&
		inRange(d.OffsetY, avatarDecorationMaxOffset)
}

// avatarDecorationRow builds the placement half of one stored decoration entry
// (`id` / `emojiName` は呼び出し側が入れる)。angle / flipH / offsetX / offsetY は
// 省略時 0 / false / 0 / 0 で、upstream と同じ。
func avatarDecorationRow(d AvatarDecorationInput) map[string]any {
	row := map[string]any{
		"angle":   0.0,
		"flipH":   false,
		"offsetX": 0.0,
		"offsetY": 0.0,
	}
	if d.Angle != nil {
		row["angle"] = *d.Angle
	}
	if d.FlipH != nil {
		row["flipH"] = *d.FlipH
	}
	if d.OffsetX != nil {
		row["offsetX"] = *d.OffsetX
	}
	if d.OffsetY != nil {
		row["offsetY"] = *d.OffsetY
	}
	// **既定 (1) は保存しない。** 既存の行は `scale` を持たないので、読み出し側は
	// どのみち「無ければ 1」を実装する必要がある。既定を書き込むと、意味が
	// 変わらないのに全員の jsonb が書き換わる。
	if d.Scale != nil && *d.Scale != avatarDecorationDefaultScale {
		row["scale"] = *d.Scale
	}
	return row
}

// emojiDecorationEntry builds the stored entry for an emoji-backed decoration.
//
// 保存するのは**行の id と DB 上の名前**。名前は表示時に引き直すが、入力の
// 写しではなく正規の値を残しておく (入力は `FindByNameAndHost` の完全一致を
// 通っているので今は同じだが、写しを保存すると lookup の条件が緩んだときに
// 食い違う)。
func emojiDecorationEntry(d AvatarDecorationInput, emoji *model.Emoji) map[string]any {
	row := avatarDecorationRow(d)
	row["id"] = emoji.ID
	row["emojiName"] = emoji.Name
	return row
}

// decorationEmojiByName resolves a local custom emoji by name for i/update.
// Returns an API error when the emoji does not exist or may not be worn.
//
// **キャッシュではなく DB を引く。** i/update は write 経路で hot ではないし、
// 作ったばかりの絵文字が TTL のあいだ使えない / センシティブにした直後の絵文字を
// TTL のあいだ設定できる、という窓を作らないため。
func (h *Handler) decorationEmojiByName(name string, allowed bool, allowedRoleIDs map[string]struct{}) (*model.Emoji, *avatarDecorationAPIError) {
	if apiErr := decorationEmojiPolicyError(allowed); apiErr != nil {
		return nil, apiErr
	}
	if h.emojiRepo == nil {
		// 絵文字を引けない構成でこの経路に入るのは配線の誤り。検証を skip して
		// 保存すると、表示側で必ず drop される行だけが残る。
		return nil, &avatarDecorationAPIError{
			status: http.StatusInternalServerError,
			body:   apierr.InternalError(),
		}
	}
	emoji, err := h.emojiRepo.FindByNameAndHost(name, nil)
	if err != nil && !repository.IsNotFound(err) {
		// **DB 障害を not-found に丸めない** (#2792)。
		return nil, &avatarDecorationAPIError{status: http.StatusInternalServerError, body: apierr.InternalError()}
	}
	if err != nil || emoji == nil {
		return nil, &avatarDecorationAPIError{status: http.StatusBadRequest, body: apierr.NoSuchEmoji()}
	}
	return emoji, validateDecorationEmoji(emoji, allowedRoleIDs)
}

// decorationEmojiByID looks up an emoji row by id for the `emojiName`-less
// fallback (#2975). Returns (nil, nil) when the id is not an emoji at all —
// the caller then emits NO_SUCH_AVATAR_DECORATION as before.
//
// **policy / センシティブ / ロールの検証は name 経由と同じものを通す。** ここを
// 素通しにすると、`emojiName` を落とすだけで全ての制限を迂回できてしまう。
func (h *Handler) decorationEmojiByID(id string, allowed bool, allowedRoleIDs map[string]struct{}) (*model.Emoji, *avatarDecorationAPIError) {
	if h.emojiRepo == nil {
		return nil, nil
	}
	emoji, err := h.emojiRepo.FindByID(id)
	if err != nil && !repository.IsNotFound(err) {
		// **DB 障害を not-found に丸めない** (#2792)。ここで nil, nil を返すと
		// 障害が NO_SUCH_AVATAR_DECORATION に化ける。
		return nil, &avatarDecorationAPIError{status: http.StatusInternalServerError, body: apierr.InternalError()}
	}
	if err != nil || emoji == nil {
		return nil, nil
	}
	if apiErr := decorationEmojiPolicyError(allowed); apiErr != nil {
		return nil, apiErr
	}
	if apiErr := validateDecorationEmoji(emoji, allowedRoleIDs); apiErr != nil {
		return nil, apiErr
	}
	return emoji, nil
}

// decorationEmojiPolicyError rejects the emoji route when the role policy
// forbids it (#2975).
func decorationEmojiPolicyError(allowed bool) *avatarDecorationAPIError {
	if allowed {
		return nil
	}
	return &avatarDecorationAPIError{
		status: http.StatusBadRequest,
		body: apierr.Error(
			"RESTRICTED_BY_ROLE",
			"This feature is restricted by your role.",
			apierr.UUIDRestrictedByRole,
		),
	}
}

// validateDecorationEmoji checks that an emoji row may be worn on an avatar.
//
// **表示側と同じ述語 (avatardecoration.EmojiUsableAsDecoration) を使う。** 片方
// だけ直すと「設定できるのに出ない」「出ているのに設定し直せない」という
// 非対称になる。
func validateDecorationEmoji(emoji *model.Emoji, allowedRoleIDs map[string]struct{}) *avatarDecorationAPIError {
	if emoji.IsSensitive {
		// **センシティブだけは専用の code に分ける。** 「存在しない」に丸めると、
		// 利用者は名前を打ち間違えたと思って探し続けることになる。
		return &avatarDecorationAPIError{
			status: http.StatusBadRequest,
			body: apierr.Error(
				"SENSITIVE_EMOJI_NOT_ALLOWED",
				"You cannot use a sensitive emoji as an avatar decoration.",
				UUIDSensitiveEmojiNotAllowed,
			),
		}
	}
	if !avatardecoration.EmojiUsableAsDecoration(emoji) {
		// ローカル限定 (#2975)。`FindByNameAndHost(name, nil)` も `ListLocal` も
		// host IS NULL で引くので通常ここには来ないが、述語を 1 箇所に閉じる
		// ため通す。id 経由の fallback は host で絞らないので、こちらは実際に
		// リモート絵文字を弾く。
		return &avatarDecorationAPIError{status: http.StatusBadRequest, body: apierr.NoSuchEmoji()}
	}
	// **絵文字側のロール制限も見る** (#2975)。`reaction_service.go` が
	// `roleIdsThatCanBeUsedThisEmojiAsReaction` をリアクションで強制している
	// ので、アイコンにだけ載せられると運営者の設定が片側だけ効く形になる。
	// catalog 由来の `roleIdsThatCanBeUsedThisDecoration` と同じ扱い。
	if len(emoji.RoleIDsThatCanBeUsedThisEmojiAsReaction) > 0 {
		ok := false
		for _, rid := range emoji.RoleIDsThatCanBeUsedThisEmojiAsReaction {
			if _, has := allowedRoleIDs[rid]; has {
				ok = true
				break
			}
		}
		if !ok {
			return &avatarDecorationAPIError{
				status: http.StatusBadRequest,
				body: apierr.Error(
					"RESTRICTED_BY_ROLE",
					"This feature is restricted by your role.",
					apierr.UUIDRestrictedByRole,
				),
			}
		}
	}
	return nil
}
