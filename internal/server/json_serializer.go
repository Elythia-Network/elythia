package server

import (
	"bytes"
	"encoding/json"
	"encoding/json/jsontext"
	jsonv2 "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"

	"github.com/labstack/echo/v4"
)

// fastJSONSerializer is the echo JSONSerializer used for all c.JSON / c.Bind
// paths. It is intentionally backed by the stdlib `encoding/json`.
//
// 経緯 (#1142 で調査確定): #507 で goccy/go-json に差し替えたが、v0.10.6 が
// remote Renote-with-Files を含む timeline payload で `ptrToString` nil-deref
// panic を起こすため revert した (#542)。v0.10.6 は goccy の最新版であり
// bump で回避する道は無い (dead end)。bytedance/sonic も評価したが、重量級の
// JIT 依存 + arch 制約 + 独自 panic surface があり、JSON encode の限定的な
// perf gain に対して drop-in 互換性最優先の server には blast radius が大き
// すぎるため見送った。したがって stdlib を意図的な恒久選択とする。vetted な
// 高速 encoder が現れたら再検討する。
type fastJSONSerializer struct{}

// Serialize writes i as JSON to the response. indent が空でなければ
// pretty-print する (echo 標準と同じセマンティクス)。
func (fastJSONSerializer) Serialize(c echo.Context, i interface{}, indent string) error {
	enc := json.NewEncoder(c.Response())
	if indent != "" {
		enc.SetIndent("", indent)
	}
	return enc.Encode(i)
}

// requestDecodeOptions are encoding/json's v1 semantics with one change:
// object keys bind to struct fields only on an exact (case-sensitive) match.
//
// v1 は完全一致の field が無いと大文字小文字を無視した一致も採るので、
// `{"reportid": ...}` が reportId に入り、`{"reportId": "r1", "REPORTID": null}`
// では後の null が勝つ。本家は ajv が JS の object をキーの完全一致で読むので、
// 前者は reportId の欠落、後者は reportId="r1" になる (#3330)。v1 にはこれを
// 切る手段が無いため、Go 1.27 で既定有効の encoding/json/v2 に v1 の option
// 一式を渡し、MatchCaseInsensitiveNames だけを外す。v1 の Unmarshal 自体が
// `jsonv2.Unmarshal(b, v, DefaultOptionsV1())` と同じ実装なので、重複キー
// (後勝ち)・不正 UTF-8・型違いの *json.UnmarshalTypeError は従来と同じ。
// **途中で切れた入力 (`{`、`{"a":"x"`、`nul`) のエラーだけ型が変わる。** 旧
// json.Decoder は io.ErrUnexpectedEOF を返していたが、この経路は
// *json.SyntaxError を返す。どちらも echo の binder で 400 の HTTPError に
// なり、/api では JSONBodyParse が先に FST_ERR_CTP_INVALID_JSON_BODY で
// 弾くので、応答は変わらない。
var requestDecodeOptions = jsonv2.JoinOptions(json.DefaultOptionsV1(), jsonv2.MatchCaseInsensitiveNames(false))

// Deserialize parses request body JSON into i. Object keys are matched
// against struct fields exactly (case-sensitively), like upstream's ajv
// validation of the parsed JS object; see requestDecodeOptions. On an API
// endpoint a body that is not a JSON object is rejected when i expects an
// object; see rejectNonObjectBody. 不正な JSON は echo の HTTPError(400) に
// 翻訳し、そのまま返すと echo の error handler に渡る。
func (fastJSONSerializer) Deserialize(c echo.Context, i interface{}) error {
	data, err := io.ReadAll(c.Request().Body)
	if err != nil {
		return err
	}
	if err := rejectNonObjectBody(c, reflect.TypeOf(i), data); err != nil {
		return err
	}
	// 従来の json.NewDecoder(...).Decode と同じく、先頭の 1 値だけを読む
	// (後続のデータは見ない)。
	dec := jsontext.NewDecoder(bytes.NewReader(data), requestDecodeOptions)
	if err := jsonv2.UnmarshalDecode(dec, i, requestDecodeOptions); err != nil {
		var ute *json.UnmarshalTypeError
		if errors.As(err, &ute) {
			return echo.NewHTTPError(http.StatusBadRequest,
				fmt.Sprintf("Unmarshal type error: expected=%v, got=%v, field=%v, offset=%v",
					ute.Type, ute.Value, ute.Field, ute.Offset)).SetInternal(err)
		}
		var se *json.SyntaxError
		if errors.As(err, &se) {
			return echo.NewHTTPError(http.StatusBadRequest,
				fmt.Sprintf("Syntax error: offset=%v, error=%v", se.Offset, se.Error())).SetInternal(err)
		}
		return err
	}
	return nil
}
