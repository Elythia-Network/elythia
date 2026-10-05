// Package gonecleanup removes the follow relations left with instances that
// became goneSuspended (#3067).
//
// shared inbox が 410 を返したインスタンスは goneSuspended になり配送は止まるが、
// フォロー関係は残り続ける。利用者から見ると、もう存在しないアカウントが
// フォロワー / フォローの一覧に並び続ける。**消すのは取り返しがつかない**
// (誤りだったら、相手の利用者全員が再フォローするしかない) ので、自動では消さず
// 管理者が候補を見て実行する。
package gonecleanup

import (
	"errors"
	"fmt"
	"sync"

	"github.com/elythia-network/elythia/internal/core/following"
	coreinstance "github.com/elythia-network/elythia/internal/core/instance"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
)

// MaxPerRun bounds how many follow relations one Clean removes. 残りは
// Result.Remaining で返し、管理者がもう一度実行する (1 リクエストを長引かせない)。
const MaxPerRun = 1000

// ErrNotGone is returned when the host is not goneSuspended (now).
var ErrNotGone = errors.New("gonecleanup: instance is not goneSuspended")

// ErrInProgress is returned when a Clean of the same host is already running.
var ErrInProgress = errors.New("gonecleanup: cleanup of the host is already running")

// recheckEvery is how often Clean re-reads the instance's state while it
// removes relations.
const recheckEvery = 100

// Repo lists and removes what is left with gone instances.
type Repo interface {
	ListGone() ([]repository.GoneInstanceSummary, error)
	ListRelations(host string, limit int) ([]*model.Following, error)
	CountRelations(host string) (int64, error)
	DeleteFollowRequests(host string) (int64, error)
}

// Instances looks up an instance's current state.
type Instances interface {
	FindByHost(host string) (*model.Instance, error)
}

// Unfollower removes one follow relation without delivering anything and
// without notifying the local user (main stream / webhook).
type Unfollower interface {
	UnfollowQuiet(followerID, followeeID string) error
}

// Result is what one Clean removed.
type Result struct {
	Host string `json:"host"`
	// RemovedFollowers は相手 → ローカルのフォロー、RemovedFollowing は
	// ローカル → 相手のフォロー。
	RemovedFollowers      int64 `json:"removedFollowers"`
	RemovedFollowing      int64 `json:"removedFollowing"`
	RemovedFollowRequests int64 `json:"removedFollowRequests"`
	// Remaining は MaxPerRun を超えて残ったフォロー関係の数。
	Remaining int64 `json:"remaining"`
}

// Service lists and cleans gone instances.
type Service struct {
	repo       Repo
	instances  Instances
	unfollower Unfollower

	// running は片付け中のホスト。**同じホストを並行して片付けない** —
	// 解除は「引いて → 消して → 数を減らす」なので、2 つが同じ行を引くと
	// 両方が数を減らし、フォロー数が二重に減る (2 人のモデレーターや 2 つの
	// タブで押すだけで起きる)。プロセス内の排他なので、別ノードからの同時実行
	// までは防がない。
	mu      sync.Mutex
	running map[string]bool
}

// NewService constructs a Service.
func NewService(repo Repo, instances Instances, unfollower Unfollower) *Service {
	return &Service{repo: repo, instances: instances, unfollower: unfollower, running: map[string]bool{}}
}

// List returns the goneSuspended instances with what is left with them.
func (s *Service) List() ([]repository.GoneInstanceSummary, error) {
	return s.repo.ListGone()
}

// Clean removes the follow relations and follow requests between host and
// local users. host must be goneSuspended at the time of the call.
//
// **実行時に状態を読み直す。** 一覧を開いてから実行するまでの間に管理者が戻して
// いれば、それは「相手は生きている」という判断なので消さない。手動停止
// (manuallySuspended) も対象外 — 「今は止めているが関係は維持する」意図のため。
//
// **配送も利用者への通知もしない** (UnfollowQuiet)。相手はもう存在しないので
// Undo / Reject を送っても届かず、管理者の操作で何百件も解除するので利用者の
// webhook やストリームへ 1 件ずつ出さない (upstream の remove-all-following が
// 使う silent と同じ)。フォロー数とチャートは通常の解除と同じく更新する。
func (s *Service) Clean(host string) (Result, error) {
	res := Result{Host: host}
	if !s.begin(host) {
		return res, ErrInProgress
	}
	defer s.end(host)
	if err := s.checkGone(host); err != nil {
		return res, err
	}

	// 先に集めてから消す。消しながらページを進めると、行が詰まって取りこぼす。
	rows, err := s.repo.ListRelations(host, MaxPerRun)
	if err != nil {
		return res, err
	}
	for i, f := range rows {
		// **途中でも状態を読み直す。** 片付けている間に管理者が戻したら、
		// それは「相手は生きている」という判断なので残りは消さない。
		if i > 0 && i%recheckEvery == 0 {
			if err := s.checkGone(host); err != nil {
				return res, err
			}
		}
		if err := s.unfollower.UnfollowQuiet(f.FollowerID, f.FolloweeID); err != nil {
			// 並行して解除済みなら数えずに進む。
			if errors.Is(err, following.ErrNotFollowing) {
				continue
			}
			return res, fmt.Errorf("unfollow %s -> %s: %w", f.FollowerID, f.FolloweeID, err)
		}
		if f.FollowerHost != nil {
			res.RemovedFollowers++
		} else {
			res.RemovedFollowing++
		}
	}
	if len(rows) > 0 {
		if err := s.checkGone(host); err != nil {
			return res, err
		}
	}
	n, err := s.repo.DeleteFollowRequests(host)
	if err != nil {
		return res, err
	}
	res.RemovedFollowRequests = n
	remaining, err := s.repo.CountRelations(host)
	if err != nil {
		return res, err
	}
	res.Remaining = remaining
	return res, nil
}

// checkGone returns nil only when host is goneSuspended right now.
func (s *Service) checkGone(host string) error {
	inst, err := s.instances.FindByHost(host)
	switch {
	case errors.Is(err, coreinstance.ErrInstanceNotFound) || repository.IsNotFound(err):
		return ErrNotGone
	case err != nil:
		return fmt.Errorf("find instance %s: %w", host, err)
	case inst.SuspensionState != model.SuspensionStateGoneSuspended:
		return ErrNotGone
	}
	return nil
}

func (s *Service) begin(host string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running[host] {
		return false
	}
	s.running[host] = true
	return true
}

func (s *Service) end(host string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.running, host)
}
