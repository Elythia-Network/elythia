package federation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/elythia-network/elythia/internal/activitypub"
	coreblocking "github.com/elythia-network/elythia/internal/core/blocking"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
)

// handleBlock processes an inbound Block activity. リモートユーザーがローカル
// ユーザーをブロックした場合にブロック関係を作成する。
func (p *Processor) handleBlock(act genericActivity) error {
	if p.blockingService == nil {
		return ErrUnsupportedActivity
	}
	blocker, err := p.resolver.ResolveActor(act.Actor)
	if err != nil {
		return err
	}
	blockeeURI, err := readObjectString(act.Object)
	if err != nil {
		return err
	}
	// 本家 block() は getUserFromApId で引き、見つからなければ skip (#3330)。
	// ローカルユーザーは user.uri が NULL なので、URI から ID を読んで引く。
	blockee, err := p.userFromAPID(blockeeURI, inboundActorHost(act.Actor))
	if err != nil {
		if repository.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("block: lookup blockee: %w", err)
	}
	// ローカルユーザーのみ対象
	if blockee.Host != nil {
		return nil
	}
	if _, err := p.blockingService.Block(blocker.ID, blockee.ID); err != nil {
		if errors.Is(err, coreblocking.ErrAlreadyBlocking) {
			return nil
		}
		return err
	}
	return nil
}

// handleUndoBlock removes a blocking relationship created by a previous Block.
func (p *Processor) handleUndoBlock(act genericActivity, inner genericActivity) error {
	if p.blockingService == nil {
		return ErrUnsupportedActivity
	}
	blocker, err := p.resolver.ResolveActor(act.Actor)
	if err != nil {
		return err
	}
	blockeeURI, err := readObjectString(inner.Object)
	if err != nil {
		return err
	}
	// 本家 undoBlock() も getUserFromApId で引き、見つからなければ skip (#3330)。
	blockee, err := p.userFromAPID(blockeeURI, inboundActorHost(act.Actor))
	if err != nil {
		if repository.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("undo block: lookup blockee: %w", err)
	}
	// #2106 L30: upstream undoBlock は blockee が remote (host != null) なら skip する
	// (handleBlock の gate と対称)。
	if blockee.Host != nil {
		return nil
	}
	if err := p.blockingService.Unblock(blocker.ID, blockee.ID); err != nil {
		if errors.Is(err, coreblocking.ErrNotBlocking) {
			return nil
		}
		return err
	}
	return nil
}

// abuseReportCommentMaxRunes は `abuse_user_report.comment` の varchar(2048)
// (migration/000013_abuse_report.up.sql)。
const abuseReportCommentMaxRunes = 2048

// handleFlag processes an inbound Flag (abuse report) activity. リモートインスタンスからの
// 通報を保存する。
func (p *Processor) handleFlag(act genericActivity) error {
	if p.abuseReportRepo == nil {
		return ErrUnsupportedActivity
	}
	reporter, err := p.resolver.ResolveActor(act.Actor)
	if err != nil {
		return err
	}
	// object から URI 配列を取得する (ユーザーや Note の URI)。upstream は
	// `getApIds(activity.object)` なので、単一値・配列に加えて
	// `{"id": "..."}` の object 形式も受ける (ApInboxService.ts:560)。旧実装は
	// string と []string しか受けず、object 形式の Flag を落としていた。
	var uris activitypub.APIDList
	if err := json.Unmarshal(act.Object, &uris); err != nil {
		slog.Info("federation: dropping Flag with an unreadable object", "actor", act.Actor)
		return nil
	}
	if len(uris) == 0 {
		slog.Info("federation: dropping Flag without an object", "actor", act.Actor)
		return nil
	}
	// upstream flag() は object URI を config.url + '/users/' prefix の LOCAL
	// user URI に絞ってから user id に map し users[0] を報告対象にする
	// (#1560、ApInboxService.ts:560-577)。mk-go も flagTargetUserID で
	// `{baseURL}/users/...` 形式のローカル URI だけを対象にし、本家と同じく最後の
	// 段を ID として引く (`/users/{id}/followers` は誰にも当たらない、#3330)。旧実装は
	// 任意 host の user/note URI を受け、note 作者 (リモート可) まで fallback
	// して別 instance のユーザーを誤って報告し得た。
	target, err := p.flagTargetUser(uris, inboundActorHost(act.Actor))
	if err != nil {
		return err
	}
	if target == nil {
		// ローカル user を 1 件も解決できない Flag は ack して drop する
		// (リモート対象や不正 URI。retry しても解決しないため error にしない)。
		slog.Info("federation: skipping Flag with no resolvable local target", "actor", act.Actor)
		return nil
	}
	// contentフィールドを取得。upstream は comment に flagged URI 一覧を
	// 付与する (`${content}\n${JSON.stringify(uris, null, 2)}`、#1560)。
	// **mk-go は pretty print しない** (upstream は 2 space indent)。通報の
	// 本文だけの差で、既存の通報との一貫性を優先して compact のままにする
	// (docs/divergence/federation.md 3-3)。
	var content struct {
		Content string `json:"content"`
	}
	_ = json.Unmarshal(act.raw, &content)
	urisJSON, _ := json.Marshal(uris)
	// `abuse_user_report.comment` は varchar(2048) (upstream の MiAbuseUserReport
	// も同じ)。**通報本文も URI 一覧も相手が自由に決められる**ので、溢れると
	// Create が 22001 で落ちて error を返し、**その inbox job が retry を
	// 使い切って dead になる = 通報が届かない**。通報本文は読むための文字列
	// なので切って NUL を落とす (#2726)。
	//
	// **URI 一覧を先に置かない。** 切るのは末尾なので、本文が長いと URI が
	// 落ちる。upstream の並び (content → uris) を保ったまま切ると、少なくとも
	// 通報の趣旨は残る。
	comment := remoteText(content.Content+"\n"+string(urisJSON), abuseReportCommentMaxRunes)
	report := &model.AbuseUserReport{
		ID:           p.abuseIDGen.Generate(nowFn()),
		TargetUserID: target.ID,
		ReporterID:   reporter.ID,
		Comment:      comment,
		// 対象の行は flagTargetUser で引いたものをそのまま使う。以前は ID から
		// 引き直し、その失敗を握り潰して TargetUserHost を nil (= ローカル扱い)
		// のまま保存しえた (#3330)。
		TargetUserHost: target.Host,
		ReporterHost:   reporter.Host,
	}
	if err := p.abuseReportRepo.Create(report); err != nil {
		slog.Warn("failed to create abuse report from flag activity", "err", err)
		return err
	}
	// 通知 (通知欄 / admin stream / abuseReport system webhook) はローカルの
	// 通報と同じ処理に任せる (#3256)。
	if p.abuseCreated != nil {
		p.abuseCreated.NotifyCreated(context.Background(), report, reporter, target)
	}
	return nil
}

// flagTargetUser picks the user an inbound Flag reports among its object
// URIs, the way upstream ApInboxService.flag does: among the existing users
// named by the `{url}/users/...` URIs (flagTargetUserID), the one with the
// smallest ID. It returns nil when none of them exists.
//
// **本家は `findBy({ id: In(userIds) })` の users[0] で、ORDER BY を持たない。**
// 返る順は PostgreSQL の plan 次第で、利用者の表が大きく ID が少数なら主キーの
// Index Scan (ID の昇順) になり、表が小さいか ID が多いと Seq Scan / Bitmap Heap
// Scan (物理的な並び) になる。mk-go は普段の plan と同じ ID の昇順に固定する
// (URI の並び順の先頭ではない、#3330)。本家と同じく host と isDeleted では
// 絞らない。
//
// 引けなかったことを「誰も居ない」に潰さない (#3121)。潰すと job が ack されて
// retry されず、通報が届かない。
func (p *Processor) flagTargetUser(uris []string, sourceHost string) (*model.User, error) {
	var ids []string
	seen := make(map[string]struct{}, len(uris))
	for _, uri := range uris {
		id := p.resolver.flagTargetUserID(uri)
		if id == "" {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return nil, nil
	}
	users, err := p.userRepo.FindManyByIDs(ids)
	if err != nil {
		return nil, fmt.Errorf("flag: lookup target users: %w", err)
	}
	var target *model.User
	for _, u := range users {
		if u != nil && (target == nil || u.ID < target.ID) {
			target = u
		}
	}
	if target != nil {
		if err := p.resolver.RequireInboundLocalUser(target, sourceHost); err != nil {
			return nil, err
		}
	}
	return target, nil
}
