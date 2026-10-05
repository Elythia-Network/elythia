// Package hashtag implements the HashtagService.updateHashtag equivalent
// of Misskey TS — maintaining the per-tag mentionedUsersCount / userIds
// arrays so /api/hashtags/list ranking and /show counts work (#680)。
package hashtag

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/misc/keyword"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
)

// Service updates the `hashtag` table when a note is created and references
// hashtags. Wired into note_create_service via HashtagHook (local note path)
// and federation/resolver via the same hook (AP receive path)。
//
// hook 内部で goroutine を spawn する fire-and-forget 設計 (#719)。caller は
// 同期/非同期の差を意識せずに OnNoteCreated を呼べる。
type Service struct {
	repo  repository.HashtagRepository
	idGen id.Generator
	// metaRepo は hiddenTags / sensitiveWords filter で使う (upstream
	// HashtagService.updateHashtagsRanking と同 semantics)。未配線なら
	// filter skip = 全 tag を record する旧挙動 (= drop-in fallback)。
	// production の router.go は必ず wire する。
	metaRepo repository.MetaRepository
	// pending は in-flight worker goroutine 数を追跡する。production では
	// 参照されないが、unit test が WaitForPendingWrites() 経由で完了を
	// 待つために使う。
	pending sync.WaitGroup
}

// NewService constructs a HashtagService.
func NewService(repo repository.HashtagRepository, idGen id.Generator) *Service {
	return &Service{repo: repo, idGen: idGen}
}

// SetMetaRepo wires a MetaRepository so OnNoteCreated can filter tags that
// match meta.hiddenTags or meta.sensitiveWords (upstream HashtagService
// updateHashtagsRanking と同 semantics)。未配線時は filter skip = 旧挙動。
func (s *Service) SetMetaRepo(r repository.MetaRepository) {
	s.metaRepo = r
}

// OnNoteCreated is the HashtagHook implementation: for every tag in
// note.Tags, record the author as having mentioned that tag.
//
// 振る舞い: caller への return を即座に行い、実際の repo 書き込みは
// goroutine で行う (#719)。caller (note_create_service / federation/resolver)
// が hook 完了を待たずに後続処理に進めるので、tag が多い note の inbox 受信
// でも drain time が直列に伸びない。失敗は best-effort で log するのみ。
//
// 実装: 各 tag を順に repo.RecordMention で upsert + dedup する。tag が
// 多い note でもタグ数は通常 1 桁なので逐次で十分。
//
// `note.Tags` は note_create_service / federation/resolver で
// hashtag.Extract 済みなのでここでは追加抽出しない。
//
// テスト用: 内部 sync.WaitGroup を Wait() で待てるようにしているので、unit
// test は `svc.WaitForPendingWrites()` で goroutine の完了を観測してから
// repo state を assert する。
func (s *Service) OnNoteCreated(note *model.Note, author *model.User) {
	if s == nil || s.repo == nil || note == nil || author == nil {
		return
	}
	if len(note.Tags) == 0 {
		return
	}
	isLocal := author.IsLocal()
	// 入力を goroutine 起動前に複製して capture race を防ぐ。
	// (caller が同 *model.Note を後段で mutate しても safe)。空 tag のみ
	// 同期的に弾いて、hiddenTags / sensitiveWords filter は goroutine 内に
	// 移して caller (note_create / federation/resolver) の同期 thread から
	// DB fetch 待ちを完全に除く (= fire-and-forget 設計の本来の意図)。
	rawTags := make([]string, 0, len(note.Tags))
	for _, name := range note.Tags {
		if name != "" {
			rawTags = append(rawTags, name)
		}
	}
	if len(rawTags) == 0 {
		return
	}
	authorID := author.ID
	createdAt := noteCreatedAt(note)

	s.pending.Go(func() {
		defer func() {
			if r := recover(); r != nil {
				slog.Error("hashtag: panic in OnNoteCreated worker", "panic", r)
			}
		}()
		// upstream HashtagService.updateHashtagsRanking と同じく hiddenTags /
		// sensitiveWords にマッチする tag は record しない (= featured /
		// trends に出ないように filter する drop-in compat)。meta fetch を
		// 含めて goroutine 内で完結させることで caller 同期 thread の DB
		// 待ちを完全に除く。
		hidden, sensitive := s.fetchHiddenAndSensitive()
		for _, name := range rawTags {
			if isHiddenTag(name, hidden) {
				continue
			}
			if keyword.IsKeyWordIncluded(name, sensitive) {
				continue
			}
			hid := s.idGen.Generate(createdAt)
			if err := s.repo.RecordMention(hid, name, authorID, isLocal); err != nil {
				slog.Warn("hashtag: RecordMention failed", "name", name, "userID", authorID, "err", err)
			}
		}
	})
}

// UpdateUsertags reconciles the hashtag attach aggregate when a user's tags
// change (profile edit / remote actor refresh). Tags newly added (in newTags
// but not oldTags) are attached, tags removed (in oldTags but not newTags) are
// detached, mirroring Misskey's HashtagService.updateUsertags. Like
// OnNoteCreated it is fire-and-forget: the diff and DB writes run in a worker
// goroutine so callers (i/update, federation resolver) never block.
//
// 追加 tag は hiddenTags / sensitiveWords filter を OnNoteCreated と同様に適用する
// (featured / trends に出さない drop-in compat)。detach は filter 対象外
// (既に集計済みの user を外すだけなので hidden でも整合のため除去する)。
func (s *Service) UpdateUsertags(userID string, isLocal bool, oldTags, newTags []string) {
	if s == nil || s.repo == nil || userID == "" {
		return
	}
	oldSet := make(map[string]struct{}, len(oldTags))
	for _, t := range oldTags {
		oldSet[t] = struct{}{}
	}
	newSet := make(map[string]struct{}, len(newTags))
	for _, t := range newTags {
		newSet[t] = struct{}{}
	}
	var added, removed []string
	for _, t := range newTags {
		if t == "" {
			continue
		}
		if _, ok := oldSet[t]; !ok {
			added = append(added, t)
		}
	}
	for _, t := range oldTags {
		if t == "" {
			continue
		}
		if _, ok := newSet[t]; !ok {
			removed = append(removed, t)
		}
	}
	if len(added) == 0 && len(removed) == 0 {
		return
	}
	now := time.Now()

	s.pending.Go(func() {
		defer func() {
			if r := recover(); r != nil {
				slog.Error("hashtag: panic in UpdateUsertags worker", "panic", r)
			}
		}()
		hidden, sensitive := s.fetchHiddenAndSensitive()
		for _, name := range added {
			if isHiddenTag(name, hidden) {
				continue
			}
			if keyword.IsKeyWordIncluded(name, sensitive) {
				continue
			}
			hid := s.idGen.Generate(now)
			if err := s.repo.RecordAttach(hid, name, userID, isLocal); err != nil {
				slog.Warn("hashtag: RecordAttach failed", "name", name, "userID", userID, "err", err)
			}
		}
		for _, name := range removed {
			if err := s.repo.RecordDetach(name, userID, isLocal); err != nil {
				slog.Warn("hashtag: RecordDetach failed", "name", name, "userID", userID, "err", err)
			}
		}
	})
}

// WaitForPendingWrites blocks until all in-flight OnNoteCreated goroutines
// finish. Production code does not call this — it exists so unit tests can
// observe the post-write repo state deterministically without polling.
// 無期限 ctx (context.TODO) で Shutdown を呼んで実装を共有し drift を防ぐ。
func (s *Service) WaitForPendingWrites() {
	_ = s.Shutdown(context.TODO())
}

// Shutdown は in-flight worker goroutine が drain するか ctx 期限切れまで
// 待つ。production の SIGTERM / graceful shutdown 経路で呼ばれる (#727)。
// fire-and-forget worker (#719) が DB write 中に process が落ちると
// hashtag table の集計がスキップされるが、idempotent な RecordMention
// なので次回同タグ note 受信時に再カウントされる。それでも shutdown
// 時の error log を抑え観測ノイズを下げるためにここで drain する。
//
// 無期限 wait したい (test 等) なら context.TODO() を渡す。
func (s *Service) Shutdown(ctx context.Context) error {
	if s == nil {
		return nil
	}
	done := make(chan struct{})
	go func() {
		s.pending.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// fetchHiddenAndSensitive returns (hiddenTags, sensitiveWords) from meta,
// or (nil, nil) when metaRepo is not wired / fetch failed / both empty.
// Best-effort: meta fetch 失敗時は filter skip = 旧挙動 (= 全 tag 通る)
// に fail-open する。filter 漏れの方が「tag が突然 featured から消える」
// より影響軽微。
func (s *Service) fetchHiddenAndSensitive() (hidden []string, sensitive []string) {
	if s.metaRepo == nil {
		return nil, nil
	}
	m, err := s.metaRepo.Fetch()
	if err != nil || m == nil {
		return nil, nil
	}
	if len(m.HiddenTags) == 0 && len(m.SensitiveWords) == 0 {
		return nil, nil
	}
	return []string(m.HiddenTags), []string(m.SensitiveWords)
}

// isHiddenTag は tag が hiddenTags 集合に case-insensitive で含まれるかを判定。
// upstream は normalizeForSearch (= lowercase) で比較しているので mk-go も
// strings.EqualFold で同等にする。tag は extract 時に lowercase 化されている
// が、admin が hiddenTags に大文字混じりを設定するケースを救済する。
func isHiddenTag(tag string, hidden []string) bool {
	for _, h := range hidden {
		if strings.EqualFold(tag, h) {
			return true
		}
	}
	return false
}

// noteCreatedAt は note ID から作成時刻を再計算するためのヘルパ。idGen の
// ParseTime に依存するが、note 作成 hook なら ID は妥当な値が入っている
// 想定。失敗時はゼロ値時刻が返るが、Hashtag.ID は INSERT ON CONFLICT で
// 既存値が優先されるので新規 row 採番にしか影響しない (= 表示順への影響
// は無視できる)。
func noteCreatedAt(_ *model.Note) (_ time.Time) { return time.Now() }
