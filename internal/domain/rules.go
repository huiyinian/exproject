package domain

import (
	"errors"
	"sort"
	"time"
)

const (
	// 段位上下界属于核心业务约束，结算结果不能超出 1～5 级。
	MinTier = 1
	MaxTier = 5
	GroupSize = 50
	DailyLimit = 500
)

var (
	ErrInvalidPoints = errors.New("points must be positive")
	ErrChannelLimit = errors.New("channel daily limit exceeded")
	ErrDailyLimit = errors.New("total daily limit exceeded")
)

// Channel 表示一个积分渠道及该渠道独立的每日上限。
type Channel struct { Name string; DailyLimit int }

type ScoreUsage struct { ChannelPoints, TotalPoints int }

func ValidateScore(usage ScoreUsage, channel Channel, points int) error {
	if points <= 0 { return ErrInvalidPoints }
	if usage.ChannelPoints+points > channel.DailyLimit { return ErrChannelLimit }
	if usage.TotalPoints+points > DailyLimit { return ErrDailyLimit }
	return nil
}

// PeriodAt 根据指定时区计算当前周赛周期，范围为周一 12:00 左闭右开。
func PeriodAt(now time.Time, loc *time.Location) (start, end time.Time) {
	n := now.In(loc)
	daysSinceMonday := (int(n.Weekday()) + 6) % 7
	start = time.Date(n.Year(), n.Month(), n.Day(), 12, 0, 0, 0, loc).AddDate(0, 0, -daysSinceMonday)
	if n.Before(start) { start = start.AddDate(0, 0, -7) }
	return start, start.AddDate(0, 0, 7)
}

var promotionPercent = map[int]int{1: 80, 2: 70, 3: 60, 4: 50, 5: 40}

// Standing 是小组结算时的冻结排名数据。
// ReachedAt 表示达到当前积分的时间，同分时更早达到者排名更高。
type Standing struct { UserID int64; Score int64; ReachedAt time.Time }
type Settlement struct { UserID int64; Rank, OldTier, NewTier int; Promoted bool }

// Settle 对单个小组排名，并根据原段位的晋升比例计算新段位。
// 排序规则固定为：积分降序、达到时间升序、用户 ID 升序。
func Settle(tier int, standings []Standing) []Settlement {
	if tier < MinTier { tier = MinTier }; if tier > MaxTier { tier = MaxTier }
	sort.SliceStable(standings, func(i, j int) bool {
		if standings[i].Score == standings[j].Score {
			a,b:=standings[i].ReachedAt,standings[j].ReachedAt
			if a.IsZero()!=b.IsZero() { return !a.IsZero() }
			if !a.Equal(b) { return a.Before(b) }
			return standings[i].UserID < standings[j].UserID
		}
		return standings[i].Score > standings[j].Score
	})
	promoteCount := (len(standings)*promotionPercent[tier] + 99) / 100
	out := make([]Settlement, 0, len(standings))
	for i, s := range standings {
		promoted := i < promoteCount
		newTier := tier - 1
		if promoted { newTier = tier + 1 }
		if newTier < MinTier { newTier = MinTier }; if newTier > MaxTier { newTier = MaxTier }
		out = append(out, Settlement{UserID:s.UserID, Rank:i+1, OldTier:tier, NewTier:newTier, Promoted:promoted})
	}
	return out
}
