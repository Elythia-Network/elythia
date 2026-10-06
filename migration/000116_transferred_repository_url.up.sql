-- リポジトリを Elythia-Network/elythia へ移した (#3394) ので、meta.repositoryUrl /
-- meta.feedbackUrl の既定値を新しい URL にする。
--
-- 書き換えるのは、000084 / 000085 か起動時の EnsureInitial が入れた以前の既定値と同じ
-- 行だけ。operator が
-- 自分の改変版を指している行には触らない。旧 URL は GitHub が転送するので放っても
-- 開けるが、/about-elythia は repositoryUrl が本体の URL と違うと「このサーバーの
-- ソース」と「本体のソース」を別々に並べるため、同じリポジトリが 2 行出てしまう。
UPDATE "meta"
SET "repositoryUrl" = 'https://github.com/Elythia-Network/elythia'
WHERE "repositoryUrl" = 'https://github.com/shiroha-a/mk';

UPDATE "meta"
SET "feedbackUrl" = 'https://github.com/Elythia-Network/elythia/issues/new'
WHERE "feedbackUrl" = 'https://github.com/shiroha-a/mk/issues/new';
