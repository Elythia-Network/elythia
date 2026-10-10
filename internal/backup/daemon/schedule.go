package daemon

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"time"

	"github.com/elythia-network/elythia/internal/config"
)

// MinInterval is the shortest accepted schedule interval.
//
// 単位の書き忘れ (`24` のつもりの `24s` など) で、取り続けて保存先を埋めないための下限。
const MinInterval = time.Minute

const day = 24 * time.Hour

var atPattern = regexp.MustCompile(`^([01]\d|2[0-3]):([0-5]\d)$`)

// Schedule decides when the daemon takes a backup.
//
// Without At, runs are Interval apart starting from an anchor. With At, the
// runs are aligned to that local time of day: an Interval that is a multiple
// of 24h steps in calendar days (so a DST change does not move the wall-clock
// time), and a shorter Interval steps from At within each day.
type Schedule struct {
	Interval time.Duration
	// HasAt reports whether At was set; Hour and Minute are its value.
	HasAt        bool
	Hour, Minute int
	Location     *time.Location
	// Keep is the number of usable generations to keep. 0 keeps all.
	Keep int
	// Verify runs the verification after every take.
	Verify bool
	// DelayAfter is how long after the newest usable generation a delay is
	// reported.
	DelayAfter time.Duration

	// anchor is the reference slot. With At it is At on the start day.
	anchor time.Time
}

// ParseSchedule validates the schedule options. It returns (nil, nil) when
// Interval is empty, which means no periodic backups.
func ParseSchedule(o config.BackupScheduleOptions, loc *time.Location) (*Schedule, error) {
	if o.Interval == "" {
		return nil, nil
	}
	if loc == nil {
		loc = time.Local
	}
	interval, err := time.ParseDuration(o.Interval)
	if err != nil {
		return nil, fmt.Errorf("backup.schedule.interval: %w", err)
	}
	if interval < MinInterval {
		return nil, fmt.Errorf("backup.schedule.interval: %s is shorter than %s", interval, MinInterval)
	}
	s := &Schedule{Interval: interval, Location: loc, Keep: o.Keep, Verify: o.Verify}
	if o.Keep < 0 {
		return nil, errors.New("backup.schedule.keep: must not be negative")
	}
	if o.At != "" {
		m := atPattern.FindStringSubmatch(o.At)
		if m == nil {
			return nil, fmt.Errorf("backup.schedule.at: %q is not HH:MM", o.At)
		}
		s.HasAt = true
		s.Hour, _ = strconv.Atoi(m[1])
		s.Minute, _ = strconv.Atoi(m[2])
	}
	s.DelayAfter = interval + interval/2
	if o.DelayAfter != "" {
		d, err := time.ParseDuration(o.DelayAfter)
		if err != nil {
			return nil, fmt.Errorf("backup.schedule.delayAfter: %w", err)
		}
		// 間隔より短いと、正常に回っていても毎回の合間に遅れを知らせてしまう。
		if d < interval {
			return nil, fmt.Errorf("backup.schedule.delayAfter: %s is shorter than the interval %s", d, interval)
		}
		s.DelayAfter = d
	}
	return s, nil
}

// AtString returns At as "HH:MM", or "" when unset.
func (s *Schedule) AtString() string {
	if !s.HasAt {
		return ""
	}
	return fmt.Sprintf("%02d:%02d", s.Hour, s.Minute)
}

// Start fixes the anchor and returns the first run time.
//
// latest is the creation time of the newest complete generation, or nil when
// there is none. When it is older than Interval (or missing) the first run is
// now, so a daemon that was down does not wait for the next slot. A latest
// after now is treated as now.
func (s *Schedule) Start(now time.Time, latest *time.Time) time.Time {
	if latest != nil && latest.After(now) {
		// 時計の狂ったホストで取った世代などで未来の時刻があると、起点が未来になり、
		// その時刻まで枠が来ない。今を超えないように丸める。
		latest = &now
	}
	catchUp := latest == nil || now.Sub(*latest) >= s.Interval
	if s.HasAt {
		if catchUp {
			local := now.In(s.Location)
			s.anchor = time.Date(local.Year(), local.Month(), local.Day(), s.Hour, s.Minute, 0, 0, s.Location)
			return now
		}
		// 枠の起点は、最新の世代以前で最後の At にする。再起動した日を起点にすると、
		// 2 日以上の間隔では再起動のたびに枠がずれ、間隔が開いて遅れの通知も出る。
		// 世代を取った日の At にすると、枠が保留されて日付をまたいでから取った世代
		// (23:30 の枠を 00:30 に取ったなど) で起点が 1 日後ろへずれる。
		s.anchor = s.lastAtNotAfter(*latest)
		return s.Next(now)
	}
	if catchUp {
		s.anchor = now
		return now
	}
	s.anchor = latest.Add(s.Interval)
	return s.anchor
}

// lastAtNotAfter returns the last At (in Location) that is not after t.
func (s *Schedule) lastAtNotAfter(t time.Time) time.Time {
	local := t.In(s.Location)
	a := time.Date(local.Year(), local.Month(), local.Day(), s.Hour, s.Minute, 0, 0, s.Location)
	if a.After(t) {
		a = time.Date(local.Year(), local.Month(), local.Day()-1, s.Hour, s.Minute, 0, 0, s.Location)
	}
	return a
}

// AtDriftsAcrossRestarts reports whether At is combined with an Interval
// that neither divides 24h nor is a multiple of it. Such slots do not repeat
// at the same times every day, so the phase depends on the anchor and can
// move when the daemon restarts.
func (s *Schedule) AtDriftsAcrossRestarts() bool {
	return s.HasAt && s.Interval%day != 0 && day%s.Interval != 0
}

// Next returns the first slot strictly after after. Start must have been
// called.
func (s *Schedule) Next(after time.Time) time.Time {
	if s.HasAt && s.Interval%day == 0 {
		days := int(s.Interval / day)
		k := int(floorDiv(after.Sub(s.anchor), s.Interval))
		slot := s.anchor.AddDate(0, 0, k*days)
		// AddDate は夏時間の切り替えで 1 時間ずれるので、割り算で求めた k を前後に直す。
		for !slot.After(after) {
			k++
			slot = s.anchor.AddDate(0, 0, k*days)
		}
		for {
			prev := s.anchor.AddDate(0, 0, (k-1)*days)
			if !prev.After(after) {
				return slot
			}
			k--
			slot = prev
		}
	}
	k := floorDiv(after.Sub(s.anchor), s.Interval) + 1
	return s.anchor.Add(time.Duration(k) * s.Interval)
}

// floorDiv is a / b rounded toward negative infinity.
func floorDiv(a, b time.Duration) int64 {
	q := int64(a / b)
	if a%b != 0 && (a < 0) != (b < 0) {
		q--
	}
	return q
}
