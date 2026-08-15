package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type Config struct { HTTPAddr, DatabaseURL, InternalToken string; Location *time.Location }

func LoadConfig() (Config, error) {
	url := os.Getenv("DATABASE_URL"); if url == "" { return Config{}, errors.New("DATABASE_URL is required") }
	tz := os.Getenv("APP_TIMEZONE"); if tz == "" { tz = "Asia/Shanghai" }
	loc, err := time.LoadLocation(tz); if err != nil { return Config{}, fmt.Errorf("load timezone: %w", err) }
	addr := os.Getenv("HTTP_ADDR"); if addr == "" { addr = ":8080" }
	return Config{HTTPAddr:addr, DatabaseURL:url, InternalToken:os.Getenv("INTERNAL_TOKEN"), Location:loc}, nil
}

type App struct { db *pgxpool.Pool; loc *time.Location; internalToken string }

func New(ctx context.Context, cfg Config) (*App, error) {
	db, err := pgxpool.New(ctx, cfg.DatabaseURL); if err != nil { return nil, err }
	if err := db.Ping(ctx); err != nil { db.Close(); return nil, err }
	return &App{db:db, loc:cfg.Location, internalToken:cfg.InternalToken}, nil
}
func (a *App) Close() { a.db.Close() }
func (a *App) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, 200, map[string]string{"status":"ok"}) })
	mux.HandleFunc("POST /v1/scores", a.addScore)
	mux.HandleFunc("GET /v1/leaderboard", a.leaderboard)
	mux.HandleFunc("GET /v1/settlements/previous", a.previousSettlementLeaderboard)
	// 周期切换和任务执行属于高风险运维操作，必须通过内部 Token 鉴权。
	mux.Handle("POST /internal/periods/prepare", a.requireInternal(http.HandlerFunc(a.preparePeriod)))
	mux.Handle("POST /internal/periods/rollover", a.requireInternal(http.HandlerFunc(a.rolloverPeriod)))
	mux.Handle("POST /internal/jobs/run-once", a.requireInternal(http.HandlerFunc(a.runJobOnce)))
	return mux
}

// requireInternal 保护会改变周期或任务状态的内部接口。
// 未配置 Token 时直接返回 503，避免误把无鉴权接口部署到生产环境。
func (a *App) requireInternal(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter,r *http.Request){
		if a.internalToken=="" { writeError(w,http.StatusServiceUnavailable,"internal_api_disabled"); return }
		if r.Header.Get("Authorization")!="Bearer "+a.internalToken { writeError(w,http.StatusUnauthorized,"unauthorized"); return }
		next.ServeHTTP(w,r)
	})
}

func (a *App) addScore(w http.ResponseWriter, r *http.Request) {
	var in struct { UserID int64 `json:"user_id"`; Channel string `json:"channel"`; Points int `json:"points"`; SourceEventID string `json:"source_event_id"`; DurationSeconds int `json:"duration_seconds"` }
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.UserID <= 0 || in.Channel == "" || in.SourceEventID == "" { writeError(w, 400, "invalid_request"); return }
	var accepted int
	// 校验、每日累计和周期累计由同一个数据库函数完成，避免并发请求只更新
	// 部分数据，或者同时读取旧值后突破每日积分上限。
	err := a.db.QueryRow(r.Context(), "select add_score($1,$2,$3,$4,$5,$6)", in.UserID, in.Channel, in.Points, in.SourceEventID, in.DurationSeconds, time.Now().In(a.loc)).Scan(&accepted)
	if err != nil { writeError(w, 409, err.Error()); return }
	writeJSON(w, 200, map[string]int{"accepted_points":accepted})
}

func (a *App) leaderboard(w http.ResponseWriter, r *http.Request) {
	uid, _ := strconv.ParseInt(r.URL.Query().Get("user_id"), 10, 64); if uid <= 0 { writeError(w,400,"invalid_user_id"); return }
	rows, err := a.db.Query(r.Context(), `select m2.user_id,coalesce(s.score,0),row_number() over(order by coalesce(s.score,0) desc,s.reached_at asc nulls last,m2.user_id),s.reached_at,m2.stage from period_members m join periods p on p.id=m.period_id and p.status='active' join period_members m2 on m2.period_id=m.period_id and m2.group_id=m.group_id left join period_scores s on s.period_id=m2.period_id and s.user_id=m2.user_id where m.user_id=$1 order by 3`, uid)
	if err != nil { writeError(w,500,"query_failed"); return }; defer rows.Close()
	type item struct { UserID int64 `json:"user_id"`; Score int64 `json:"score"`; Rank int `json:"rank"`; ReachedAt *time.Time `json:"reached_at,omitempty"`; Stage int `json:"stage"` }
	items := []item{}; for rows.Next() { var x item; if rows.Scan(&x.UserID,&x.Score,&x.Rank,&x.ReachedAt,&x.Stage)==nil { items=append(items,x) } }
	writeJSON(w,200,items)
}

func (a *App) previousSettlementLeaderboard(w http.ResponseWriter, r *http.Request) {
	uid,_:=strconv.ParseInt(r.URL.Query().Get("user_id"),10,64)
	if uid<=0 { writeError(w,400,"invalid_user_id"); return }
	rows,err:=a.db.Query(r.Context(),`select s.period_id,s.group_id,s.user_id,s.final_score,s.rank,s.old_tier,s.new_tier,s.promoted,s.reached_at,s.stage from settlements mine join settlements s on s.period_id=mine.period_id and s.group_id=mine.group_id join periods p on p.id=mine.period_id where mine.user_id=$1 and p.status='finished' and mine.period_id=(select max(x.period_id) from settlements x join periods px on px.id=x.period_id where x.user_id=$1 and px.status='finished') order by s.rank,s.user_id`,uid)
	if err!=nil { writeError(w,500,"query_failed"); return }
	defer rows.Close()
	type item struct { PeriodID int64 `json:"period_id"`; GroupID int64 `json:"group_id"`; UserID int64 `json:"user_id"`; FinalScore int64 `json:"final_score"`; Rank int `json:"rank"`; OldTier int `json:"old_tier"`; NewTier int `json:"new_tier"`; Promoted bool `json:"promoted"`; ReachedAt *time.Time `json:"reached_at,omitempty"`; Stage int `json:"stage"` }
	items:=[]item{}
	for rows.Next(){var x item;if err:=rows.Scan(&x.PeriodID,&x.GroupID,&x.UserID,&x.FinalScore,&x.Rank,&x.OldTier,&x.NewTier,&x.Promoted,&x.ReachedAt,&x.Stage);err!=nil{writeError(w,500,"scan_failed");return};items=append(items,x)}
	writeJSON(w,200,items)
}

func (a *App) preparePeriod(w http.ResponseWriter, r *http.Request) { var id int64; err:=a.db.QueryRow(r.Context(),"select prepare_next_period($1)",time.Now().In(a.loc)).Scan(&id); if err!=nil {writeError(w,409,err.Error());return}; writeJSON(w,200,map[string]int64{"period_id":id}) }
func (a *App) rolloverPeriod(w http.ResponseWriter, r *http.Request) { var oldID,newID int64; err:=a.db.QueryRow(r.Context(),"select old_period_id,new_period_id from rollover_period($1)",time.Now().In(a.loc)).Scan(&oldID,&newID); if err!=nil {writeError(w,409,err.Error());return}; writeJSON(w,200,map[string]int64{"old_period_id":oldID,"new_period_id":newID}) }
func (a *App) runJobOnce(w http.ResponseWriter, r *http.Request) { ran,err:=a.RunOneJob(r.Context()); if err!=nil {writeError(w,500,err.Error());return}; writeJSON(w,200,map[string]bool{"ran":ran}) }
func writeJSON(w http.ResponseWriter, status int, v any) { w.Header().Set("Content-Type","application/json"); w.WriteHeader(status); _=json.NewEncoder(w).Encode(v) }
func writeError(w http.ResponseWriter,status int,msg string){writeJSON(w,status,map[string]string{"error":msg})}
