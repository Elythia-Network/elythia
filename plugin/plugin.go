// Package plugin is the public API for mk-go server plugins.
//
// A plugin is an ordinary Go module that mk-go compiles into its binary. It
// declares a [Definition] and receives callbacks to register HTTP routes and
// background jobs.
//
// # 触れる範囲について
//
// **このパッケージはセキュリティ境界ではない。** プラグインは mk-go と同一
// プロセスで動く Go コードなので、`os` / `net/http` / `os/exec` を普通に使え、
// 設定ファイルを読むこともできる。ここで提供しているのは「mk-go 側が壊さない
// と約束する範囲」であって、プラグインができることの上限ではない。
//
// 運営者は信頼できる作者のプラグインのみを組み込むこと。
//
// # 公開範囲を最小に保つ方針
//
// 本パッケージは公開した時点から semver の義務を負う。実際のプラグインが要求
// するまで機能を追加しない。追加は tools/pluginspec の golden 差分としてレビュー
// に現れる。
package plugin

import (
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"sync"
)

// APIVersion is the plugin API contract version mk-go implements.
//
// プラグインは自身が想定する版を [Definition.APIVersion] に書く。一致しない
// プラグインは**読み込まずにエラーにする** (黙って動かして不定の壊れ方を
// させない)。破壊的変更のときだけ上げる。
const APIVersion = 1

// Definition describes a plugin. A plugin module exposes it as a package-level
// variable named Plugin, which the build-time generator references.
type Definition struct {
	// Name identifies the plugin. It also namespaces the plugin's HTTP routes
	// (/api/plugin/<name>/...), jobs, and storage.
	Name string

	// Version is the plugin's own version, surfaced in diagnostics.
	Version string

	// APIVersion is the plugin API version this plugin was written against.
	// Must equal [APIVersion].
	APIVersion int

	// Migrations are applied to the plugin's schema before Routes and Jobs
	// run. May be nil.
	//
	// **ここで宣言する。** Routes / Jobs の中で [Storage.Migrate] を呼ぶと、
	// ロール分割 (#2459) したときに片方でしか走らない。Routes 側だけに書くと
	// queue 専用プロセスがテーブルの無い schema でジョブを回すことになる。
	// 宣言にしておけば mk-go がどちらのロールでも必ず適用する。
	Migrations []Migration

	// Routes registers HTTP endpoints. Called only in processes that serve
	// HTTP. May be nil.
	//
	// ロールでプロセスを分割できる (#2459) ため、ルートとジョブは別々の
	// callback に分ける。1 つにまとめると queue 専用プロセスがルートを登録
	// しようとする (あるいはその逆) ことになる。
	Routes func(Context, Router) error

	// Jobs registers background work. Called only in processes that run the
	// job queue. May be nil.
	Jobs func(Context, Jobs) error

	// Peered opts this plugin into [Peer] — the private channel to the same
	// plugin on other mk-go instances.
	//
	// **宣言したものだけが nodeinfo に出る。** 入れているプラグインを全部
	// 晒すと、運営者がどんな拡張を使っているかが攻撃面の情報になる。
	// 連合しないプラグインは名前も出さない。
	//
	// **[Definition.Routes] は要らない。** 受信の受け口は Peered だけで張る
	// ので、ジョブしか持たないプラグインでも受け取れる (#2822)。
	Peered bool

	// Peer registers the plugin's peer handlers. Called in every process,
	// regardless of role. May be nil.
	//
	// **ここで登録する (#2819)。** 送信の再送はキューに載るので、実際に POST
	// するのは queue ロールのプロセスになる。[Peer.OnReply] を Routes の中で
	// 登録していると、ロールを分割した構成で応答が届かない。[Peer.Handle] も
	// 同じ理由でここへ置く。
	//
	// 呼ばれる順は Migrations の後、EffectivePolicies / Routes / Jobs より前。
	// Routes / Jobs とは別の (ロールを見ない) 段階で呼ばれる。
	Peer func(Context, Peer) error

	// PeerMaxBody caps the peer request and reply body this plugin accepts,
	// in bytes. 0 uses the host default.
	//
	// **上限はプラグインごとに決める。** 小さな値をやりとりするプラグインの
	// ために、インスタンス全体で大きな本文を受けられる状態にしておく理由が
	// 無い。ここで宣言した値が「読む前」の上限になるので、宣言を小さくした
	// 分だけ外から押し込める量が減る。
	//
	// **本文はエンベロープ込みで測る。** payload に使えるのはこの値から
	// 相関 ID の分 (aidx なら 36 バイト) を引いた残り。
	//
	// **単独で広げられるのは既定値まで。** それより大きい宣言は、運営者が
	// 設定で許可したときだけ有効になる (許可が無ければ既定値に丸めて warn を
	// 出す)。プラグインを 1 つ足しただけでインスタンスの露出が広がる形に
	// しないため。詳細は docs/plugin-peer-protocol.md。
	PeerMaxBody int64

	// EffectivePolicies declares effective-policy contributions. The host calls
	// it after Migrations and before any Routes or Jobs callback.
	EffectivePolicies func(Context, EffectivePolicyInvalidator) (EffectivePolicyRegistration, error)

	// Secrets declares the secret values the operator enters from the control
	// panel (#3470). Read them with [Context.Secrets]. May be nil.
	//
	// **宣言は入力欄を出すためのもの。** 値の保存と読み出しは宣言の有無に
	// 関係なく [Context.Secrets] でできるが、管理画面から入れられるのは
	// ここに書いた名前だけになる。
	Secrets []SecretSpec
}

// Validate reports whether the definition is usable.
func (d Definition) Validate() error {
	if d.Name == "" {
		return fmt.Errorf("plugin: Name が空です")
	}
	if !validName(d.Name) {
		return fmt.Errorf("plugin: Name %q が不正です (使えるのは小文字英数字とハイフンのみ)", d.Name)
	}
	if d.APIVersion != APIVersion {
		return fmt.Errorf("plugin %q: APIVersion %d はこの Elythia (APIVersion %d) と互換がありません",
			d.Name, d.APIVersion, APIVersion)
	}
	if d.PeerMaxBody < 0 {
		return fmt.Errorf("plugin %q: PeerMaxBody が負の値です", d.Name)
	}
	if d.PeerMaxBody > 0 && !d.Peered {
		// 宣言しても効かない組み合わせ。黙って無視すると「上限を設定した
		// つもり」で通ってしまう。
		return fmt.Errorf("plugin %q: PeerMaxBody を指定するなら Peered も立てること", d.Name)
	}
	if d.Peer != nil && !d.Peered {
		return fmt.Errorf("plugin %q: Peer を指定するなら Peered も立てること", d.Name)
	}
	seen := make(map[string]struct{}, len(d.Secrets))
	for _, sp := range d.Secrets {
		if !validSecretName(sp.Name) {
			return fmt.Errorf("plugin %q: Secrets の名前 %q が不正です (英字で始まり、英数字と _ - だけで 64 文字以内)", d.Name, sp.Name)
		}
		if _, dup := seen[sp.Name]; dup {
			return fmt.Errorf("plugin %q: Secrets の名前 %q が重複しています", d.Name, sp.Name)
		}
		seen[sp.Name] = struct{}{}
	}
	if d.Routes == nil && d.Jobs == nil && d.EffectivePolicies == nil && d.Peer == nil {
		return fmt.Errorf("plugin %q: Routes も Jobs も EffectivePolicies も Peer も設定されていません", d.Name)
	}
	return nil
}

// validName restricts names to what is safe in a URL path segment, a queue task
// type, and a PostgreSQL schema name at once. 3 箇所で使う識別子なので、
// どれか 1 つでも通らない文字を最初から許さない。
func validName(s string) bool {
	if s == "" || len(s) > 32 {
		return false
	}
	for i, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9':
		case r == '-' && i > 0 && i < len(s)-1:
		default:
			return false
		}
	}
	return true
}

var (
	registryMu sync.Mutex
	registry   = map[string]Definition{}
)

// Register adds a plugin to the global registry.
//
// ビルド時に生成されるコードから呼ばれる。同名の重複は**エラーにせず panic**
// する: 生成コードの不具合であって運用時の状態ではないため、起動を許すと
// どちらが有効か分からないまま動いてしまう。
func Register(d Definition) {
	if err := d.Validate(); err != nil {
		panic(err)
	}
	registryMu.Lock()
	defer registryMu.Unlock()
	if _, dup := registry[d.Name]; dup {
		panic(fmt.Errorf("plugin: %q が二重に登録されました", d.Name))
	}
	registry[d.Name] = d
}

// Registered returns the registered plugins sorted by name.
//
// 順序を固定するのは、ルート登録順やログの並びがビルドごとに変わらないように
// するため。
func Registered() []Definition {
	registryMu.Lock()
	defer registryMu.Unlock()
	out := make([]Definition, 0, len(registry))
	for _, d := range registry {
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Context gives a plugin access to mk-go facilities during registration and at
// request time.
type Context interface {
	// Name returns the plugin's name.
	Name() string

	// Logger returns a logger tagged with the plugin name, so plugin output is
	// attributable in the instance's logs.
	Logger() *slog.Logger

	// API calls mk-go's own REST endpoints in-process.
	API() API

	// Storage returns this plugin's own PostgreSQL schema.
	Storage() Storage

	// Config returns this plugin's settings from the instance configuration.
	Config() Config

	// Accounts manages the local accounts this plugin owns — accounts that
	// nobody can sign in to, such as bots (#3468).
	//
	// **常に非 nil。** 本体の配線が無い環境では、呼ぶとエラーを返す実装になる。
	Accounts() Accounts
	// Secrets returns this plugin's encrypted secret values (#3470).
	//
	// **自分の値しか見えない。** 名前はプラグインごとに分かれているので、
	// 他のプラグインの同名の値とは混ざらない。nil は返さない。
	Secrets() Secrets

	// Queue enqueues jobs onto this plugin's own queue.
	//
	// [Jobs.Handle] で登録した名前を使う。**Routes からも Jobs からも呼べる**
	// ので、HTTP ハンドラの中で重い処理を後回しにできる。
	Queue() Queue

	// Peer returns the channel to the same plugin on other mk-go instances.
	//
	// [Definition.Peered] を立てていない場合、呼び出しはエラーを返す
	// (nil は返さないので、nil チェックは要らない)。
	Peer() Peer

	// HTTP returns an HTTP client wired with the instance's outbound policy.
	//
	// **自分で `&http.Client{}` を作らないこと。** この client は
	//
	//   - SSRF ガード (プライベート IP / 非 http(s) への接続を落とす)
	//   - 運営者の `proxy` / `outgoingAddress` / `outgoingAddressFamily`
	//   - 送信の timeout
	//
	// を既に持っている。素の client を使うと、運営者が proxy を設定していても
	// **そのプラグインだけがサーバーの素の IP で外へ出る**。
	//
	// **Transport を差し替えないこと。** 差し替えると上の 2 つが消える。
	// per-request の timeout は `http.NewRequestWithContext` で付ける。
	//
	// これはセキュリティ境界ではない (パッケージの doc を参照) — プラグインは
	// `net/http` を直接使えるので、これは「間違えにくい既定」を配るもの。
	HTTP() *http.Client

	// Go runs fn in a new goroutine, recovering panics.
	//
	// **プラグインが自分で `go` を書くとプロセスごと落ちる。** Go は他の
	// goroutine の panic を回収できないため、mk-go 側の recover
	// (echo の Recover middleware 等) では止められない。
	Go(fn func())
}
