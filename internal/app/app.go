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

type Config struct { HTTPAddr, DatabaseURL string; Location *time.Location }

func LoadConfig() (Config, error) {
	url := os.Getenv("DATABASE_URL"); if url == "" { return Config{}, errors.New("DATABASE_URL is required") }
	tz := os.Getenv("APP_TIMEZONE"); if tz == "" { tz = "Asia/Shanghai" }
	loc, err := time.LoadLocation(tz); if err != nil { return Config{}, fmt.Errorf("load timezone: %w", err) }
	addr := os.Getenv("HTTP_ADDR"); if addr == "" { addr = ":8080" }
	return Config{HTTPAddr:addr, DatabaseURL:url, Location:loc}, nil
}

type App struct { db *pgxpool.Pool; loc *time.Location }

func New(ctx context.Context, cfg Config) (*App, error) {
	db, err := pgxpool.New(ctx, cfg.DatabaseURL); if err != nil { return nil, err }
	if err := db.Ping(ctx); err != nil { db.Close(); return nil, err }
	return &App{db:db, loc:cfg.Location}, nil
}
func (a *App) Close() { a.db.Close() }
func (a *App) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, 200, map[string]string{"status":"ok"}) })
	mux.HandleFunc("POST /v1/scores", a.addScore)
	mux.HandleFunc("GET /v1/leaderboard", a.leaderboard)
	mux.HandleFunc("POST /internal/periods/start", a.startPeriod)
	mux.HandleFunc("POST /internal/periods/settle", a.settlePeriod)
	mux.HandleFunc("POST /v1/rewards/{tier}/claim", a.claimReward)
	return mux
}

func (a *App) addScore(w http.ResponseWriter, r *http.Request) {
	var in struct { UserID int64 `json:"user_id"`; Channel string `json:"channel"`; Points int `json:"points"`; SourceEventID string `json:"source_event_id"`; DurationSeconds int `json:"duration_seconds"` }
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.UserID <= 0 || in.Channel == "" || in.SourceEventID == "" { writeError(w, 400, "invalid_request"); return }
	var accepted int
	err := a.db.QueryRow(r.Context(), "select add_score($1,$2,$3,$4,$5,$6)", in.UserID, in.Channel, in.Points, in.SourceEventID, in.DurationSeconds, time.Now().In(a.loc)).Scan(&accepted)
	if err != nil { writeError(w, 409, err.Error()); return }
	writeJSON(w, 200, map[string]int{"accepted_points":accepted})
}

func (a *App) leaderboard(w http.ResponseWriter, r *http.Request) {
	uid, _ := strconv.ParseInt(r.URL.Query().Get("user_id"), 10, 64); if uid <= 0 { writeError(w,400,"invalid_user_id"); return }
	rows, err := a.db.Query(r.Context(), `select m2.user_id, coalesce(s.score,0), dense_rank() over(order by coalesce(s.score,0) desc, m2.user_id) from period_members m join period_members m2 on m2.period_id=m.period_id and m2.group_id=m.group_id left join period_scores s on s.period_id=m2.period_id and s.user_id=m2.user_id where m.user_id=$1 order by 3, m2.user_id`, uid)
	if err != nil { writeError(w,500,"query_failed"); return }; defer rows.Close()
	type item struct { UserID int64 `json:"user_id"`; Score int64 `json:"score"`; Rank int `json:"rank"` }
	items := []item{}; for rows.Next() { var x item; if rows.Scan(&x.UserID,&x.Score,&x.Rank)==nil { items=append(items,x) } }
	writeJSON(w,200,items)
}

func (a *App) startPeriod(w http.ResponseWriter, r *http.Request) { var id int64; err:=a.db.QueryRow(r.Context(),"select start_weekly_period($1)",time.Now().In(a.loc)).Scan(&id); if err!=nil {writeError(w,409,err.Error());return}; writeJSON(w,200,map[string]int64{"period_id":id}) }
func (a *App) settlePeriod(w http.ResponseWriter, r *http.Request) { var id int64; err:=a.db.QueryRow(r.Context(),"select settle_weekly_period($1)",time.Now().In(a.loc)).Scan(&id); if err!=nil {writeError(w,409,err.Error());return}; writeJSON(w,200,map[string]int64{"period_id":id}) }
func (a *App) claimReward(w http.ResponseWriter, r *http.Request) { tier,_:=strconv.Atoi(r.PathValue("tier")); uid,_:=strconv.ParseInt(r.URL.Query().Get("user_id"),10,64); tag,err:=a.db.Exec(r.Context(),`insert into reward_claims(user_id,tier,reward_snapshot) select u.id,r.tier,r.reward from users u join tier_rules r on r.tier=$2 where u.id=$1 and u.highest_tier>=r.tier on conflict do nothing`,uid,tier); if err!=nil {writeError(w,500,"claim_failed");return}; if tag.RowsAffected()==0 {writeError(w,409,"already_claimed_or_not_eligible");return}; writeJSON(w,200,map[string]any{"claimed":true,"tier":tier}) }
func writeJSON(w http.ResponseWriter, status int, v any) { w.Header().Set("Content-Type","application/json"); w.WriteHeader(status); _=json.NewEncoder(w).Encode(v) }
func writeError(w http.ResponseWriter,status int,msg string){writeJSON(w,status,map[string]string{"error":msg})}
