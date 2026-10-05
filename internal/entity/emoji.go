package entity

import "github.com/elythia-network/elythia/internal/model"

// EmojiDetailed is the emoji representation returned by admin emoji endpoints.
// Matches the TypeScript EmojiDetailed schema (packDetailed).
type EmojiDetailed struct {
	ID                                      string   `json:"id"`
	Aliases                                 []string `json:"aliases"`
	Name                                    string   `json:"name"`
	Category                                *string  `json:"category"`
	Host                                    *string  `json:"host"`
	URL                                     string   `json:"url"`
	License                                 *string  `json:"license"`
	IsSensitive                             bool     `json:"isSensitive"`
	LocalOnly                               bool     `json:"localOnly"`
	RoleIDsThatCanBeUsedThisEmojiAsReaction []string `json:"roleIdsThatCanBeUsedThisEmojiAsReaction"`
}

// PackEmojiDetailed converts a model.Emoji to an EmojiDetailed DTO.
// URL is computed as publicUrl || originalUrl for backward compatibility.
func PackEmojiDetailed(e *model.Emoji) EmojiDetailed {
	url := e.PublicURL
	if url == "" {
		url = e.OriginalURL
	}
	aliases := make([]string, len(e.Aliases))
	copy(aliases, e.Aliases)
	roleIDs := make([]string, len(e.RoleIDsThatCanBeUsedThisEmojiAsReaction))
	copy(roleIDs, e.RoleIDsThatCanBeUsedThisEmojiAsReaction)
	return EmojiDetailed{
		ID:                                      e.ID,
		Aliases:                                 aliases,
		Name:                                    e.Name,
		Category:                                e.Category,
		Host:                                    e.Host,
		URL:                                     url,
		License:                                 e.License,
		IsSensitive:                             e.IsSensitive,
		LocalOnly:                               e.LocalOnly,
		RoleIDsThatCanBeUsedThisEmojiAsReaction: roleIDs,
	}
}

// PackEmojiDetailedList converts a slice of model.Emoji to EmojiDetailed DTOs.
func PackEmojiDetailedList(emojis []*model.Emoji) []EmojiDetailed {
	out := make([]EmojiDetailed, len(emojis))
	for i, e := range emojis {
		out[i] = PackEmojiDetailed(e)
	}
	return out
}
