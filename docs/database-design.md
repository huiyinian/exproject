# 数据库设计

## 规模假设

- 注册/参赛用户约 60 万。
- 每组最多 50 人，每期最多约 1.2 万组。
- 用户完成一次可得分行为至少需要 3 分钟。若 60 万用户全部持续活跃，理论上限约为 `600000 / 180 ≈ 3333` 次请求/秒；实际容量必须按业务在线率压测。
- 每个用户每天最多形成 3 条渠道汇总记录、1 条周期积分记录。事件明细量取决于单次积分值，不用事件明细计算排行榜。

## 核心表

| 表 | 预计单期/单日数据量 | 作用 | 关键约束或索引 |
|---|---:|---|---|
| `users` | 60 万总量 | 当前段位、历史最高段位 | PK `id` |
| `channel_rules` | 3 | 渠道每日上限、最小时长 | PK `channel` |
| `tier_rules` | 5 | 晋升比例、奖励快照来源 | PK `tier` |
| `periods` | 每周 1 | 周期及状态 | `starts_at`、`ends_at` 唯一；左闭右开查询 |
| `period_groups` | 约 1.2 万/期 | 段位内的组 | UNIQUE `(period_id,tier,group_no)` |
| `period_members` | 60 万/期 | 冻结本期用户分组、段位和学段 | PK `(period_id,user_id)`；UNIQUE `(group_id,user_id)`；INDEX `(user_id,period_id desc)` |
| `period_scores` | 最多 60 万/期 | 本期累计积分及达到时间 | PK `(period_id,user_id)` |
| `daily_scores` | 最多 180 万/日 | 用户各渠道当日累计值 | PK `(user_id,score_date,channel)` |
| `score_events` | 取决于活跃度 | 审计和来源事件幂等 | PK `source_event_id`；INDEX `(user_id,created_at desc)` |
| `settlements` | 60 万/期 | 冻结最终名次、段位变化 | PK `(period_id,user_id)` |
| `reward_grants` | 最多 300 万总量 | 待发/已发奖励单及快照 | UNIQUE `(user_id,tier)`，从结构上保证只能发一次 |
| `reward_deliveries` | 最多 300 万总量 | 示例奖励系统的幂等交付记录 | PK `idempotency_key` |

## 为什么分开保存积分

`score_events` 是不可变流水，用于幂等、审计和问题追踪；`daily_scores` 用于校验渠道上限与总上限；`period_scores` 用于排行榜。查询排行榜不扫描流水，写入时三者在同一事务更新，因此逻辑直接且可恢复。

排行榜查询的索引路径：当前榜先通过 `period_members(user_id,period_id)` 找到用户所在组，再利用 `UNIQUE(group_id,user_id)` 读取最多 50 名组员，并通过 `period_scores(period_id,user_id)` 回表取分数。上期结算榜通过 `settlements(user_id,period_id desc)` 找最近一期，再通过 `settlements(period_id,group_id,rank,user_id)` 按名次顺序读取整组结果。

## 60 万用户下的处理方式

开赛分组使用窗口函数按 `tier,id` 稳定排序，再用 `ceil(row_number/50)` 生成组号。数据库一次批量生成约 1.2 万组和 60 万成员，禁止在 Go 中循环逐条插入。生产调度应在开赛前进入准备阶段完成分组，周一 12:00 只切换状态。

当前示例由 PostgreSQL 直接提供 50 人小组排行榜，单次查询只涉及一个小组。若轮询量达到每秒数千次，可把小组榜同步到 Redis ZSET，但 PostgreSQL 仍作为最终结算事实源。

结算不会按 5 个段位同时开启 5 个协程。数据库将约 1.2 万个小组按每 100 组拆成约 120 个任务，每个事务最多处理约 5000 人；用户段位更新使用相同批次。任务默认在 12:05 后开始，数据库 advisory lock 将多实例 Worker 的全局重任务并发限制为 1。奖励每批 1000 条并与其他任务串行执行。这样牺牲部分完成速度，换取中午高峰期更可控的数据库负载。

## 数据生命周期

- `period_members`、`period_scores`、`settlements`：按 `period_id` 保留，数据持续增长后按周期归档。
- `daily_scores`：只服务近期限额判断，建议保留 30～90 天后归档。
- `score_events`：数据量最大；生产环境建议按月分区，并根据审计要求保留 3～6 个月。
- `reward_grants`、`reward_deliveries`：长期保留，不可随周期删除。

## 并发与幂等

1. 上游为每次完成行为生成全局唯一 `source_event_id`，重复回调直接返回 0 分。
2. 服务端校验 `duration_seconds >= channel_rules.min_duration_seconds`。生产中时长应来自可信业务服务，不能信任客户端直传。
3. `pg_advisory_xact_lock(user_id)` 只串行化同一用户的加分，不阻塞其他用户，确保渠道上限和每日 500 总上限不会被并发突破。
4. 周期用 `starts_at <= now AND now < ends_at`，严格满足左闭右开。
