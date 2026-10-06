-- 000116 の逆。新しい既定値と同じ行を、以前の既定値に戻す。
--
-- **up の後に operator が新しい URL を自分で入れた行も戻る** (up 以前からの値か
-- 区別できないため)。旧 URL は GitHub が転送するので、戻っても開ける。
UPDATE "meta"
SET "repositoryUrl" = 'https://github.com/shiroha-a/mk'
WHERE "repositoryUrl" = 'https://github.com/Elythia-Network/elythia';

UPDATE "meta"
SET "feedbackUrl" = 'https://github.com/shiroha-a/mk/issues/new'
WHERE "feedbackUrl" = 'https://github.com/Elythia-Network/elythia/issues/new';
