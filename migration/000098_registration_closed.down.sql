-- data loss: 「受け付けない」の設定が失われる (列ごと消えるので既定の false 相当に戻る)。
-- 有効にした更新で disableRegistration も true にしてあるので、承認制でなければ戻した後は
-- 招待制になる。**閉じたときに承認制だった場合は、承認制と招待制が重なって残る** —
-- 承認制の入口は招待制で、/api/signup は承認制で塞がるので、どこからも登録できない
-- (開く側には倒れない)。受け付け方は管理画面で選び直すこと。
ALTER TABLE "meta" DROP COLUMN IF EXISTS "registrationClosed";
