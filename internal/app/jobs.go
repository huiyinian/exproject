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

func (a *App) RunOneJob(ctx context.Context) (bool, error) {
	var job periodJob
	err := a.db.QueryRow(ctx, `select id,period_id,job_type,payload from claim_period_job()`).Scan(&job.ID,&job.PeriodID,&job.JobType,&job.Payload)
	if err != nil {
		// claim_period_job returns no row when the queue is empty.
		if errors.Is(err,pgx.ErrNoRows) { return false,nil }
		return false,err
	}

	var runErr error
	switch job.JobType {
	case "settle_tier":
		var payload struct { Tier int `json:"tier"` }
		if err:=json.Unmarshal(job.Payload,&payload); err!=nil { runErr=err; break }
		_,runErr=a.db.Exec(ctx,"select settle_period_tier($1,$2)",job.PeriodID,payload.Tier)
	case "finalize_settlement":
		_,runErr=a.db.Exec(ctx,"select finalize_settlement($1)",job.PeriodID)
	case "create_groups":
		_,runErr=a.db.Exec(ctx,"select create_period_groups($1)",job.PeriodID)
	case "activate_period":
		_,runErr=a.db.Exec(ctx,"select activate_period($1)",job.PeriodID)
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
	now := a.now()
	if _,err:=a.db.Exec(ctx,"select prepare_next_period($1)",now); err!=nil{return err}
	_,err:=a.db.Exec(ctx,"select * from rollover_period($1)",now)
	return err
}

func (a *App) now() any { return nowIn(a.loc) }
