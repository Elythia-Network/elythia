package transfer

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/shiroha-a/mk/internal/model"
	"gorm.io/datatypes"
)

// antennaImportEntry is the JSON shape produced by exportAntennas and
// accepted here on import. Only fields that map directly to model.Antenna
// are respected; unknown fields are ignored.
type antennaImportEntry struct {
	Name            string          `json:"name"`
	Src             string          `json:"src"`
	Keywords        json.RawMessage `json:"keywords"`
	ExcludeKeywords json.RawMessage `json:"excludeKeywords"`
	Users           []string        `json:"users"`
	// userListId は読まない (import は常に null で作る)。ImportAntennas の
	// コメント参照。
	// UserListAccts は upstream ExportedAntenna の list source 用 acct 配列。import 側で
	// list→users 変換に使う (#2106 N24)。
	UserListAccts                  []string `json:"userListAccts"`
	CaseSensitive                  bool     `json:"caseSensitive"`
	LocalOnly                      bool     `json:"localOnly"`
	ExcludeBots                    bool     `json:"excludeBots"`
	WithReplies                    bool     `json:"withReplies"`
	WithFile                       bool     `json:"withFile"`
	ExcludeNotesInSensitiveChannel bool     `json:"excludeNotesInSensitiveChannel"`
}

// importAntennas decodes a JSON array of antenna entries and creates one
// antenna row per entry. 無効なエントリはスキップしてカウントする。
func (i *Importer) importAntennas(user *model.User, body []byte) (*ImportResult, error) {
	var entries []antennaImportEntry
	if err := json.Unmarshal(body, &entries); err != nil {
		return nil, fmt.Errorf("parse antennas json: %w", err)
	}
	res := &ImportResult{Total: len(entries)}
	for _, ent := range entries {
		if ent.Name == "" || ent.Src == "" {
			res.Skipped++
			continue
		}
		keywords := normalizeJSON(ent.Keywords, "[]")
		excludeKeywords := normalizeJSON(ent.ExcludeKeywords, "[]")

		// #2106 N24: upstream ImportAntennasProcessorService 互換 — list source antenna は
		// userListAccts を users source へ倒して取り込む (cross-instance では userListId が
		// 無意味なため)。userListAccts が null / 欠落なら src は維持する。upstream は
		// `antenna.userListAccts` の truthy で判定するので、空配列 [] も users へ倒す
		// (encoding/json は [] を非 nil の空 slice にするので != nil がそれに当たる)。
		//
		// userListId は upstream と同じく**常に null** で作る。ファイルの値は利用者が
		// 自由に書けるので、そのまま保存すると他人の list ID を検証なしでアンテナの
		// ソースにできる (antennas/create / update の所有者検証を迂回する)。
		src := model.AntennaSource(ent.Src)
		users := model.StringArray(ent.Users)
		if src == model.AntennaSourceList && ent.UserListAccts != nil {
			src = model.AntennaSourceUsers
			users = model.StringArray(ent.UserListAccts)
		}
		if users == nil {
			users = model.StringArray{}
		}

		antenna := &model.Antenna{
			ID:                             i.deps.IDGen.Generate(time.Now()),
			UserID:                         user.ID,
			Name:                           ent.Name,
			Src:                            src,
			UserListID:                     nil,
			Users:                          users,
			Keywords:                       datatypes.JSON(keywords),
			ExcludeKeywords:                datatypes.JSON(excludeKeywords),
			CaseSensitive:                  ent.CaseSensitive,
			LocalOnly:                      ent.LocalOnly,
			ExcludeBots:                    ent.ExcludeBots,
			WithReplies:                    ent.WithReplies,
			WithFile:                       ent.WithFile,
			ExcludeNotesInSensitiveChannel: ent.ExcludeNotesInSensitiveChannel,
			IsActive:                       true,
			LastUsedAt:                     time.Now(),
		}
		if err := i.deps.AntennaRepo.Create(antenna); err != nil {
			res.Skipped++
			slog.Warn("transfer import: antenna create failed",
				"name", ent.Name, "err", err)
			continue
		}
		res.Applied++
	}
	return res, nil
}

// normalizeJSON returns raw if it's non-empty, otherwise the fallback literal.
// 本家の export フォーマットは keywords: [] を省略せず出力するが、念のため。
func normalizeJSON(raw json.RawMessage, fallback string) []byte {
	if len(raw) == 0 {
		return []byte(fallback)
	}
	return raw
}
