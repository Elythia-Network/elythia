package entity

import (
	"encoding/json"
	"strings"

	"github.com/elythia-network/elythia/internal/model"
)

// parseCustomEmojiReaction extracts the (name, host) pair from a reaction
// string. Custom emoji reactions are wrapped in colons (`:smile:` for local
// or `:smile@remote.example.com:` for remote); raw unicode reactions (no
// surrounding colons, e.g. heart) are returned with ok=false. Local
// reactions return host="".
//
// reaction_service.resolveReaction が local 絵文字を `:name@.:` 形式 (host
// を `.` で表す TS 互換 canonical 形式) で永続化するため、parser 側でも `@.`
// を空 host に正規化する。これを忘れると DB lookup が host="." vs DB の
// host IS NULL で空振りし、reactionEmojis が空のままになる。
func parseCustomEmojiReaction(s string) (name, host string, ok bool) {
	if len(s) < 3 || s[0] != ':' || s[len(s)-1] != ':' {
		return "", "", false
	}
	inner := s[1 : len(s)-1]
	if at := strings.Index(inner, "@"); at >= 0 {
		host = inner[at+1:]
		if host == "." {
			host = ""
		}
		return inner[:at], host, true
	}
	return inner, "", true
}

// emojiNameHost is a (name, host) pair preserving distinct hosts when a
// single note's reactions contain the same emoji name from multiple
// remote hosts. Keying solely by name would collapse such cases.
type emojiNameHost struct {
	name string
	host string
}

// reactionEmojiKey は frontend の lookup (`reaction.substring(1, length-1)`)
// と一致するキーを返す。local は "smile"、remote は "smile@host"。
func reactionEmojiKey(name, host string) string {
	if host == "" {
		return name
	}
	return name + "@" + host
}

// EmojiLookup is the minimal interface required to batch-fetch emoji rows
// for populating UserLite.Emojis and NoteEntity.Emojis. 循環依存を避ける
// ため interface で受け取る (実装は repository.EmojiRepository)。
type EmojiLookup interface {
	FindManyByNamesAndHost(names []string, host *string) ([]*model.Emoji, error)
}

// EmojiKeyLookup is the optional cross-host batch lookup. When the
// EmojiLookup handed to NewEmojiResolver also implements it, all (name, host)
// pairs are resolved in one call instead of one call per host.
// repository.CachedEmojiRepository implements it with a cache shared across
// requests, so the next timeline page does not fetch the same emojis (#3383).
type EmojiKeyLookup interface {
	FindManyByKeys(keys []model.EmojiKey) ([]*model.Emoji, error)
}

// EmojiResolver caches (name, host) → URL lookups so that packers can
// populate Emojis maps without repeating DB queries. InstanceResolverと同
// パターンで、1回のhost別batch fetch後にO(1)参照する。
//
// nilレシーバは常にno-opを返す (EmojiLookupが未配線な呼出し元向け)。
type EmojiResolver struct {
	cache map[string]string // "name@host" → url
	// reactionPairs memoizes the custom-emoji (name, host) pairs decoded from
	// each note's Reactions during construction, so PopulateNoteReactionEmojis
	// does not decode the same JSONB a second time.
	//
	// PackNotes / PackNoteWithInstance が渡すのは flattenNotesPlusRelations の
	// 結果 (top-level + 1 段目の Renote / Reply) なので、大半の note はここに
	// 載る。一方 applyNoteResolvers は entity の木をそれより深く辿る (depth-2 の
	// renote.renote など) ため、未登録の note が来る。PackNotifications はさらに
	// User だけを持つ合成 note を足して渡す (Reactions は nil なので nil エントリ
	// が増えるだけ)。未登録のときは PopulateNoteReactionEmojis 側でデコードして
	// 従来どおりの結果を返す。
	//
	// 値が nil のエントリは「見たが remote custom emoji reaction が無かった」を
	// 表す。ここを省くと、`{}` や unicode だけの reactions を持つ note で毎回
	// フォールバックのデコードが走ってしまう (Reactions が空の note は
	// PopulateNoteReactionEmojis の最初の guard で抜けるのでメモまで来ない)。
	reactionPairs map[*model.Note][]emojiNameHost
}

// NewEmojiResolver collects unique (name, host) pairs from the notes and
// their authors, then batch-fetches matching emoji rows grouped by host.
//
// lookup==nilなら空cacheのresolverを返す (PopulateXxx は全てno-op)。
func NewEmojiResolver(lookup EmojiLookup, notes []*model.Note) *EmojiResolver {
	r := &EmojiResolver{cache: map[string]string{}}
	if lookup == nil {
		return r
	}
	r.reactionPairs = make(map[*model.Note][]emojiNameHost, len(notes))
	// host別に絵文字名を集約
	hostNames := map[string]map[string]struct{}{}
	addNames := func(names []string, host *string) {
		if len(names) == 0 {
			return
		}
		h := ""
		if host != nil {
			h = *host
		}
		if hostNames[h] == nil {
			hostNames[h] = map[string]struct{}{}
		}
		for _, n := range names {
			hostNames[h][n] = struct{}{}
		}
	}
	for _, n := range notes {
		if n == nil {
			continue
		}
		addNames(n.Emojis, n.UserHost)
		// ローカルの利用者の絵文字は解決しない (PopulateUserEmojis 参照) ので引かない。
		if n.User != nil && n.User.Host != nil {
			addNames(n.User.Emojis, n.User.Host)
		}
		// note.Reactions の JSON キーから custom emoji を抽出して
		// host 別 batch fetch のリストに追加する (#459)。reaction 元の
		// host は note.UserHost と必ずしも一致しないため、reaction
		// 文字列内の `@host` を信頼する。同名の絵文字が複数 host から
		// 届くケースを失わないよう slice で受け取る。
		pairs := collectReactionEmojiNames(n.Reactions)
		r.reactionPairs[n] = pairs
		for _, pair := range pairs {
			h := pair.host
			var hostPtr *string
			if h != "" {
				hostPtr = &h
			}
			addNames([]string{pair.name}, hostPtr)
		}
	}
	if kl, ok := lookup.(EmojiKeyLookup); ok {
		keys := make([]model.EmojiKey, 0, len(hostNames))
		for host, nameSet := range hostNames {
			for n := range nameSet {
				keys = append(keys, model.EmojiKey{Name: n, Host: host})
			}
		}
		// エラーでも返った行 (cache に載っていた分など) は使う。host 単位で
		// 引いていた従来の経路も、失敗した host を飛ばして残りは解決していた。
		if len(keys) > 0 {
			emojis, _ := kl.FindManyByKeys(keys)
			for _, e := range emojis {
				k := model.EmojiKeyOf(e)
				r.cache[k.Name+"@"+k.Host] = emojiDisplayURL(e)
			}
		}
		return r
	}
	// hostごとにbatch fetch
	for host, nameSet := range hostNames {
		names := make([]string, 0, len(nameSet))
		for n := range nameSet {
			names = append(names, n)
		}
		var hostPtr *string
		if host != "" {
			hostPtr = &host
		}
		emojis, err := lookup.FindManyByNamesAndHost(names, hostPtr)
		if err != nil {
			continue
		}
		for _, e := range emojis {
			r.cache[e.Name+"@"+host] = emojiDisplayURL(e)
		}
	}
	return r
}

// emojiDisplayURL is the URL an emoji is shown with: PublicURL, falling back
// to OriginalURL.
func emojiDisplayURL(e *model.Emoji) string {
	if e.PublicURL != "" {
		return e.PublicURL
	}
	return e.OriginalURL
}

// PopulateNoteEmojis resolves emoji names stored in note.Emojis to URLs and
// fills entity.Emojis. local note (host==nil) は packNoteAtDepth が
// entity.Emojis を nil にしているため何もしない (upstream は local の emojis を
// 出さない、#1639)。remote note は packNoteAtDepth が &{} を確保済みなので、
// 解決できた emoji を流し込む (解決ゼロでも &{} のまま = 空 object 出力)。
func (r *EmojiResolver) PopulateNoteEmojis(note *model.Note, entity *NoteEntity) {
	if r == nil || note == nil || entity == nil || entity.Emojis == nil {
		return
	}
	if len(note.Emojis) == 0 {
		return
	}
	host := ""
	if note.UserHost != nil {
		host = *note.UserHost
	}
	for _, name := range note.Emojis {
		if url, ok := r.cache[name+"@"+host]; ok {
			(*entity.Emojis)[name] = url
		}
	}
}

// PopulateNoteReactionEmojis resolves the custom emoji used inside
// note.Reactions and populates entity.ReactionEmojis with `key → url`
// where key matches the frontend's lookup format
// (`reaction.substring(1, length-1)`): `name` for local custom emoji,
// `name@host` for remote. Raw unicode reactions are skipped — frontend
// renders them directly without map lookup. (#459)
func (r *EmojiResolver) PopulateNoteReactionEmojis(note *model.Note, entity *NoteEntity) {
	if r == nil || note == nil || entity == nil || len(note.Reactions) == 0 {
		return
	}
	// cache が空なら一致する emoji は 1 つも無いので、デコードするだけ無駄。
	// lookup==nil で構築された resolver もここで抜ける。
	if len(r.cache) == 0 {
		return
	}
	pairs, memoized := r.reactionPairs[note]
	if !memoized {
		pairs = collectReactionEmojiNames(note.Reactions)
	}
	if len(pairs) == 0 {
		return
	}
	out := make(map[string]string)
	for _, pair := range pairs {
		if url, ok := r.cache[pair.name+"@"+pair.host]; ok {
			out[reactionEmojiKey(pair.name, pair.host)] = url
		}
	}
	if len(out) > 0 {
		entity.ReactionEmojis = out
	}
}

// collectReactionEmojiNames decodes the per-note Reactions JSON map and
// returns custom-emoji entries as a slice of (name, host) pairs. Raw
// unicode reactions are filtered out. Same-name-different-host pairs
// are preserved as separate entries (a name-keyed map would collapse
// them and lose remote emoji URLs).
//
// upstream NoteEntityService:389-391 は reactionEmojiNames を
// `.filter(x => x.startsWith(':') && x.includes('@') && !x.includes('@.'))`
// で remote custom emoji のみに絞る (コメント: リモートカスタム絵文字のみ)。
// local custom emoji (bare `:name:` / canonical `:name@.:`、いずれも
// host="") は frontend が instance の emoji registry から解決するため
// reactionEmojis には載せない。そこで host="" の entry は除外する (#1781)。
func collectReactionEmojiNames(raw []byte) []emojiNameHost {
	if len(raw) == 0 {
		return nil
	}
	var counts map[string]int
	if err := json.Unmarshal(raw, &counts); err != nil {
		return nil
	}
	var out []emojiNameHost
	for reaction := range counts {
		name, host, ok := parseCustomEmojiReaction(reaction)
		if !ok || host == "" {
			continue
		}
		out = append(out, emojiNameHost{name: name, host: host})
	}
	return out
}

// PopulateUserEmojis resolves emoji names stored in user.Emojis to URLs
// and sets lite.Emojis. Local users are left empty, like upstream.
//
// 本家 CustomEmojiService.populateEmoji は host が null だと解決しないので、
// ローカルの利用者の emojis は常に {} になる。mk-go も i/update で列を書く
// ようになった (#3270) ので、解決すると本家に無い値が API に出る。
func (r *EmojiResolver) PopulateUserEmojis(user *model.User, lite *UserLite) {
	if r == nil || user == nil || lite == nil || len(user.Emojis) == 0 || user.Host == nil {
		return
	}
	host := *user.Host
	emojis := make(map[string]string, len(user.Emojis))
	for _, name := range user.Emojis {
		if url, ok := r.cache[name+"@"+host]; ok {
			emojis[name] = url
		}
	}
	if len(emojis) > 0 {
		lite.Emojis = emojis
	}
}
