package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

type periodJob struct {
	ID int64
	PeriodID int64
	JobType string
	Payload []byte
}

// RunOneJob 每次只领取并执行一个数据库任务。
//
// claim_period_job 在数据库中使用全局 advisory lock，因此即使部署多个
// Worker 实例，默认也只有一个重任务在执行，避免多个实例同时压满数据库。
// Worker 崩溃后，超过 5 分钟的 running 任务会被其他实例重新领取。
func (a *App) RunOneJob(ctx context.Context) (bool, error) {
	var job periodJob
	err := a.db.QueryRow(ctx, `select id,period_id,job_type,payload from claim_period_job()`).Scan(&job.ID,&job.PeriodID,&job.JobType,&job.Payload)
	if err != nil {
		// 队列中没有可执行任务时，数据库函数不返回记录，这不属于异常。
		if errors.Is(err,pgx.ErrNoRows) { return false,nil }
		return false,err
	}

	var runErr error
	switch job.JobType {
	case "settle_groups":
		// 一个批次最多包含 100 个小组，即最多约 5000 名用户。
		var payload struct { MinGroupID int64 `json:"min_group_id"`; MaxGroupID int64 `json:"max_group_id"` }
		if err:=json.Unmarshal(job.Payload,&payload); err!=nil { runErr=err; break }
		_,runErr=a.db.Exec(ctx,"select settle_period_groups($1,$2,$3)",job.PeriodID,payload.MinGroupID,payload.MaxGroupID)
	case "finalize_settlement":
		// 这里只校验结算数量并生成“批量更新用户段位”任务，不直接更新 60 万行。
		_,runErr=a.db.Exec(ctx,"select finalize_settlement($1)",job.PeriodID)
	case "apply_tier_groups":
		var payload struct { MinGroupID int64 `json:"min_group_id"`; MaxGroupID int64 `json:"max_group_id"` }
		if err:=json.Unmarshal(job.Payload,&payload); err!=nil { runErr=err; break }
		_,runErr=a.db.Exec(ctx,"select apply_settlement_groups($1,$2,$3)",job.PeriodID,payload.MinGroupID,payload.MaxGroupID)
	case "finish_settlement":
		_,runErr=a.db.Exec(ctx,"select finish_settlement($1)",job.PeriodID)
	case "create_groups":
		_,runErr=a.db.Exec(ctx,"select create_period_groups($1)",job.PeriodID)
	case "activate_period":
		_,runErr=a.db.Exec(ctx,"select activate_period($1)",job.PeriodID)
	case "create_rewards":
		_,runErr=a.db.Exec(ctx,"select create_reward_grants($1)",job.PeriodID)
	case "dispatch_rewards":
		// 奖励每批最多 1000 条；下一批由 complete_period_job 继续投递。
		_,runErr=a.db.Exec(ctx,"select dispatch_reward_batch($1)",job.PeriodID)
	default:
		runErr=fmt.Errorf("unknown job type %q",job.JobType)
	}

	if runErr != nil {
		_,_ = a.db.Exec(ctx,`update period_jobs set status='pending',retry_count=retry_count+1,last_error=$2,available_at=now()+least(interval '5 minutes',interval '5 seconds'*(retry_count+1)),updated_at=now() where id=$1`,job.ID,runErr.Error())
		return true,runErr
	}
	_,err=a.db.Exec(ctx,"select complete_period_job($1)",job.ID)
	return true,err
}

func (a *App) PrepareAndRollover(ctx context.Context) error {
	// 两个函数都支持幂等调用，所以每个实例都可以每分钟检查一次；数据库锁会
	// 保证周一 12:00 的周期切换只有一个实例真正执行。
	now := a.now()
	if _,err:=a.db.Exec(ctx,"select prepare_next_period($1)",now); err!=nil{return err}
	_,err:=a.db.Exec(ctx,"select * from rollover_period($1)",now)
	return err
}

func (a *App) now() any { return nowIn(a.loc) }
