# アカウント削除ポリシー

Related #3207

## 対象

`canDeleteAccount` と `canPurgeAccount` は、利用者本人が
`i/delete-account` を実行する場合だけに適用する。管理者による削除、リモート
actor の削除、ノート・ドライブの個別削除 API の挙動は変更しない。

- `canDeleteAccount`: 本人退会を許可する。明示的な `bool true` の場合だけ許可し、
  `false`、欠落、bool 以外は 403 にする。管理者にも bypass はない。
- `canPurgeAccount`: cleanup 後に `user` と `user_profile` を物理削除してよいかを
  決める。明示的な `bool true` だけが物理削除を許可する。`false`、欠落、bool
  以外は行を保持する。

両ポリシーの既定値は `true` で、未設定の instance は従来の本人退会・物理削除
動作を維持する。

## 解決と enqueue

本人退会では checked resolver を資格情報や 2FA に触る前に一度だけ呼び、同じ
結果から二つのポリシーを読む。resolver、`meta.policies` の取得、または JSON
decode に失敗した場合は 500 とし、削除フラグ、2FA token、token/WebSocket
失効、ActivityPub Delete、queue enqueue の副作用を発生させない。

`canPurgeAccount` の判断は enqueue 時点で `preserveAccount` に固定する。worker は
role/base policy を再解決しない。これにより enqueue 後の role 変更で同じ job の
結果が変わらない。

本人退会の checked resolver は、`canDeleteAccount` / `canPurgeAccount` を宣言する
plugin provider だけを呼ぶ。これらに影響できない provider の失敗で退会を拒否しない。
対象 provider の失敗、`meta.policies` の取得失敗、JSON decode 失敗は 500 にする。

unchecked resolver の既存 consumer は error を捨てる従来の fallback を維持する。
base policy を読めない場合は default と native role override から計算し、従来どおり
plugin provider も呼ぶ。checked consumer だけが meta の失敗を fail-closed に扱う。

## worker の動作

`preserveAccount` は既存の `soft` とは独立している。`soft=false` かつ
`preserveAccount=true` の場合、worker はコンテンツの cleanup より先に資格情報を
削除する。対象は `access_token`、`auth_session`、`user_security_key`（パスキーを含む）、
`password_reset_request`、`sw_subscription`、secret を含む `webhook` の行と、
`user.token`、`user_profile` の password・メール確認コード・TOTP secret・一時 secret・
backup code である。2FA・パスキー・passwordless login の有効フラグも解除する。
削除済みローカルユーザーだけを対象に、トランザクション内で実行する。
失敗や未配線時にはジョブを再試行し、コンテンツの cleanup に進まない。
再試行は冪等で、他ユーザーの資格情報や remote soft delete は変更しない。

続いて従来どおり、次の cleanup をすべて実行する。

1. ノート
2. ページ（`PageRepository.Delete` を通し、参照ノートの `pageCount` を減らす
   #3293 の処理を維持）
3. ドライブファイル
4. follow/follower 行

その後、`soft=false` かつ `preserveAccount=false` の場合だけ `user` 行を物理削除
する。`preserveAccount=true` では `user` と `user_profile` を保持する。

この保持は匿名化ではない。username、プロフィール、その他 `user` /
`user_profile` に残る識別情報はそのまま保持される。さらに、最後の `user` 削除に
伴う FK cascade が起きないため、上記4種類以外の user 子行も保持される。現行 schema
での主な例は次のとおり。

- security / audit: `user_keypair*`、`user_publickey*`、`signin` の履歴
- integration: user 所有の `app`
- content / relationship: `flash`、`gallery_post`、`clip`、`antenna`、`note_draft`、
  chat room/message/membership/invitation、reversi/bubble-game record、`registry_item`、
  `user_list`、role assignment、block/mute など

上記の保持対象は匿名化・削除しない。保持した local user は
admin unsuspend で復活できないが、moderator は管理目的の `users/show` で参照できる。
非 moderator の `users/show` と ActivityPub actor / WebFinger では local user を隠す一方、
remote user の表示と ActivityPub redirect は upstream 互換の挙動を維持する。gallery / flash /
chat などの retained content は残し、この差分では公開停止を追加しない。
webhook / push 登録は資格情報の cleanup で削除するが、既に送信中の処理や別キューの
payload を取り消すものではない。worker の cleanup が完了するまでは登録が残りうる。
push 登録を消したときは、Web Push の配送が読む購読キャッシュ (Redis の
`userSwSubscriptions:{userId}` と、削除を実行したプロセスのメモリ上の層) も破棄
する。物理削除で `sw_subscription` が FK cascade で消える場合も同じく破棄する。
他のプロセスのメモリ上の層は、TTL (3 分) が切れるまで残りうる。
cleanup 済みコンテンツを復元できる機能でもない。また、物理削除する場合も再登録防止用の
`used_username` は削除しない。

## queue 互換性と rollout

`preserveAccount` は JSON payload で `omitempty` とする。field のない旧 payload は
Go の zero value (`false`) に decode され、従来どおり物理削除候補になる。

新 producer より先に、新 field を理解する worker を全台へ rollout すること。ここで
「旧 worker」は古い mk-go binary の delete-account queue consumer を指す。TS 側の
job は payload / worker が別であり、この互換説明の対象ではない。旧 mk-go worker は
未知の `preserveAccount` を無視して `soft=false` の job を物理削除するため、producer
を先行させると保持を要求したアカウントを削除する危険がある。
