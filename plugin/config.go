package plugin

// Config gives a plugin its own settings from the instance configuration.
//
// 運営者は `.config/default.yml` の `plugins:` セクションに、プラグイン名を
// キーとして書く。
//
//	plugins:
//	  gameinfo:
//	    enabled: true
//	    apiKey: "..."
//	    endpoint: https://api.example-game.com
//
// `enabled` は mk-go が解釈する予約キーで (無効なプラグインはそもそも登録
// されない)、残りがプラグインに渡る。
//
// # 秘密情報の扱い
//
// **設定ダンプ (`elythia config-dump`) では `enabled` 以外の値を既定でマスクする。**
// プラグインの設定キーは mk-go 側からは未知なので、どれが秘密かを判別できない。
// 「出してよい」と宣言する仕組みは、必要になってから足す。
//
// 環境変数によるオーバーライドは無い。Viper は既知のキーにしか環境変数を
// 適用できず、プラグインのキーは事前に列挙できないため。設定ファイルに書け
// ない値は、プラグイン自身が `os.Getenv` で読むこと。
type Config interface {
	// Unmarshal decodes this plugin's settings into v, which should be a
	// pointer to a struct with `json` tags.
	//
	// 設定が 1 つも書かれていない場合は v を変更せずに nil を返すので、
	// 呼び出し側で既定値を入れてから渡せる。
	//
	//	var c struct {
	//	    APIKey   string `json:"apiKey"`
	//	    Endpoint string `json:"endpoint"`
	//	}
	//	c.Endpoint = "https://api.example-game.com" // 既定値
	//	if err := ctx.Config().Unmarshal(&c); err != nil { ... }
	//
	// # キーの大文字小文字について
	//
	// 設定の読み込みに使っている Viper は**キーを小文字化する** (`apiKey` は
	// `apikey` になる)。構造体のフィールドは encoding/json が大文字小文字を
	// 無視して照合するため、上の例のように camelCase のタグで問題なく一致する。
	//
	// ただし **map で受ける設定はキーの大文字小文字が復元されない**。
	// 任意のキーを取る設定 (HTTP ヘッダ等) が要る場合は、小文字で来ることを
	// 前提に書くか、配列で受けること。
	Unmarshal(v any) error
}
