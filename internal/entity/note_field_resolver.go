package entity

import (
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
)

// DriveFileLookup batch-fetches drive files by ID. Implemented by
// repository.DriveFileRepository. nil 受信時は resolver は no-op になる。
type DriveFileLookup interface {
	FindByIDs(ids []string) ([]*model.DriveFile, error)
}

// DriveFolderLookup fetches a single folder by ID. Used for embedding folder
// metadata in DriveFile entities (#317)。
type DriveFolderLookup interface {
	FindByID(id string) (*model.DriveFolder, error)
}

// FileOwnerLookup fetches a single user by ID for embedding owner metadata
// in DriveFile entities. UserLookup などと衝突しない命名にしている。
type FileOwnerLookup interface {
	FindByID(id string) (*model.User, error)
}

// NoteReactionLookup batch-fetches a viewer's reactions across multiple notes
// to populate NoteEntity.MyReaction.
type NoteReactionLookup interface {
	FindByUserAndNoteIDs(userID string, noteIDs []string) (map[string]*model.NoteReaction, error)
}

// ChannelLookup batch-fetches channels by ID for NoteEntity.Channel embed.
type ChannelLookup interface {
	FindByIDs(ids []string) ([]*model.Channel, error)
}

// PollVoteLookup batch-fetches the viewer's votes across multiple notes so
// the resolver can mark Poll.choices[i].IsVoted=true for the choices the
// viewer has cast a vote on (#690). Returns noteID → []choice index.
type PollVoteLookup interface {
	FindByUserAndNoteIDs(userID string, noteIDs []string) (map[string][]int, error)
}

// NoteFieldResolver applies post-pack file / viewer-dependent / channel
// resolution to packed NoteEntity slices including their embedded Renote /
// Reply targets (#426)。
//
// /api/notes/* 以外の handler (antennas / channels / users / clips / roles /
// drive / i pinned 等) でも PackNotes を経由するため、共通化して quote renote
// の files が空配列 / myReaction が欠落する問題を一括で解消する。
//
// nil レシーバはすべて no-op として扱われ、構築側 (router) の wiring が
// 不完全でも panic しない。
type NoteFieldResolver struct {
	driveFile    DriveFileLookup
	driveFolder  DriveFolderLookup
	fileOwner    FileOwnerLookup
	noteReaction NoteReactionLookup
	channel      ChannelLookup
	pollVote     PollVoteLookup
	idGen        id.Generator
}

// NewNoteFieldResolver constructs a resolver. 個々の lookup が nil でも
// 適切に skip される (例えば driveFile が nil なら ResolveFiles は no-op、
// channel が nil なら ResolveViewerFields の channel 解決のみ skip)。
func NewNoteFieldResolver(driveFile DriveFileLookup, driveFolder DriveFolderLookup, fileOwner FileOwnerLookup, noteReaction NoteReactionLookup, channel ChannelLookup, idGen id.Generator) *NoteFieldResolver {
	return &NoteFieldResolver{
		driveFile:    driveFile,
		driveFolder:  driveFolder,
		fileOwner:    fileOwner,
		noteReaction: noteReaction,
		channel:      channel,
		idGen:        idGen,
	}
}

// SetPollVoteLookup attaches a PollVoteLookup so ResolveViewerFields populates
// Poll.choices[i].IsVoted for the viewer (#690). Setter rather than
// constructor arg to keep New… signature stable for existing call sites.
func (r *NoteFieldResolver) SetPollVoteLookup(p PollVoteLookup) {
	if r == nil {
		return
	}
	r.pollVote = p
}

// Apply runs ResolveFiles + ResolveViewerFields in order. caller の典型 1 行
// 適用パスを縮める用途。
func (r *NoteFieldResolver) Apply(notes []NoteEntity, viewer *model.User) {
	if r == nil {
		return
	}
	r.ResolveFiles(notes)
	r.ResolveViewerFields(notes, viewer)
}

// ResolveFiles collects file IDs from the slice (including embedded Renote /
// Reply targets), batch-fetches DriveFile rows, then populates each entity's
// `Files` field with packed entities (folder / owner embed best-effort)。
func (r *NoteFieldResolver) ResolveFiles(notes []NoteEntity) {
	if r == nil || r.driveFile == nil {
		return
	}
	fileMap, ok := r.fetchNoteFiles(notes)
	if !ok {
		return
	}
	files := make([]*model.DriveFile, 0, len(fileMap))
	for _, f := range fileMap {
		files = append(files, f)
	}
	folderCache, userCache := r.resolveFileOwners(files)
	for i := range notes {
		r.populateNoteFiles(&notes[i], fileMap, folderCache, userCache)
	}
}

// ResolveFilesShallow is ResolveFiles without the folder / owner embeds: it
// packs each file with PackDriveFile, leaving `folder` / `user` as null.
//
// **Web Push 用** (#2737)。upstream の push payload は
// `driveFileEntityService.packManyByIds` (オプション無し) を通り、folder / user を
// embed しない。**その 1 点で揃える**もので、shape 全体が upstream と同じに
// なるわけではない — upstream は `withUser` falsy のとき `userId` も null に
// するし、`webpublicUrl` は mk-go 拡張 (entity/drive.go の doc 参照)。
//
// 加えて payload が小さい。実測 (現実的な長さの URL / blurhash / 代替テキストを
// 持つ画像) で 16 ファイルぶんの files 配列が 13.1 KB / 18.1 KB
// (shallow / relations 込み)。note 全体はどちらも同額の overhead が乗るだけ
// (この fixture では +569 B) なので、差は files 配列の差そのもの。Web Push の
// 実質上限 (約 4 KB) に対してはどちらも超えるが、超えるまでの余裕が違う。
// folder / owner の追加クエリ 2 本も省ける。
func (r *NoteFieldResolver) ResolveFilesShallow(notes []NoteEntity) {
	if r == nil || r.driveFile == nil {
		return
	}
	fileMap, ok := r.fetchNoteFiles(notes)
	if !ok {
		return
	}
	for i := range notes {
		r.populateNoteFilesShallow(&notes[i], fileMap)
	}
}

// fetchNoteFiles collects the file IDs of the slice (including embeds) and
// batch-fetches the rows. ok=false means there is nothing to populate.
func (r *NoteFieldResolver) fetchNoteFiles(notes []NoteEntity) (map[string]*model.DriveFile, bool) {
	var allIDs []string
	for i := range notes {
		allIDs = appendNoteFileIDs(allIDs, &notes[i])
	}
	if len(allIDs) == 0 {
		return nil, false
	}
	files, err := r.driveFile.FindByIDs(allIDs)
	if err != nil || len(files) == 0 {
		return nil, false
	}
	fileMap := make(map[string]*model.DriveFile, len(files))
	for _, f := range files {
		fileMap[f.ID] = f
	}
	return fileMap, true
}

// populateNoteFilesShallow mirrors populateNoteFiles without folder / owner.
func (r *NoteFieldResolver) populateNoteFilesShallow(n *NoteEntity, fileMap map[string]*model.DriveFile) {
	if n == nil {
		return
	}
	packed := make([]any, 0, len(n.FileIDs))
	for _, fid := range n.FileIDs {
		f, ok := fileMap[fid]
		if !ok {
			continue
		}
		packed = append(packed, PackDriveFile(f, r.idGen))
	}
	n.Files = packed
	r.populateNoteFilesShallow(n.Renote, fileMap)
	r.populateNoteFilesShallow(n.Reply, fileMap)
}

// ResolveViewerFields fills MyReaction (when viewer != nil) and Channel on
// each note plus its Renote / Reply embed using batch lookups.
func (r *NoteFieldResolver) ResolveViewerFields(notes []NoteEntity, viewer *model.User) {
	if r == nil || len(notes) == 0 {
		return
	}
	if viewer != nil && r.noteReaction != nil {
		var noteIDs []string
		for i := range notes {
			noteIDs = appendNoteIDs(noteIDs, &notes[i])
		}
		if len(noteIDs) > 0 {
			reactionMap, err := r.noteReaction.FindByUserAndNoteIDs(viewer.ID, noteIDs)
			if err == nil {
				for i := range notes {
					applyMyReaction(&notes[i], reactionMap)
				}
			}
		}
	}
	// Poll.choices[i].IsVoted を viewer の poll_vote から populate (#690)。
	// frontend MkPoll の `showResult = isVoted || closed` 判定が reload 後も
	// true 維持されるため、結果表示 toggle が永続化される。
	if viewer != nil && r.pollVote != nil {
		var pollNoteIDs []string
		for i := range notes {
			pollNoteIDs = appendPollNoteIDs(pollNoteIDs, &notes[i])
		}
		if len(pollNoteIDs) > 0 {
			voteMap, err := r.pollVote.FindByUserAndNoteIDs(viewer.ID, pollNoteIDs)
			if err == nil {
				for i := range notes {
					applyMyPollVotes(&notes[i], voteMap)
				}
			}
		}
	}
	if r.channel != nil {
		var channelIDs []string
		for i := range notes {
			channelIDs = appendNoteChannelIDs(channelIDs, &notes[i])
		}
		if len(channelIDs) > 0 {
			channels, err := r.channel.FindByIDs(channelIDs)
			if err == nil {
				chMap := make(map[string]*model.Channel, len(channels))
				for _, ch := range channels {
					chMap[ch.ID] = ch
				}
				for i := range notes {
					applyChannel(&notes[i], chMap)
				}
			}
		}
	}
}

func (r *NoteFieldResolver) populateNoteFiles(n *NoteEntity, fileMap map[string]*model.DriveFile, folderCache map[string]*model.DriveFolder, userCache map[string]*model.User) {
	if n == nil {
		return
	}
	n.Files = r.packNoteFiles(n.FileIDs, fileMap, folderCache, userCache)
	r.populateNoteFiles(n.Renote, fileMap, folderCache, userCache)
	r.populateNoteFiles(n.Reply, fileMap, folderCache, userCache)
}

func (r *NoteFieldResolver) packNoteFiles(fileIDs []string, fileMap map[string]*model.DriveFile, folderCache map[string]*model.DriveFolder, userCache map[string]*model.User) []any {
	packed := make([]any, 0, len(fileIDs))
	for _, fid := range fileIDs {
		f, ok := fileMap[fid]
		if !ok {
			continue
		}
		var folder *model.DriveFolder
		if f.FolderID != nil {
			folder = folderCache[*f.FolderID]
		}
		var user *model.User
		if f.UserID != nil {
			user = userCache[*f.UserID]
		}
		packed = append(packed, PackDriveFileWithRelations(f, r.idGen, folder, user))
	}
	return packed
}

// batchDriveFolderLookup / batchFileOwnerLookup are optional capabilities. When
// the wired lookup implements them, resolveFileOwners fetches all distinct
// folder / owner IDs in a single query each instead of one FindByID per distinct
// ID. これは media が多い timeline での distinct(folder)+distinct(owner) 個別
// round-trip を畳む (#1389)。未実装なら従来の per-ID fetch に fall back する。
type batchDriveFolderLookup interface {
	FindByIDs(ids []string) ([]*model.DriveFolder, error)
}

type batchFileOwnerLookup interface {
	FindManyByIDs(ids []string) ([]*model.User, error)
}

func (r *NoteFieldResolver) resolveFileOwners(files []*model.DriveFile) (map[string]*model.DriveFolder, map[string]*model.User) {
	folderCache := map[string]*model.DriveFolder{}
	userCache := map[string]*model.User{}

	// distinct な folderID / userID を収集する。cache に nil を seed して「収集
	// 済み」を兼ねる (fill 側で実体に上書き、見つからなければ nil のまま = 従来の
	// 「FindByID 失敗時 nil」と同じ挙動)。
	var folderIDs, userIDs []string
	for _, f := range files {
		if r.driveFolder != nil && f.FolderID != nil {
			if _, seen := folderCache[*f.FolderID]; !seen {
				folderCache[*f.FolderID] = nil
				folderIDs = append(folderIDs, *f.FolderID)
			}
		}
		if r.fileOwner != nil && f.UserID != nil {
			if _, seen := userCache[*f.UserID]; !seen {
				userCache[*f.UserID] = nil
				userIDs = append(userIDs, *f.UserID)
			}
		}
	}

	r.fillFolderCache(folderIDs, folderCache)
	r.fillUserCache(userIDs, userCache)
	return folderCache, userCache
}

// fillFolderCache populates cache for the given distinct folder IDs, batching
// via FindByIDs when the lookup supports it, else per-ID FindByID.
func (r *NoteFieldResolver) fillFolderCache(ids []string, cache map[string]*model.DriveFolder) {
	if r.driveFolder == nil || len(ids) == 0 {
		return
	}
	if b, ok := r.driveFolder.(batchDriveFolderLookup); ok {
		if rows, err := b.FindByIDs(ids); err == nil {
			for _, f := range rows {
				cache[f.ID] = f
			}
		}
		return
	}
	for _, id := range ids {
		if f, err := r.driveFolder.FindByID(id); err == nil {
			cache[id] = f
		}
	}
}

// fillUserCache populates cache for the given distinct owner IDs, batching via
// FindManyByIDs when the lookup supports it, else per-ID FindByID.
func (r *NoteFieldResolver) fillUserCache(ids []string, cache map[string]*model.User) {
	if r.fileOwner == nil || len(ids) == 0 {
		return
	}
	if b, ok := r.fileOwner.(batchFileOwnerLookup); ok {
		if rows, err := b.FindManyByIDs(ids); err == nil {
			for _, u := range rows {
				cache[u.ID] = u
			}
		}
		return
	}
	for _, id := range ids {
		if u, err := r.fileOwner.FindByID(id); err == nil {
			cache[id] = u
		}
	}
}

// appendNoteFileIDs / appendNoteIDs / appendNoteChannelIDs / applyMyReaction /
// applyChannel は NoteEntity と embed 1 階層を統一的に走査する補助関数。
// 元は internal/api/notes/handler.go に閉じていたが、本 resolver の他
// handler への展開 (#426) に伴って entity に再配置している。

// embed の深さは packer 側 (maxNoteEmbedDepth) が抑えるので、走査は素直に
// 再帰する。depth-2 の renote.renote / renote.reply も files を持つため、
// 1 階層で止めると添付が空のまま返る。
func appendNoteFileIDs(dst []string, n *NoteEntity) []string {
	if n == nil {
		return dst
	}
	dst = append(dst, n.FileIDs...)
	dst = appendNoteFileIDs(dst, n.Renote)
	return appendNoteFileIDs(dst, n.Reply)
}

// appendNoteIDs collects note IDs whose viewer myReaction should be batch
// fetched. reactionCount==0 の note は誰も reaction していない = viewer の
// reaction もあり得ないので fetch 対象から外す (upstream populateMyReaction の
// `if (reactionsCount === 0) return undefined` 相当、#1641)。これにより
// reaction の無い note が多い timeline では IN リストが縮み myReaction の
// batch SELECT が軽くなる。
//
// upstream の「作成 2 秒以内は fetch skip」guard と reactionAndUserPairCache
// 優先参照は mk-go では採用しない (#1641 の意図的 divergence):
//   - 2 秒 guard は per-note fetch を避ける TS の最適化だが、mk-go は元から
//     ページ単位の単一 batch SELECT なので不要。むしろ guard を入れると直前に
//     自分が reaction した直後 (<2s) の note で myReaction が undefined になり
//     機能的に劣化する。mk-go は実値を返す方を優先する。
//   - reactionAndUserPairCache 優先は REST note が pairCache を持たない
//     (#1640 で streaming 限定) ため REST 経路では効かない。
func appendNoteIDs(dst []string, n *NoteEntity) []string {
	if n == nil {
		return dst
	}
	if n.ReactionCount > 0 {
		dst = append(dst, n.ID)
	}
	if n.Renote != nil && n.Renote.ReactionCount > 0 {
		dst = append(dst, n.Renote.ID)
	}
	// reply embed は detail:false で pack され myReaction を出さない (#1816) ので
	// fetch 対象に含めない (applyMyReaction も reply には適用しない)。
	return dst
}

// appendPollNoteIDs collects note IDs that have an attached poll (top-level
// + Renote / Reply embed) so the viewer's votes can be batch-fetched.
func appendPollNoteIDs(dst []string, n *NoteEntity) []string {
	if n == nil {
		return dst
	}
	if n.Poll != nil {
		dst = append(dst, n.ID)
	}
	if n.Renote != nil && n.Renote.Poll != nil {
		dst = append(dst, n.Renote.ID)
	}
	if n.Reply != nil && n.Reply.Poll != nil {
		dst = append(dst, n.Reply.ID)
	}
	return dst
}

// applyMyPollVotes marks Poll.choices[i].IsVoted=true for the viewer's
// recorded choices (#690). Operates on top-level + Renote / Reply embed.
func applyMyPollVotes(n *NoteEntity, voteMap map[string][]int) {
	if n == nil {
		return
	}
	if n.Poll != nil {
		applyVotedChoices(n.Poll, voteMap[n.ID])
	}
	if n.Renote != nil && n.Renote.Poll != nil {
		applyVotedChoices(n.Renote.Poll, voteMap[n.Renote.ID])
	}
	if n.Reply != nil && n.Reply.Poll != nil {
		applyVotedChoices(n.Reply.Poll, voteMap[n.Reply.ID])
	}
}

func applyVotedChoices(p *PollEntity, choices []int) {
	if p == nil || len(choices) == 0 {
		return
	}
	for _, idx := range choices {
		if idx >= 0 && idx < len(p.Choices) {
			p.Choices[idx].IsVoted = true
		}
	}
}

func appendNoteChannelIDs(dst []string, n *NoteEntity) []string {
	if n == nil {
		return dst
	}
	if n.ChannelID != nil && *n.ChannelID != "" {
		dst = append(dst, *n.ChannelID)
	}
	if n.Renote != nil && n.Renote.ChannelID != nil && *n.Renote.ChannelID != "" {
		dst = append(dst, *n.Renote.ChannelID)
	}
	if n.Reply != nil && n.Reply.ChannelID != nil && *n.Reply.ChannelID != "" {
		dst = append(dst, *n.Reply.ChannelID)
	}
	return dst
}

func applyMyReaction(n *NoteEntity, reactionMap map[string]*model.NoteReaction) {
	if n == nil {
		return
	}
	// myReaction も upstream populateMyReaction と同じく legacy text alias を
	// Unicode に変換してから colon-form 正規化する (reactions map と一貫、#1816)。
	if r, ok := reactionMap[n.ID]; ok {
		nr := normalizeReactionWithLegacy(r.Reaction)
		n.MyReaction = &nr
	}
	if n.Renote != nil {
		if r, ok := reactionMap[n.Renote.ID]; ok {
			nr := normalizeReactionWithLegacy(r.Reaction)
			n.Renote.MyReaction = &nr
		}
	}
	// reply embed は upstream で detail:false の pack のため myReaction を持たない
	// (NoteEntityService.ts:437 reply は detail:false / :453-459 myReaction は
	// detail block 内、#1816)。renote embed (detail:true) のみ myReaction を出す。
}

func applyChannel(n *NoteEntity, chMap map[string]*model.Channel) {
	if n == nil {
		return
	}
	setChannel := func(target *NoteEntity) {
		if target == nil || target.ChannelID == nil {
			return
		}
		ch, ok := chMap[*target.ChannelID]
		if !ok {
			return
		}
		target.Channel = &ChannelLite{
			ID:                    ch.ID,
			Name:                  ch.Name,
			UserID:                ch.UserID,
			Color:                 ch.Color,
			IsSensitive:           ch.IsSensitive,
			AllowRenoteToExternal: ch.AllowRenoteToExternal,
		}
	}
	setChannel(n)
	setChannel(n.Renote)
	setChannel(n.Reply)
}
