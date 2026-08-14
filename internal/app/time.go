package app

import "time"

func nowIn(loc *time.Location) time.Time { return time.Now().In(loc) }
