// Package backupadmin is what the main server does with the database backups
// for the admin page (#3462): list the generations and the storage usage,
// delete a generation, hand out a download, and ask the backup service to take
// or verify a backup.
//
// 本体の image には pg_dump が無いので、取る・確かめるはバックアップ用の
// サービス (elythia backup daemon、#3460) の制御 API に頼む。本体が保存先に
// 直接触るのは、一覧・使用量・削除・ダウンロードだけ。
package backupadmin

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/elythia-network/elythia/internal/backup"
)

// Errors returned by Service.
var (
	// ErrNotConfigured: the config file has no usable `backup:` storage.
	ErrNotConfigured = errors.New("backupadmin: backup storage is not configured")
	// ErrNotFound: the generation does not exist.
	ErrNotFound = errors.New("backupadmin: generation not found")
	// ErrInvalidID: the generation ID is malformed.
	ErrInvalidID = errors.New("backupadmin: invalid generation id")
	// ErrIncomplete: the generation has no readable meta.json.
	ErrIncomplete = errors.New("backupadmin: generation is incomplete")
)

// DefaultDownloadTTL is how long a download URL stays valid.
//
// 署名付き URL は漏れたら期限まで誰でも使えるので短くする。数 GB の dump でも
// 期限は「ダウンロードを始めるまで」の時間なので (S3 は始まった転送を切らない)、
// 5 分あれば足りる。
const DefaultDownloadTTL = 5 * time.Minute

// bytesPerGB is the unit of PricePerGBMonth. S3 and R2 bill storage per
// GB-month where 1 GB is 2^30 bytes.
const bytesPerGB = 1 << 30

// Generation is one generation as shown in the admin page.
type Generation struct {
	ID        string    `json:"id"`
	CreatedAt time.Time `json:"createdAt"`
	// Complete is false when meta.json is missing or unreadable, or when the
	// dump it names is missing (backup.Generation.Complete). Such a
	// generation cannot be restored; it still uses storage.
	Complete bool `json:"complete"`
	// MetaError says why meta.json could not be read.
	MetaError string `json:"metaError,omitempty"`
	// Size is the total size of every object of the generation.
	Size        int64 `json:"size"`
	ObjectCount int   `json:"objectCount"`

	DumpSize        int64                   `json:"dumpSize,omitempty"`
	Encrypted       bool                    `json:"encrypted"`
	ElythiaVersion  string                  `json:"elythiaVersion,omitempty"`
	ElythiaCommit   string                  `json:"elythiaCommit,omitempty"`
	PostgresVersion string                  `json:"postgresVersion,omitempty"`
	Database        string                  `json:"database,omitempty"`
	Migrations      []backup.MigrationState `json:"migrations"`
	// Verify is the result of the last `backup verify`, or nil when the
	// generation has not been verified.
	Verify *VerifySummary `json:"verify"`
}

// VerifySummary is the part of verify.json the admin page shows.
type VerifySummary struct {
	OK         bool                 `json:"ok"`
	VerifiedAt time.Time            `json:"verifiedAt"`
	Stages     []backup.StageResult `json:"stages"`
	Mismatches []backup.RowMismatch `json:"mismatches"`
	// Error says why verify.json could not be read.
	Error string `json:"error,omitempty"`
}

// Usage is the storage usage.
type Usage struct {
	// TotalBytes and ObjectCount cover every object in the storage root,
	// including objects outside the generations.
	TotalBytes      int64 `json:"totalBytes"`
	ObjectCount     int   `json:"objectCount"`
	GenerationCount int   `json:"generationCount"`
	// PricePerGBMonth and MonthlyCost are nil when no price is configured.
	PricePerGBMonth *float64 `json:"pricePerGbMonth"`
	MonthlyCost     *float64 `json:"monthlyCost"`
}

// Overview is the response of List.
type Overview struct {
	StorageType string       `json:"storageType"`
	Generations []Generation `json:"generations"`
	Usage       Usage        `json:"usage"`
	Service     ServiceState `json:"service"`
}

// ServiceState is what the backup service reports.
type ServiceState struct {
	// Configured is false when backup.server.serviceUrl is empty; taking and
	// verifying from the admin page are then unavailable.
	Configured bool `json:"configured"`
	// Reachable is false when the status request failed.
	Reachable bool `json:"reachable"`
	// Error says why the status request failed.
	Error string `json:"error,omitempty"`
	// The rest comes from GET /status of the backup service. It is kept only
	// in the service's memory and is empty after the service restarts.
	Running         *Job              `json:"running"`
	NextRunAt       *time.Time        `json:"nextRunAt"`
	LastTake        *JobResult        `json:"lastTake"`
	LastVerify      *JobResult        `json:"lastVerify"`
	LatestUsable    *UsableGeneration `json:"latestUsable"`
	Overdue         bool              `json:"overdue"`
	LastNotifyError string            `json:"lastNotifyError,omitempty"`
}

// Download is how the admin downloads a generation.
type Download struct {
	// URL is a presigned storage URL (S3) or a short-lived URL served by the
	// main server (directory storage).
	URL       string    `json:"url"`
	ExpiresAt time.Time `json:"expiresAt"`
	// Via is "storage" (presigned) or "server".
	Via       string `json:"via"`
	FileName  string `json:"fileName"`
	Size      int64  `json:"size"`
	Encrypted bool   `json:"encrypted"`
}

// Options configures a Service.
type Options struct {
	// StorageType is shown in the admin page ("s3" or "dir").
	StorageType string
	// Storage is nil when the backup storage is not configured.
	Storage backup.Storage
	// Control is nil when backup.server.serviceUrl is empty.
	Control Control
	// Tokens issues the server-side download URLs for storages that cannot
	// presign. Required with a directory storage.
	Tokens DownloadTokens
	// DownloadURLBase is the absolute URL that server-side download tokens
	// are appended to (e.g. https://example.com/backup-download?token=).
	DownloadURLBase string
	// PricePerGBMonth enables the monthly estimate when positive.
	PricePerGBMonth float64
	// DownloadTTL defaults to DefaultDownloadTTL.
	DownloadTTL time.Duration
	// Now defaults to time.Now.
	Now func() time.Time
}

// Service implements the admin operations.
type Service struct {
	o Options
}

// NewService returns a Service.
func NewService(o Options) *Service {
	if o.DownloadTTL <= 0 {
		o.DownloadTTL = DefaultDownloadTTL
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return &Service{o: o}
}

// List returns the generations, the storage usage and the state of the backup
// service.
func (s *Service) List(ctx context.Context) (*Overview, error) {
	st := s.o.Storage
	if st == nil {
		return nil, ErrNotConfigured
	}
	// 使用量は保存先の根の下の全て (世代の外のものも) を数えるので、世代の一覧
	// (generations/ の下だけ) とは別に数える。
	objects, err := st.List(ctx, "")
	if err != nil {
		return nil, fmt.Errorf("list storage: %w", err)
	}
	ov := &Overview{StorageType: s.o.StorageType, Generations: []Generation{}}
	for _, o := range objects {
		ov.Usage.TotalBytes += o.Size
		ov.Usage.ObjectCount++
	}
	gens, err := backup.ListGenerations(ctx, st)
	if err != nil {
		return nil, fmt.Errorf("list generations: %w", err)
	}
	for _, bg := range gens {
		ov.Generations = append(ov.Generations, s.generation(ctx, bg))
	}
	// 新しい世代を先に出す。ID は UTC の時刻で、文字列の順が時刻の順になる。
	sort.Slice(ov.Generations, func(i, j int) bool { return ov.Generations[i].ID > ov.Generations[j].ID })
	ov.Usage.GenerationCount = len(ov.Generations)
	if p := s.o.PricePerGBMonth; p > 0 {
		cost := float64(ov.Usage.TotalBytes) / bytesPerGB * p
		ov.Usage.PricePerGBMonth = &p
		ov.Usage.MonthlyCost = &cost
	}
	ov.Service = s.serviceState(ctx)
	return ov, nil
}

// generation converts a generation read by backup.ListGenerations.
func (s *Service) generation(ctx context.Context, bg backup.Generation) Generation {
	g := Generation{ID: bg.ID, Size: bg.Size, ObjectCount: len(bg.Objects), Migrations: []backup.MigrationState{}}
	if t, err := backup.IDTime(bg.ID); err == nil {
		g.CreatedAt = t
	}
	switch m := bg.Meta; {
	case bg.MetaError != nil:
		g.MetaError = bg.MetaError.Error()
	case m == nil:
		g.MetaError = backup.MetaFile + " is missing"
	default:
		if err := checkDumpFile(m); err != nil {
			g.MetaError = err.Error()
			break
		}
		// meta.json が読めても dump が無ければ戻せないので、完成とは数えない
		// (backup.Generation.Complete の契約)。
		if bg.Complete() {
			g.Complete = true
		} else {
			g.MetaError = m.DumpFile + " is missing"
		}
		if !m.CreatedAt.IsZero() {
			g.CreatedAt = m.CreatedAt
		}
		g.DumpSize = m.DumpSize
		g.Encrypted = m.Encrypted
		g.ElythiaVersion = m.ElythiaVersion
		g.ElythiaCommit = m.ElythiaCommit
		g.PostgresVersion = m.PostgresVersion
		g.Database = m.Database
		if m.Migrations != nil {
			g.Migrations = m.Migrations
		}
	}
	g.Verify = s.verifySummary(ctx, bg)
	return g
}

// verifySummary summarizes verify.json of bg, or returns nil when the
// generation has not been verified.
func (s *Service) verifySummary(ctx context.Context, bg backup.Generation) *VerifySummary {
	vr := bg.Verify
	if vr == nil {
		if !hasObject(bg, backup.VerifyFile) {
			return nil
		}
		// ListGenerations は読めない verify.json を黙って捨てるので、管理画面に
		// 理由を出すためにもう一度読む。
		var err error
		if vr, err = backup.ReadVerify(ctx, s.o.Storage, bg.ID); err != nil {
			return &VerifySummary{Error: err.Error(), Stages: []backup.StageResult{}, Mismatches: []backup.RowMismatch{}}
		}
	}
	vs := &VerifySummary{OK: vr.OK, VerifiedAt: vr.VerifiedAt, Stages: vr.Stages, Mismatches: vr.Mismatches}
	if vs.Stages == nil {
		vs.Stages = []backup.StageResult{}
	}
	if vs.Mismatches == nil {
		vs.Mismatches = []backup.RowMismatch{}
	}
	return vs
}

func hasObject(bg backup.Generation, name string) bool {
	key := backup.Key(bg.ID, name)
	for _, o := range bg.Objects {
		if o.Key == key {
			return true
		}
	}
	return false
}

// checkDumpFile rejects a dumpFile that would name an object outside the
// generation.
//
// 保存先の中身は書き込める誰かが作れるので信用しきらない。dumpFile から作った
// key でダウンロードの URL を出すので、世代の外を指す値はここで落とす。
func checkDumpFile(m *backup.Meta) error {
	if m.DumpFile == "" || strings.Contains(m.DumpFile, "/") || strings.Contains(m.DumpFile, "..") {
		return fmt.Errorf("%s has invalid dumpFile %q", backup.MetaFile, m.DumpFile)
	}
	return nil
}

// readMeta reads and checks meta.json of generation id.
func (s *Service) readMeta(ctx context.Context, id string) (*backup.Meta, error) {
	m, err := backup.ReadMeta(ctx, s.o.Storage, id)
	if err != nil {
		if errors.Is(err, backup.ErrNotFound) {
			return nil, fmt.Errorf("%s is missing", backup.MetaFile)
		}
		return nil, err
	}
	if err := checkDumpFile(m); err != nil {
		return nil, err
	}
	return m, nil
}

func (s *Service) serviceState(ctx context.Context) ServiceState {
	if s.o.Control == nil {
		return ServiceState{}
	}
	st := ServiceState{Configured: true}
	status, err := s.o.Control.Status(ctx)
	if err != nil {
		st.Error = err.Error()
		return st
	}
	st.Reachable = true
	st.Running = status.Running
	st.NextRunAt = status.NextRunAt
	st.LastTake = status.LastTake
	st.LastVerify = status.LastVerify
	st.LatestUsable = status.LatestUsable
	st.Overdue = status.Overdue
	st.LastNotifyError = status.LastNotifyError
	return st
}

// generationObjects returns the objects of generation id.
func (s *Service) generationObjects(ctx context.Context, id string) ([]backup.ObjectInfo, error) {
	if s.o.Storage == nil {
		return nil, ErrNotConfigured
	}
	if !backup.ValidID(id) {
		return nil, ErrInvalidID
	}
	objects, err := s.o.Storage.List(ctx, backup.Key(id, ""))
	if err != nil {
		return nil, fmt.Errorf("list generation: %w", err)
	}
	if len(objects) == 0 {
		return nil, ErrNotFound
	}
	return objects, nil
}

// Delete removes every object of generation id, meta.json first (see
// backup.DeleteGeneration). It returns the number of bytes freed.
func (s *Service) Delete(ctx context.Context, id string) (int64, error) {
	if s.o.Storage == nil {
		return 0, ErrNotConfigured
	}
	if !backup.ValidID(id) {
		return 0, ErrInvalidID
	}
	freed, err := backup.DeleteGeneration(ctx, s.o.Storage, id)
	if errors.Is(err, backup.ErrNotFound) {
		return 0, ErrNotFound
	}
	return freed, err
}

// Download returns a short-lived URL for the dump of generation id. An
// encrypted dump is handed out encrypted. userID is the administrator who
// asked; it is recorded when the server-side URL is used.
func (s *Service) Download(ctx context.Context, id, userID string) (*Download, error) {
	if _, err := s.generationObjects(ctx, id); err != nil {
		return nil, err
	}
	m, err := s.readMeta(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrIncomplete, err)
	}
	key := backup.Key(id, m.DumpFile)
	info, err := s.o.Storage.Stat(ctx, key)
	if err != nil {
		if errors.Is(err, backup.ErrNotFound) {
			return nil, fmt.Errorf("%w: %s is missing", ErrIncomplete, m.DumpFile)
		}
		return nil, fmt.Errorf("stat dump: %w", err)
	}
	d := &Download{
		ExpiresAt: s.o.Now().Add(s.o.DownloadTTL),
		// 署名付き URL の Content-Disposition (backup.S3Storage.PresignGet) と同じ
		// 名前にし、どちらの保存先でも同じ名前で保存されるようにする。
		FileName:  id + "-" + m.DumpFile,
		Size:      info.Size,
		Encrypted: m.Encrypted,
	}
	if p, ok := s.o.Storage.(backup.Presigner); ok {
		u, err := p.PresignGet(ctx, key, s.o.DownloadTTL)
		if err != nil {
			return nil, fmt.Errorf("presign: %w", err)
		}
		d.URL, d.Via = u, "storage"
		return d, nil
	}
	if s.o.Tokens == nil || s.o.DownloadURLBase == "" {
		return nil, errors.New("backupadmin: server-side download is not wired")
	}
	token, err := s.o.Tokens.Issue(ctx, DownloadGrant{Key: key, FileName: d.FileName, UserID: userID}, s.o.DownloadTTL)
	if err != nil {
		return nil, fmt.Errorf("issue download token: %w", err)
	}
	d.URL, d.Via = s.o.DownloadURLBase+token, "server"
	return d, nil
}

// Open opens the object a server-side download token grants. The caller
// streams it to the client.
func (s *Service) Open(ctx context.Context, token string) (*DownloadGrant, io.ReadCloser, backup.ObjectInfo, error) {
	if s.o.Storage == nil || s.o.Tokens == nil {
		return nil, nil, backup.ObjectInfo{}, ErrNotConfigured
	}
	g, err := s.o.Tokens.Resolve(ctx, token)
	if err != nil {
		return nil, nil, backup.ObjectInfo{}, err
	}
	info, err := s.o.Storage.Stat(ctx, g.Key)
	if err != nil {
		if errors.Is(err, backup.ErrNotFound) {
			return nil, nil, backup.ObjectInfo{}, ErrNotFound
		}
		return nil, nil, backup.ObjectInfo{}, fmt.Errorf("stat: %w", err)
	}
	rc, err := s.o.Storage.Get(ctx, g.Key)
	if err != nil {
		if errors.Is(err, backup.ErrNotFound) {
			return nil, nil, backup.ObjectInfo{}, ErrNotFound
		}
		return nil, nil, backup.ObjectInfo{}, fmt.Errorf("open: %w", err)
	}
	return g, rc, info, nil
}

// Take asks the backup service to take a backup now. clientIP is the
// administrator's address.
func (s *Service) Take(ctx context.Context, clientIP string) (*Job, error) {
	if s.o.Control == nil {
		return nil, ErrServiceNotConfigured
	}
	return s.o.Control.Take(ctx, clientIP)
}

// Verify asks the backup service to verify generation id now.
func (s *Service) Verify(ctx context.Context, id, clientIP string) (*Job, error) {
	if s.o.Control == nil {
		return nil, ErrServiceNotConfigured
	}
	if _, err := s.generationObjects(ctx, id); err != nil {
		return nil, err
	}
	if _, err := s.readMeta(ctx, id); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrIncomplete, err)
	}
	return s.o.Control.Verify(ctx, id, clientIP)
}
