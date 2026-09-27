// Package effectivepolicy contains the host schema shared by production policy
// resolution and plugin authoring tests.
package effectivepolicy

import (
	"fmt"
	"math"
	"strings"

	"github.com/shiroha-a/mk/plugin"
)

var defaults = map[string]any{
	"gtlAvailable":          true,
	"ltlAvailable":          true,
	"canPublicNote":         true,
	"mentionLimit":          20,
	"canInvite":             false,
	"inviteLimit":           0,
	"inviteLimitCycle":      10080,
	"inviteExpirationTime":  0,
	"canManageCustomEmojis": false,
	// 絵文字の登録申請 (#2934)。**default true** — 登録は必ずモデレーターの
	// 承認を通るので、申請そのものを既定で塞ぐ必要が無い。canCreateChannel と
	// 同じく、絞りたい運営者が role で false にする。
	// canManageCustomEmojis を持つ人は申請ではなく直接登録できる。
	"canRequestCustomEmojis":     true,
	"canManageAvatarDecorations": false,
	"canSearchNotes":             false,
	"canSearchUsers":             true,
	"canUseTranslator":           true,
	"canHideAds":                 false,
	// upstream Misskey #17121のchannel作成権限。default trueで全員を許可し、
	// adminがrole経由で個別userを絞る。
	"canCreateChannel":       true,
	"driveCapacityMb":        100,
	"maxFileSizeMb":          30,
	"alwaysMarkNsfw":         false,
	"canUpdateBioMedia":      true,
	"pinLimit":               5,
	"antennaLimit":           5,
	"wordMuteLimit":          200,
	"webhookLimit":           3,
	"clipLimit":              10,
	"noteEachClipsLimit":     200,
	"userListLimit":          10,
	"userEachUserListsLimit": 50,
	"rateLimitFactor":        1,
	"avatarDecorationLimit":  1,
	"canImportAntennas":      false,
	"canImportBlocking":      false,
	"canImportFollowing":     false,
	"canImportMuting":        false,
	"canImportUserLists":     false,
	"chatAvailability":       "available",
	"uploadableFileTypes":    []string{"text/*", "application/json", "image/*", "video/*", "audio/*"},
	"noteDraftLimit":         10,
	"scheduledNoteLimit":     1,
	"watermarkAvailable":     true,
	// 分割uploadはmk-go独自。instance側の機能flagがfalseなら、このdefault
	// だけでは有効にならない。
	"canUseChunkedUpload":                true,
	"chunkedUploadMaxConcurrentSessions": 4,
	"chunkedUploadMaxPendingMb":          1024,
	// カスタム絵文字申請の期間上限 (#2958、mk-go独自)。**0は無制限**。
	// ローリング期間 (過去24時間 / 7日 / 30日) で数える — 固定暦にすると
	// タイムゾーン依存になり、切り替わり直前と直後に連続で申請できる。
	//
	// 既定は0 (無制限)。既存インスタンスの挙動を変えないため。
	// 申請そのものを止めるのはcanRequestCustomEmojisの仕事。
	"emojiApplicationMaxPerDay":   0,
	"emojiApplicationMaxPerWeek":  0,
	"emojiApplicationMaxPerMonth": 0,
	// 同時に審査待ちにできる件数 (#2977)。**上の期間上限とは数え方が逆**で、
	// pendingだけを数えるので却下・取り下げ・承認で枠が戻る。既定は0 (無制限)。
	"emojiApplicationMaxPending": 0,
	// カスタム絵文字をアバターデコレーションとして重ねられるか (#2975、mk-go独自)。
	// **個数は専用のpolicyを持たず、既存のavatarDecorationLimitに合算する** —
	// 管理者が登録したデコレーションと同じ場所に並ぶので、別枠にすると
	// 「1つしか付けられない」と言いながら合計2つ付いている状態になる。
	//
	// 既定はtrue。ローカル絵文字は本文・リアクションで既に誰にでも見えており、
	// センシティブなものは設定時にも表示時にも弾く。canCreateChannelと同じく、
	// 絞りたい運営者がroleでfalseにする。
	"canUseEmojiAsAvatarDecoration": true,
	// IP から関連アカウントを引く権限 (#3104)。**default false** — upstream の
	// `admin/get-user-ips` は requireAdmin なので、既定では同じ「管理者のみ」に
	// なる。モデレーターへ開きたい運営者だけがロールで true にする。
	"canSearchIpHistory": false,
	// optOutNotificationTypesはmk-go独自 (#2898)。ロール単位で受け取らない通知
	// タイプを列挙する。型ごとにcanReceiveXxxを増やす形にすると、固有通知を
	// 足すたびにpolicyが増えるので1キーにまとめている。
	//
	// **集約はintersection** (role_service.goのaggregatePolicyValues)。
	// uploadableFileTypesと同じset unionにすると、複数ロールに属するほど通知が
	// 減る = 厳しい方に倒れ、upstreamの「緩い方に倒す」思想と食い違う。
	"optOutNotificationTypes": []string{},
}

// Defaults returns a mutable copy of the host's native policy defaults.
// Mutable values are copied as well.
func Defaults() map[string]any {
	result := make(map[string]any, len(defaults))
	for key, value := range defaults {
		if values, ok := value.([]string); ok {
			// **`append([]string(nil), 空...)` は nil を返す。** 空の既定を持つ
			// policy (optOutNotificationTypes) がそのまま JSON の `null` になり、
			// meta.policies を読む frontend が配列を期待して壊れる (#2898 の
			// 本番確認で実際に `null` が返っていた)。uploadableFileTypes は
			// 既定が非空なので露見していなかった。
			result[key] = append(make([]string, 0, len(values)), values...)
			continue
		}
		result[key] = value
	}
	return result
}

// ValidateRegistration applies the host's startup validation to a provider
// registration.
func ValidateRegistration(reg plugin.EffectivePolicyRegistration) error {
	if err := reg.Validate(); err != nil {
		return err
	}
	for _, key := range reg.Keys {
		if _, ok := defaults[key]; !ok {
			return fmt.Errorf("effectivepolicy: provider は既定外の policy key %q を宣言しています", key)
		}
	}
	return nil
}

// ValidateContributions reports whether contributions satisfy the host's native
// policy schema.
//
// activeRoles are the role IDs the request carried in
// [plugin.EffectivePolicyRequest.ActiveAssignments]. A contribution that sets
// ReplaceRoleID must name one of them — otherwise it would replace a
// contribution that does not exist in this request — and must leave Priority and
// Order at 0, because the replaced entry inherits the role's own declared
// priority. Breaking either rule fails the whole provider, like any other
// malformed output.
func ValidateContributions(keys, activeRoles []string, contributions []plugin.EffectivePolicyContribution) bool {
	type contributionTie struct {
		key string
		// order only distinguishes additive contributions; a replacement always
		// carries 0, so replacements are told apart by roleID instead.
		order  int
		roleID string
	}
	seen := make(map[contributionTie]struct{}, len(contributions))
	for _, contribution := range contributions {
		if !declaresKey(keys, contribution.Key) || contribution.Priority < 0 || contribution.Priority > 2 {
			return false
		}
		if contribution.ReplaceRoleID != "" &&
			(contribution.Priority != 0 || contribution.Order != 0 || !declaresActiveRole(activeRoles, contribution.ReplaceRoleID)) {
			return false
		}
		native, ok := defaults[contribution.Key]
		if !ok {
			return false
		}
		tie := contributionTie{key: contribution.Key, order: contribution.Order, roleID: contribution.ReplaceRoleID}
		if _, duplicate := seen[tie]; duplicate {
			return false
		}
		seen[tie] = struct{}{}
		if !contribution.UseDefault && !valueValid(contribution.Key, native, contribution.Value) {
			return false
		}
	}
	return true
}

// declaresActiveRole reports whether roleID is one of the request's active manual
// roles. Only a role that carries a native contribution can have it replaced.
func declaresActiveRole(activeRoles []string, roleID string) bool {
	for _, active := range activeRoles {
		if active == roleID {
			return true
		}
	}
	return false
}

func declaresKey(keys []string, key string) bool {
	for _, declared := range keys {
		if declared == key {
			return true
		}
	}
	return false
}

// ValidatePolicyValue reports whether value has the type the policy expects.
//
// **管理者が入れる値にも型検査が要る (#3037)。** policy の consumer は
// `if limit, ok := role.PolicyNumber(v); ok { ...gate... }` の形で読むので、
// 数値の policy に `"10"` のような**文字列が入ると ok が false になり、
// 上限違反で弾かれるのではなく上限そのものが消える** (#2611 と同じ壊れ方)。
// `admin/roles/create` / `update` / `update-default-policies` は値の型を
// 見ていないので、管理画面の外から 1 回叩けばその状態を作れた。
//
// **未知のキーは通す。** upstream は JS の object lookup なので、既定に無い
// キーは誰も読まない = 何の影響も無い。ここで弾くと、upstream が新しい
// policy を足したときに mk-go だけがその設定を拒否する側になる。
func ValidatePolicyValue(key string, value any) bool {
	native, ok := Defaults()[key]
	if !ok {
		return true
	}
	// **空文字を含む文字列配列は弾かない (#3037 レビュー)。**
	//
	// `valueValid` はプラグインの contribution 用で、そちらは「宣言した値を
	// そのまま使う」前提なので空要素を拒否している。管理画面の入力は違う —
	// `uploadableFileTypes` の編集 UI は `MkTextarea` を `split('\n')` する
	// だけなので、**末尾で Enter を押す / 欄を空にするだけ**で `[""]` が飛ぶ。
	// 受け側の `aggregateStringSetUnion` は元から「trim して空は読み飛ばす」
	// fail-soft なので、書き込み時に拒否するのは**今まで通っていた入力を
	// 落とす回帰**になる。
	if _, isStrings := native.([]string); isStrings {
		return stringSliceValue(value)
	}
	return valueValid(key, native, value)
}

// stringSliceValue reports whether value is a list of strings (空要素可)。
func stringSliceValue(value any) bool {
	switch v := value.(type) {
	case []string:
		return true
	case []any:
		for _, item := range v {
			if _, ok := item.(string); !ok {
				return false
			}
		}
		return true
	}
	return false
}

func valueValid(key string, native, value any) bool {
	switch native.(type) {
	case bool:
		_, ok := value.(bool)
		return ok
	case int:
		return numberValid(value)
	case string:
		v, ok := value.(string)
		if !ok {
			return false
		}
		if key == "chatAvailability" {
			return v == "available" || v == "readonly" || v == "unavailable"
		}
		return true
	case []string:
		switch v := value.(type) {
		case []string:
			for _, item := range v {
				if strings.TrimSpace(item) == "" {
					return false
				}
			}
			return true
		case []any:
			for _, item := range v {
				s, ok := item.(string)
				if !ok || strings.TrimSpace(s) == "" {
					return false
				}
			}
			return true
		}
		return false
	default:
		return false
	}
}

func numberValid(value any) bool {
	switch v := value.(type) {
	case int:
		return true
	case int64:
		converted := int(v)
		return int64(converted) == v
	case float64:
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return false
		}
		minInclusive := float64(math.MinInt)
		maxExclusive := -minInclusive
		return v >= minInclusive && v < maxExclusive
	default:
		return false
	}
}
