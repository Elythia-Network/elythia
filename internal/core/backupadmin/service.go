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
	"encoding/json"
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
	// Complete is false when meta.json is missing or unreadable. Such a
	// generation is an interrupted upload; it still uses storage.
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

// metaReadLimit caps meta.json and verify.json. 行数の表を含めても数百 KB に
// 収まる。保存先の中身を信用しきらず、壊れた巨大なファイルでメモリを使い切らない。
const metaReadLimit = 16 << 20

// List returns the generations, the storage usage and the state of the backup
// service.
func (s *Service) List(ctx context.Context) (*Overview, error) {
	st := s.o.Storage
	if st == nil {
		return nil, ErrNotConfigured
	}
	objects, err := st.List(ctx, "")
	if err != nil {
		return nil, fmt.Errorf("list storage: %w", err)
	}
	ov := &Overview{StorageType: s.o.StorageType, Generations: []Generation{}}
	byID := map[string]*Generation{}
	for _, o := range objects {
		ov.Usage.TotalBytes += o.Size
		ov.Usage.ObjectCount++
		id := backup.GenerationIDFromKey(o.Key)
		if id == "" {
			continue
		}
		g := byID[id]
		if g == nil {
			g = &Generation{ID: id, Migrations: []backup.MigrationState{}}
			if t, err := backup.IDTime(id); err == nil {
				g.CreatedAt = t
			}
			byID[id] = g
		}
		g.Size += o.Size
		g.ObjectCount++
	}
	for _, g := range byID {
		s.fill(ctx, g)
		ov.Generations = append(ov.Generations, *g)
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

// fill reads meta.json and verify.json of g.
func (s *Service) fill(ctx context.Context, g *Generation) {
	m, err := s.readMeta(ctx, g.ID)
	if err != nil {
		g.MetaError = err.Error()
		return
	}
	g.Complete = true
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
	var vr backup.VerifyResult
	switch err := s.readJSON(ctx, backup.Key(g.ID, backup.VerifyFile), &vr); {
	case errors.Is(err, backup.ErrNotFound):
	case err != nil:
		g.Verify = &VerifySummary{Error: err.Error(), Stages: []backup.StageResult{}, Mismatches: []backup.RowMismatch{}}
	default:
		vs := &VerifySummary{OK: vr.OK, VerifiedAt: vr.VerifiedAt, Stages: vr.Stages, Mismatches: vr.Mismatches}
		if vs.Stages == nil {
			vs.Stages = []backup.StageResult{}
		}
		if vs.Mismatches == nil {
			vs.Mismatches = []backup.RowMismatch{}
		}
		g.Verify = vs
	}
}

// readMeta reads and checks meta.json of generation id.
func (s *Service) readMeta(ctx context.Context, id string) (*backup.Meta, error) {
	var m backup.Meta
	if err := s.readJSON(ctx, backup.Key(id, backup.MetaFile), &m); err != nil {
		if errors.Is(err, backup.ErrNotFound) {
			return nil, fmt.Errorf("%s is missing", backup.MetaFile)
		}
		return nil, err
	}
	// 知らない版のメタ情報は、欄の意味が変わっている可能性があるので読まない
	// (MetaFormatVersion の契約)。
	if m.FormatVersion != backup.MetaFormatVersion {
		return nil, fmt.Errorf("%s has unknown formatVersion %d", backup.MetaFile, m.FormatVersion)
	}
	if m.ID != id {
		return nil, fmt.Errorf("%s has id %q", backup.MetaFile, m.ID)
	}
	if m.DumpFile == "" || strings.Contains(m.DumpFile, "/") || strings.Contains(m.DumpFile, "..") {
		return nil, fmt.Errorf("%s has invalid dumpFile %q", backup.MetaFile, m.DumpFile)
	}
	return &m, nil
}

func (s *Service) readJSON(ctx context.Context, key string, v any) error {
	rc, err := s.o.Storage.Get(ctx, key)
	if err != nil {
		return err
	}
	defer func() { _ = rc.Close() }()
	data, err := io.ReadAll(io.LimitReader(rc, metaReadLimit+1))
	if err != nil {
		return fmt.Errorf("read %s: %w", key, err)
	}
	if len(data) > metaReadLimit {
		return fmt.Errorf("%s is larger than %d bytes", key, metaReadLimit)
	}
	if err := json.Unmarshal(data, v); err != nil {
		return fmt.Errorf("decode %s: %w", key, err)
	}
	return nil
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

// Delete removes every object of generation id. It returns the number of
// bytes freed.
func (s *Service) Delete(ctx context.Context, id string) (int64, error) {
	objects, err := s.generationObjects(ctx, id)
	if err != nil {
		return 0, err
	}
	// **meta.json を最初に消す。** meta.json の無い世代は未完成として扱われる
	// (Meta の契約) ので、途中で失敗しても、一部だけ残った世代が「戻せる世代」に
	// 見えることがない。
	metaKey := backup.Key(id, backup.MetaFile)
	if err := s.o.Storage.Delete(ctx, metaKey); err != nil {
		return 0, fmt.Errorf("delete %s: %w", metaKey, err)
	}
	var freed int64
	for _, o := range objects {
		if o.Key == metaKey {
			freed += o.Size
		}
	}
	for _, o := range objects {
		if o.Key == metaKey {
			continue
		}
		if err := s.o.Storage.Delete(ctx, o.Key); err != nil {
			return freed, fmt.Errorf("delete %s: %w", o.Key, err)
		}
		freed += o.Size
	}
	return freed, nil
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
		FileName:  "elythia-backup-" + id + "-" + m.DumpFile,
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
