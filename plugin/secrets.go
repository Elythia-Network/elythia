package plugin

import (
	"context"
	"errors"
)

// SecretSpec declares a secret value (an API key, a token, ...) that the
// operator enters from the control panel (#3470).
//
// 宣言したものだけが、管理画面の入力欄に出る。プラグインが実行時に自分で
// 得た値 (OAuth で受け取ったトークンなど) は、宣言しなくても [Secrets.Set] で
// 置ける。
type SecretSpec struct {
	// Name identifies the secret within the plugin. Use ASCII letters, digits,
	// '_' and '-', starting with a letter, at most 64 characters.
	Name string

	// Description is shown next to the input in the control panel.
	Description string
}

// Secrets stores this plugin's secret values, encrypted at rest (#3470).
//
// # 書き込み専用であること
//
// 保存した値は**プラグインのサーバー側のコードからしか読めない**。管理画面にも
// 本体の API の応答にも値は出ず、出るのは「設定済みか」と末尾の数文字だけ。
// 読んだ値をプラグインのルートの応答へそのまま返すと、この約束をプラグインが
// 自分で破ることになるので、返さないこと。
//
// # 暗号化
//
// 値は運営者が設定ファイルに置いた鍵 (`pluginSecretKey`) で暗号化して DB に
// 保存する。鍵が設定されていなければ、どの操作も [ErrSecretsUnavailable] を
// 返す (平文で置く経路は作らない)。鍵を入れ替えたり失ったりすると、それまでの
// 値は [ErrSecretUnreadable] になり、管理画面から入れ直すことになる。
//
// 値は呼ぶたびに DB から読んで復号する。管理画面で入れ替えた値は、次の
// Get から使われる (プロセスを再起動しなくてよい)。
type Secrets interface {
	// Get returns the secret. It returns [ErrSecretNotSet] when the value has
	// not been entered, [ErrSecretUnreadable] when it was encrypted with a key
	// that is no longer configured, and [ErrSecretsUnavailable] when no key is
	// configured.
	Get(ctx context.Context, name string) (string, error)

	// Set stores value under name, replacing any previous value. The value
	// must be non-empty and at most 8 KiB.
	Set(ctx context.Context, name, value string) error

	// Delete removes the secret. Deleting a secret that is not set is not an
	// error.
	Delete(ctx context.Context, name string) error
}

var (
	// ErrSecretNotSet reports that the secret has not been entered.
	ErrSecretNotSet = errors.New("plugin: 秘密の値が設定されていません")

	// ErrSecretsUnavailable reports that the operator has not configured
	// pluginSecretKey, so secrets can be neither stored nor read.
	ErrSecretsUnavailable = errors.New("plugin: 秘密の値を使えません (設定ファイルに pluginSecretKey がありません)")

	// ErrSecretUnreadable reports that the stored value cannot be decrypted
	// with the configured key (the key was replaced or lost). The operator has
	// to enter the value again.
	ErrSecretUnreadable = errors.New("plugin: 秘密の値を復号できません (鍵が変わっています。管理画面から入れ直してください)")
)

// validSecretName mirrors the rule the host enforces when storing a secret.
//
// 宣言の段階で落とす。起動してから管理画面で保存しようとして初めて弾かれると、
// 作者ではなく運営者が原因を探すことになる。
func validSecretName(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for i, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z':
		case i > 0 && (r >= '0' && r <= '9' || r == '_' || r == '-'):
		default:
			return false
		}
	}
	return true
}
