package transfer

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/repository"
)

// exportFavorites reuses the note export pipeline but pulls rows from
// NoteFavoriteRepository instead. Each favorite is unwrapped to its note so
// the output matches the notes export shape (easy round-trip).
//
// keyset pagination で順送りに取り込む (#424 でリポジトリ側を offset から
// untilID 方式に変更した移行)。collectClipNotes と同じパターン。
//
// 閲覧資格を失ったノートは exportableFor で行ごと落とす。cursor は落とす前の
// rows で進める (落とした行で止まるとページが途切れる)。
func (e *Exporter) exportFavorites(user *model.User) ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteByte('[')
	first := true
	untilID := ""
	for {
		rows, err := e.deps.NoteFavoriteRepo.ListByUser(user.ID, untilID, "", notesExportBatchSize)
		if err != nil {
			return nil, err
		}
		if len(rows) == 0 {
			break
		}
		for _, fav := range rows {
			if fav.Note == nil {
				continue
			}
			ok, verr := e.exportableFor(user, fav.Note)
			if verr != nil {
				return nil, verr
			}
			if !ok {
				continue
			}
			entry := map[string]any{
				"id":        fav.ID,
				"favoritee": fav.UserID,
				"note":      e.packNoteForExport(fav.Note),
			}
			raw, jerr := json.Marshal(entry)
			if jerr != nil {
				return nil, jerr
			}
			if !first {
				buf.WriteByte(',')
			}
			buf.Write(raw)
			first = false
		}
		if len(rows) < notesExportBatchSize {
			break
		}
		// 結果は id DESC で帰ってくるので最後の (= 最も古い) ID を次の
		// untilID に渡して次ページを取得する。favorites は note_favorite.id で
		// ページネーションするので cursor も note_favorite.id (= rows[].ID)。
		untilID = rows[len(rows)-1].ID
	}
	buf.WriteByte(']')
	return buf.Bytes(), nil
}

// exportAntennas emits a JSON array with antenna settings suitable for re-import.
// 本家の ExportAntennasProcessorService と同じく keywords / excludeKeywords は
// 既に jsonb として保存されているので RawMessage のまま埋め込む。
func (e *Exporter) exportAntennas(userID string) ([]byte, error) {
	rows, err := e.deps.AntennaRepo.ListByUser(userID)
	if err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0, len(rows))
	for _, a := range rows {
		// #2106 N24: users は nil でも [] を出す (upstream Ajv の required array を満たす)。
		users := []string(a.Users)
		if users == nil {
			users = []string{}
		}
		entry := map[string]any{
			"name":            a.Name,
			"src":             string(a.Src),
			"keywords":        json.RawMessage(a.Keywords),
			"excludeKeywords": json.RawMessage(a.ExcludeKeywords),
			"users":           users,
			"caseSensitive":   a.CaseSensitive,
			"localOnly":       a.LocalOnly,
			"excludeBots":     a.ExcludeBots,
			"withReplies":     a.WithReplies,
			"withFile":        a.WithFile,
			// #2106 N24: upstream ExportedAntenna 互換のため excludeNotesInSensitiveChannel と
			// userListAccts も出力する。list source は member を acct 解決して配列に、それ以外は null
			// (ImportAntennasProcessorService が userListAccts で list→users 変換する)。
			"excludeNotesInSensitiveChannel": a.ExcludeNotesInSensitiveChannel,
			"userListAccts":                  nil,
		}
		if a.UserListID != nil {
			owned, err := e.ownsList(userID, *a.UserListID)
			if err != nil {
				return nil, err
			}
			// 自分の list のときだけ ID とメンバーを出す。1aac65d より前は他人の
			// list ID を保存できたので、そのまま解決すると他人の list のメンバー
			// 構成を export 経由で読めてしまう (照合側の所有者検証と同じ理由)。
			if owned {
				entry["userListId"] = *a.UserListID
				if accts, err := e.listAccts(*a.UserListID); err == nil {
					entry["userListAccts"] = accts
				}
			}
		}
		out = append(out, entry)
	}
	return json.Marshal(out)
}

// ownsList reports whether listID exists and belongs to userID. A missing list
// is reported as not owned; other lookup errors are returned.
func (e *Exporter) ownsList(userID, listID string) (bool, error) {
	list, err := e.deps.UserListRepo.FindByID(listID)
	if err != nil {
		if repository.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("find user list %s: %w", listID, err)
	}
	return list != nil && list.UserID == userID, nil
}

// listAccts resolves a userList's members to acct strings (username / username@host)
// for the antenna export userListAccts field (#2106 N24)。CSV user-list export と同じ
// member 解決を再利用する。
func (e *Exporter) listAccts(listID string) ([]string, error) {
	members, err := e.deps.UserListRepo.ListMembers(listID)
	if err != nil {
		return nil, err
	}
	accts := make([]string, 0, len(members))
	for _, m := range members {
		u := m.User
		if u == nil {
			u2, err := e.deps.UserRepo.FindByID(m.UserID)
			if err != nil || u2 == nil {
				continue
			}
			u = u2
		}
		accts = append(accts, acct(u))
	}
	return accts, nil
}

// exportClips emits a JSON array of the user's clips, each with the packed
// notes contained in the clip. 各 clip 内の note は単純な配列 (本家互換)。
// 自分の clip でも中身は他人のノートなので、閲覧資格の無いものは落とす
// (collectClipNotes 参照)。
func (e *Exporter) exportClips(user *model.User) ([]byte, error) {
	clips, err := e.deps.ClipRepo.ListByUser(user.ID, "", "", 1000, 0)
	if err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0, len(clips))
	for _, c := range clips {
		notes, err := e.collectClipNotes(user, c.ID)
		if err != nil {
			return nil, err
		}
		packed := make([]map[string]any, 0, len(notes))
		for _, n := range notes {
			packed = append(packed, e.packNoteForExport(n))
		}
		out = append(out, map[string]any{
			"id":          c.ID,
			"name":        c.Name,
			"description": c.Description,
			"isPublic":    c.IsPublic,
			"notes":       packed,
		})
	}
	return json.Marshal(out)
}

// collectClipNotes walks ClipNoteRepository with pagination and resolves each
// referenced note into its model.Note form for exporting. Notes the viewer can
// no longer see are skipped (upstream ExportClipsProcessorService parity).
func (e *Exporter) collectClipNotes(viewer *model.User, clipID string) ([]*model.Note, error) {
	var out []*model.Note
	untilID := ""
	for {
		rows, err := e.deps.ClipNoteRepo.ListByClip(clipID, untilID, "", 100)
		if err != nil {
			return nil, err
		}
		if len(rows) == 0 {
			break
		}
		for _, cn := range rows {
			n, err := e.deps.NoteRepo.FindByIDWithRelations(cn.NoteID)
			if err != nil || n == nil {
				continue
			}
			ok, verr := e.exportableFor(viewer, n)
			if verr != nil {
				return nil, verr
			}
			if !ok {
				continue
			}
			out = append(out, n)
		}
		// ListByClip は note.id (= clip_note.noteId) で keyset ページネーションする
		// ため、次ページの cursor も note.id を渡す。clip_note.id を渡すと無関係な
		// ULID で比較されページ抜け/重複が起きる (#1950)。
		untilID = rows[len(rows)-1].NoteID
		if len(rows) < 100 {
			break
		}
	}
	return out, nil
}
