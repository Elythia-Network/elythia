package entity

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"

	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/misc/reactionlegacy"
	"github.com/elythia-network/elythia/internal/model"
	"gorm.io/datatypes"
)

// firstNonNil returns the first non-nil/non-empty string among the pointers,
// mirroring upstream `url ?? uri`. Used by the name-prefixed text formatting.
func firstNonNil(ps ...*string) string {
	for _, p := range ps {
		if p != nil && *p != "" {
			return *p
		}
	}
	return ""
}

// BufferedReactionsReader reads buffered reaction deltas (Redis-side)
// for a batch of notes. enableReactionsBuffering=true な instance では
// reaction count が Redis 側にバッファされ、定期 flush で DB の
// note.Reactions JSONB に反映されるが、flush までの間は DB が stale で
// timeline で reaction が見えない。PackNotes/PackNoteWithInstance に
// この interface を渡すと merge してから serialize する (#647)。
//
// nil 実装も受け付ける (= buffering 無し / direct writer 使用時)。
type BufferedReactionsReader interface {
	GetBufferedMany(ctx context.Context, noteIDs []string) (map[string]map[string]int64, error)
}

// localEmojiPattern matches `:name:` without `@host` suffix.
var localEmojiPattern = regexp.MustCompile(`^:([\w+\-]+):$`)

// NoteEntity is the note representation returned by API endpoints.
type NoteEntity struct {
	ID                 string            `json:"id"`
	CreatedAt          string            `json:"createdAt"`
	UserID             string            `json:"userId"`
	User               UserLite          `json:"user"`
	Text               *string           `json:"text"`
	CW                 *string           `json:"cw"`
	Visibility         string            `json:"visibility"`
	LocalOnly          bool              `json:"localOnly"`
	ReactionAcceptance *string           `json:"reactionAcceptance"`
	Reactions          datatypes.JSON    `json:"reactions"`
	ReactionCount      int               `json:"reactionCount"`
	ReactionEmojis     map[string]string `json:"reactionEmojis"`
	RenoteCount        int16             `json:"renoteCount"`
	RepliesCount       int16             `json:"repliesCount"`
	// ClippedCount は upstream の detail block (opts.detail) に属し、reply embed
	// (detail:false) では key ごと省かれる (note.ts:255 optional:true)。pointer +
	// omitempty で「detail のとき &n (0 含む) を出力 / reply embed では nil 省略」
	// を表す (#1816)。plain int + omitempty だと detail 時の clippedCount:0 を
	// 誤って落とすため pointer 必須。
	ClippedCount *int        `json:"clippedCount,omitempty"`
	URI          *string     `json:"uri,omitempty"`
	URL          *string     `json:"url,omitempty"`
	ReplyID      *string     `json:"replyId"`
	RenoteID     *string     `json:"renoteId"`
	Reply        *NoteEntity `json:"reply,omitempty"`
	Renote       *NoteEntity `json:"renote,omitempty"`
	FileIDs      []string    `json:"fileIds"`
	Files        []any       `json:"files"`
	Tags         []string    `json:"tags,omitempty"`
	Poll         *PollEntity `json:"poll,omitempty"`
	// Emojis は upstream NoteEntityService.ts:412 `host != null ? populateEmojis
	// : undefined` に合わせ、remote note (author host != nil) のみ出力する
	// (custom emoji が無くても `{}`)。local note は nil → omitempty で省略
	// (#1639)。pointer 型なので nil(省略) と &{}(空 object 出力) を区別できる。
	Emojis    *map[string]string `json:"emojis,omitempty"`
	ChannelID *string            `json:"channelId,omitempty"`
	Channel   *ChannelLite       `json:"channel,omitempty"`
	// visibleUserIds / mentions / hasPoll は upstream NoteEntityService が
	// specified 以外 / 空 / false のとき undefined にして key を落とす
	// (note.ts optional:true)。mk-go も omitempty + packer 側 conditional で
	// 揃える (#1561)。
	VisibleUserIDs []string `json:"visibleUserIds,omitempty"`
	Mentions       []string `json:"mentions,omitempty"`
	HasPoll        bool     `json:"hasPoll,omitempty"`
	MyReaction     *string  `json:"myReaction,omitempty"`
	// ReactionAndUserPairCache は `userId/reaction` 形式の pair 配列で、
	// クライアントが myReaction を API 無しで解決するために使う。upstream は
	// opts.withReactionAndUserPairCache=true のときだけ出力し (空でも `[]`)、
	// その指定は streaming / AP 経路 (NoteCreateService / ApInboxService) に
	// 限られる (#1640)。REST pack では undefined。pointer 型で nil(REST→省略)
	// と &[](streaming→空配列出力) を区別する。
	ReactionAndUserPairCache *[]string `json:"reactionAndUserPairCache,omitempty"`
	IsHidden                 bool      `json:"isHidden,omitempty"`
}

// ChannelLite is the minimal channel info embedded in NoteEntity.
type ChannelLite struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// UserID は channel の作成者。upstream note.ts:194-197 で channel object 内
	// の non-optional / nullable:true field として定義され、pack 時に必ず含める
	// (#1561)。system channel 等で nil のときは null を出すため非 omitempty。
	UserID                *string `json:"userId"`
	Color                 string  `json:"color"`
	IsSensitive           bool    `json:"isSensitive"`
	AllowRenoteToExternal bool    `json:"allowRenoteToExternal"`
}

// PollEntity is the poll representation in a note.
type PollEntity struct {
	ExpiresAt *string      `json:"expiresAt"`
	Multiple  bool         `json:"multiple"`
	Choices   []PollChoice `json:"choices"`
}

// PollChoice represents a single poll choice.
type PollChoice struct {
	Text    string `json:"text"`
	Votes   int    `json:"votes"`
	IsVoted bool   `json:"isVoted"`
}

// maxNoteEmbedDepth caps how many levels of Renote chain are expanded when
// packing. 純粋リノート (boost) が引用投稿 (quote) を包む場合、TL の最上位は
// pure renote で、その renote (= quote) の renote (= 引用先) まで出さないと
// frontend が引用先を「削除されたノート」として描画してしまう。upstream
// NoteEntityService は renote 埋め込みを detail:true で再帰 pack するため
// renote チェーンは実質無制限だが、frontend の quote box (MkNoteSimple) は
// 再帰しないので表示には 2 段で足りる。reply 埋め込みは detail:false (子を
// 持たない) なので depth ではなく detail gate で 1 段に止める (下記 packNoteAtDepth)。
// 2 にすることで preload / batch loader 側も renote ブランチを 2 段供給する。
const maxNoteEmbedDepth = 2

// PackNote converts a model.Note to a NoteEntity. Renote / Reply の embed は
// maxNoteEmbedDepth で制限する。
func PackNote(n *model.Note, idGen id.Generator) NoteEntity {
	return packNoteAtDepth(n, idGen, 0, true)
}

// packPoll maps a preloaded model.Poll onto the Misskey-compatible PollEntity.
// nil 入力 (Poll relation が preload されていない / hasPoll=false) は nil を
// 返し、上位の `json:"poll,omitempty"` で response から省略させる。
//
// IsVoted は viewer context が無いと判定できないため常に false で埋める
// (#690 の最小修正範囲)。viewer の vote 判定は別 path で enrich する想定で、
// 投稿直後の create response では「自分の投稿に自分はまだ vote していない」
// が常に正解なので false 固定でも frontend 表示は正しい。
func packPoll(p *model.Poll) *PollEntity {
	if p == nil {
		return nil
	}
	choices := make([]PollChoice, len(p.Choices))
	for i, text := range p.Choices {
		votes := 0
		if i < len(p.Votes) {
			votes = int(p.Votes[i])
		}
		choices[i] = PollChoice{Text: text, Votes: votes, IsVoted: false}
	}
	var expiresAt *string
	if p.ExpiresAt != nil {
		s := p.ExpiresAt.UTC().Format("2006-01-02T15:04:05.000Z")
		expiresAt = &s
	}
	return &PollEntity{
		ExpiresAt: expiresAt,
		Multiple:  p.Multiple,
		Choices:   choices,
	}
}

// HideNoteEntity blanks an embedded note's content for a viewer who is not
// allowed to see it, mirroring upstream NoteEntityService.hideNote. It clears
// exactly the 7 fields upstream clears (text/cw/poll/files/fileIds/visibleUserIds
// + isHidden) and keeps everything else (id/userId/user/createdAt/reactions/
// counts/hasPoll), so the frontend still renders a "hidden post" placeholder
// card. Used by both the REST and streaming embed-visibility gates (#1536).
//
// fileIds / files は non-nil 空スライスにして JSON で [] を出す。
// visibleUserIds は upstream hideNote (NoteEntityService.ts:184) と同じく nil に
// して omitempty で key を落とす (#1561 で packer 全体を specified 限定 +
// omitempty 化したことに合わせる)。
func HideNoteEntity(n *NoteEntity) {
	if n == nil {
		return
	}
	n.Text = nil
	n.CW = nil
	n.Poll = nil
	n.FileIDs = make([]string, 0)
	n.Files = []any{}
	n.VisibleUserIDs = nil
	n.IsHidden = true
}

// packNoteAtDepth packs a note at the given embed depth. detail mirrors
// upstream NoteEntityService's `opts.detail`: when false (reply embeds) the
// clippedCount / poll / myReaction fields and the reply/renote sub-embeds are
// omitted. Top-level notes and renote embeds use detail:true; reply embeds use
// detail:false (upstream NoteEntityService.ts:432-460, reply at :437 detail:false
// / renote at :445 detail:true). myReaction is applied later by
// note_field_resolver (which skips reply embeds), #1816.
func packNoteAtDepth(n *model.Note, idGen id.Generator, depth int, detail bool) NoteEntity {
	createdAt := ""
	if t, err := idGen.ParseTime(n.ID); err == nil {
		createdAt = t.UTC().Format("2006-01-02T15:04:05.000Z")
	}

	fileIDs := make([]string, 0)
	if n.FileIDs != nil {
		fileIDs = n.FileIDs
	}

	// visibleUserIds は visibility=specified のときだけ出す (upstream
	// NoteEntityService.ts:405 `visibility==='specified' ? visibleUserIds :
	// undefined`、#1561)。それ以外は nil のままにして omitempty で省略する。
	var visibleUserIDs []string
	if n.Visibility == model.NoteVisibilitySpecified {
		visibleUserIDs = make([]string, 0)
		if n.VisibleUserIDs != nil {
			visibleUserIDs = n.VisibleUserIDs
		}
	}

	// mentions は空のとき省略する (upstream NoteEntityService.ts:427
	// `mentions.length>0 ? mentions : undefined`、#1561)。空 slice を渡すと
	// omitempty で消えるので nil/空のままで良い。
	var mentions []string
	if len(n.Mentions) > 0 {
		mentions = n.Mentions
	}

	// emojis は remote note (author host != nil) のときだけ出力する (upstream
	// NoteEntityService.ts:412、#1639)。remote は custom emoji が無くても &{} で
	// 空 object を出し、local は nil のままにして omitempty で key を省略する。
	// 実際の URL は PopulateNoteEmojis が後段でこの map に流し込む。
	var emojis *map[string]string
	if n.UserHost != nil {
		m := make(map[string]string)
		emojis = &m
	}

	// name 付き note (AP の Page/Link 等) は upstream NoteEntityService.ts:379-381
	// と同じく text を `【name】\n{text}\n\n{url??uri}` に整形する (#1561)。
	text := n.Text
	if n.Name != nil && *n.Name != "" {
		if link := firstNonNil(n.URL, n.URI); link != "" {
			body := ""
			if n.Text != nil {
				body = strings.TrimSpace(*n.Text)
			}
			formatted := "【" + *n.Name + "】\n" + body + "\n\n" + link
			text = &formatted
		}
	}

	// reactions JSONB は正規化と合計の両方に要るので、1 回だけデコードする。
	packedReactions, reactionCount := packReactions(n.Reactions)

	entity := NoteEntity{
		ID:                 n.ID,
		CreatedAt:          createdAt,
		UserID:             n.UserID,
		Text:               text,
		CW:                 n.CW,
		Visibility:         string(n.Visibility),
		LocalOnly:          n.LocalOnly,
		ReactionAcceptance: n.ReactionAcceptance,
		Reactions:          packedReactions,
		ReactionCount:      reactionCount,
		ReactionEmojis:     make(map[string]string),
		RenoteCount:        n.RenoteCount,
		RepliesCount:       n.RepliesCount,
		URI:                n.URI,
		URL:                n.URL,
		ReplyID:            n.ReplyID,
		RenoteID:           n.RenoteID,
		FileIDs:            fileIDs,
		Files:              []any{},
		Tags:               n.Tags,
		Emojis:             emojis,
		ChannelID:          n.ChannelID,
		VisibleUserIDs:     visibleUserIDs,
		Mentions:           mentions,
		HasPoll:            n.HasPoll,
	}

	// clippedCount / poll は upstream の detail block (opts.detail) に属する。
	// reply embed (detail:false) では出力しない (#1816)。hasPoll は detail 外なので
	// 上の struct literal で常に出す。
	if detail {
		clipped := int(n.ClippedCount)
		entity.ClippedCount = &clipped
		entity.Poll = packPoll(n.Poll)
	}

	if n.User != nil {
		entity.User = PackUserLite(n.User)
	}

	// Renote / Reply の target は repository 層で preload (Renote.User /
	// Renote.Renote.User / Reply.User …) されている前提で展開する。preload が
	// 無ければ n.Renote == nil になり、フロントエンドはこれを「削除された投稿」
	// として描画する (renoteId だけが入ってる状態と区別するため #416)。
	//
	// 展開は detail==true のときだけ行う。upstream NoteEntityService は renote
	// embed を detail:true (NoteEntityService.ts:445)、reply embed を detail:false
	// (:437) で pack し、detail:false の note は renote/reply を一切持たない。
	// renote チェーンは detail:true のまま maxNoteEmbedDepth まで再帰させ、
	// 「pure renote → quote → 引用先」の引用先 (renote.renote) を出す。これが
	// 無いと frontend が引用先を「削除されたノート」として描画する。
	//
	// reply embed も detail 中は depth ごとに展開する。upstream は renote を
	// detail:true で pack するので、その中の reply (= renote.reply、depth 2) も
	// 出る。reply 自体は detail:false で pack するため reply.reply へは伸びない。
	// depth-2 embed には可視性ゲートが要るので、notehide / streaming note_filter /
	// webpush / webhook の 4 箇所も renote.reply を同じ深さまで見る。
	// reply embed では clippedCount/poll/myReaction を省く (#1816)。
	if detail && depth < maxNoteEmbedDepth {
		if n.Renote != nil {
			r := packNoteAtDepth(n.Renote, idGen, depth+1, true)
			entity.Renote = &r
		}
		if n.Reply != nil {
			r := packNoteAtDepth(n.Reply, idGen, depth+1, false)
			entity.Reply = &r
		}
	}

	return entity
}

// PackNotes packs a slice of notes and populates UserLite.Instance for remote
// users in a single batch fetch via lookup. lookup == nil keeps Instance as
// nil (convenient for handlers not yet wired or for contexts where instance
// embed is unnecessary).
//
// reactionReader が non-nil なら buffered reactions を batch fetch して
// flat 内の各 note.Reactions に in-place merge してから resolver を構築する
// (#647)。これにより enableReactionsBuffering=true でも timeline / show
// レスポンスで最新 reaction count / emoji が返る。reader が nil なら
// 旧挙動 (DB のみ) で動く。ctx は reader.GetBufferedMany に渡され、
// handler 側の deadline / cancellation / tracing が下流に伝播する (#657)。
//
// flattenNotesPlusRelations で top-level + Renote/Reply の target note を
// 1 まとめにしてから resolver を作る。CollectNoteAuthors も flatten 済みの
// スライスから author を拾うので、埋め込み note の remote user にも Instance /
// emoji が正しく載る。
func PackNotes(ctx context.Context, notes []*model.Note, idGen id.Generator, instLookup InstanceLookup, emojiLookup EmojiLookup, reactionReader BufferedReactionsReader) []NoteEntity {
	flat := flattenNotesPlusRelations(notes)
	mergeBufferedReactions(ctx, flat, reactionReader)
	instResolver := NewInstanceResolver(instLookup, CollectNoteAuthors(flat)...)
	emojiResolver := NewEmojiResolver(emojiLookup, flat)
	out := make([]NoteEntity, 0, len(notes))
	for _, n := range notes {
		packed := PackNote(n, idGen)
		applyNoteResolvers(n, &packed, instResolver, emojiResolver)
		out = append(out, packed)
	}
	return out
}

// PackNoteWithInstance is a single-note convenience wrapper: pack + populate.
//
// **Single-note only.** Each call spins up a fresh InstanceResolver (1 DB
// query via lookup.FindManyByHosts). For a slice of notes, call `PackNotes`
// instead — calling this in a loop produces N+1 queries.
//
// reactionReader / ctx 引数の意味は PackNotes と同じ (#647 / #657)。
func PackNoteWithInstance(ctx context.Context, n *model.Note, idGen id.Generator, instLookup InstanceLookup, emojiLookup EmojiLookup, reactionReader BufferedReactionsReader) NoteEntity {
	flat := flattenNotesPlusRelations([]*model.Note{n})
	mergeBufferedReactions(ctx, flat, reactionReader)
	packed := PackNote(n, idGen)
	instResolver := NewInstanceResolver(instLookup, CollectNoteAuthors(flat)...)
	emojiResolver := NewEmojiResolver(emojiLookup, flat)
	applyNoteResolvers(n, &packed, instResolver, emojiResolver)
	return packed
}

// mergeBufferedReactions fetches buffered deltas for the given notes via
// reader and overwrites each note.Reactions with the merged JSON. Pure
// data transform; receiver-side mutation is intentional (#647) so that
// downstream EmojiResolver / PackNote pick up the merged map automatically.
//
// reader が nil または notes が空ならば no-op。ctx は reader への伝播用
// (#657)。
func mergeBufferedReactions(ctx context.Context, notes []*model.Note, reader BufferedReactionsReader) {
	if reader == nil || len(notes) == 0 {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ids := make([]string, 0, len(notes))
	seen := make(map[string]struct{}, len(notes))
	for _, n := range notes {
		if n == nil || n.ID == "" {
			continue
		}
		if _, dup := seen[n.ID]; dup {
			continue
		}
		seen[n.ID] = struct{}{}
		ids = append(ids, n.ID)
	}
	if len(ids) == 0 {
		return
	}
	deltas, err := reader.GetBufferedMany(ctx, ids)
	if err != nil || len(deltas) == 0 {
		// reader 失敗時は stale な DB 値を返す (旧挙動と同等)。
		return
	}
	// 同一 *Note ポインタが flat に複数回現れうる (例: 2 件の note が同じ renote
	// 先を持つと、repository の batch hydration では両者の .Renote が同一ポインタを
	// 指す)。mergeReactionsJSON は加算的なので、同じオブジェクトに 2 回適用すると
	// delta が二重加算される。ポインタ単位で dedup して各オブジェクトへの適用を
	// 1 回に限定する (id 単位ではなく: preload 経由の別オブジェクト同 id はそれぞれ
	// base から 1 回ずつ merge されるべきで、正しい)。
	applied := make(map[*model.Note]struct{}, len(notes))
	for _, n := range notes {
		if n == nil {
			continue
		}
		if _, done := applied[n]; done {
			continue
		}
		applied[n] = struct{}{}
		if d, ok := deltas[n.ID]; ok && len(d) > 0 {
			n.Reactions = mergeReactionsJSON(n.Reactions, d)
		}
	}
}

// mergeReactionsJSON merges buffered deltas into the existing reactions
// JSONB and returns a new datatypes.JSON. 0 以下になったキーは結果から
// 取り除く (TS の mergeReactions と同等)。core/reaction.MergeReactions
// と同じロジックだが、layer 上 entity → core 依存を作らないために
// inline 実装している。
func mergeReactionsJSON(reactionsJSON datatypes.JSON, buffered map[string]int64) datatypes.JSON {
	if len(buffered) == 0 {
		return reactionsJSON
	}
	reactions := make(map[string]int64)
	if len(reactionsJSON) > 0 {
		var f map[string]float64
		if err := json.Unmarshal(reactionsJSON, &f); err == nil {
			for k, v := range f {
				reactions[k] = int64(v)
			}
		}
	}
	for k, delta := range buffered {
		reactions[k] += delta
		if reactions[k] <= 0 {
			delete(reactions, k)
		}
	}
	if len(reactions) == 0 {
		return datatypes.JSON([]byte("{}"))
	}
	data, err := json.Marshal(reactions)
	if err != nil {
		return reactionsJSON
	}
	return datatypes.JSON(data)
}

// applyNoteResolvers fills Instance + emojis on the packed entity and its
// embedded renote/reply children. Preload は 1 段だけなので Renote.Renote や
// Reply.Reply は常に nil で、深い再帰には成らない。
func applyNoteResolvers(n *model.Note, e *NoteEntity, instResolver *InstanceResolver, emojiResolver *EmojiResolver) {
	instResolver.FillUserLite(&e.User)
	emojiResolver.PopulateNoteEmojis(n, e)
	emojiResolver.PopulateNoteReactionEmojis(n, e)
	if n.User != nil {
		emojiResolver.PopulateUserEmojis(n.User, &e.User)
	}
	if n.Renote != nil && e.Renote != nil {
		applyNoteResolvers(n.Renote, e.Renote, instResolver, emojiResolver)
	}
	if n.Reply != nil && e.Reply != nil {
		applyNoteResolvers(n.Reply, e.Reply, instResolver, emojiResolver)
	}
}

// flattenNotesPlusRelations returns notes plus any preloaded Renote/Reply
// targets (1 level deep, since GORM only preloads the relations we ask for).
// resolver 構築時に embed 先 note の author / emoji も拾うために使う。
func flattenNotesPlusRelations(notes []*model.Note) []*model.Note {
	// 最悪ケース (全 note が Renote + Reply 両方を持つ) で *3 要素になるので
	// 容量もそれに合わせる。多くの note は片方以下なので余剰確保だが、
	// timeline fetch 30 件 × 2 の再アロケートよりは安上がり。
	flat := make([]*model.Note, 0, len(notes)*3)
	for _, n := range notes {
		if n == nil {
			continue
		}
		flat = append(flat, n)
		if n.Renote != nil {
			flat = append(flat, n.Renote)
		}
		if n.Reply != nil {
			flat = append(flat, n.Reply)
		}
	}
	return flat
}

// CollectNoteAuthors returns the author `User` pointer of each note that has
// one preloaded. Used by packers and handlers when building an
// InstanceResolver over a pre-fetched slice of notes.
//
// note.User のみを拾う。reply/renote の author も含めたい場合は呼び出し側で
// flattenNotesPlusRelations を事前適用してから渡すこと (PackNotes はそうして
// いる)。
func CollectNoteAuthors(notes []*model.Note) []*model.User {
	users := make([]*model.User, 0, len(notes))
	for _, n := range notes {
		if n == nil || n.User == nil {
			continue
		}
		users = append(users, n.User)
	}
	return users
}

// NormalizeReactionKey converts a legacy `:name:` key to the canonical
// `:name@.:` form used by the frontend. Remote and non-custom reactions
// are returned unchanged.
func NormalizeReactionKey(key string) string {
	if m := localEmojiPattern.FindStringSubmatch(key); m != nil {
		return ":" + m[1] + "@.:"
	}
	return key
}

// normalizeReactionWithLegacy applies the legacy text-alias conversion
// (like→👍 等) and then the colon-form normalization (`:name:`→`:name@.:`),
// the per-key transform shared by the reactions map and myReaction (#1816).
// upstream NoteEntityService の reactions / populateMyReaction はどちらも
// convertLegacyReaction を通すため、両者で同じ変換を使う。
func normalizeReactionWithLegacy(raw string) string {
	if u, ok := reactionlegacy.Convert(raw); ok {
		raw = u
	}
	return NormalizeReactionKey(raw)
}

// packReactions decodes the reactions JSONB once and derives both values the
// packer needs from it: the key-normalized JSON and the total reaction count.
//
// Key normalization converts legacy text aliases (like→👍) to the Unicode
// emoji and merges legacy `:name:` entries into `:name@.:`. Counts for keys that
// collapse to the same canonical reaction are summed. The returned total is
// computed from the pre-normalization values.
//
// 統合前は normalizeReactionKeys と sumReactions が同じ bytes を別々に
// デコードしていた。timeline は 1 リクエストで数十件を pack するので、
// note ごとに 1 回で済ませる。
//
// 合計を **正規化前** の各値から求めるのは、統合前の sumReactions と結果を
// 揃えるため。集約後の値から求めると、小数を含む不正なレコードで切り捨ての
// 位置が変わる ({"like":1.5,"👍":1.5} は正規化前なら 1+1=2、正規化後なら
// int(3.0)=3)。実データに小数は入らないが、統合で挙動を変えない方を採る。
//
// TS 時代のレコードと mk 時代のレコードが同一キーに集約される。upstream
// NoteEntityService.ts:373 は reactions を必ず convertLegacyReactions
// (legacies map + decodeReaction) に通す (#1816)。upstream の `count>0` filter は
// 適用しない: mk-native の write path (repository.IncrementReaction /
// count_writer) は count が 0 以下になった key を削除するため 0-count entry が
// 残らない。TS から移行直後の DB に TS が残した 0-count key が含まれる可能性は
// あるが、それは別途扱う drop-in データ移行の話で本変換の対象外。なお
// `:name:`→`:name@.:` の colon-form は mk-go の canonical (decodeReaction の逆)
// で、reactions map の永続/出力形式として既存挙動を維持する。
func packReactions(raw datatypes.JSON) (datatypes.JSON, int) {
	if len(raw) == 0 {
		// golden Note.reactions は Record (object) 必須。reactions 未設定の note
		// (create 直後の in-memory note 等、DB default '{}' を経ていないもの) は
		// nil datatypes.JSON が JSON null になり drift するため {} に coalesce する
		// (#1312。channels pinnedNoteIds #1283 と同種の null-object drift)。
		return datatypes.JSON("{}"), 0
	}
	var m map[string]float64
	if err := json.Unmarshal(raw, &m); err != nil {
		return raw, 0
	}
	total := 0
	normalized := make(map[string]float64, len(m))
	for k, v := range m {
		total += int(v)
		// legacy text alias (like→👍 等) を Unicode へ変換し colon-form 正規化を
		// 通す。同一 canonical key に集約される count は += でマージする。
		normalized[normalizeReactionWithLegacy(k)] += v
	}
	// キーが 1 つも変わらなかったときに raw をそのまま返せば marshal を丸ごと
	// 省けるが、**それはレスポンスのキー順を変える**。marshal 後は Go の順
	// (バイト列) で固定されるのに対し、raw の順は出どころで変わる。DB 由来なら
	// PostgreSQL jsonb の順 (長さ → バイト列)、reactionsBuffering 有効時は
	// mergeBufferedReactions が Go で marshal し直したものが入る。
	//
	// キー順はフロントエンドから見える。MkReactionsViewer.vue は count 降順に
	// ソートするので効くのは同着のときだが、**並び順だけの話ではない**。
	// タイムラインは MkNote.vue が maxNumber=16 を渡し、viewer は sort の
	// **後** に index で filter するので、同着が 16 件目の境界に掛かると
	// キー順が「そのリアクションを表示するかどうか」を決める。
	// MkNoteDetailed.vue のリアクションタブは `Object.keys(...)` を素通しする
	// のでキー順がそのまま出る。省リソース化の範囲で黙って入れてよい変更では
	// ないので、従来どおり必ず marshal し直す。
	data, err := json.Marshal(normalized)
	if err != nil {
		return raw, total
	}
	return data, total
}

// NormalizeReactionWithLegacy normalizes a raw reaction string (colon-form +
// legacy text alias), e.g. `:smile:`→`:smile@.:`, `like`→`👍`。streaming channel の
// renote.myReaction inject が REST 経路と同じ正規化を適用するための export wrapper
// (#2058)。reactionAndUserPairCache は raw reaction を保持するため必須。
func NormalizeReactionWithLegacy(raw string) string {
	return normalizeReactionWithLegacy(raw)
}
