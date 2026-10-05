package entitycompat

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/pgarray"
)

// **モデルを JSON 化したときに秘密が出ないことを固定する。**
//
// mk-go にはモデルをそのまま JSON 化する経路があり、`json:"-"` が唯一の
// 防波堤になっているフィールドがある。実測で `model.User.Token` のタグを
// `json:"token"` に変えると **native token が JSON に出る**が、`make gates`
// も既存のテストも全て緑のままだった。native token を取れればそのユーザー
// として API を叩けるので、あらゆる権限ゲートを迂回できる。
//
// **モデルを直接 JSON 化する経路は 6 系統ある** (数え方: `internal/model` の型が
// `encoding/json` の Marshal/Unmarshal/Encode/Decode か `echo.Context.JSON` に
// 静的型で到達する非テスト箇所):
//
//   - `internal/core/moderationlog/service.go` が `json.Marshal(info)` で
//     モデルごと記録する (`*model.Meta` の before/after、
//     `[]*model.RegistrationTicket`、`*model.Role`、`*model.Ad`、
//     `*model.SystemWebhook` など)。`admin/show-moderation-logs` が
//     `info` をそのまま返す
//   - `internal/core/ephemeral/store.go` が `model.Note` / `model.User` を
//     Redis へ入れる
//   - `internal/core/webpush/cache.go` が `[]*model.SwSubscription` を
//     Redis へ入れて読み戻す
//   - **admin API のレスポンス本体**: `internal/api/admin/relays.go` が
//     `*model.Relay` / `[]*model.Relay` を `c.JSON` にそのまま渡す
//   - **同**: `internal/api/admin/abuse_report_notification.go` の
//     `packedRecipient` が `*model.AbuseReportNotificationRecipient` を
//     埋め込んで返す。`admin/show-user` も `[]*model.Role` を入れた map を返す
//   - `internal/core/drive/chunked_upload.go` が `[]model.ChunkedUploadPart` を
//     jsonb 列へ往復し、`internal/core/instance/instance_service.go` が
//     `[]model.SuspendedSoftwareEntry` を読み戻す
//
// **「API のレスポンスは必ず map か entity を通る」は誤り** — 上の 2 つは
// モデルの json タグがそのままレスポンスの shape になる。
//
// **つまりタグは「今すぐ効く」。** 1 周目のレビュー後に 4 件を `json:"-"` へ
// 変えたところ、うち 2 件 (`Meta.SMTPPass` / `RegistrationTicket.Code`) で
// **moderation log の記録が壊れた** — しかも既存のテストは 1 つも落ちな
// かった。しかもその判断の根拠にした「直接 marshal は 2 箇所だけ」も、
// 直したはずの「3 系統」も、どちらも数え落としだった。**タグを動かす前に
// この 6 系統に乗るかを必ず確かめること。**
//
// **名前だけでは判定できない。** `Meta` の captcha secret は `admin/meta` が
// 管理画面へ返すうえ modlog にも載るし、drive の `accessKey` は URL の構成
// 要素で秘密ではない。したがって #2792 と同じ allowlist 方式にし、**出して
// よいものには実態を書かせる** (「出してよい理由」ではなく「どの経路で実際に
// 出るか」を書く — 前者だと、タグが使われていないという誤った前提のまま
// 動かしてしまう)。
//
// **allowlist は該当集合と突き合わせる。** 実在しないキーを書いても黙って
// 無視される形だと、検査していないのに緑になる。初版は
// `Meta.SensitiveMediaDetectionAPIKey` を登録していたが、当時の正規表現は
// `ApiKey` しか見ておらず **`APIKey` には一致しないので何も検査していなかった**
// (この突き合わせを足して初めて発覚した)。ただし**これが守るのは allowlist に
// 該当があるキーだけ**で、`Pass` / `Code` のように該当が全て `json:"-"` 側に
// あるものは正規表現から外しても緑のままになる。そちらは
// `mustDetectSecretFields` で別に固定してある。
//
// **`internal/model` には置けない。** あそこは `_test.go` を 1 つも持たない
// ので、テストを足すと CI のカバレッジ閾値 (90%) の対象に**初めて**入り、
// wire 層と同じ理由で 0% に張り付いて落ちる (#462 と同型)。gate の置き場は
// ここで、対象パッケージのソースはファイルとして読む。
//
// **既知の取りこぼし** (どれも実測で確認した穴):
//
//   - 名前に該当の語を含まない秘密は拾えない
//   - **`Pass` / `Code` / `Auth` / `Hash` は部分一致**なので、`PassedAt` /
//     `StatusCode` / `CountryCode` のような普通の名前も拾う。fail-closed
//     (allowlist に理由を書けば通る) なので危険側には倒れないが、素の語を
//     足すときはこの費用を見込むこと
//   - **`datatypes.JSON` 列の中身は見えない** (`Meta.ClientOptions` など)
//   - **`internal/model` 以外の構造体は対象外**。`internal/queue` の
//     `WebhookPayload.OverrideSecret` は実際に marshal されて Redis の
//     ジョブに載るが、ここでは見ていない
//   - **`internal/model` の直下しか見ない** (glob が `*.go` で非再帰)
//   - **名前付き型の中に匿名 struct を入れると見えない**
//     (`Creds struct { Token string }` の形)
//   - `MarshalJSON` を自前実装した型は静的検査を抜ける。下の
//     `TestModelJSONDoesNotContainSecrets` が押さえるのは**そこに書いた型
//     だけ**で、それ以外の型は素通りする
//   - 埋め込みは `internal/model` 内で宣言された型なら昇格フィールドも見える
//     (TypeSpec を総なめするため)。見えないのは**別パッケージの型を
//     埋め込んだ場合**
//
// `Key` 全体には広げていない — 実測で **19 件増えて全部が偽陽性**になる。
// `PublicKey` / `*SiteKey` (captcha のサイトキーはフロントへ配る公開値) /
// `SortKeys` / `ExcludeKeywords` / `RegistryItem.Key` /
// `UserPublickey.KeyPEM` などが入って
// allowlist が誤検知で埋まり、本物が紛れる。
var secretFieldNameRe = regexp.MustCompile(`Password|Passwd|Pass|Token|Secret|PrivateKey|PrivKey|Credential|Code|ApiKey|APIKey|AuthKey|AccessKey|Auth|Hash`)

// serializableSecretLike lists fields that match the name pattern but are
// intentionally serialized. **理由を書くこと。**
var serializableSecretLike = map[string]string{
	// **moderation log がモデルごと marshal するので、このタグは実際に使われる。**
	// `internal/core/moderationlog/service.go` が `json.Marshal(info)` し、
	// `admin/show-moderation-logs` (RequireAdmin) が `info` をそのまま返す。
	// upstream も同じものを mask せずに記録するので、落とすと監査記録が欠ける。
	"Meta.SMTPPass":           "moderation log が update-meta の before/after を *model.Meta ごと marshal する (upstream も mask しない)",
	"RegistrationTicket.Code": "moderation log が createInvitation で []*model.RegistrationTicket を marshal する (upstream も同じ)",
	"SystemWebhook.Secret":    "moderation log が before/after を *model.SystemWebhook ごと marshal する (upstream も同じ)",

	// **Redis のキャッシュが JSON で往復するので、落とすと復元に失敗する。**
	"SwSubscription.Auth": "internal/core/webpush/cache.go が []*model.SwSubscription を Redis へ JSON で入れて読み戻す。落とすと Web Push の暗号化に要る auth secret が失われる",

	// `admin/meta` (RequireAdmin + read:admin:meta) と `admin/captcha/current` が
	// map を手で組んで返す運営者の設定値。公開 `/api/meta` には出ない。
	// **ただし上の modlog 経路でも出る**ので、タグを落とせば監査記録が欠ける。
	"Meta.HcaptchaSecretKey":             "admin/meta が返す運営者の設定値。modlog にも載る",
	"Meta.RecaptchaSecretKey":            "同上",
	"Meta.TurnstileSecretKey":            "同上",
	"Meta.McaptchaSecretKey":             "同上",
	"Meta.SwPrivateKey":                  "同上",
	"Meta.ObjectStorageAccessKey":        "同上",
	"Meta.ObjectStorageSecretKey":        "同上",
	"Meta.SensitiveMediaDetectionAPIKey": "同上",
	"Meta.DeeplAuthKey":                  "同上",
	"Meta.TruemailAuthKey":               "同上",
	"Meta.VerifymailAuthKey":             "同上",

	// URL の構成要素であって秘密ではない (`GET /files/:accessKey` は公開ルート)。
	"DriveFile.AccessKey":            "URL の構成要素で秘密ではない",
	"DriveFile.ThumbnailAccessKey":   "同上",
	"DriveFile.WebpublicAccessKey":   "同上",
	"ChunkedUploadSession.AccessKey": "同上",

	// 本人 / 作成者にだけ返る。
	"AccessToken.Token":          "auth/session/userkey と miauth が本人に返す。`i/apps` は token 値を返さない",
	"AccessToken.Hash":           "sha256(token)。middleware は sha256(提示値) を計算して引く (auth.go:445) ので、hash 値そのものでは認証できない",
	"App.Secret":                 "packApp が includeSecret のときだけ map に入れる",
	"AuthSession.Token":          "認証フローで本人に返る",
	"Webhook.Secret":             "作成者にだけ map で返る",
	"PasswordResetRequest.Token": "メールのリンクに載せる (本人にだけ届く)",

	// 名前が引っかかるだけで秘密ではない。
	"NoteDraft.Hashtag":                    "ハッシュタグ。`Hash` に部分一致するだけ",
	"UserProfile.UsePasswordLessLogin":     "真偽値であって秘密ではない",
	"UserSecurityKey.CredentialDeviceType": "WebAuthn の種別。秘密ではない",
	"UserSecurityKey.CredentialBackedUp":   "真偽値であって秘密ではない",
}

// **検出集合そのものを固定する。** allowlist の dead-entry 検査は
// 「allowlist に該当があるキー」しか守らないので、**allowlist に 1 つも該当が
// 無い alternative は正規表現から外しても緑のまま**になる。`Pass` / `Code` を
// 足した直後が実際にそうで、1 周目の指摘を塞いだ修正そのものが黙って巻き
// 戻せる状態だった (敵対的レビュー 2 周目で実測)。ここに並べたものが検出
// されなくなったら落とす。**`json:"-"` のフィールドを足したらここにも足すこと**
// (一覧は手書きなので自動では増えない)。
var mustDetectSecretFields = []string{
	"User.Token",
	"UserProfile.Password",
	"UserProfile.TwoFactorSecret",
	"UserProfile.TwoFactorBackupSecret",
	"UserProfile.TwoFactorTempSecret",
	"UserProfile.EmailVerifyCode",
	"UserPending.Password",
	"UserPending.Code",
	"UserKeypair.PrivateKey",
	"UserKeypairExtra.Ed25519PrivateKey",
	"SignupApplication.ClaimCodeHash",
	"Meta.SMTPPass",
	"RegistrationTicket.Code",
	"SwSubscription.Auth",
	"AccessToken.Hash",
	"AccessToken.Token",
	"App.Secret",
	"DriveFile.AccessKey",
}

// scanSecretLikeFields returns every exported struct field in src whose name
// looks like a secret, keyed by "<Type>.<Field>" and valued by its json tag.
func scanSecretLikeFields(src []byte, filename string) (map[string]string, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, filename, src, 0)
	if err != nil {
		return nil, err
	}

	out := map[string]string{}
	ast.Inspect(file, func(n ast.Node) bool {
		ts, ok := n.(*ast.TypeSpec)
		if !ok {
			return true
		}
		st, ok := ts.Type.(*ast.StructType)
		if !ok {
			return true
		}
		for _, fld := range st.Fields.List {
			if len(fld.Names) == 0 {
				continue
			}
			for _, name := range fld.Names {
				if !name.IsExported() || !secretFieldNameRe.MatchString(name.Name) {
					continue
				}
				// **タグ無しも拾う。** `encoding/json` はタグの無い exported
				// フィールドを Go の名前でそのまま出すので、ここで skip すると
				// 「フィールドを足してタグを忘れる」形が素通りする。
				var tag reflect.StructTag
				if fld.Tag != nil {
					tag = reflect.StructTag(strings.Trim(fld.Tag.Value, "`"))
				}
				out[ts.Name.Name+"."+name.Name] = tag.Get("json")
			}
		}
		return true
	})
	return out, nil
}

// leakingSecretFields returns the keys of found whose json tag serializes the
// value and that allow does not justify. **判定はここ 1 箇所にまとめる** —
// 人工ソースのテストと実モデルのテストが同じ枝を通るようにするため。
func leakingSecretFields(found, allow map[string]string) []string {
	var leaks []string
	for key, jsonTag := range found {
		if jsonTag == "-" {
			continue
		}
		if _, ok := allow[key]; ok {
			continue
		}
		leaks = append(leaks, key)
	}
	sort.Strings(leaks)
	return leaks
}

// modelSecretLikeFields scans internal/model for secret-looking fields.
func modelSecretLikeFields(t *testing.T) map[string]string {
	t.Helper()

	files, err := filepath.Glob(filepath.Join(repoRoot(t), "internal", "model", "*.go"))
	require.NoError(t, err)

	out := map[string]string{}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		require.NoError(t, err)
		got, err := scanSecretLikeFields(src, f)
		require.NoErrorf(t, err, "parse %s", f)
		for k, v := range got {
			out[k] = v
		}
	}
	return out
}

// **検出そのものが動くことを、実モデルとは独立に固定する。**
// 実モデルは allowlist で全て許可済みなので leak が 0 件であり、実モデルだけを
// 見ていると**検出の枝が一度も実行されない**。ここで人工のソースを食わせる。
func TestScanSecretLikeFields(t *testing.T) {
	src := []byte(`package p

type Sample struct {
	ID                 string  ` + "`json:\"id\"`" + `
	Token              *string ` + "`json:\"token\"`" + `
	HiddenToken        *string ` + "`json:\"-\"`" + `
	TwoFactorSecret    *string ` + "`json:\"twoFactorSecret,omitempty\"`" + `
	ObjectStorageAccessKey *string ` + "`json:\"objectStorageAccessKey\"`" + `
	unexportedSecret   string  ` + "`json:\"unexportedSecret\"`" + `
	Nickname           string  ` + "`json:\"nickname\"`" + `
	PublicKey          string  ` + "`json:\"publicKey\"`" + `
	SecretNoTag        string
	NoTag              string
}

type Other struct {
	Password string ` + "`json:\"password\"`" + `
}
`)

	got, err := scanSecretLikeFields(src, "sample.go")
	require.NoError(t, err)

	require.Equal(t, map[string]string{
		"Sample.Token":                  "token",
		"Sample.HiddenToken":            "-",
		"Sample.TwoFactorSecret":        "twoFactorSecret,omitempty",
		"Sample.ObjectStorageAccessKey": "objectStorageAccessKey",
		// **タグを書き忘れた形も拾う。** `encoding/json` はタグの無い exported
		// フィールドを Go の名前でそのまま出すので、ここを skip すると
		// 「フィールドを足してタグを忘れる」という最頻のミスが素通りする。
		"Sample.SecretNoTag": "",
		"Other.Password":     "password",
	}, got, "秘密らしい名前のフィールドを、タグ無しも含めて拾うこと")

	// leak 判定そのもの。**allowlist は人工のものを渡す** — 実の
	// serializableSecretLike に依存させると、そちらを編集しただけでこの
	// テストが落ちて、検出ロジックの検査という役目を果たさなくなる。
	allow := map[string]string{"Sample.ObjectStorageAccessKey": "テスト用"}
	require.Equal(t,
		[]string{"Other.Password", "Sample.SecretNoTag", "Sample.Token", "Sample.TwoFactorSecret"},
		leakingSecretFields(got, allow),
		"json:\"-\" でなく allowlist にも無いものが leak として出ること "+
			"(HiddenToken は json:\"-\"、ObjectStorageAccessKey は allowlist なので出ない)")
}

func TestModelSecretFieldsAreNotSerialized(t *testing.T) {
	found := modelSecretLikeFields(t)

	// 1 つも拾えなかったら落とす。書式が変わって正規表現が空振りすると、
	// 検査していないのに緑になる (#2874 / #2828 と同じ判断)。
	require.NotEmpty(t, found, "秘密らしい名前のフィールドを 1 つも拾えない (書式が変わった?)")

	// 検出集合が痩せたら落とす。allowlist の dead-entry 検査では守れない
	// alternative があるため (mustDetectSecretFields の doc を参照)。
	var undetected []string
	for _, key := range mustDetectSecretFields {
		if _, ok := found[key]; !ok {
			undetected = append(undetected, key)
		}
	}
	sort.Strings(undetected)
	require.Emptyf(t, undetected,
		"検出集合から外れた: %v\n"+
			"secretFieldNameRe を狭めたか、フィールドを rename / 削除した", undetected)

	leaks := leakingSecretFields(found, serializableSecretLike)
	for i, key := range leaks {
		leaks[i] = key + " (json:\"" + found[key] + "\")"
	}
	require.Emptyf(t, leaks,
		"秘密のフィールドが JSON に出る: %v\n"+
			"モデルをそのまま JSON 化する経路があるので、`json:\"-\"` を付けるか、\n"+
			"出してよい理由を serializableSecretLike に書くこと", leaks)
}

// **allowlist に死んだ項目を残さない。** 実在しないキーが黙って無視される形
// だと、そのフィールドを守っているつもりで何も検査していない状態になる。
func TestSerializableSecretLikeHasNoDeadEntries(t *testing.T) {
	found := modelSecretLikeFields(t)

	var dead, contradictory []string
	for key, reason := range serializableSecretLike {
		require.NotEmptyf(t, reason, "%s: 出してよい理由を書くこと", key)
		tag, ok := found[key]
		if !ok {
			dead = append(dead, key)
			continue
		}
		// **allowlist に載せた = 出ることが前提**なので、`json:"-"` は宣言と
		// 矛盾する。2 周目で `Meta.SMTPPass` / `RegistrationTicket.Code` の
		// タグを落として moderation log の記録を壊したのがこの形で、当時は
		// 何も落ちなかった。allowlist 全件をこれで守る。
		if tag == "-" {
			contradictory = append(contradictory, key)
		}
	}
	sort.Strings(contradictory)
	require.Emptyf(t, contradictory,
		"allowlist に載っているのに `json:\"-\"` になっている: %v\n"+
			"落とすなら allowlist からも外し、**どの経路が壊れるか**を先に確かめること "+
			"(moderation log / ephemeral store / webpush cache / admin API のレスポンス本体)",
		contradictory)
	sort.Strings(dead)
	require.Emptyf(t, dead,
		"serializableSecretLike に、検出対象に無いキーが残っている: %v\n"+
			"rename / 削除で消えたか、secretFieldNameRe が一致しなくなっている", dead)
}

// **実際に JSON 化して漏れないことも見る。** タグの静的検査だけだと、
// `MarshalJSON` を自前で実装した型で抜ける。
func TestModelJSONDoesNotContainSecrets(t *testing.T) {
	tok := "must-not-appear-token"
	b, err := json.Marshal(model.User{ID: "u1", Username: "alice", Token: &tok})
	require.NoError(t, err)
	require.NotContainsf(t, string(b), tok,
		"User を JSON 化すると native token が出る。これを取れると全ての権限ゲートを迂回できる")

	pw, sec, bak, tmp := "pw-hash", "totp-secret", "backup-secret", "temp-secret"
	pb, err := json.Marshal(model.UserProfile{
		UserID:                "u1",
		Password:              &pw,
		TwoFactorSecret:       &sec,
		TwoFactorBackupSecret: pgarray.StringArray{bak},
		TwoFactorTempSecret:   &tmp,
	})
	require.NoError(t, err)
	for _, s := range []string{pw, sec, bak, tmp} {
		require.NotContainsf(t, string(pb), s, "UserProfile を JSON 化すると %q が出る", s)
	}

	kb, err := json.Marshal(model.UserKeypair{UserID: "u1", PrivateKey: "PRIVATE-PEM"})
	require.NoError(t, err)
	require.NotContainsf(t, string(kb), "PRIVATE-PEM", "UserKeypair を JSON 化すると署名鍵が出る")

	eb, err := json.Marshal(model.UserKeypairExtra{UserID: "u1", Ed25519PrivateKey: "ED25519-PEM"})
	require.NoError(t, err)
	require.NotContainsf(t, string(eb), "ED25519-PEM", "UserKeypairExtra を JSON 化すると Ed25519 の署名鍵が出る")

	ub, err := json.Marshal(model.UserPending{ID: "p1", Username: "bob", Password: "pending-pw-hash"})
	require.NoError(t, err)
	require.NotContainsf(t, string(ub), "pending-pw-hash", "UserPending を JSON 化すると password hash が出る")
}

// **タグが「使われている」側も固定する。**
//
// allowlist に載っているものの一部は、単に許されているのではなく
// **落とすと壊れる**。1 周目のレビュー後に `Meta.SMTPPass` と
// `RegistrationTicket.Code` を `json:"-"` にしたところ、moderation log の
// 記録が黙って欠けた (既存のテストは 1 つも落ちなかった)。同じことを
// 繰り返さないよう、出ることをここで固定する。
func TestModelJSONKeepsAuditedFields(t *testing.T) {
	// moderation log は `admin/update-meta` の before/after を `*model.Meta`
	// ごと marshal する (`internal/core/moderationlog/service.go`)。
	// upstream も SMTP secret を mask せずに記録する。
	sp := "smtp-secret"
	mb, err := json.Marshal(model.Meta{SMTPPass: &sp})
	require.NoError(t, err)
	require.Containsf(t, string(mb), `"smtpPass"`,
		"Meta を JSON 化すると smtpPass が消える。moderation log の before/after が欠ける")

	// `admin/invite/create` は `[]*model.RegistrationTicket` をそのまま
	// moderation log へ渡す。code が消えると招待の監査記録が成立しない。
	tb, err := json.Marshal(model.RegistrationTicket{ID: "t1", Code: "INVITE-CODE"})
	require.NoError(t, err)
	require.Containsf(t, string(tb), "INVITE-CODE",
		"RegistrationTicket を JSON 化すると code が消える。招待の監査記録が空になる")

	// `internal/core/webpush/cache.go` は `[]*model.SwSubscription` を Redis へ
	// JSON で入れて読み戻す。auth が消えると Web Push の暗号化ができない。
	sb, err := json.Marshal(model.SwSubscription{ID: "s1", Auth: "auth-secret"})
	require.NoError(t, err)
	require.Containsf(t, string(sb), "auth-secret",
		"SwSubscription を JSON 化すると auth が消える。Redis から復元した購読で Web Push が壊れる")
}
