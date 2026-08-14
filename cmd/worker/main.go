package main

import (
	"context"
	"log"
	"os/signal"
	"syscall"
	"time"

	"github.com/huiyinian/exproject/internal/app"
)

func main() {
	ctx,stop:=signal.NotifyContext(context.Background(),syscall.SIGINT,syscall.SIGTERM)
	defer stop()
	cfg,err:=app.LoadConfig(); if err!=nil{log.Fatal(err)}
	a,err:=app.New(ctx,cfg); if err!=nil{log.Fatal(err)}
	defer a.Close()

	scheduleTicker:=time.NewTicker(time.Minute)
	jobTicker:=time.NewTicker(200*time.Millisecond)
	defer scheduleTicker.Stop(); defer jobTicker.Stop()
	for {
		select {
		case <-ctx.Done(): return
		case <-scheduleTicker.C:
			if err:=a.PrepareAndRollover(ctx); err!=nil{log.Printf("schedule: %v",err)}
		case <-jobTicker.C:
			ran,err:=a.RunOneJob(ctx); if err!=nil{log.Printf("job: %v",err)}
			if !ran{time.Sleep(800*time.Millisecond)}
		}
	}
}
