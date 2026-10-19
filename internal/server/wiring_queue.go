package server

import (
	"time"

	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/queue/processors"
	"github.com/elythia-network/elythia/internal/repository"
)

// queueProcs holds the queue processors setupRoutes builds through buildQueue
// (#3690).
//
// Fields are named after the local variables setupRoutes has always used and
// keep the concrete type the constructor returns. Only processors whose
// constructor just builds a value are listed: no DB or Redis access, no
// outbound call or log, no goroutine, timer, shutdown hook or global write,
// and dependencies limited to the repositories, the core services, server
// configuration, the DB handle and the ID generator. Processors that need the
// outbound HTTP client (Web Push, webhooks), depend on a part setupRoutes
// still builds itself (the exporter, the emoji importer, the AP client and
// inbox processor, the reaction count writer, the chart engines, the metadata
// fetcher, the moderator activity service, the quote outbox), stay in
// setupRoutes (docs/architecture.md 4.2).
//
// buildQueue only constructs. Every setter on these processors, and every
// handler registration, is still done at its old position in setupRoutes.
type queueProcs struct {
	importProcessor              *processors.ImportProcessor
	cleanProcessor               *processors.CleanRemoteNotesProcessor
	orphanUserProcessor          *processors.OrphanUserCleanupProcessor
	orphanAttachmentProcessor    *processors.OrphanAttachmentCleanupProcessor
	checkExpiredMutingsProcessor *processors.CheckExpiredMutingsProcessor
	cleanGenericProcessor        *processors.CleanProcessor
	chunkedUploadGCProcessor     *processors.ChunkedUploadGCProcessor
	objectStorageProcessor       *processors.ObjectStorageProcessor
	deleteAccountProcessor       *processors.DeleteAccountProcessor
	unfollowProcessor            *processors.UnfollowProcessor
	followProcessor              *processors.FollowProcessor
	blockProcessor               *processors.BlockProcessor
	unblockProcessor             *processors.UnblockProcessor
	retentionProc                *processors.RetentionAggregateProcessor
	postScheduledNoteProcessor   *processors.PostScheduledNoteProcessor
}

// buildQueue builds the queue processors from w.repos and coreSvcs, in the
// order setupRoutes used to build them. It reads s.config and s.db. coreSvcs
// is the value buildCore returned; it is passed in rather than read back from
// w.core so that calling buildQueue before buildCore does not compile
// (docs/architecture.md 4.2).
func (s *Server) buildQueue(w *wiring, coreSvcs core, idGen id.Generator) queueProcs {
	// 下の行は setupRoutes にあった行をそのまま移した。ゲートが構築の式を引数まで
	// 含めて照合しうるので、repository と core も setupRoutes と同じ名前の別名で受ける。
	userRepo := w.repos.user
	noteRepo := w.repos.note
	var metaRepo repository.MetaRepository = w.repos.cachedMeta
	followingRepo := w.repos.following
	mutingRepo := w.repos.muting
	driveFileRepo := w.repos.driveFile
	var antennaRepo repository.AntennaRepository = w.repos.cachedAntenna
	roleAssignmentRepo := w.repos.roleAssignment
	reversiRepo := w.repos.reversi
	userIPRepo := w.repos.userIP
	channelMutingRepo := w.repos.channelMuting
	noteDraftRepo := w.repos.noteDraft
	importer := coreSvcs.importer
	ephemeralStore := coreSvcs.ephemeralStore
	driveService := coreSvcs.drive
	driveStorage := coreSvcs.driveStorage
	localDriveStorage := coreSvcs.localDriveStorage
	followingService := coreSvcs.following
	blockingService := coreSvcs.blocking
	retentionSvc := coreSvcs.retention
	noteCreateService := coreSvcs.noteCreate

	importProcessor := processors.NewImportProcessor(importer)

	// Remote notes cleaning (issue #46): enqueue 契機は scheduler の cron
	// (RegisterCleanRemoteNotesJob, 毎日 04:00, TS 'cleanRemoteNotes' 相当,
	// #1563)。旧実装は 6h time.Ticker だった。Handle は常に登録し、processor が
	// cfg.Enabled で gate する (起動時 meta から構築)。
	// **毎回読む。** 起動時に固定すると、運営者が管理画面で止めてもプロセスを
	// 再起動するまで削除が走り続ける (不可逆な操作の唯一の停止手段)。
	cleanCfgFn := func() processors.CleanRemoteNotesConfig {
		cleanMeta, err := metaRepo.Fetch()
		if err != nil || cleanMeta == nil {
			// **読めなければ動かさない。** 判定材料が無いまま不可逆な削除を
			// 続けるより、次の cron まで待つほうが安全。
			return processors.CleanRemoteNotesConfig{}
		}
		return processors.CleanRemoteNotesConfig{
			Enabled:              cleanMeta.EnableRemoteNotesCleaning,
			ExpiryDays:           cleanMeta.RemoteNotesCleaningExpiryDaysForEachNotes,
			MaxProcessingMinutes: cleanMeta.RemoteNotesCleaningMaxProcessingDurationInMinutes,
		}
	}
	cleanProcessor := processors.NewCleanRemoteNotesProcessor(noteRepo, cleanCfgFn)

	// リレー由来の孤児リモートユーザーの掃除 (#2340)。転送活動の LD-Signature
	// 検証は著者の公開鍵を DB に載せる必要があるため、その経路だけは揮発化
	// できない。後追いで回収する。対象は relay_observed_user に載っている行だけ。
	orphanUserProcessor := processors.NewOrphanUserCleanupProcessor(userRepo,
		func() processors.OrphanUserCleanerConfig {
			m, err := metaRepo.Fetch()
			if err != nil || m == nil {
				return processors.OrphanUserCleanerConfig{}
			}
			return processors.OrphanUserCleanerConfig{
				Enabled:   m.EnableRelayOrphanUserCleanup,
				GraceDays: m.RelayOrphanUserGraceDays,
			}
		})

	// owner 無しのリモート添付の掃除 (#2722)。著者が materialize されていない
	// 添付は owner 無しで保存され (#2717)、ephemeral note が Redis の TTL で
	// 消えても drive_file の行は残り続ける。orphanUserCleanup が親 user を
	// 消して SET NULL になった行も同じ形。
	//
	// **gate は 2 つの meta の OR。** どちらの機能もこの形の行を作るので、
	// 片方だけ有効な構成で掃除が回らないと元の問題がそのまま残る。
	// 猶予と cutoff の組み立ては processor 側 (ID 生成方式は設定依存なので
	// generator をそのまま渡す)。
	//
	// cutoff 用の generator は**専用に作る**。`ulid` の monotonic reader は
	// 過去時刻で呼ぶと内部の基準時刻を巻き戻すので、採番と共有すると直後の
	// 単調性が 1 回崩れる。未知の方式なら主 idGen (setupRoutes の先頭) と同じく
	// aidx へ倒す — **採番と cutoff で別の方式に分かれるほうが危険**で、その場合は
	// 「常に対象外」か「年齢を無視して対象」のどちらかに倒れる。
	cutoffGen, cutoffErr := id.NewGenerator(s.config.ID)
	if cutoffErr != nil {
		cutoffGen, _ = id.NewGenerator("aidx")
	}
	orphanAttachmentProcessor := processors.NewOrphanAttachmentCleanupProcessor(driveFileRepo, ephemeralStore,
		func() processors.OrphanAttachmentCleanerConfig {
			m, err := metaRepo.Fetch()
			if err != nil || m == nil {
				return processors.OrphanAttachmentCleanerConfig{}
			}
			return processors.OrphanAttachmentCleanerConfig{
				Enabled:      m.EnableEphemeralRelayNotes || m.EnableRelayOrphanUserCleanup,
				EphemeralTTL: time.Duration(m.EphemeralRelayNoteTTLMinutes) * time.Minute,
			}
		}, cutoffGen.Generate)

	// Expired-mute prune (#1563 / #1603): scheduler の cron (*/5) が enqueue する。
	// user mute と channel mute の両方の期限切れ行を prune する。
	checkExpiredMutingsProcessor := processors.NewCheckExpiredMutingsProcessor(mutingRepo, channelMutingRepo)

	// Daily generic clean (#1563): scheduler の cron (0 0 * * *) が enqueue する。
	// user_ip 90 日 prune / 期限切れ role_assignment 削除 / reversi outdated game 削除 /
	// 未使用 antenna の deactivate (#1604, deactivateAntennaThreshold ミリ秒)。
	// 最後の引数は、buildRepos の userPendingRepo と同じ種類の repository をここで
	// 作っている (#3684 でまとめなかった)。ゲート (TestCleanProcessorReceivesThePendingPruner)
	// が、nil を渡せないよう構築の式そのものを照合しているため。db しか持たないので、
	// 2 つあっても動きは同じ。
	cleanGenericProcessor := processors.NewCleanProcessor(userIPRepo, roleAssignmentRepo, reversiRepo, idGen, antennaRepo, time.Duration(s.config.DeactivateAntennaThreshold)*time.Millisecond, repository.NewUserPendingRepository(s.db))

	// 分割アップロードセッションの GC (#2313): scheduler の cron (*/15) が
	// enqueue する。期限切れセッションの multipart upload を abort してから
	// 行を消す。日次 clean に相乗りさせないのは、未完了マルチパートアップロード
	// が課金対象で、TTL 60 分に対し 24 時間ぶん残るのを避けるため。
	chunkedUploadGCProcessor := processors.NewChunkedUploadGCProcessor(driveService)

	// object storage の実体削除 (#2325)。upstream Misskey と同じく専用 queue に
	// 逃がすことで、大量削除で API リクエストがブロックされるのと、storage 不調で
	// 実体が orphan になるのを防ぐ。cleanRemoteFiles は job 1 本の中でバッチ削除を
	// 回す (件数ぶん job を積むと Redis を圧迫するため)。
	objectStorageProcessor := processors.NewObjectStorageProcessor(driveStorage, localDriveStorage, driveFileRepo)

	// Account cascade deletion (issue #187): admin/accounts/delete の後続
	// バックグラウンド処理。note / drive_file / following 関連行を掃除する。
	deleteAccountProcessor := processors.NewDeleteAccountProcessor(noteRepo, driveFileRepo, followingRepo)

	// Per-pair Unfollow job (#587): admin/federation/remove-all-following
	// が enqueue するペアごとに Service.Unfollow を呼んで row 削除 + Reject
	// 配送を行う。Misskey TS の relationshipQueue 'unfollow' job 相当。
	unfollowProcessor := processors.NewUnfollowProcessor(followingService)

	// Per-pair Follow / Block / Unblock job (#1605): 本家
	// RelationshipProcessorService の processFollow / processBlock /
	// processUnblock 相当。大量関係操作 (将来の AccountMove フォロワー移行
	// 等) の非同期バックプレッシャ surface。import は完了通知の
	// Applied/Skipped 集計を保つため同期適用を維持する (core/transfer)。
	followProcessor := processors.NewFollowProcessor(followingService)
	blockProcessor := processors.NewBlockProcessor(blockingService)
	unblockProcessor := processors.NewUnblockProcessor(blockingService)

	// Daily retention aggregation (#421)。
	// 本家 AggregateRetentionProcessorService と同等で、retention_aggregation
	// テーブルに 1 日 1 行 cohort を追加し、過去 31 日分の data フィールドを
	// 更新する。/api/retention と admin overview の「定着率」heatmap が読む。
	// 起動時に 1 回だけ流す集計は、setupRoutes の元の位置のまま。
	retentionProc := processors.NewRetentionAggregateProcessor(retentionSvc)

	// PostScheduledNote processor (#1040): delayed queue 経由で draft を
	// note 化する。dependencies は既存 noteCreateService + userRepo +
	// noteDraftRepo を共有する。lock と notifier の setter は setupRoutes の元の位置。
	postScheduledNoteProcessor := processors.NewPostScheduledNoteProcessor(noteDraftRepo, userRepo, noteCreateService)

	return queueProcs{
		importProcessor:              importProcessor,
		cleanProcessor:               cleanProcessor,
		orphanUserProcessor:          orphanUserProcessor,
		orphanAttachmentProcessor:    orphanAttachmentProcessor,
		checkExpiredMutingsProcessor: checkExpiredMutingsProcessor,
		cleanGenericProcessor:        cleanGenericProcessor,
		chunkedUploadGCProcessor:     chunkedUploadGCProcessor,
		objectStorageProcessor:       objectStorageProcessor,
		deleteAccountProcessor:       deleteAccountProcessor,
		unfollowProcessor:            unfollowProcessor,
		followProcessor:              followProcessor,
		blockProcessor:               blockProcessor,
		unblockProcessor:             unblockProcessor,
		retentionProc:                retentionProc,
		postScheduledNoteProcessor:   postScheduledNoteProcessor,
	}
}
