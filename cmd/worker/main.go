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

	// 调度循环只负责检查周期并投递任务，不在 12:00 同步执行结算。
	// 结算、更新段位、分组和奖励均写入 period_jobs 后逐个消费。
	scheduleTicker:=time.NewTicker(time.Minute)
	// 单进程只有一个消费循环；数据库领取函数还会把多实例总并发限制为 1。
	// 每批之间至少间隔 1 秒，用完成速度换取中午高峰期更平稳的数据库负载。
	jobTicker:=time.NewTicker(time.Second)
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
