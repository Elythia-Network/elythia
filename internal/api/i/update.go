package i

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"math"
	"net/http"
	"regexp"
	"strings"

	"github.com/elythia-network/elythia/internal/api/apierr"
	"github.com/elythia-network/elythia/internal/api/jsnum"
	"github.com/elythia-network/elythia/internal/core/following"
	corelocalfederation "github.com/elythia-network/elythia/internal/core/localfederation"
	"github.com/elythia-network/elythia/internal/core/role"
	"github.com/elythia-network/elythia/internal/core/user"
	"github.com/elythia-network/elythia/internal/core/wordmute"
	"github.com/elythia-network/elythia/internal/misc/colfit"
	"github.com/elythia-network/elythia/internal/misc/jsstr"
	"github.com/elythia-network/elythia/internal/misc/langmap"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/elythia-network/elythia/internal/server/middleware"
	"github.com/labstack/echo/v4"
)

// UpdateRequest is the request body for i/update.
// 各フィールドはポインタで「未指定なら変更しない」セマンティクスを持つ。
// 文字列のnullable化はrawMessageでなくJSONで対応するため省略している。
type UpdateRequest struct {
	// Name / Description / Location / Birthday / Lang / FollowedMessage は
	// 「キー省略 = 不変」「null = クリア」「文字列 = 設定」を区別する必要が
	// あるので RawMessage で受ける (*string だと省略と null がどちらも nil に
	// なり、frontend が空欄を null で送っても消えなかった。#3886)。
	Name              json.RawMessage `json:"name"`
	Description       json.RawMessage `json:"description"`
	Location          json.RawMessage `json:"location"`
	Birthday          json.RawMessage `json:"birthday"`
	Lang              json.RawMessage `json:"lang"`
	FollowedMessage   json.RawMessage `json:"followedMessage"`
	PublicReactions   *bool           `json:"publicReactions"`
	IsLocked          *bool           `json:"isLocked"`
	IsBot             *bool           `json:"isBot"`
	IsCat             *bool           `json:"isCat"`
	IsExplorable      *bool           `json:"isExplorable"`
	HideOnlineStatus  *bool           `json:"hideOnlineStatus"`
	AlwaysMarkNsfw    *bool           `json:"alwaysMarkNsfw"`
	AutoSensitive     *bool           `json:"autoSensitive"`
	NoCrawle          *bool           `json:"noCrawle"`
	PreventAiLearning *bool           `json:"preventAiLearning"`
	// 以下 4 field は upstream Misskey TS i/update.ts:178-188 の paramDef に
	// あり、profile の bool 設定として persist される。response 側は
	// MeDetailed packer (#969) 経由で含まれていたが、UpdateRequest struct
	// から抜けていて silent drop される asymmetric drift があった (#972)。
	AutoAcceptFollowed       *bool `json:"autoAcceptFollowed"`
	CarefulBot               *bool `json:"carefulBot"`
	InjectFeaturedNote       *bool `json:"injectFeaturedNote"`
	ReceiveAnnouncementEmail *bool `json:"receiveAnnouncementEmail"`
	// ChatScope は 1-on-1 DM の受信許可レベル (#692)。
	// upstream paramDef enum: everyone / followers / following / mutual / none
	ChatScope *string `json:"chatScope"`
	// FollowingVisibility / FollowersVisibility はフォロー/フォロワー一覧の
	// 公開範囲 (#1546)。upstream Misskey TS i/update.ts:191-192 paramDef enum:
	// public / followers / private。model 列 (user_profile) と entity packer
	// (read 側) は既にあったが write 経路が抜けていて silent drop していた。
	// users/show 等に followersVisibility gate (#1461) があるのに設定できず
	// privacy 上の drift だったため write を追加する。
	FollowingVisibility *string `json:"followingVisibility"`
	FollowersVisibility *string `json:"followersVisibility"`
	// AvatarID / BannerID — drive_file の ID。upstream は「キー省略は不変、
	// null は CLEAR、文字列値は SET」なので json.RawMessage で 3 状態を
	// 区別する (*string では省略と null が同じ nil になってしまう)。
	// core の UpdateInput へは mk-go の規約 (空文字列 = CLEAR) に変換して渡す。
	AvatarID json.RawMessage `json:"avatarId"`
	BannerID json.RawMessage `json:"bannerId"`
	// AvatarDecorations は装着するデコレーション配列。nil (省略) なら不変、
	// `[]` (空配列) なら全外し、`[{id,...}, ...]` で上書き。各要素は
	// avatar_decoration テーブルに登録された id を参照する。
	AvatarDecorations *[]AvatarDecorationInput `json:"avatarDecorations"`
	// MutedWords / HardMutedWords は ワードミュート設定 (#787)。
	// frontend は upstream Misskey TS と同じ `[["foo"], ["bar","baz"]]` 形式
	// (内側 array が AND、外側が OR) を JSON で送ってくるので変換せず jsonb
	// 列にそのまま保存する。Bind が json.RawMessage に詰めた段階で内部構造
	// (string / regex) は触らない。soft mute は frontend が `/api/i` のレス
	// ポンスから読んで client-side filter、hard mute は backend が TL fetch
	// 時に CheckWordMute で除外する。
	MutedWords     json.RawMessage `json:"mutedWords"`
	HardMutedWords json.RawMessage `json:"hardMutedWords"`
	// Fields はプロフィール上の name/value ペア (#956)。upstream paramDef は
	// `array of {name, value}` で maxItems 16。nil (= 省略) は不変、`[]` で
	// クリア、要素ありで上書き。core/user 側で trim + 空 entry 排除を行う。
	Fields *[]FieldInput `json:"fields"`
	// AlsoKnownAs は引越し元エイリアス (#1546)。upstream Misskey TS
	// i/update.ts:226-231 paramDef: array maxItems 10 uniqueItems。各要素は
	// acct (@user@host) または canonical URI。handler が resolveAlsoKnownAs で
	// canonical URI 列へ解決し core に渡す。nil (省略) は不変、`[]` は NULL
	// クリア。i/move の前提となる alsoKnownAs を設定する経路 (これが無いと
	// 移行 flow が成立しない)。
	AlsoKnownAs *[]string `json:"alsoKnownAs"`
	// 以下は #1546 で追加した i/update 残りの paramDef field。いずれも model 列 /
	// entity packer (read 側) は既にあり write 経路だけ抜けていた asymmetric drift。
	//
	// NotificationRecieveConfig は通知種別ごとの受信設定 object (jsonb)。
	// MutedInstances / EmailNotificationTypes は string 配列 (jsonb)。
	// いずれも json.RawMessage で受けて jsonb 列へそのまま書く (len 0 = 省略=不変)。
	NotificationRecieveConfig json.RawMessage `json:"notificationRecieveConfig"`
	MutedInstances            json.RawMessage `json:"mutedInstances"`
	EmailNotificationTypes    json.RawMessage `json:"emailNotificationTypes"`
	// PinnedPageID はプロフィールにピン留めする page id (nullable)。upstream は
	// truthy-string で所有権検証後に SET、null で CLEAR、undefined で不変。
	// json.RawMessage で 3 状態 (省略 / null / 値) を区別する。
	PinnedPageID json.RawMessage `json:"pinnedPageId"`
	// RequireSigninToViewContents は未ログインへのコンテンツ非表示設定 (bool)。
	RequireSigninToViewContents *bool `json:"requireSigninToViewContents"`
	// MakeNotesFollowersOnlyBefore / MakeNotesHiddenBefore は過去ノートの自動
	// followers-only 化 / 非表示化の基準時刻 (integer nullable)。null で解除、
	// 値で設定、省略で不変。json.RawMessage で 3 状態を区別する。
	MakeNotesFollowersOnlyBefore json.RawMessage `json:"makeNotesFollowersOnlyBefore"`
	MakeNotesHiddenBefore        json.RawMessage `json:"makeNotesHiddenBefore"`
	// FollowApprovalLocalSeconds / FollowApprovalRemoteSeconds は、作られてから
	// 日の浅いアカウントからのフォローを止める期間 (本家 PR 17998 の paramDef:
	// integer、nullable、0〜2592000、#3466)。null と省略を区別する。
	FollowApprovalLocalSeconds  json.RawMessage `json:"followApprovalLocalSeconds"`
	FollowApprovalRemoteSeconds json.RawMessage `json:"followApprovalRemoteSeconds"`
	// FollowApprovalAction は止め方 (Elythia 独自の追加、#3466)。
	// request / silentRequest / silentFollow。null は省略と同じ。
	FollowApprovalAction *string `json:"followApprovalAction"`
}

// FieldInput is the {name, value} shape accepted by i/update for profile
// fields (#956). upstream paramDef のように name / value を string で
// 受け取り、core/user 側で trim + 空 entry 除外を行う。
type FieldInput struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// AvatarDecorationInput is one entry of the avatarDecorations array on
// i/update. Only id is required; angle / flipH / offsetX / offsetY default
// to 0 / false / 0 / 0 when omitted (matching upstream).
//
// EmojiName is the mk-go additive parameter for wearing a local custom emoji
// (#2975). 指定があるとそちらが優先で `id` は読まない — クライアントは
// `avatarDecorations` をまるごと送り直す作りなので、絵文字エントリを編集する
// たびに前回保存された `emoji` 行の id が一緒に返ってくる。両方を見る形だと
// 「name は A、id は B」という入力の扱いを決めなければならず、どちらに倒しても
// 利用者の意図と食い違いうる。
type AvatarDecorationInput struct {
	ID        string   `json:"id"`
	EmojiName *string  `json:"emojiName,omitempty"`
	Angle     *float64 `json:"angle,omitempty"`
	FlipH     *bool    `json:"flipH,omitempty"`
	OffsetX   *float64 `json:"offsetX,omitempty"`
	OffsetY   *float64 `json:"offsetY,omitempty"`
	// Scale は mk-go 独自の additive パラメータ (#2975)。省略で 1 (= upstream と
	// 同じ大きさ)。**1 を超えられない** — `MkAvatar` の `.decoration` は
	// `top:-50%; left:-50%; width:200%` でアバターの 2 倍の枠に描くので、
	// 1 より大きくすると周囲の UI の上へはみ出す。縮小だけを許す。
	Scale *float64 `json:"scale,omitempty"`
}

// jsonbArray normalizes a raw jsonb byte slice into []any for the Me response.
// Empty or malformed payloads become an empty array.
func jsonbArray(raw []byte) any {
	if len(raw) == 0 {
		return []any{}
	}
	var out []any
	if err := json.Unmarshal(raw, &out); err != nil || out == nil {
		return []any{}
	}
	return out
}

// countMuteWords returns the total length of the words in a muted-words
// payload, flattening AND-groups, as upstream Misskey TS i/update.ts
// `checkMuteWordCount` counts it: the sum of `item.length` (UTF-16 code units)
// over every string. raw が nil / 空なら 0。
//
// 以前は件数を数えていた (#1029 から)。既定の 200 は本家では文字数の上限で、
// 件数として読むと 1 件の長さに上限が無くなり、長い正規表現をいくつでも並べ
// られた。
func countMuteWords(raw json.RawMessage) int {
	if len(raw) == 0 {
		return 0
	}
	var arr []any
	if err := json.Unmarshal(raw, &arr); err != nil {
		return 0
	}
	length := 0
	for _, item := range arr {
		switch v := item.(type) {
		case string:
			length += jsstr.Len(v)
		case []any:
			for _, sub := range v {
				if str, ok := sub.(string); ok {
					length += jsstr.Len(str)
				}
			}
		}
	}
	return length
}

// normalizeMutedWords validates and normalizes the mutedWords / hardMutedWords
// payload from i/update. Returns (raw, ok, err): ok=false means "field not
// present in request" (caller should leave the column unchanged); err != nil
// means the payload is invalid (caller should 400 the request).
//
// Accepts only top-level JSON arrays (including the empty array which clears
// the column). Internal structure (string | array of string for AND-grouping,
// regex literals like `/foo/i`) is forwarded verbatim — frontend matches the
// same wire format and the backend CheckWordMute helper interprets it later.
func normalizeMutedWords(raw json.RawMessage) (json.RawMessage, bool, error) {
	if len(raw) == 0 {
		return nil, false, nil
	}
	// `null` は明示的に omit と同義 (frontend が clear 意図で送ることは無い、
	// upstream paramDef も nullable: false)。
	trimmed := bytes.TrimSpace(raw)
	if bytes.Equal(trimmed, []byte("null")) {
		return nil, false, nil
	}
	// top-level が配列であることだけ確認する (内側は CheckWordMute が解釈)。
	var v any
	if err := json.Unmarshal(trimmed, &v); err != nil {
		return nil, false, err
	}
	if _, ok := v.([]any); !ok {
		return nil, false, errors.New("mutedWords must be an array")
	}
	out := append(json.RawMessage(nil), trimmed...)
	return out, true, nil
}

// containsProhibitedWord reports whether name contains any entry from words
// case-insensitively (substring match). Empty or whitespace-only entries are
// skipped so a misconfigured empty element cannot ban every display name.
func containsProhibitedWord(name string, words []string) bool {
	lname := strings.ToLower(name)
	for _, w := range words {
		w = strings.TrimSpace(w)
		if w == "" {
			continue
		}
		if strings.Contains(lname, strings.ToLower(w)) {
			return true
		}
	}
	return false
}

// isValidFollowVisibility reports whether v is one of the
// followingVisibility / followersVisibility enum values accepted by upstream
// Misskey i/update (public / followers / private). Used to fail-close on any
// other value instead of silently persisting a bogus privacy setting.
// parseNullableInt decodes a request json.RawMessage for an integer-nullable
// i/update field into the **int 3-state convention used by user.UpdateInput
// (#1546): absent (len 0) → (nil,false) no change; explicit null →
// (&(*int)(nil),true) clear; integer value → (&&v,true) set. Integral numbers
// written with a fraction or an exponent (10.0, 1e1) are integers, as
// upstream's ajv `type: integer` judges them (#3886). A fraction (1.5) or a
// non-number is a bind error.
func parseNullableInt(raw json.RawMessage) (**int, bool, error) {
	if len(raw) == 0 {
		return nil, false, nil
	}
	if string(bytes.TrimSpace(raw)) == "null" {
		var clear *int
		return &clear, true, nil
	}
	var n jsnum.Int
	if err := json.Unmarshal(raw, &n); err != nil {
		return nil, false, err
	}
	v := int(n)
	p := &v
	return &p, true, nil
}

// errInvalidSeconds is returned by parseNullableSeconds for a value outside
// the accepted range or with a fractional part.
var errInvalidSeconds = errors.New("invalid seconds")

// parseNullableSeconds decodes an integer-nullable number of seconds in
// 0..maxSeconds into the **int 3-state convention of parseNullableInt.
// Unlike parseNullableInt it accepts integral numbers written with a
// fraction or an exponent (2.0, 1e3), as upstream's ajv `type: integer` does,
// and rejects 1.5.
//
// parseNullableInt と違い、期間 (#3466) の範囲も見る。
func parseNullableSeconds(raw json.RawMessage, maxSeconds int) (**int, bool, error) {
	if len(raw) == 0 {
		return nil, false, nil
	}
	if string(bytes.TrimSpace(raw)) == "null" {
		var clear *int
		return &clear, true, nil
	}
	var f float64
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, false, err
	}
	if f != math.Trunc(f) || f < 0 || f > float64(maxSeconds) {
		return nil, false, errInvalidSeconds
	}
	n := int(f)
	p := &n
	return &p, true, nil
}

func isValidFollowVisibility(v string) bool {
	switch model.FollowingVisibility(v) {
	case model.FollowingVisibilityPublic,
		model.FollowingVisibilityFollowers,
		model.FollowingVisibilityPrivate:
		return true
	default:
		return false
	}
}

// Update handles POST /api/i/update.
// birthdayPattern mirrors upstream birthdaySchema (User.ts:325)。
var birthdayPattern = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}$`)

// profileTextWithinLimit reports whether s (when present and non-empty) is
// within max code points, mirroring upstream nameSchema/descriptionSchema/
// locationSchema/followedMessageSchema maxLength (#1918)。nil / "" (= mk-go の
// クリア要求) は常に許容する。長さは他の mk-go handler と同じく rune 数で数える。
func profileTextWithinLimit(s *string, max int) bool {
	if s == nil || *s == "" {
		return true
	}
	// NUL と不正な UTF-8 は長さに関わらず保存できない (SQLSTATE 22021 で 500 に
	// なっていた)。同じ 400 に畳む。
	return colfit.Fits(*s, max)
}

// decodeNullableString maps the 3 JSON states of a nullable string field onto
// the core layer's `**string`. ok=false は「キー省略 = 不変」、返り値 nil は
// 「null = クリア」、非 nil は「設定」。
//
// *string で受けると省略と null がどちらも nil になり、null クリアが効かない。
func decodeNullableString(raw json.RawMessage) (*string, bool) {
	if len(raw) == 0 {
		return nil, false
	}
	if string(raw) == "null" {
		return nil, true
	}
	var v string
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, false
	}
	return &v, true
}

// nullableStringParam decodes a nullable string param of i/update into the
// core layer's `**string`: nil for an absent key (no change), a pointer to
// nil for null (clear), and a pointer to the string otherwise. A value of
// another type is an error.
func nullableStringParam(raw json.RawMessage) (**string, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var v *string
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, err
	}
	return &v, nil
}

// deref returns the value nullableStringParam decoded, or nil for an absent
// key or null.
func deref(v **string) *string {
	if v == nil {
		return nil
	}
	return *v
}

// driveIDUpdate decodes an avatarId / bannerId field into the core layer's
// convention. ok=false は「キー省略 = 不変」、value="" は「null = CLEAR」、
// それ以外は SET したい drive file ID。文字列以外の値は不変として扱う。
func driveIDUpdate(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 {
		return "", false
	}
	if string(raw) == "null" {
		return "", true
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", false
	}
	return s, true
}

func (h *Handler) Update(c echo.Context) error {
	me := middleware.GetUser(c)

	var req UpdateRequest
	if err := c.Bind(&req); err != nil {
		return apierr.JSONInvalidParam(c)
	}
	// **jsonb 列へそのまま入る field を先に弾く (#3037)。** PostgreSQL の jsonb は
	// NUL エスケープを受け付けず SQLSTATE 22P05 でクエリごと落とすので、
	// 引く前に弾かないと**任意の認証ユーザーが 500 を起こせる**。
	//
	// **一覧は手で持つ。** `UpdateRequest` の `json.RawMessage` には
	// varchar 列へ落ちるもの (`name` / `birthday` / `avatarId` など) も混ざって
	// いるので、型で機械的には選べない。ここは jsonb 列へ**中身をそのまま**
	// 書くものだけ。
	//
	// **`fields` はここに入らない。** あれは `[]FieldInput` (プレーンな
	// `string`) で、`core/user` が marshal し直してから jsonb へ渡すので、
	// 判定も要素ごとに下で行う。手で持つ一覧の失敗形として 1 度漏らしている。
	for _, raw := range []json.RawMessage{
		req.MutedWords, req.HardMutedWords,
		req.NotificationRecieveConfig, req.MutedInstances, req.EmailNotificationTypes,
	} {
		if !colfit.JSONStorable(raw) {
			return apierr.JSONInvalidParam(c)
		}
	}

	// upstream paramDef の maxLength / pattern / enum を検証する (#1918)。
	// name≤50 / description≤1500 / location≤50 / followedMessage≤256、
	// birthday は ^YYYY-MM-DD$、lang は langmap enum。空文字 ("") は mk-go の
	// クリア要求として通すため検証は非空値のみに適用し、違反は INVALID_PARAM(400)。
	nameValue, _ := decodeNullableString(req.Name)
	birthdayValue, _ := decodeNullableString(req.Birthday)
	description, errDescription := nullableStringParam(req.Description)
	location, errLocation := nullableStringParam(req.Location)
	lang, errLang := nullableStringParam(req.Lang)
	followedMessage, errFollowedMessage := nullableStringParam(req.FollowedMessage)
	if errors.Join(errDescription, errLocation, errLang, errFollowedMessage) != nil {
		return apierr.JSONInvalidParam(c)
	}
	if !profileTextWithinLimit(nameValue, 50) ||
		!profileTextWithinLimit(deref(description), 1500) ||
		!profileTextWithinLimit(deref(location), 50) ||
		!profileTextWithinLimit(deref(followedMessage), 256) {
		return apierr.JSONInvalidParam(c)
	}
	if birthdayValue != nil && *birthdayValue != "" && !birthdayPattern.MatchString(*birthdayValue) {
		return apierr.JSONInvalidParam(c)
	}
	if l := deref(lang); l != nil && *l != "" && !langmap.IsValid(*l) {
		return apierr.JSONInvalidParam(c)
	}

	// upstream Misskey TS i/update.ts: role policy alwaysMarkNsfw=true の user は
	// profile flag を変更できない (admin 強制 NSFW marker の保護、#1028)。
	//
	// 「許可型」policy (canUpdateBioMedia 等) と挙動が逆で、true = 制限 ON な
	// 「禁止型」policy なので、HasRolePolicy の admin bypass (admin に対して
	// 常に true を返す) を使うと admin が逆に制限されてしまう。GetUserPolicies
	// で生値を直接見て、admin / moderator は明示的に bypass する。
	if req.AlwaysMarkNsfw != nil && h.roleProvider != nil &&
		!h.roleProvider.IsAdministrator(me.ID) && !h.roleProvider.IsModerator(me.ID) {
		policies := h.roleProvider.GetUserPolicies(me.ID)
		if v, ok := policies[role.PolicyAlwaysMarkNsfw].(bool); ok && v {
			return apierr.JSONRestrictedByRole(c)
		}
	}

	// 全 *bool field は req と in の field type が一致するので struct literal
	// で直接代入する。req.X が nil なら in.X も nil (= service 側で no-op
	// 扱い) になり、`if != nil` block 経由と意味的に等価。
	in := user.UpdateInput{
		IsLocked:                 req.IsLocked,
		IsBot:                    req.IsBot,
		IsCat:                    req.IsCat,
		IsExplorable:             req.IsExplorable,
		HideOnlineStatus:         req.HideOnlineStatus,
		AlwaysMarkNsfw:           req.AlwaysMarkNsfw,
		AutoSensitive:            req.AutoSensitive,
		NoCrawle:                 req.NoCrawle,
		PreventAiLearning:        req.PreventAiLearning,
		PublicReactions:          req.PublicReactions,
		AutoAcceptFollowed:       req.AutoAcceptFollowed,
		CarefulBot:               req.CarefulBot,
		InjectFeaturedNote:       req.InjectFeaturedNote,
		ReceiveAnnouncementEmail: req.ReceiveAnnouncementEmail,
	}
	if name, ok := decodeNullableString(req.Name); ok {
		// upstream update.ts:280-287 と同じ: null はそのまま null、文字列は
		// trim して空になったら null にする。
		if name != nil {
			trimmed := jsstr.Trim(*name)
			if trimmed == "" {
				name = nil
			} else {
				name = &trimmed
			}
		}
		// 表示名の禁止ワードチェック。meta 未注入 / 未設定時は素通りする。
		// 本家 Misskey と同様に部分一致 case-insensitive で、クリア
		// (null) は検査対象外 (ユーザー体験上必要)。
		if h.metaRepo != nil && name != nil {
			if m, err := h.metaRepo.Fetch(); err == nil && containsProhibitedWord(*name, m.ProhibitedWordsForNameOfUser) {
				// upstream update.ts: code=YOUR_NAME_CONTAINS_PROHIBITED_WORDS /
				// id=0b3f9f6a-2f4d-4b1f-9fb4-49d3a2fd7191 / httpStatusCode=422 (#1546)。
				return c.JSON(http.StatusUnprocessableEntity, apierr.Error("YOUR_NAME_CONTAINS_PROHIBITED_WORDS", "Your new name contains prohibited words.", "0b3f9f6a-2f4d-4b1f-9fb4-49d3a2fd7191"))
			}
		}
		in.Name = &name
	}
	in.Description = description
	in.Location = location
	if birthday, ok := decodeNullableString(req.Birthday); ok {
		in.Birthday = &birthday
	}
	in.Lang = lang
	in.FollowedMessage = followedMessage
	// ワードミュート (#787)。空 byte = field 未指定 (omit) なので不変。
	// `[]` (= 2 byte の空配列) はクリア要求として通す。validation は配列形式
	// であることだけ。upstream paramDef も内側構造は `array of (string |
	// string[])` で、要素 0 個も許容する。
	// wordMuteLimit role policy gate (#1029)。upstream
	// `checkMuteWordCount(mutedWords, policies.wordMuteLimit)` と同 logic。
	// mutedWords / hardMutedWords 両方が指定された request で同 policy を 2 度
	// lookup しないよう、helper closure で memoize する (canUpdateBioMedia と
	// 同 pattern, upstream の `policies ??= await ...` と等価)。
	var (
		wordMuteLimitChecked bool
		wordMuteLimitValue   int
		wordMuteLimitOk      bool
	)
	wordMuteLimit := func() (int, bool) {
		if !wordMuteLimitChecked {
			wordMuteLimitChecked = true
			if h.roleProvider != nil {
				// 小数は切り捨てる (厳しい側)。ゲートが消える方向ではない (#2613)。
				// 負の値は upstream と同じく、空の配列も含めて保存できない (`length > limit` が常に真)。
				if v, ok := role.PolicyNumber(h.roleProvider.GetUserPolicies(me.ID)["wordMuteLimit"]); ok {
					wordMuteLimitValue = int(math.Floor(v))
					wordMuteLimitOk = true
				}
			}
		}
		return wordMuteLimitValue, wordMuteLimitOk
	}
	// #2106 L3: upstream i/update.ts は validateMuteWordRegex で /pattern/flags リテラルが
	// compile 不能なら INVALID_REGEXP(400) を投げる。mk-go も書き込み時に検証する (読み取り側
	// wordmute は不正 regex を nil rule で握り潰すため、ここで弾かないとユーザーへの
	// フィードバックが無く黙殺される)。
	//
	// hardMutedWords はサーバーがノートごとに評価するので、正規表現の大きさの上限も
	// 見る (超えたものも INVALID_REGEXP。upstream の RE2 もコンパイルできない巨大な
	// パターンを同じエラーで拒否する)。
	//
	// **件数の検査を正規表現の検査より先に行う** (upstream と同じ順)。逆だと、件数で
	// 断られる大量の巨大なパターンを先に全部コンパイルさせられる。
	invalidRegexp := func() error {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_REGEXP", "Invalid Regular Expression.", "0d786918-10df-41cd-8f33-8dec7d9a89a5"))
	}
	if mw, ok, err := normalizeMutedWords(req.MutedWords); err != nil {
		return apierr.JSONInvalidParam(c)
	} else if ok {
		if limit, lok := wordMuteLimit(); lok && countMuteWords(mw) > limit {
			return apierr.JSONTooManyMutedWords(c)
		}
		if wordmute.ValidateMutedWords(req.MutedWords) != nil {
			return invalidRegexp()
		}
		in.MutedWords = &mw
	}
	if mw, ok, err := normalizeMutedWords(req.HardMutedWords); err != nil {
		return apierr.JSONInvalidParam(c)
	} else if ok {
		if limit, lok := wordMuteLimit(); lok && countMuteWords(mw) > limit {
			return apierr.JSONTooManyMutedWords(c)
		}
		if wordmute.ValidateHardMutedWords(req.HardMutedWords) != nil {
			return invalidRegexp()
		}
		in.HardMutedWords = &mw
	}
	// upstream Misskey TS i/update.ts: avatarId / bannerId が non-null 文字列で
	// 指定された場合に canUpdateBioMedia policy gate。null (= 解除) は対象外
	// (= 自分の avatar/banner 解除は常に可、設定だけ制限される #1024)。
	// avatarId と bannerId 両方が指定された request で同 policy を 2 度 lookup
	// しないよう、helper closure で memoize する (upstream の
	// `policies ??= await ...` と同 pattern)。
	var bioMediaChecked, bioMediaAllowed bool
	canUpdateBioMedia := func() bool {
		if !bioMediaChecked {
			bioMediaAllowed = h.roleProvider == nil ||
				h.roleProvider.HasRolePolicy(me.ID, role.PolicyCanUpdateBioMedia)
			bioMediaChecked = true
		}
		return bioMediaAllowed
	}
	if v, ok := driveIDUpdate(req.AvatarID); ok {
		if v != "" && !canUpdateBioMedia() {
			return apierr.JSONRestrictedByRole(c)
		}
		in.AvatarID = &v
	}
	if v, ok := driveIDUpdate(req.BannerID); ok {
		if v != "" && !canUpdateBioMedia() {
			return apierr.JSONRestrictedByRole(c)
		}
		in.BannerID = &v
	}
	if req.AvatarDecorations != nil {
		normalized, apiErr := h.normalizeAvatarDecorations(me.ID, *req.AvatarDecorations)
		if apiErr != nil {
			return c.JSON(apiErr.status, apiErr.body)
		}
		in.AvatarDecorations = &normalized
	}
	if req.ChatScope != nil {
		// upstream paramDef と同じ enum のみ受け付ける (#692)。
		// 不正値を弾かないと cherrypick の chatScope 判定 (none/followers/...)
		// から漏れて "誰からも受信不能" のような壊れた状態を作りうる。
		switch *req.ChatScope {
		case "everyone", "followers", "following", "mutual", "none":
			in.ChatScope = req.ChatScope
		default:
			return apierr.JSONInvalidParam(c)
		}
	}
	// followingVisibility / followersVisibility は upstream paramDef と同じ
	// enum (public / followers / private) のみ受け付ける (#1546)。enum 外は
	// INVALID_PARAM で fail-close し、不正値が privacy gate を素通りしない
	// ようにする。null / 省略は no-op。
	if req.FollowingVisibility != nil {
		if !isValidFollowVisibility(*req.FollowingVisibility) {
			return apierr.JSONInvalidParam(c)
		}
		in.FollowingVisibility = req.FollowingVisibility
	}
	if req.FollowersVisibility != nil {
		if !isValidFollowVisibility(*req.FollowersVisibility) {
			return apierr.JSONInvalidParam(c)
		}
		in.FollowersVisibility = req.FollowersVisibility
	}
	if req.Fields != nil {
		// upstream paramDef は maxItems 16。超過は INVALID_PARAM で弾く
		// (= cherrypick / Misskey TS と同じ挙動)。各要素の trim / 空除外は
		// core/user 側で行う。
		if len(*req.Fields) > 16 {
			return apierr.JSONInvalidParam(c)
		}
		items := make([]user.FieldItem, len(*req.Fields))
		for i, f := range *req.Fields {
			// **`fields` も jsonb 列へ入る (#3037)。** ここはプレーンな
			// `string` なので NUL のエスケープが**実 NUL バイト**として
			// decode され、`core/user` が `json.Marshal` で戻したものが
			// jsonb へ渡って SQLSTATE 22P05 で落ちる。上の
			// `json.RawMessage` の一覧とは経路が違うので、手で持つ一覧からは
			// 漏れていた (敵対的レビューで実測)。
			if !colfit.Storable(f.Name) || !colfit.Storable(f.Value) {
				return apierr.JSONInvalidParam(c)
			}
			items[i] = user.FieldItem{Name: f.Name, Value: f.Value}
		}
		in.Fields = &items
	}
	if req.AlsoKnownAs != nil {
		// 引越し元エイリアス (#1546)。upstream update.ts:226-231,445-475 と同順:
		//  1. 既に movedToUri 済なら YOUR_ACCOUNT_MOVED (403)
		//  2. 各 line を acct/URI → canonical URI へ解決
		//     - 解決失敗で NO_SUCH_USER、自分指定で FORBIDDEN_TO_SET_YOURSELF、
		//       resolved user の URI が空なら URI_NULL
		//  3. 重複排除した URI 群を core に渡す (空なら NULL クリア)
		// upstream paramDef maxItems 10 / uniqueItems も合わせて enforce する。
		if me.MovedToURI != nil && *me.MovedToURI != "" {
			return c.JSON(http.StatusForbidden, apierr.Error(
				"YOUR_ACCOUNT_MOVED", "You have moved your account.",
				"56f20ec9-fd06-4fa5-841b-edd6d7d4fa31",
			))
		}
		// **連合 OFF・未確定のアカウントは移行先にもならない** (ローカル専用アカウントの
		// 設計 7.8)。alsoKnownAs は「このアカウントへ移ってよい」と移行元に告げる印で、
		// 受け取る側の Move は移行先の状態を見ない。UI (migration.vue) だけが止めていて
		// API では設定できた (2026-10-31 の監査)。i/move と同じ FEDERATION_DISABLED で断る。
		// 空の配列 (= 解除) は通す。ON のときに設定してから OFF にした利用者が消せなくなる。
		if len(*req.AlsoKnownAs) > 0 && !corelocalfederation.IsExternallyAvailable(me) {
			return c.JSON(http.StatusBadRequest, apierr.Error(
				"FEDERATION_DISABLED", "Account aliases are unavailable while federation is disabled for this account.",
				"6f0e3d5c-2b1a-4c8e-9d7f-3a5b1c2d4e6f",
			))
		}
		if len(*req.AlsoKnownAs) > 10 {
			return apierr.JSONInvalidParam(c)
		}
		resolved, apiErr := h.resolveAlsoKnownAs(me.ID, *req.AlsoKnownAs)
		if apiErr != nil {
			return c.JSON(apiErr.status, apiErr.body)
		}
		in.AlsoKnownAs = &resolved
	}
	// #1546 残り field の request → input 変換。
	// jsonb passthrough (notificationRecieveConfig / mutedInstances /
	// emailNotificationTypes)。len 0 = 省略=不変。親 Unmarshal 済バイト列を
	// コピーして所有権を切り離す。
	if len(req.NotificationRecieveConfig) > 0 {
		v := append(json.RawMessage(nil), req.NotificationRecieveConfig...)
		in.NotificationRecieveConfig = &v
	}
	if len(req.MutedInstances) > 0 {
		v := append(json.RawMessage(nil), req.MutedInstances...)
		in.MutedInstances = &v
	}
	if len(req.EmailNotificationTypes) > 0 {
		v := append(json.RawMessage(nil), req.EmailNotificationTypes...)
		in.EmailNotificationTypes = &v
	}
	in.RequireSigninToViewContents = req.RequireSigninToViewContents
	if v, ok, err := parseNullableInt(req.MakeNotesFollowersOnlyBefore); err != nil {
		return apierr.JSONInvalidParam(c)
	} else if ok {
		in.MakeNotesFollowersOnlyBefore = v
	}
	if v, ok, err := parseNullableInt(req.MakeNotesHiddenBefore); err != nil {
		return apierr.JSONInvalidParam(c)
	} else if ok {
		in.MakeNotesHiddenBefore = v
	}
	// 期間は本家の paramDef と同じく整数で 0〜2592000 (30 日)。範囲外・小数・
	// 文字列は INVALID_PARAM (#3466)。
	for _, f := range []struct {
		raw json.RawMessage
		dst ***int
	}{
		{req.FollowApprovalLocalSeconds, &in.FollowApprovalLocalSeconds},
		{req.FollowApprovalRemoteSeconds, &in.FollowApprovalRemoteSeconds},
	} {
		v, ok, err := parseNullableSeconds(f.raw, following.FollowApprovalMaxSeconds)
		if err != nil {
			return apierr.JSONInvalidParam(c)
		}
		if !ok {
			continue
		}
		*f.dst = v
	}
	if req.FollowApprovalAction != nil {
		if !model.IsFollowApprovalAction(*req.FollowApprovalAction) {
			return apierr.JSONInvalidParam(c)
		}
		in.FollowApprovalAction = req.FollowApprovalAction
	}
	// 鍵を外すときだけ、溜まったリクエストを承認する (本家 i/update の
	// `user.isLocked && ps.isLocked === false`)。更新前の値は DB から読む —
	// middleware の me は認証キャッシュ (30 秒) の写しで、直前に鍵を掛けた
	// ばかりだと古い値 (未施錠) のままになる。
	unlocking := false
	if req.IsLocked != nil && !*req.IsLocked && h.followRequestAccepter != nil {
		wasLocked := me.IsLocked
		if h.userRepo != nil {
			if cur, err := h.userRepo.FindByID(me.ID); err == nil {
				wasLocked = cur.IsLocked
			}
		}
		unlocking = wasLocked
	}

	// pinnedPageId は upstream と同じ 3 分岐: null → CLEAR、truthy id →
	// 所有権検証して SET、それ以外 (省略 / "") → 不変 (upstream の
	// `if (ps.pinnedPageId)` は "" が falsy で no-op)。
	if len(req.PinnedPageID) > 0 {
		if string(bytes.TrimSpace(req.PinnedPageID)) == "null" {
			var clear *string
			in.PinnedPageID = &clear
		} else {
			var pid string
			if err := json.Unmarshal(req.PinnedPageID, &pid); err != nil {
				return apierr.JSONInvalidParam(c)
			}
			if pid != "" {
				// 所有権検証: 自分の page でなければ NO_SUCH_PAGE。
				if h.pageRepo != nil {
					p, perr := h.pageRepo.FindByID(pid)
					if perr != nil && !repository.IsNotFound(perr) {
						// **DB 障害を not-found に丸めない** (#2792)。
						return apierr.JSONInternalError(c)
					}
					if perr != nil || p == nil || p.UserID != me.ID {
						return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_PAGE", "No such page.", "8e01b590-7eb9-431b-a239-860e086c408e"))
					}
				}
				ptr := &pid
				in.PinnedPageID = &ptr
			}
		}
	}

	bundle, err := h.userService.UpdateProfile(me.ID, in)
	if err != nil {
		switch {
		case errors.Is(err, user.ErrUserNotFound):
			return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_USER", "No such user.", "fcd2eef9-a9b2-4c4f-8624-038099e90aa5"))
		case errors.Is(err, user.ErrManagedAccountMustBeBot):
			// Elythia 独自 (#3468)。プラグインが管理するアカウントは bot の印を
			// 外せない。
			return c.JSON(http.StatusBadRequest, apierr.Error("PLUGIN_MANAGED_ACCOUNT_MUST_BE_BOT", "An account managed by a plugin must stay a bot.", "226cc907-c7b3-4e3d-aef4-f583967e3dd1"))
		case errors.Is(err, user.ErrAvatarNotFound):
			// upstream Misskey の NO_SUCH_AVATAR error UUID を流用 (frontend
			// がコード固有の locale 表示をしているため一致が望ましい)。
			return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_AVATAR", "No such avatar file.", "539f3a45-f215-4f81-a9a8-31293640207f"))
		case errors.Is(err, user.ErrAvatarNotImage):
			return c.JSON(http.StatusBadRequest, apierr.Error("AVATAR_NOT_AN_IMAGE", "The file specified as an avatar is not an image.", "f419f9f8-2f4d-46b1-9fb4-49d3a2fd7191"))
		case errors.Is(err, user.ErrBannerNotFound):
			return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_BANNER", "No such banner file.", "0d8f5629-f210-41c2-9433-735831a58595"))
		case errors.Is(err, user.ErrBannerNotImage):
			return c.JSON(http.StatusBadRequest, apierr.Error("BANNER_NOT_AN_IMAGE", "The file specified as a banner is not an image.", "75aedb19-2afd-4e6d-87fc-67941256fa60"))
		case errors.Is(err, user.ErrAvatarIsSensitive):
			return c.JSON(http.StatusBadRequest, apierr.Error("AVATAR_IS_SENSITIVE", "The file specified as an avatar is marked as sensitive.", "71bb5e53-4742-4609-b465-36081e131208"))
		case errors.Is(err, user.ErrBannerIsSensitive):
			return c.JSON(http.StatusBadRequest, apierr.Error("BANNER_IS_SENSITIVE", "The file specified as a banner is marked as sensitive.", "e148b34c-9f33-4300-93e0-7817008fb366"))
		}
		return apierr.JSONInternalError(c)
	}

	// 本家は鍵の解除で溜まったリクエストを承認し終えてから応答する。期間の
	// 条件に当たる人は残す (#3466)。失敗しても設定は保存済みなので応答は
	// 成功のまま返し、記録だけ残す (本家はここで 500 になる。残ったリクエストは
	// following/requests/list から手で承認できる)。
	if unlocking {
		if err := h.followRequestAccepter.AcceptAllRequests(me.ID); err != nil {
			slog.Error("i/update: failed to accept pending follow requests on unlock", "user", me.ID, "error", err)
		}
	}

	// upstream i/update.ts:532-593: verifiedLinks を一旦 [] にリセットし、profile
	// fields の https URL それぞれについて rel=me backlink を非同期に検証して
	// verifiedLinks に積み直す (#1786)。同期で [] に reset (応答も空で返す)、
	// goroutine で再検証して書き込む。fire-and-forget は upstream と同様。
	if h.verifyLinkClient != nil && me.Username != "" && h.serverURL != "" {
		if err := h.userService.UpdateProfileFields(me.ID, map[string]any{"verifiedLinks": model.StringArray{}}); err == nil && bundle.Profile != nil {
			bundle.Profile.VerifiedLinks = nil
		}
		myLink := strings.TrimRight(h.serverURL, "/") + "/@" + me.Username
		if urls := httpsFieldValues(bundle.Profile); len(urls) > 0 {
			uid := me.ID
			h.background.Go(func(context.Context) {
				if verified := verifyLinks(h.verifyLinkClient, myLink, urls); len(verified) > 0 {
					_ = h.userService.UpdateProfileFields(uid, map[string]any{"verifiedLinks": model.StringArray(verified)})
				}
			})
		}
	}

	// hardMutedWords が変更された場合は streaming connection に reload signal
	// を流して即時反映する (#791)。soft (mutedWords) は frontend が /api/i から
	// 取り直して client-side で filter するので publish 不要。
	if in.HardMutedWords != nil && h.hardMutePublisher != nil {
		h.hardMutePublisher.PublishHardMuteReload(me.ID)
	}
	// mutedInstances は MuteBlockSnapshot に載るので、変更時は接続中の snapshot も
	// 取り直させる (#2400)。これが無いとインスタンスミュート直後も既存の
	// WebSocket に対象 host の event が届き続ける。
	if in.MutedInstances != nil && h.relationReload != nil {
		h.relationReload.PublishMuteBlockReload(me.ID)
	}

	// profile 変更を remote followers に Update(Person) で配信する (#1560)。
	if h.profileUpdateHook != nil {
		h.profileUpdateHook.OnLocalProfileUpdated(me.ID)
	}

	// auth middleware の tokenCache (30 秒 TTL) は name / description などの
	// mutable field を持つ user object を保持するため、更新後も stale な値が
	// 返り続けてしまう (#960)。本 request の token だけでなく、利用者の全 token
	// を全プロセスで捨てる (#3664。本家の localUserUpdated と同じく利用者単位)。
	h.invalidateUserTokenCache(me.ID)

	// upstream Misskey TS UserEntityService.ts:590-630 の MeDetailed shape を
	// drop-in 互換で返す。i/update のレスポンスはフロントの
	// updateCurrentAccount にそのまま流し込まれるため、isExplorable /
	// noCrawle / preventAiLearning など self-view-only field が UserDetailed
	// 側に無いと session の `$i` が更新されず stale なまま残る (#968)。
	// unread 系は meDetailedWithUnread で実値を埋める。生 PackMeDetailed だと
	// unreadNotificationsCount:0 等の default が `$i` を clobber する (#1258 fu)。
	resp := h.meDetailedWithUnread(c.Request().Context(), bundle.User, bundle.Profile)
	// #2106 N1: isAdmin/isModerator は role 依存で PackMeDetailed が埋めない (MeDetailed の
	// 両 field は omitempty 無し bool なので default false が乗る)。Me handler 同様
	// roleProvider から override し upstream の MeDetailed isMe ブロック (実値を返す) に揃える。
	//
	// policies / roles も同じ理由で埋める。upstream i/update は MeDetailed を
	// そのまま返すので両 field を含むが、mk-go は落としていた。フロントは
	// i/update の応答を updateCurrentAccount で `$i` にマージするため、
	// 欠けているとプロフィール更新のたびに role 由来の権限が消える。
	// meUpdated (ストリーミング) は逆に policies を含めてはならないので
	// (#1240)、共通 helper ではなくこの API 応答側だけで足す。
	isAdmin, isMod, userPolicies, userRoles := h.rolePayload(bundle.User.ID)
	resp["isAdmin"] = isAdmin
	resp["isModerator"] = isMod
	resp["policies"] = userPolicies
	resp["roles"] = userRoles
	// upstream i/update は pack(..., {includeSecrets: isSecure}) を返すため native
	// session では email / emailVerified / securityKeysList を含める (#1919)。Token
	// 判定は middleware が確実に load する me を使い、email は更新後の最新 profile を
	// 使う。app token (token != users.token) では出さない。
	h.appendIncludeSecrets(c, me, bundle.Profile, resp)
	return c.JSON(http.StatusOK, resp)
}
