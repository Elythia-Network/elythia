package mfm

import "unicode/utf16"

// CollectHashtags parses each input as MFM and returns the hashtag tags
// (from #tag tokens) found across all texts, ordered by first appearance and
// dedup'd exactly. Because extraction goes through the full MFM parser,
// hashtags inside code blocks, URLs, links and mentions are correctly excluded
// — unlike a flat regex scan.
//
// 返り値は `#` を含まない bare tag (例: "golang")。Empty input は nil を返す。
// case-insensitive dedup / 長さ truncation などの呼び出し側固有の正規化は
// 行わない (hashtag.Extract がそれを担う)。
func CollectHashtags(texts ...string) []string {
	if len(texts) == 0 {
		return nil
	}
	seen := make(map[string]struct{})
	var out []string
	var walk func(n *Node)
	walk = func(n *Node) {
		if n.Type == NodeHashtag {
			if tag, ok := n.Props["hashtag"].(string); ok && tag != "" {
				if _, dup := seen[tag]; !dup {
					seen[tag] = struct{}{}
					out = append(out, tag)
				}
			}
		}
		for _, c := range n.Children {
			walk(c)
		}
	}
	for _, t := range texts {
		if t == "" {
			continue
		}
		for _, n := range Parse(t) {
			walk(n)
		}
	}
	return out
}

// CollectEmojiCodes parses each input as MFM and returns the union of
// custom emoji names (from :code: tokens) found across all texts, dedup'd
// and ordered by first appearance.
//
// 用途: note 作成時に text + cw 等を渡して note.Emojis に格納する emoji 名一覧を
// 得る (#629)。受信側 (federation/resolver) は AP Tag から拾うが、送信側はこちらで
// MFM AST を walk して拾う。user.Emojis は読み方を項目ごとに変えるので
// ExtractCustomEmojis を使う (#3270)。
//
// 返り値の各要素は `:` を含まない bare name (例: "foo")。Empty input は
// nil を返す。
func CollectEmojiCodes(texts ...string) []string {
	if len(texts) == 0 {
		return nil
	}
	seen := make(map[string]struct{})
	var out []string
	var walk func(n *Node)
	walk = func(n *Node) {
		if n.Type == NodeEmojiCode {
			if name, ok := n.Props["name"].(string); ok && name != "" {
				if _, dup := seen[name]; !dup {
					seen[name] = struct{}{}
					out = append(out, name)
				}
			}
		}
		for _, c := range n.Children {
			walk(c)
		}
	}
	for _, t := range texts {
		if t == "" {
			continue
		}
		for _, n := range Parse(t) {
			walk(n)
		}
	}
	return out
}

// customEmojiNameMax is upstream extractCustomEmojisFromMfm's limit on the
// emoji name length (JavaScript string length, i.e. UTF-16 code units).
const customEmojiNameMax = 100

// ExtractCustomEmojis returns the custom emoji names (`:name:`) in already
// parsed nodes, dedup'd and ordered by first appearance. Mirrors upstream
// extractCustomEmojisFromMfm: names longer than 100 UTF-16 code units are
// skipped.
//
// CollectEmojiCodes と違って入力を自分で parse しない。本家 i/update は名前と
// 補足情報を parseSimple、プロフィールとフォローされたときのメッセージを parse で
// 読み分けるので、呼び出し側が読み方を選べるようにする。
func ExtractCustomEmojis(nodes []*Node) []string {
	seen := make(map[string]struct{})
	var out []string
	var walk func(n *Node)
	walk = func(n *Node) {
		if n.Type == NodeEmojiCode {
			if name, ok := n.Props["name"].(string); ok && len(utf16.Encode([]rune(name))) <= customEmojiNameMax {
				if _, dup := seen[name]; !dup {
					seen[name] = struct{}{}
					out = append(out, name)
				}
			}
		}
		for _, c := range n.Children {
			walk(c)
		}
	}
	for _, n := range nodes {
		walk(n)
	}
	return out
}
