package federation

import "fmt"

// existingLocalUserIDs returns the set of IDs named by the local user URIs in
// hrefs (as read by ExtractLocalUserID) that exist in the user table, looked up
// in one query. It returns a nil set when there is no user repository, in
// which case the caller keeps the IDs unverified.
//
// 本家 ApPersonService.fetchPerson は `findOneBy({ id })` で引き、無ければ null
// (メンション・宛先に入らない)。ExtractLocalUserID は URI を読むだけなので、
// `/users/{id}/followers` の `followers` のような実在しない ID がそのまま
// mentions / visibleUserIds に入っていた (#3330)。1 件ずつ引くと Mention を
// 並べるだけで N 回の問い合わせになるのでまとめて引く。
func (r *Resolver) existingLocalUserIDs(hrefs []string) (map[string]struct{}, error) {
	if r.userRepo == nil {
		return nil, nil
	}
	// 同じ利用者への Mention を並べても IN 句が伸びないよう、重複を除いてから引く。
	var ids []string
	seen := make(map[string]struct{}, len(hrefs))
	for _, href := range hrefs {
		id := r.ExtractLocalUserID(href)
		if id == "" {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	found := make(map[string]struct{}, len(ids))
	if len(ids) == 0 {
		return found, nil
	}
	users, err := r.userRepo.FindManyByIDs(ids)
	if err != nil {
		// 引けなかったことを「実在しない」に潰さない (#3121 と同じ理由)。
		return nil, fmt.Errorf("resolve local mentioned users: %w", err)
	}
	for _, u := range users {
		if u != nil {
			found[u.ID] = struct{}{}
		}
	}
	return found, nil
}
