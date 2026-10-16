-- drop-in (#2246): TS 製 DB で二重化した index のうち、upstream と定義が
-- 同一のものを落とす。
--
-- mk-go は index を `IDX_<table>_<col>` で命名するが upstream (TypeORM) は
-- `IDX_e5848eac4940934e23dbc17581` のような hash 名を生成する。
-- `CREATE INDEX IF NOT EXISTS` は **index 名**で存在判定するため、定義が同一
-- でも名前が違えば新規作成される。結果、docs/migration-from-ts.md の手順で
-- TS 製 DB に migration を流すと共有テーブルの index が原則すべて二重化する。
--
-- 実測 (Misskey TS が作った DB に mk-go の全 migration を適用):
--   適用前 407 本 → 適用後 617 本 (mk-go が 210 本作成)
--   うち定義が完全一致する重複が 147 本
--
-- note は最大テーブルなので、GIN index の二重化は INSERT / UPDATE のたびに
-- 2 本分の更新コストがかかる。読み取り性能には影響しないが書き込み
-- スループットと容量に効く。
--
-- 方針は 000058 (upstream の hash 名をそのまま使って二重化を避けた前例) の
-- 横展開だが、既存 migration の index 名を書き換えると適用済み DB との整合が
-- 取れなくなるので、**作成後に冗長なものを落とす**形にした。
--
-- 落とすのは mk-go の migration が作る index だけ。upstream 由来の index は
-- 絶対に触らない (TS へ戻したとき本家が再作成できず復路が壊れるため)。
-- mk-go 由来 DB では同一定義の別名 index が存在しないので何も落ちない。
--
-- 注: TS 製 DB では「作ってから落とす」ことになるため、巨大インスタンスでは
-- migration 中に一時的な index 構築コストがかかる。正しさには影響しない。
-- DROP INDEX は短時間 ACCESS EXCLUSIVE を取るが、drop-in 切替は backend 停止中に
-- 行う手順なので実運用上の待ちは発生しない。

DO $$
DECLARE
    mk_index_names text[] := ARRAY[
        'IDX_6dd314e96806b7df65ddadff72',
        'IDX___chart__active_users_date',
        'IDX___chart__ap_request_date',
        'IDX___chart__drive_date',
        'IDX___chart__federation_date',
        'IDX___chart__instance_date_group',
        'IDX___chart__notes_date',
        'IDX___chart__per_user_drive_date_group',
        'IDX___chart__per_user_following_date_group',
        'IDX___chart__per_user_notes_date_group',
        'IDX___chart__per_user_pv_date_group',
        'IDX___chart__per_user_reaction_date_group',
        'IDX___chart__users_date',
        'IDX___chart_day__active_users_date',
        'IDX___chart_day__ap_request_date',
        'IDX___chart_day__drive_date',
        'IDX___chart_day__federation_date',
        'IDX___chart_day__instance_date_group',
        'IDX___chart_day__notes_date',
        'IDX___chart_day__per_user_drive_date_group',
        'IDX___chart_day__per_user_following_date_group',
        'IDX___chart_day__per_user_notes_date_group',
        'IDX___chart_day__per_user_pv_date_group',
        'IDX___chart_day__per_user_reaction_date_group',
        'IDX___chart_day__users_date',
        'IDX_abuse_user_report_reporterId',
        'IDX_abuse_user_report_resolved',
        'IDX_abuse_user_report_targetUserId',
        'IDX_access_token_hash',
        'IDX_access_token_token',
        'IDX_access_token_userId',
        'IDX_announcement_isActive',
        'IDX_announcement_read_announcementId',
        'IDX_announcement_read_pair',
        'IDX_announcement_read_userId',
        'IDX_antenna_isActive',
        'IDX_antenna_lastUsedAt',
        'IDX_antenna_note_unread_userId',
        'IDX_antenna_userId',
        'IDX_app_secret',
        'IDX_app_userId',
        'IDX_auth_session_token',
        'IDX_blocking_blockeeId',
        'IDX_blocking_blockerId',
        'IDX_blocking_blockerId_blockeeId',
        'IDX_bubble_game_record_score',
        'IDX_bubble_game_record_seededAt',
        'IDX_bubble_game_record_userId',
        'IDX_channel_favorite_userId',
        'IDX_channel_following_followeeId',
        'IDX_channel_following_followerId',
        'IDX_channel_following_pair',
        'IDX_channel_isArchived',
        'IDX_channel_lastNotedAt',
        'IDX_channel_muting_userId',
        'IDX_channel_note_unread_userId',
        'IDX_channel_notesCount',
        'IDX_channel_userId',
        'IDX_channel_usersCount',
        'IDX_chat_approval_otherId',
        'IDX_chat_approval_userId',
        'IDX_chat_approval_userId_otherId',
        'IDX_chat_message_fromUserId',
        'IDX_chat_message_fromUserId_toUserId',
        'IDX_chat_message_toRoomId',
        'IDX_chat_message_toUserId',
        'IDX_chat_room_invitation_roomId',
        'IDX_chat_room_invitation_userId',
        'IDX_chat_room_membership_roomId',
        'IDX_chat_room_membership_userId',
        'IDX_chat_room_ownerId',
        'IDX_clip_favorite_clipId',
        'IDX_clip_favorite_userId',
        'IDX_clip_lastClippedAt',
        'IDX_clip_note_clipId',
        'IDX_clip_note_noteId',
        'IDX_clip_note_pair',
        'IDX_clip_userId',
        'IDX_drive_file_accessKey',
        'IDX_drive_file_thumbnailAccessKey',
        'IDX_drive_file_thumbnailUrl',
        'IDX_drive_file_uri',
        'IDX_drive_file_url',
        'IDX_drive_file_userId',
        'IDX_drive_file_webpublicAccessKey',
        'IDX_drive_file_webpublicUrl',
        'IDX_emoji_host',
        'IDX_emoji_name',
        'IDX_emoji_name_host',
        'IDX_flash_like_flashId',
        'IDX_flash_like_pair',
        'IDX_flash_like_userId',
        'IDX_flash_likedCount',
        'IDX_flash_updatedAt',
        'IDX_flash_userId',
        'IDX_follow_request_followeeId',
        'IDX_follow_request_followerId',
        'IDX_follow_request_followerId_followeeId',
        'IDX_following_followeeId',
        'IDX_following_followerId',
        'IDX_following_followerId_followeeId',
        'IDX_gallery_like_pair',
        'IDX_gallery_post_tags',
        'IDX_gallery_post_userId',
        'IDX_hashtag_name',
        'IDX_instance_host',
        'IDX_moderation_log_type',
        'IDX_moderation_log_userId',
        'IDX_muting_expiresAt',
        'IDX_muting_muteeId',
        'IDX_muting_muterId',
        'IDX_muting_muterId_muteeId',
        'IDX_note_draft_userId',
        'IDX_note_favorite_createdAt',
        'IDX_note_favorite_noteId',
        'IDX_note_favorite_pair',
        'IDX_note_favorite_userId',
        'IDX_note_fileIds',
        'IDX_note_mentions',
        'IDX_note_reaction_noteId',
        'IDX_note_reaction_userId',
        'IDX_note_reaction_userId_noteId',
        'IDX_note_tags',
        'IDX_note_thread_muting_userId',
        'IDX_note_unread_userId',
        'IDX_note_unread_userId_isMentioned',
        'IDX_note_unread_userId_isSpecified',
        'IDX_note_uri',
        'IDX_note_userHost',
        'IDX_note_userId',
        'IDX_note_visibility',
        'IDX_page_like_pageId',
        'IDX_page_like_pair',
        'IDX_page_like_userId',
        'IDX_page_name',
        'IDX_page_updatedAt',
        'IDX_page_userId',
        'IDX_page_userId_name',
        'IDX_password_reset_request_token',
        'IDX_password_reset_request_userId',
        'IDX_poll_expired_unnotified',
        'IDX_poll_vote_noteId',
        'IDX_poll_vote_userId',
        'IDX_poll_vote_userId_noteId_choice',
        'IDX_promo_note_userId',
        'IDX_promo_read_userId',
        'IDX_promo_read_userId_noteId',
        'IDX_registry_item_domain',
        'IDX_registry_item_scope',
        'IDX_registry_item_userId',
        'IDX_renote_muting_muteeId',
        'IDX_renote_muting_muterId',
        'IDX_renote_muting_muterId_muteeId',
        'IDX_retention_aggregation_createdAt',
        'IDX_reversi_game_user1Id',
        'IDX_reversi_game_user2Id',
        'IDX_role_assignment_expiresAt',
        'IDX_role_assignment_roleId',
        'IDX_role_assignment_userId',
        'IDX_role_assignment_userId_roleId',
        'IDX_role_isPublic',
        'IDX_role_target',
        'IDX_signin_userId',
        'IDX_sw_subscription_userId',
        'IDX_system_account_userId',
        'IDX_user_host',
        'IDX_user_ip_userId',
        'IDX_user_ip_userId_ip',
        'IDX_user_list_favorite_userId',
        'IDX_user_list_membership_pair',
        'IDX_user_list_membership_userId',
        'IDX_user_list_membership_userListId',
        'IDX_user_list_userId',
        'IDX_user_memo_targetUserId',
        'IDX_user_memo_userId',
        'IDX_user_memo_userId_targetUserId',
        'IDX_user_note_pining_noteId',
        'IDX_user_note_pining_userId',
        'IDX_user_note_pining_userId_noteId',
        'IDX_user_publickey_extra_keyId',
        'IDX_user_publickey_keyId',
        'IDX_user_security_key_publicKey',
        'IDX_user_security_key_userId',
        'IDX_user_tags',
        'IDX_user_token',
        'IDX_user_usernameLower',
        'IDX_user_usernameLower_host_unique',
        'IDX_user_usernameLower_local_unique',
        'IDX_webhook_active',
        'IDX_webhook_userId',
        'UQ_antenna_note_unread_triple',
        'UQ_channel_note_unread_triple',
        'UQ_note_unread_userId_noteId'
    ];
    -- upstream の full index が同じ役割を果たすと個別に判断した partial index。
    -- 構造的な規則 (WHERE を外すと一致する) だけで落とすと、
    -- IDX_note_unread_userId_isMentioned のような **意図的に絞った** partial まで
    -- 巻き込むので、明示列挙にしている。
    partial_subset_names text[] := ARRAY[
        'IDX_note_uri',
        'IDX_drive_file_uri',
        'IDX_user_usernameLower_host_unique'
    ];
    -- public の index の表名・名前・定義。先に配列へ読み切ってから比べる (#3369)。
    idx_tables text[];
    idx_names text[];
    idx_defs text[];
    dup record;
BEGIN
    -- 定義は SELECT の側 (集約の入力) でだけ読む。以前は pg_indexes を 2 回引き、
    -- EXISTS の中の WHERE で定義を比べていた。その条件は 2 つの index の定義を
    -- 参照する join の条件なので、#3365 (1 つの index の scan まで押し下げられる形)
    -- ほど直接ではないが、join の順によっては schema で絞る前の index に対して
    -- pg_get_indexdef が評価されうる (理論上の危険)。並行する DDL (テストでは
    -- 他のパッケージの schema に流れる migration) がその index を消していると
    -- `could not open relation with OID` で落ちる。集約の入力は
    -- 絞り込みの後に評価されるので、ここで読むのは public の index だけになる。
    -- 比べるのは配列に移した値なので、plan がどうなっても catalog は読み直さない。
    -- 落とす対象を決める規則 (A) / (B) は変えていない。
    SELECT array_agg(tablename ORDER BY indexname),
           array_agg(indexname ORDER BY indexname),
           array_agg(indexdef ORDER BY indexname)
      INTO idx_tables, idx_names, idx_defs
      FROM pg_indexes
     WHERE schemaname = 'public';

    FOR dup IN
        WITH idx AS (
            SELECT t.tablename, t.indexname, t.def
            FROM unnest(idx_tables, idx_names, idx_defs) AS t(tablename, indexname, def)
        )
        SELECT mine.indexname
        FROM idx mine
        WHERE mine.indexname = ANY(mk_index_names)
          AND EXISTS (
              SELECT 1
              FROM idx other
              WHERE other.tablename = mine.tablename
                AND other.indexname <> mine.indexname
                -- 生存させるのは upstream 由来の index のみ。mk-go 同士を
                -- 突き合わせて片方を落とすことは無い。
                AND NOT (other.indexname = ANY(mk_index_names))
                AND (
                    -- (A) 定義が完全一致 (index 名だけを除いて比較)
                    regexp_replace(other.def, '^CREATE (UNIQUE )?INDEX \S+ ON ', '\1|')
                      = regexp_replace(mine.def, '^CREATE (UNIQUE )?INDEX \S+ ON ', '\1|')
                    OR
                    -- (B) mk-go 側が partial で、WHERE を外すと upstream の
                    --     full index と一致する (明示列挙したものだけ)
                    (
                        mine.indexname = ANY(partial_subset_names)
                        AND other.def !~ ' WHERE '
                        AND regexp_replace(other.def, '^CREATE (UNIQUE )?INDEX \S+ ON ', '\1|')
                          = regexp_replace(
                                regexp_replace(mine.def, ' WHERE .*$', ''),
                                '^CREATE (UNIQUE )?INDEX \S+ ON ', '\1|')
                    )
                )
          )
    LOOP
        RAISE NOTICE 'dropping redundant index %', dup.indexname;
        EXECUTE format('DROP INDEX IF EXISTS public.%I', dup.indexname);
    END LOOP;
END $$;
