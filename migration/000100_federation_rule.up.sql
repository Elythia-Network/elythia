-- federation_rule: 受信した activity / 投稿に適用する連合のルール (#3090)。
-- mk-go 独自テーブル。
--
-- meta の blockedHosts / silencedHosts などはホストを丸ごと扱う設定しか表現
-- できない。ここに「条件と動作の組」を置き、ホスト単位の設定に**追加の層として**
-- 重ねる (置き換えない)。TS へ戻すとこのテーブルは読まれず、ルールだけが効かなく
-- なる (ホスト単位の設定は meta にあるので残る)。
--
-- 条件は指定したものすべてを満たすと当たる (AND)。配列の条件は要素のどれかに
-- 当たればよい (OR)。空配列 / NULL はその条件を問わない。
CREATE TABLE IF NOT EXISTS "federation_rule" (
    "id"             varchar(32) PRIMARY KEY,
    "createdAt"      timestamp with time zone NOT NULL,
    "updatedAt"      timestamp with time zone NOT NULL,
    "name"           varchar(128) NOT NULL DEFAULT '',
    -- disabled / record (当たった記録だけ残す) / enforce (効かせる)
    "mode"           varchar(16) NOT NULL DEFAULT 'record',
    -- note (投稿の中身まで見る) / activity (受信した activity を種別で見る)
    "target"         varchar(16) NOT NULL DEFAULT 'note',
    "position"       integer NOT NULL DEFAULT 0,
    "hosts"          varchar(128)[] NOT NULL DEFAULT '{}',
    "activityTypes"  varchar(64)[] NOT NULL DEFAULT '{}',
    "isBot"          boolean,
    "newWithinHours" integer,
    "patterns"       varchar(1024)[] NOT NULL DEFAULT '{}',
    "hasAttachment"  boolean,
    "tags"           varchar(128)[] NOT NULL DEFAULT '{}',
    "reject"         boolean NOT NULL DEFAULT false,
    "stripMedia"     boolean NOT NULL DEFAULT false,
    "sensitive"      boolean NOT NULL DEFAULT false,
    "unlist"         boolean NOT NULL DEFAULT false,
    "cw"             varchar(512)
);
