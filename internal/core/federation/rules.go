package federation

import (
	"log/slog"
	"strings"

	"github.com/shiroha-a/mk/internal/activitypub"
	"github.com/shiroha-a/mk/internal/core/fedrule"
	"github.com/shiroha-a/mk/internal/misc/hashtag"
	"github.com/shiroha-a/mk/internal/misc/searchnorm"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/repository"
)

// RuleEvaluator applies the admin-defined federation rules (#3090).
// 実装は fedrule.Service。
type RuleEvaluator interface {
	// HasActivityRules / HasNoteRules は評価するルールがあるか。無ければ
	// 判定のための DB 参照や tag の抽出を省く。
	HasActivityRules() bool
	HasNoteRules() bool
	ActorFacts(u *model.User) fedrule.Actor
	EvaluateActivity(fedrule.ActivityInput) fedrule.Decision
	EvaluateNote(fedrule.NoteInput) fedrule.Decision
}

// SetRuleEvaluator wires the federation rules into activity dispatch.
func (p *Processor) SetRuleEvaluator(e RuleEvaluator) { p.rules = e }

// SetRuleEvaluator wires the federation rules into note ingestion.
func (r *Resolver) SetRuleEvaluator(e RuleEvaluator) { r.rules = e }

// activityRejectedByRules reports whether an enforced activity rule rejects
// act.
//
// **署名検証の後にだけ呼ぶ。** inbox の worker が検証を済ませてから
// ProcessWithSigner に渡すので、dispatchActivity の入口がその位置になる。
// Collection の中身は 1 件ずつ dispatchActivity を通るので、それぞれの種別で
// 評価される。
func (p *Processor) activityRejectedByRules(act genericActivity, signer *model.User) bool {
	if p.rules == nil || !p.rules.HasActivityRules() {
		return false
	}
	host, err := hostFromURI(act.Actor)
	if err != nil {
		return false
	}
	host = strings.ToLower(host)
	// 通常の配送は署名者 = actor で、inbox の検証が actor を取り込んでいる。
	// リレーの転送などで違うときだけ DB を引く (無ければ初めて見る actor)。
	var actor *model.User
	switch {
	case signer != nil && signer.URI != nil && *signer.URI == act.Actor:
		actor = signer
	case p.userRepo != nil:
		if u, err := p.userRepo.FindByURI(act.Actor); err == nil {
			actor = u
		}
	}
	subject := act.ID
	if subject == "" {
		subject = act.Actor
	}
	d := p.rules.EvaluateActivity(fedrule.ActivityInput{
		Host: host, Type: act.Type, Actor: p.rules.ActorFacts(actor), Subject: subject,
	})
	if d.Reject {
		slog.Info("federation: dropping activity rejected by a federation rule",
			"type", act.Type, "actor", act.Actor, "id", act.ID)
	}
	return d.Reject
}

// noteRuleFacts builds what the note rules see. tags は note.tags と同じ規則で
// 本文 / CW / AP の tag 配列から抜く (取り込み本体より前に評価するので、ここで
// 同じ計算をする)。
func noteRuleFacts(actor *model.User, text, cw *string, apNote *activitypub.Note, hasAttachment bool) fedrule.NoteInput {
	// 投票の選択肢もパターンの対象にする (禁止語と同じ)。見ないと、本文を
	// 無害にして選択肢に書くだけで素通りできる。
	in := fedrule.NoteInput{HasAttachment: hasAttachment, Subject: apNote.ID, PollChoices: apPollChoices(apNote)}
	if actor != nil && actor.Host != nil {
		in.Host = strings.ToLower(*actor.Host)
	}
	sources := extractHashtagTagNames(apNote.Tag)
	if text != nil {
		in.Text = *text
		sources = append(sources, *text)
	}
	if cw != nil {
		in.CW = *cw
		sources = append(sources, *cw)
	}
	// **件数で打ち切らない。** note.tags は 32 個までしか持たないが、判定にそれを
	// 使うと、AP の tag 配列を詰め物で埋めて本文の #tag を 33 個目以降へ押し出す
	// だけでタグの条件をすり抜けられる (#3090 の敵対的レビューで実測)。
	seen := map[string]bool{}
	for _, t := range hashtag.Extract(sources...) {
		if n := searchnorm.Normalize(t); n != "" && !seen[n] {
			seen[n] = true
			in.Tags = append(in.Tags, n)
		}
	}
	return in
}

// ruleCW returns the CW a rule adds, or nil when the note already has one.
//
// **既に CW があれば上書きしない。** 本文は既に隠れているので、ルールの文言で
// 送信者の書いた警告文を消す理由が無い。空の CW (sensitive だけ立っていた投稿)
// は文言を補う。
func ruleCW(d fedrule.Decision, current *string) *string {
	if d.CW == nil || (current != nil && *current != "") {
		return nil
	}
	cw := truncateRunes(*d.CW, noteCWMaxRunes)
	return &cw
}

// applyRulesToAttachments drops or keeps the attachments per the decision.
func applyRulesToAttachments(d fedrule.Decision, docs []activitypub.Document) []activitypub.Document {
	if d.StripMedia {
		return nil
	}
	if d.Sensitive {
		for i := range docs {
			docs[i].Sensitive = true
		}
	}
	return docs
}

// markFilesSensitive marks the attached files owned by authorID sensitive and
// reports whether every file is now sensitive.
//
// upsertAttachments は同じ URL の drive_file を再利用するので、新しく作る行に
// sensitive を立てるだけでは、既に取り込んだ添付 (編集や同じ画像の再投稿) に
// 効かない。
//
// **投稿者自身の行だけを書き換える。** 再利用は持ち主を見ないので、他人
// (別のサーバーの利用者) の添付の URL を指す投稿を送るだけで、その人のファイルを
// センシティブにできてしまう (#3090 の敵対的レビューで実測)。他人の行を指す
// 添付は書き換えないので、戻り値が false になる (呼び出し側が投稿ごと畳む)。
func (r *Resolver) markFilesSensitive(ids model.StringArray, authorID string) bool {
	return r.markAuthorFiles(ids, authorID, nil, map[string]any{"isSensitive": true, "maybeSensitive": true})
}

// markAuthorFiles writes fields to the attached files that are not sensitive
// yet and are owned by authorID (or accepted by alsoWritable), and reports
// whether every file is now sensitive.
func (r *Resolver) markAuthorFiles(ids model.StringArray, authorID string, alsoWritable func(*model.DriveFile) bool, fields map[string]any) bool {
	if r.driveFileRepo == nil || len(ids) == 0 {
		return true
	}
	files, err := r.driveFileRepo.FindByIDs(ids)
	if err != nil {
		slog.Warn("federation: cannot load attachments to mark sensitive", "err", err)
		return false
	}
	// 同じ URL の添付が 2 つあると同じ ID が 2 回並ぶ (再利用される) ので、
	// 件数は重複を除いて比べる。
	unique := map[string]bool{}
	for _, id := range ids {
		unique[id] = true
	}
	all := len(files) == len(unique)
	for _, f := range files {
		// 頼まれた印がすでに全部付いていれば書かない。isSensitive だけで判定すると、
		// 別の取り込み (メディアサイレンス) で isSensitive だけ立った行にルールの
		// maybeSensitive が付かない。
		if f.IsSensitive && (fields["maybeSensitive"] == nil || f.MaybeSensitive) {
			continue
		}
		if (f.UserID == nil || *f.UserID != authorID) && (alsoWritable == nil || !alsoWritable(f)) {
			all = false
			continue
		}
		if err := r.driveFileRepo.Update(f.ID, fields); err != nil {
			slog.Warn("federation: cannot mark attachment sensitive", "fileId", f.ID, "err", err)
			all = false
		}
	}
	return all
}

// foldCW is the CW used when some attachment could not be marked sensitive.
//
// 他人の添付の URL を指せば再利用される行は書き換えられない (上)。そのままだと
// 「センシティブにする」が効かないので、投稿ごと空の CW で畳む (送信者の sensitive
// だけが立った投稿と同じ見え方)。既に CW があればそのまま。
func foldCW(current *string) *string {
	if current != nil {
		return nil
	}
	empty := ""
	return &empty
}

// noteAuthorForRules loads the author of an existing note for rule
// evaluation. 行が無ければ nil (初めて見る actor と同じ扱い)。
func (r *Resolver) noteAuthorForRules(userID string) (*model.User, error) {
	if r.userRepo == nil {
		return nil, nil
	}
	u, err := r.userRepo.FindByID(userID)
	if err != nil {
		if repository.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return u, nil
}
