package transfer

import (
	"bytes"
	"encoding/json"

	"github.com/elythia-network/elythia/internal/model"
)

// notesExportBatchSize mirrors the upstream ExportNotesProcessorService.ts
// batch size used for streaming notes out of the DB.
const notesExportBatchSize = 100

// exportNotes emits a JSON array of the user's notes ordered by id descending.
// 各 note は本家の Export*Notes フォーマット互換を目指すが、必要最低限の
// フィールドだけをフラットに出力する。poll は別テーブルなので PollRepo で
// 解決して埋め込む。
func (e *Exporter) exportNotes(userID string) ([]byte, error) {
	return e.exportNotesStream(func(untilID string) ([]*model.Note, error) {
		return e.deps.NoteRepo.ListByUserID(userID, untilID, "", notesExportBatchSize)
	})
}

// exportNotesStream walks a pagination-producing function and marshals each
// note into the JSON array body. Shared between note and favorites exports
// since both use the same item shape.
func (e *Exporter) exportNotesStream(fetch func(untilID string) ([]*model.Note, error)) ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteByte('[')
	first := true
	untilID := ""
	for {
		notes, err := fetch(untilID)
		if err != nil {
			return nil, err
		}
		if len(notes) == 0 {
			break
		}
		for _, n := range notes {
			payload := e.packNoteForExport(n)
			raw, jerr := json.Marshal(payload)
			if jerr != nil {
				return nil, jerr
			}
			if !first {
				buf.WriteByte(',')
			}
			buf.Write(raw)
			first = false
		}
		untilID = notes[len(notes)-1].ID
		if len(notes) < notesExportBatchSize {
			break
		}
	}
	buf.WriteByte(']')
	return buf.Bytes(), nil
}

// packNoteForExport reduces a model.Note to the export-facing subset.
// Misskey フロントエンドが後からインポートする際にこの形状を期待する。
func (e *Exporter) packNoteForExport(n *model.Note) map[string]any {
	if n == nil {
		return nil
	}
	out := map[string]any{
		"id":                 n.ID,
		"text":               n.Text,
		"cw":                 n.CW,
		"userId":             n.UserID,
		"visibility":         string(n.Visibility),
		"localOnly":          n.LocalOnly,
		"reactionAcceptance": n.ReactionAcceptance,
		"fileIds":            []string(n.FileIDs),
		"replyId":            n.ReplyID,
		"renoteId":           n.RenoteID,
	}
	if len(n.VisibleUserIDs) > 0 {
		out["visibleUserIds"] = []string(n.VisibleUserIDs)
	}
	if n.HasPoll && e.deps.PollRepo != nil {
		if p, err := e.deps.PollRepo.FindByNoteID(n.ID); err == nil && p != nil {
			out["poll"] = map[string]any{
				"multiple":  p.Multiple,
				"choices":   []string(p.Choices),
				"expiresAt": p.ExpiresAt,
			}
		}
	}
	return out
}
