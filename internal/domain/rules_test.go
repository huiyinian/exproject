package domain

import (
	"errors"
	"testing"
	"time"
)

func TestPeriodAtBoundary(t *testing.T) {
	loc := time.FixedZone("CST", 8*3600)
	before := time.Date(2026, 8, 17, 11, 59, 59, 0, loc)
	s, e := PeriodAt(before, loc)
	if s.Day() != 10 || e.Day() != 17 { t.Fatalf("unexpected period %v - %v", s, e) }
	at := time.Date(2026, 8, 17, 12, 0, 0, 0, loc)
	s, e = PeriodAt(at, loc)
	if s.Day() != 17 || e.Day() != 24 { t.Fatalf("boundary must enter new period: %v - %v", s, e) }
}

func TestValidateScore(t *testing.T) {
	c := Channel{Name:"lesson", DailyLimit:300}
	if err := ValidateScore(ScoreUsage{ChannelPoints:250, TotalPoints:450}, c, 50); err != nil { t.Fatal(err) }
	if !errors.Is(ValidateScore(ScoreUsage{ChannelPoints:250, TotalPoints:450}, c, 51), ErrChannelLimit) { t.Fatal("want channel limit") }
	if !errors.Is(ValidateScore(ScoreUsage{ChannelPoints:100, TotalPoints:480}, c, 21), ErrDailyLimit) { t.Fatal("want daily limit") }
}

func TestSettle(t *testing.T) {
	rows := make([]Standing, 10)
	for i := range rows { rows[i] = Standing{UserID:int64(i+1), Score:int64(100-i)} }
	got := Settle(2, rows)
	for i := 0; i < 7; i++ { if got[i].NewTier != 3 { t.Fatalf("rank %d should promote", i+1) } }
	for i := 7; i < 10; i++ { if got[i].NewTier != 1 { t.Fatalf("rank %d should demote", i+1) } }
}
