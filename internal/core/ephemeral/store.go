// Package ephemeral keeps relay-delivered notes and their authors in Redis
// instead of the database.
//
// リレー経由でしか観測しない投稿は、ローカルの誰とも関係が無いまま流れていく
// だけなので DB に入れる価値が無い。それでも保存すると、リレーの流量に比例して
// INSERT と DELETE が両方発生し、WAL / インデックス更新 / デッドタプル /
// autovacuum の負荷が二重に掛かる。これがリレー参加の障壁になっている (#2332)。
//
// そこで TTL 付きで Redis に置き、誰も触らなければ期限切れで消えるようにする。
// ローカルユーザーの操作や連合からの参照で **DB 行への外部キー参照が必要に
// なったとき** にだけ materialize する。
//
// 保存するのは packed JSON ではなく `model.Note` / `model.User` そのもの。
// 下流 (ApplyFilter / PackNotes / PublishNote) はいずれも `*model.Note` を
// 受け取るので、読み戻して渡せばフィルタ・パック・ストリーミングが無改造で通る。
package ephemeral

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/elythia-network/elythia/internal/model"
)

// Key prefixes. keyPrefix (`<host>:`) が更に前に付く。
const (
	notePrefix    = "ephNote:"
	userPrefix    = "ephUser:"
	noteURIPrefix = "ephNoteURI:"
	userURIPrefix = "ephUserURI:"
	// filePrefix marks a drive_file row as referenced by a live ephemeral note.
	//
	// **添付の掃除 (#2722) はこの印だけを見る。** owner 無しのリモート添付は
	// note が Redis にしか無いので、DB を見ても「参照されていない」ようにしか
	// 見えない。行の作成時刻から猶予を測る方式では、dedup (upsertAttachments の
	// FindByURI) が古い行を新しい ephemeral note に結び直した瞬間に破綻する
	// (その晩の掃除で表示中の添付が消える)。
	filePrefix = "ephFile:"
)

// userTTLFactor extends the author TTL relative to the note TTL.
//
// 著者は複数のノートから共有される。ノートと同じ TTL にすると、連投の途中で
// 著者だけ先に消えて後続ノートが著者不明になる。ノートより長く持たせる。
const userTTLFactor = 4

// Settings carries the operator-controlled knobs for the ephemeral store.
type Settings struct {
	// Enabled gates *ingestion*. 無効でも読み出しは続ける: 既に置かれている
	// ぶんは TTL 切れまで表示できた方が、切り替えた瞬間に timeline から
	// 消えるより自然なため。
	Enabled bool
	// TTL は Redis 上の保持時間。0 以下なら defaultTTL。
	TTL time.Duration
}

// NoteLookup resolves notes that live in the database.
//
// ephemeral ノートの引用先は Redis と DB のどちらにもありうる。リレー由来の
// 投稿を引用していれば Redis、既に取り込み済みの投稿を引用していれば DB。
// 埋められなかった参照は frontend が「削除された投稿」として描画するので
// (entity/note.go の Renote preload の項)、両方を引く必要がある (#2397)。
type NoteLookup interface {
	FindManyByIDsWithUser(ids []string) ([]*model.Note, error)
}

// Store persists ephemeral notes and authors in Redis.
type Store struct {
	client    *redis.Client
	keyPrefix string
	settings  func() Settings
	// notes は引用先を DB へ引きに行くための optional な参照。未配線なら
	// Redis 側にある引用先だけが埋まる。
	notes NoteLookup
}

// SetNoteLookup wires the database fallback used to resolve quote targets that
// are not (or no longer) in Redis.
func (s *Store) SetNoteLookup(l NoteLookup) {
	if s == nil {
		return
	}
	s.notes = l
}

// NewStore constructs a Store bound to the given Redis database.
//
// settings は meta 由来の設定を都度読むための関数。起動時に固定すると管理画面
// での変更が再起動まで効かない。keyPrefix は FanoutTimelineService と同じ
// `<host>:`。
func NewStore(client *redis.Client, keyPrefix string, settings func() Settings) *Store {
	return &Store{client: client, keyPrefix: keyPrefix, settings: settings}
}

// Enabled reports whether relay-delivered notes should be diverted to Redis
// instead of the database.
//
// これが false のときは取り込み経路が通常の DB 保存に倒れる。既定無効の約束を
// ここで担保する (sink が配線されているだけで機能が入ってしまわないように)。
func (s *Store) Enabled() bool {
	if s == nil || s.client == nil || s.settings == nil {
		return false
	}
	return s.settings().Enabled
}

// defaultTTL is used when the configured TTL is unset or non-positive.
const defaultTTL = time.Hour

func (s *Store) noteTTL() time.Duration {
	if s == nil || s.settings == nil {
		return defaultTTL
	}
	if d := s.settings().TTL; d > 0 {
		return d
	}
	return defaultTTL
}

func (s *Store) key(prefix, id string) string { return s.keyPrefix + prefix + id }

// PutNote stores a note together with its author.
//
// note.User は別キーに分けて保存する (同じ著者の連投で本文が重複しないように)。
// 呼び出し側が組み立てた構造体は変更しない。
func (s *Store) PutNote(ctx context.Context, n *model.Note, author *model.User) error {
	if s == nil || s.client == nil || n == nil || author == nil {
		return nil
	}
	if n.ID == "" || n.URI == nil || *n.URI == "" {
		return fmt.Errorf("ephemeral: note requires id and uri")
	}
	ttl := s.noteTTL()

	// リレーションを外した複製を保存する。User は ephUser 側が持つので、
	// ここに埋めると同じ著者ぶん JSON が重複する。
	stored := *n
	stored.User = nil
	stored.Reply = nil
	stored.Renote = nil
	stored.Poll = nil

	noteJSON, err := json.Marshal(&stored)
	if err != nil {
		return fmt.Errorf("ephemeral: marshal note: %w", err)
	}
	userJSON, err := json.Marshal(author)
	if err != nil {
		return fmt.Errorf("ephemeral: marshal user: %w", err)
	}

	pipe := s.client.Pipeline()
	pipe.Set(ctx, s.key(notePrefix, n.ID), noteJSON, ttl)
	pipe.Set(ctx, s.key(noteURIPrefix, *n.URI), n.ID, ttl)
	pipe.Set(ctx, s.key(userPrefix, author.ID), userJSON, ttl*userTTLFactor)
	if author.URI != nil && *author.URI != "" {
		pipe.Set(ctx, s.key(userURIPrefix, *author.URI), author.ID, ttl*userTTLFactor)
	}
	// 添付の印は note と同じ TTL。DropNote で消さない — 同じ添付を別の
	// ephemeral note が参照していることがあるので、TTL に任せる。
	//
	// **印を作れるのはここだけ** (`Touch` は `Expire` なので既存の印しか
	// 延ばせない)。したがって機能を有効にした直後は、既に Redis に居る
	// ephemeral note に印が無い状態が 1 TTL ぶん続く。
	for _, fileID := range n.FileIDs {
		if fileID == "" {
			continue
		}
		pipe.Set(ctx, s.key(filePrefix, fileID), "1", ttl)
	}
	_, err = pipe.Exec(ctx)
	return err
}

// GetNote returns the ephemeral note for id with its author attached, or nil
// when the entry is absent or expired.
func (s *Store) GetNote(ctx context.Context, id string) (*model.Note, error) {
	notes, err := s.GetNotes(ctx, []string{id})
	if err != nil || len(notes) == 0 {
		return nil, err
	}
	return notes[0], nil
}

// GetNotes returns the ephemeral notes for ids, skipping any that are absent.
// 戻り値の順序は ids の順序を保つ (欠損は詰める)。
func (s *Store) GetNotes(ctx context.Context, ids []string) ([]*model.Note, error) {
	out, err := s.getNotesShallow(ctx, ids)
	if err != nil || len(out) == 0 {
		return out, err
	}
	// 引用先を埋める。PutNote が関連を落として保存するので、ここで埋め直さないと
	// renoteId だけが残り frontend が「削除された投稿」を描く (#2397)。
	s.attachQuoteTargets(ctx, out)
	return out, nil
}

// getNotesShallow reads notes and attaches their authors, without resolving
// quote targets. attachQuoteTargets から呼ぶため分けてある (GetNotes を再帰
// させると引用チェーンぶん無限に降りる)。
func (s *Store) getNotesShallow(ctx context.Context, ids []string) ([]*model.Note, error) {
	if s == nil || s.client == nil || len(ids) == 0 {
		return nil, nil
	}
	pipe := s.client.Pipeline()
	cmds := make([]*redis.StringCmd, 0, len(ids))
	for _, id := range ids {
		cmds = append(cmds, pipe.Get(ctx, s.key(notePrefix, id)))
	}
	// redis.Nil は「そのキーが無い」だけなので握り潰す。それ以外の error は
	// 呼び出し側 (timeline hydrate) が DB fallback を選べるように返す。
	if _, err := pipe.Exec(ctx); err != nil && err != redis.Nil {
		return nil, err
	}

	out := make([]*model.Note, 0, len(ids))
	authors := map[string]*model.User{}
	for _, cmd := range cmds {
		raw, err := cmd.Bytes()
		if err != nil {
			// **届かない防御。** 上の `pipe.Exec` が redis.Nil 以外の error を
			// そのまま返して early return するので、ここへ来る error は実質
			// redis.Nil だけ (WRONGTYPE を混ぜて実測。Exec が返して到達しない)。
			//
			// それでも分岐を残すのは、呼び出し側 (timeline) が**解決できない ID を
			// list から取り除く**ようになったため。一時障害を「消えた」と扱うと
			// 生きている note の ID を消してしまう。Exec の扱いが変わったときに
			// 破壊側へ倒れないよう、ここでも「無い」を redis.Nil に限る
			// (#2718 review LOW-3)。
			if err == redis.Nil {
				continue
			}
			return nil, err
		}
		var n model.Note
		if err := json.Unmarshal(raw, &n); err != nil {
			// payload が壊れている。materialize 経路でも使えないので「無い」
			// 扱いでよいが、消える前に気付けるようログには残す。
			slog.WarnContext(ctx, "ephemeral: dropping unreadable note payload", "err", err)
			continue
		}
		authors[n.UserID] = nil
		out = append(out, &n)
	}
	if len(out) == 0 {
		return nil, nil
	}

	// 著者をまとめて引いて詰め直す。PackNotes / ApplyFilter は note.User を見る。
	if err := s.attachAuthors(ctx, out, authors); err != nil {
		return nil, err
	}
	return out, nil
}

// attachQuoteTargets fills note.Renote for notes that carry a RenoteID.
//
// Redis を先に引き、無いぶんだけ DB へ落とす。best-effort で、引けなかった参照は
// nil のまま残す (= 本当に消えている引用先は従来どおり削除ノート表示になる)。
//
// 埋めるのは 1 段だけ。ネストした引用 (renote.renote) は DB 経路では
// FindManyByIDsWithUser が 2 段目まで埋めるが、ephemeral 側で同じ深さを追うと
// Redis 往復が段数ぶん増える。報告された症状 (引用先が削除ノート) は 1 段で解ける。
//
// **返信先 (ReplyID) は埋めない。** リレー由来の投稿は未知 URI の返信先を fetch
// しない設計 (resolver.go の InReplyTo 解決) なので ReplyID がそもそも付かず、
// 宙ぶらりんの参照が生まれない。埋める対象が無い。
func (s *Store) attachQuoteTargets(ctx context.Context, notes []*model.Note) {
	want := make([]string, 0, len(notes))
	seen := make(map[string]struct{}, len(notes))
	for _, n := range notes {
		if n.RenoteID == nil || *n.RenoteID == "" || n.Renote != nil {
			continue
		}
		if _, dup := seen[*n.RenoteID]; dup {
			continue
		}
		seen[*n.RenoteID] = struct{}{}
		want = append(want, *n.RenoteID)
	}
	if len(want) == 0 {
		return
	}

	resolved := make(map[string]*model.Note, len(want))
	// 1. Redis 側。引用先もリレー由来ならこちらにある。
	//    GetNotes を再帰させると attachQuoteTargets が無限に降りるので、
	//    生読み + 著者付けだけを行う内部関数を使う。
	if eph, err := s.getNotesShallow(ctx, want); err == nil {
		for _, n := range eph {
			resolved[n.ID] = n
		}
	}
	// 2. Redis に無かったぶんを DB から。
	if s.notes != nil {
		missing := make([]string, 0, len(want))
		for _, id := range want {
			if _, ok := resolved[id]; !ok {
				missing = append(missing, id)
			}
		}
		if len(missing) > 0 {
			if rows, err := s.notes.FindManyByIDsWithUser(missing); err == nil {
				for _, n := range rows {
					resolved[n.ID] = n
				}
			}
		}
	}

	for _, n := range notes {
		if n.RenoteID == nil || n.Renote != nil {
			continue
		}
		n.Renote = resolved[*n.RenoteID]
	}
}

// attachAuthors fills note.User for every note in notes.
//
// 著者を引けなかったノートは落とさずそのまま返す。PackNotes は User == nil でも
// 落ちない設計なので、著者の TTL 切れで timeline 全体が欠けるより良い。
func (s *Store) attachAuthors(ctx context.Context, notes []*model.Note, want map[string]*model.User) error {
	ids := make([]string, 0, len(want))
	for id := range want {
		ids = append(ids, id)
	}
	pipe := s.client.Pipeline()
	cmds := make(map[string]*redis.StringCmd, len(ids))
	for _, id := range ids {
		cmds[id] = pipe.Get(ctx, s.key(userPrefix, id))
	}
	if _, err := pipe.Exec(ctx); err != nil && err != redis.Nil {
		return err
	}
	for id, cmd := range cmds {
		raw, err := cmd.Bytes()
		if err != nil {
			continue
		}
		var u model.User
		if err := json.Unmarshal(raw, &u); err != nil {
			continue
		}
		want[id] = &u
	}
	for _, n := range notes {
		n.User = want[n.UserID]
	}
	return nil
}

// NoteIDByURI returns the ephemeral note ID assigned to uri, or "" when absent.
func (s *Store) NoteIDByURI(ctx context.Context, uri string) (string, error) {
	return s.lookupID(ctx, noteURIPrefix, uri)
}

// UserIDByURI returns the ephemeral user ID assigned to an actor URI, or ""
// when absent.
//
// 同じ著者の 2 件目以降の投稿で同じ ID を再利用するために引く。これが無いと
// 投稿ごとに別 ID を採番してしまい、同一人物が別人として並ぶ。
func (s *Store) UserIDByURI(ctx context.Context, uri string) (string, error) {
	return s.lookupID(ctx, userURIPrefix, uri)
}

func (s *Store) lookupID(ctx context.Context, prefix, uri string) (string, error) {
	if s == nil || s.client == nil || uri == "" {
		return "", nil
	}
	id, err := s.client.Get(ctx, s.key(prefix, uri)).Result()
	if err == redis.Nil {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return id, nil
}

// GetUser returns the ephemeral author for id, or nil when absent.
func (s *Store) GetUser(ctx context.Context, id string) (*model.User, error) {
	if s == nil || s.client == nil || id == "" {
		return nil, nil
	}
	raw, err := s.client.Get(ctx, s.key(userPrefix, id)).Bytes()
	if err == redis.Nil {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var u model.User
	if err := json.Unmarshal(raw, &u); err != nil {
		return nil, err
	}
	return &u, nil
}

// DropNote removes an ephemeral note and its URI mapping.
//
// materialize したときと、同じ URI が直接配送で DB に入ったときに呼ぶ。著者は
// 他のノートが参照しているかもしれないので消さない (TTL に任せる)。
func (s *Store) DropNote(ctx context.Context, id, uri string) error {
	if s == nil || s.client == nil || id == "" {
		return nil
	}
	keys := []string{s.key(notePrefix, id)}
	if uri != "" {
		keys = append(keys, s.key(noteURIPrefix, uri))
	}
	return s.client.Del(ctx, keys...).Err()
}

// Touch extends the TTL of a note (and its author) so that entries being
// actively viewed do not expire out from under the reader.
//
// 閲覧では materialize しない方針 (#2332) なので、詳細を開いた直後に TTL 切れ
// でリアクションできなくなる穴が空く。読み取り時に打ち直して塞ぐ。
func (s *Store) Touch(ctx context.Context, n *model.Note) error {
	if s == nil || s.client == nil || n == nil || n.ID == "" {
		return nil
	}
	ttl := s.noteTTL()
	pipe := s.client.Pipeline()
	pipe.Expire(ctx, s.key(notePrefix, n.ID), ttl)
	if n.URI != nil && *n.URI != "" {
		pipe.Expire(ctx, s.key(noteURIPrefix, *n.URI), ttl)
	}
	if n.UserID != "" {
		pipe.Expire(ctx, s.key(userPrefix, n.UserID), ttl*userTTLFactor)
	}
	// **添付の印も一緒に打ち直す。** Touch は TTL をスライドさせるので、
	// note だけ延ばして印を放置すると、`/api/notes/show` で開かれ続けている
	// ノートの添付が掃除の対象になる。`Set` ではなく `Expire` なので、印が既に失効して
	// いれば何も起きない (掃除済みの行を復活させない)。
	for _, fileID := range n.FileIDs {
		if fileID == "" {
			continue
		}
		pipe.Expire(ctx, s.key(filePrefix, fileID), ttl)
	}
	_, err := pipe.Exec(ctx)
	return err
}

// LiveFileIDs returns the subset of ids that a live ephemeral note still
// references.
//
// 掃除 (#2722) はこれを見てから消す。**引けなかったときは error を返すだけで、
// 「印が無い」と答えない。** 呼び出し側は error を受けたら 1 件も消さずに中断
// する責務を持つ (処理を続けると、Redis が一時的に落ちている間の実行で表示中の
// 添付を全部消すことになる)。
func (s *Store) LiveFileIDs(ctx context.Context, ids []string) (map[string]bool, error) {
	live := make(map[string]bool, len(ids))
	if len(ids) == 0 {
		return live, nil
	}
	if s == nil || s.client == nil {
		// **未配線は「印が無い」ではなく error。** 空の map を返すと呼び出し側
		// (掃除) は候補を全部ゴミと見なす。配線が外れた瞬間に表示中の添付を
		// 全消しするより、掃除が止まるほうが安全側。
		return nil, fmt.Errorf("ephemeral: store is not wired")
	}
	keys := make([]string, 0, len(ids))
	for _, id := range ids {
		keys = append(keys, s.key(filePrefix, id))
	}
	vals, err := s.client.MGet(ctx, keys...).Result()
	if err != nil {
		return nil, fmt.Errorf("ephemeral: mget file marks: %w", err)
	}
	for i, v := range vals {
		if v != nil && i < len(ids) {
			live[ids[i]] = true
		}
	}
	return live, nil
}
