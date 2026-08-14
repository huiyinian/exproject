# Weekly Contest Service

一个保持规则简单、事务边界明确的 Go 周赛后端示例。

## 已实现规则

- 三个积分渠道分别限额；所有渠道合计每日最多 500 分。一次行为达到渠道配置的最小时长（默认 180 秒）后才能得分。
- 周期为北京时间每周一 12:00 至下周一 12:00，左闭右开。
- 五个段位；新用户默认段位 1；每个段位奖励每个用户终身只能发放一次。
- 每期按当前段位重新分组，每组最多 50 人。
- 晋升比例依次为 80%、70%、60%、50%、40%；其余降一级，段位限制在 1～5。
- 用户 ID 作为同分时的稳定排序条件，确保结算可重放。

## 设计

PostgreSQL 是唯一事实源。加分、时长校验、每日限额判断、来源事件幂等写入在同一数据库事务中完成；按用户使用 advisory lock，避免并发突破限额。开赛和结算函数均可重复调用。

`channel_rules` 与 `tier_rules` 保存可调整配置。示例渠道限额为 300、250、200，实际业务可直接修改。

## 启动

```bash
docker compose up --build
```

API 与 Worker 分开运行。Worker 每分钟幂等检查周期：提前创建 `preparing` 周期；周一 12:00 后把旧周期切到 `settling`，新周期立即开始收分，再异步完成五个段位结算、更新段位、分组和激活。内部接口应增加生产鉴权。

```sql
insert into users(id) select generate_series(1,120);
```

```bash
curl -X POST localhost:8080/internal/periods/prepare
curl -X POST localhost:8080/internal/periods/rollover
curl -X POST localhost:8080/v1/scores -H 'content-type: application/json' \
  -d '{"user_id":1,"channel":"channel_a","points":100,"source_event_id":"event-1","duration_seconds":180}'
curl 'localhost:8080/v1/leaderboard?user_id=1'
curl 'localhost:8080/v1/settlements/previous?user_id=1'
```

正常情况下无需手工结算；`worker` 会消费 `period_jobs`。也可用 `POST /internal/jobs/run-once` 手工执行一个任务以便调试。

## 关键约定

- 晋升人数按 `ceil(组人数 × 晋升比例)` 计算，小组人数不足 50 时仍按实际人数计算。
- 5 段晋升者仍保持 5 段；1 段未晋升者仍保持 1 段。
- 结算后自动生成奖励单；`reward_grants(user_id,tier)` 唯一约束和 `idempotency_key` 双重保证每档奖励终身只发一次。`reward_deliveries` 是可运行的示例交付表，接入真实奖励服务时使用相同幂等键替换该写入。

详细表结构、索引和 60 万用户容量估算见 [数据库设计](docs/database-design.md)。
