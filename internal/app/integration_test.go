//go:build integration

package app

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"
)

func newIntegrationApp(t *testing.T) *App {
	t.Helper()
	url:=os.Getenv("DATABASE_URL")
	if url=="" { t.Fatal("DATABASE_URL is required") }
	loc,err:=time.LoadLocation("Asia/Shanghai")
	if err!=nil { t.Fatal(err) }
	a,err:=New(context.Background(),Config{DatabaseURL:url,HTTPAddr:":0",Location:loc})
	if err!=nil { t.Fatal(err) }
	t.Cleanup(a.Close)
	return a
}

func TestHTTPLeaderboards(t *testing.T) {
	a:=newIntegrationApp(t)
	h:=a.Handler()

	res:=httptest.NewRecorder()
	h.ServeHTTP(res,httptest.NewRequest(http.MethodGet,"/v1/leaderboard?user_id=2",nil))
	if res.Code!=http.StatusOK { t.Fatalf("active leaderboard status=%d body=%s",res.Code,res.Body.String()) }
	var active []map[string]any
	if err:=json.Unmarshal(res.Body.Bytes(),&active);err!=nil { t.Fatal(err) }
	if len(active)==0||len(active)>50 { t.Fatalf("active leaderboard size=%d",len(active)) }

	res=httptest.NewRecorder()
	h.ServeHTTP(res,httptest.NewRequest(http.MethodGet,"/v1/settlements/previous?user_id=1",nil))
	if res.Code!=http.StatusOK { t.Fatalf("previous leaderboard status=%d body=%s",res.Code,res.Body.String()) }
	var previous []map[string]any
	if err:=json.Unmarshal(res.Body.Bytes(),&previous);err!=nil { t.Fatal(err) }
	if len(previous)!=50 { t.Fatalf("previous leaderboard size=%d, want 50",len(previous)) }
}

func TestHTTPAddScore(t *testing.T) {
	a:=newIntegrationApp(t)
	ctx:=context.Background()
	// The SQL workflow uses fixed historical dates. Temporarily expose the period
	// containing the CI clock so the HTTP handler can use its real time source.
	if _,err:=a.db.Exec(ctx,`update periods set status=case when starts_at='2026-08-10 04:00:00+00' then 'active' else 'preparing' end`);err!=nil { t.Fatal(err) }
	t.Cleanup(func(){_,_=a.db.Exec(context.Background(),`update periods set status=case when starts_at='2026-08-10 04:00:00+00' then 'finished' else 'active' end`)})

	body:=[]byte(`{"user_id":3,"channel":"channel_a","points":10,"source_event_id":"http-event-1","duration_seconds":180}`)
	res:=httptest.NewRecorder()
	a.Handler().ServeHTTP(res,httptest.NewRequest(http.MethodPost,"/v1/scores",bytes.NewReader(body)))
	if res.Code!=http.StatusOK { t.Fatalf("add score status=%d body=%s",res.Code,res.Body.String()) }

	res=httptest.NewRecorder()
	a.Handler().ServeHTTP(res,httptest.NewRequest(http.MethodPost,"/v1/scores",bytes.NewReader(body)))
	if res.Code!=http.StatusOK { t.Fatalf("duplicate score status=%d body=%s",res.Code,res.Body.String()) }
	var got map[string]int
	if err:=json.Unmarshal(res.Body.Bytes(),&got);err!=nil { t.Fatal(err) }
	if got["accepted_points"]!=0 { t.Fatalf("duplicate accepted=%d",got["accepted_points"]) }
}

func TestWorkerDrainsAndRecoversJobs(t *testing.T) {
	a:=newIntegrationApp(t)
	ctx:=context.Background()
	for i:=0;i<100;i++ {
		ran,err:=a.RunOneJob(ctx)
		if err!=nil { t.Fatalf("run job %d: %v",i,err) }
		if !ran { break }
		if i==99 { t.Fatal("job queue did not drain") }
	}

	var periodID int64
	if err:=a.db.QueryRow(ctx,`select id from periods order by starts_at desc limit 1`).Scan(&periodID);err!=nil { t.Fatal(err) }
	if _,err:=a.db.Exec(ctx,`insert into period_jobs(job_key,period_id,job_type,status,updated_at) values('integration:stale',$1,'activate_period','running',now()-interval '10 minutes') on conflict(job_key) do update set status='running',updated_at=excluded.updated_at`,periodID);err!=nil { t.Fatal(err) }
	ran,err:=a.RunOneJob(ctx)
	if err!=nil||!ran { t.Fatalf("recover stale job: ran=%v err=%v",ran,err) }
	var status string
	if err:=a.db.QueryRow(ctx,`select status from period_jobs where job_key='integration:stale'`).Scan(&status);err!=nil { t.Fatal(err) }
	if status!="succeeded" { t.Fatalf("stale job status=%s",status) }
}
