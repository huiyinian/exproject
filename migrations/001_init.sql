begin;

-- User profile and configurable business rules.

create table users (
  id bigint primary key,
  -- stage is mutable user profile data; every period copies it into period_members.
  stage smallint not null default 0 check(stage>=0),
  tier smallint not null default 1 check (tier between 1 and 5),
  highest_tier smallint not null default 1 check (highest_tier between 1 and 5),
  created_at timestamptz not null default now()
);

create table channel_rules (
  channel text primary key,
  daily_limit integer not null check (daily_limit > 0),
  min_duration_seconds integer not null default 180 check (min_duration_seconds >= 0)
);
insert into channel_rules(channel,daily_limit) values ('channel_a',300),('channel_b',250),('channel_c',200);

create table tier_rules (
  tier smallint primary key check (tier between 1 and 5),
  promotion_percent smallint not null check (promotion_percent between 0 and 100),
  reward jsonb not null
);
insert into tier_rules values
 (1,80,'{"name":"tier-1 reward"}'),(2,70,'{"name":"tier-2 reward"}'),
 (3,60,'{"name":"tier-3 reward"}'),(4,50,'{"name":"tier-4 reward"}'),
 (5,40,'{"name":"tier-5 reward"}');

-- Period, grouping, and frozen membership data.
create table periods (
  id bigint generated always as identity primary key,
  starts_at timestamptz not null unique,
  ends_at timestamptz not null unique,
  previous_period_id bigint references periods(id),
  status text not null check (status in ('preparing','active','settling','finished')),
  check (ends_at = starts_at + interval '7 days')
);
create table period_groups (
  id bigint generated always as identity primary key,
  period_id bigint not null references periods(id), tier smallint not null,
  group_no integer not null, unique(period_id,tier,group_no)
);
create table period_members (
  period_id bigint not null references periods(id), group_id bigint not null references period_groups(id),
  user_id bigint not null references users(id), tier smallint not null,
  -- Frozen stage: later profile changes must not rewrite historical periods.
  stage smallint not null,
  primary key(period_id,user_id), unique(group_id,user_id)
);
create table period_scores (
  period_id bigint not null references periods(id), user_id bigint not null references users(id),
  score bigint not null default 0 check(score>=0),
  -- Server time when the user most recently reached the current score.
  reached_at timestamptz,
  primary key(period_id,user_id)
);
-- Score ledger plus daily and weekly aggregates. Leaderboards never scan the
-- high-volume score_events table.
create table daily_scores (
  user_id bigint not null references users(id), score_date date not null,
  channel text not null references channel_rules(channel), score integer not null default 0,
  primary key(user_id,score_date,channel)
);
create table score_events (
  source_event_id text primary key, user_id bigint not null, period_id bigint not null,
  channel text not null, points integer not null, duration_seconds integer not null,
  created_at timestamptz not null
);
create index idx_score_events_user_created on score_events(user_id,created_at desc);
create index idx_period_members_user_period on period_members(user_id,period_id desc);
-- Immutable settlement snapshots support historical group leaderboards.
create table settlements (
  period_id bigint not null, user_id bigint not null, group_id bigint not null,
  rank integer not null, old_tier smallint not null, new_tier smallint not null,
  final_score bigint not null, reached_at timestamptz, stage smallint not null,
  promoted boolean not null,
  primary key(period_id,user_id)
);
create index idx_settlements_user_period on settlements(user_id,period_id desc);
create index idx_settlements_period_group_rank on settlements(period_id,group_id,rank,user_id);
-- Reward grants are durable orders. reward_deliveries simulates an idempotent
-- downstream reward provider in this standalone example.
create table reward_grants (
  id bigint generated always as identity primary key,
  source_period_id bigint not null references periods(id),
  user_id bigint not null references users(id),
  tier smallint not null references tier_rules(tier),
  idempotency_key text not null unique,
  reward_snapshot jsonb not null,
  status text not null default 'pending' check(status in('pending','succeeded')),
  created_at timestamptz not null default now(),
  delivered_at timestamptz,
  unique(user_id,tier)
);
create index idx_reward_grants_dispatch on reward_grants(source_period_id,status,id);
create table reward_deliveries (
  idempotency_key text primary key,
  user_id bigint not null,
  reward_snapshot jsonb not null,
  delivered_at timestamptz not null default now()
);
-- Durable orchestration queue for settlement, regrouping, and rewards.
create table period_jobs (
  id bigint generated always as identity primary key,
  job_key text not null unique,
  period_id bigint not null references periods(id),
  job_type text not null check(job_type in('settle_groups','finalize_settlement','apply_tier_groups','finish_settlement','create_groups','activate_period','create_rewards','dispatch_rewards')),
  payload jsonb not null default '{}',
  status text not null default 'pending' check(status in('pending','running','succeeded')),
  retry_count integer not null default 0,
  available_at timestamptz not null default now(),
  last_error text,
  created_at timestamptz not null default now(),
  updated_at timestamptz not null default now()
);
create index idx_period_jobs_claim on period_jobs(status,available_at,id);

-- Atomically validates a score event and updates its immutable event row,
-- daily counters, and current-period aggregate.
create or replace function add_score(p_user bigint,p_channel text,p_points int,p_source_event text,p_duration_seconds int,p_now timestamptz) returns int language plpgsql as $$
declare v_period bigint; v_channel_limit int; v_min_duration int; v_channel_used int; v_total_used int;
begin
  if p_points <= 0 then raise exception 'invalid_points'; end if;
  if exists(select 1 from score_events where source_event_id=p_source_event) then return 0; end if;
  select daily_limit,min_duration_seconds into v_channel_limit,v_min_duration from channel_rules where channel=p_channel;
  if not found then raise exception 'invalid_channel'; end if;
  if p_duration_seconds<v_min_duration then raise exception 'duration_not_enough'; end if;
  select id into v_period from periods where status in('preparing','active') and starts_at<=p_now and p_now<ends_at for update;
  if not found then raise exception 'no_active_period'; end if;
  perform pg_advisory_xact_lock(p_user);
  select coalesce(score,0) into v_channel_used from daily_scores where user_id=p_user and score_date=(p_now at time zone 'Asia/Shanghai')::date and channel=p_channel;
  select coalesce(sum(score),0) into v_total_used from daily_scores where user_id=p_user and score_date=(p_now at time zone 'Asia/Shanghai')::date;
  if v_channel_used+p_points>v_channel_limit then raise exception 'channel_daily_limit_exceeded'; end if;
  if v_total_used+p_points>500 then raise exception 'total_daily_limit_exceeded'; end if;
  insert into daily_scores values(p_user,(p_now at time zone 'Asia/Shanghai')::date,p_channel,p_points) on conflict(user_id,score_date,channel) do update set score=daily_scores.score+excluded.score;
  insert into period_scores(period_id,user_id,score,reached_at) values(v_period,p_user,p_points,p_now)
  on conflict(period_id,user_id) do update set score=period_scores.score+excluded.score,reached_at=excluded.reached_at;
  insert into score_events values(p_source_event,p_user,v_period,p_channel,p_points,p_duration_seconds,p_now);
  return p_points;
end $$;

-- Creates the next empty period before noon without doing any heavy work.
create or replace function prepare_next_period(p_now timestamptz) returns bigint language plpgsql as $$
declare v_start timestamptz; v_period bigint; v_previous bigint;
begin
  v_start := date_trunc('week',p_now at time zone 'Asia/Shanghai') at time zone 'Asia/Shanghai' + interval '12 hours';
  if p_now>=v_start then v_start:=v_start+interval '7 days'; end if;
  select id into v_previous from periods where ends_at=v_start;
  insert into periods(starts_at,ends_at,previous_period_id,status) values(v_start,v_start+interval '7 days',v_previous,'preparing') on conflict(starts_at) do update set starts_at=excluded.starts_at returning id into v_period;
  return v_period;
end $$;

-- 周一 12:00 只切换状态并生成结算批次，不在高峰期同步扫描 60 万用户。
create or replace function rollover_period(p_now timestamptz) returns table(old_period_id bigint,new_period_id bigint) language plpgsql as $$
declare v_start timestamptz;
begin
  perform pg_advisory_xact_lock(861204);
  v_start:=date_trunc('week',p_now at time zone 'Asia/Shanghai') at time zone 'Asia/Shanghai'+interval '12 hours';
  if p_now<v_start then return; end if;
  select id into old_period_id from periods where ends_at=v_start and status in('active','settling');
  select id into new_period_id from periods where starts_at=v_start;
  if new_period_id is null then
    insert into periods(starts_at,ends_at,previous_period_id,status) values(v_start,v_start+interval '7 days',old_period_id,'preparing') returning id into new_period_id;
  end if;
  if old_period_id is null then
    insert into period_jobs(job_key,period_id,job_type) values(new_period_id||':groups',new_period_id,'create_groups') on conflict do nothing;
    return next; return;
  end if;
  update periods set status='settling' where id=old_period_id and status='active';
  -- 每 100 个小组一个批次。每组最多 50 人，所以单事务最多处理约 5000 人。
  -- available_at 延后 5 分钟，避开 12:00 整点的请求高峰。
  insert into period_jobs(job_key,period_id,job_type,payload,available_at)
  select old_period_id||':settle:'||batch_no,old_period_id,'settle_groups',
         jsonb_build_object('min_group_id',min(id),'max_group_id',max(id)),p_now+interval '5 minutes'
  from (
    select id,((row_number() over(order by id)-1)/100)::integer batch_no
    from period_groups where period_id=old_period_id
  ) batches group by batch_no
  on conflict do nothing;
  return next;
end $$;

-- 结算一批小组。批次边界使用全局唯一 group_id，默认最多写入约 5000 行。
-- 排名规则：积分降序、达到时间升序、UID 升序。
create or replace function settle_period_groups(p_period bigint,p_min_group bigint,p_max_group bigint) returns void language plpgsql as $$
begin
  insert into settlements(period_id,user_id,group_id,rank,old_tier,new_tier,final_score,reached_at,stage,promoted)
  select period_id,user_id,group_id,rn,old_tier,case when promoted then least(5,old_tier+1) else greatest(1,old_tier-1) end,score,reached_at,stage,promoted from (
    select x.*,rn<=ceil(cnt*promotion_percent/100.0) promoted from (
      select m.period_id,m.user_id,m.group_id,m.tier old_tier,coalesce(s.score,0) score,r.promotion_percent,
      s.reached_at,m.stage,row_number() over(partition by m.group_id order by coalesce(s.score,0) desc,s.reached_at asc nulls last,m.user_id) rn,count(*) over(partition by m.group_id) cnt
      from period_members m left join period_scores s using(period_id,user_id) join tier_rules r on r.tier=m.tier
      where m.period_id=p_period and m.group_id between p_min_group and p_max_group
    ) x
  ) y on conflict(period_id,user_id) do update set rank=excluded.rank,final_score=excluded.final_score,reached_at=excluded.reached_at,stage=excluded.stage,new_tier=excluded.new_tier,promoted=excluded.promoted;
end $$;

create or replace function finalize_settlement(p_period bigint) returns void language plpgsql as $$
declare v_members bigint; v_settled bigint;
begin
  select count(*) into v_members from period_members where period_id=p_period;
  select count(*) into v_settled from settlements where period_id=p_period;
  if v_members<>v_settled then raise exception 'settlement_count_mismatch: members %, settled %',v_members,v_settled; end if;
  -- 与结算相同，更新用户段位也按 100 个小组拆批，避免一次 UPDATE 60 万行。
  insert into period_jobs(job_key,period_id,job_type,payload)
  select p_period||':apply-tier:'||batch_no,p_period,'apply_tier_groups',
         jsonb_build_object('min_group_id',min(id),'max_group_id',max(id))
  from (
    select id,((row_number() over(order by id)-1)/100)::integer batch_no
    from period_groups where period_id=p_period
  ) batches group by batch_no
  on conflict do nothing;
end $$;

-- 分批把结算后的段位更新回用户表，单事务最多约 5000 名用户。
create or replace function apply_settlement_groups(p_period bigint,p_min_group bigint,p_max_group bigint) returns void language plpgsql as $$
begin
  update users u set tier=s.new_tier,highest_tier=greatest(u.highest_tier,s.new_tier)
  from settlements s
  where s.period_id=p_period and s.group_id between p_min_group and p_max_group and s.user_id=u.id;
end $$;

-- 所有段位更新批次完成后，才结束旧周期并投递分组与奖励任务。
create or replace function finish_settlement(p_period bigint) returns void language plpgsql as $$
declare v_next bigint;
begin
  update periods set status='finished' where id=p_period;
  select id into v_next from periods where previous_period_id=p_period;
  if v_next is null then raise exception 'next_period_not_found'; end if;
  insert into period_jobs(job_key,period_id,job_type) values(v_next||':groups',v_next,'create_groups') on conflict do nothing;
  insert into period_jobs(job_key,period_id,job_type) values(p_period||':rewards',p_period,'create_rewards') on conflict do nothing;
end $$;

-- Backfills every reached tier that the user has never received before.
create or replace function create_reward_grants(p_period bigint) returns void language plpgsql as $$
begin
  insert into reward_grants(source_period_id,user_id,tier,idempotency_key,reward_snapshot)
  select p_period,s.user_id,r.tier,'weekly-tier:'||s.user_id||':'||r.tier,r.reward
  from settlements s join tier_rules r on r.tier<=s.new_tier where s.period_id=p_period
  on conflict(user_id,tier) do nothing;
  insert into period_jobs(job_key,period_id,job_type) values(p_period||':dispatch:initial',p_period,'dispatch_rewards') on conflict do nothing;
end $$;

-- Dispatches at most 1000 rewards per transaction to bound lock and WAL volume.
create or replace function dispatch_reward_batch(p_period bigint) returns integer language plpgsql as $$
declare v_count integer;
begin
  with batch as (
    select id,idempotency_key,user_id,reward_snapshot from reward_grants
    where source_period_id=p_period and status='pending' order by id for update skip locked limit 1000
  ), delivered as (
    insert into reward_deliveries(idempotency_key,user_id,reward_snapshot)
    select idempotency_key,user_id,reward_snapshot from batch on conflict do nothing returning idempotency_key
  )
  update reward_grants g set status='succeeded',delivered_at=now()
  where g.id in(select id from batch);
  get diagnostics v_count=row_count;
  return v_count;
end $$;

-- Creates deterministic-but-different-per-period groups and freezes stage/tier.
create or replace function create_period_groups(p_period bigint) returns void language plpgsql as $$
begin
  insert into period_groups(period_id,tier,group_no)
  select p_period,tier,grp from (select tier,ceil(row_number() over(partition by tier order by hashtextextended(id::text,p_period))/50.0)::int grp from users) x group by tier,grp on conflict do nothing;
  insert into period_members(period_id,group_id,user_id,tier,stage)
  select p_period,g.id,u.id,u.tier,u.stage from (select id,tier,stage,ceil(row_number() over(partition by tier order by hashtextextended(id::text,p_period))/50.0)::int grp from users) u join period_groups g on g.period_id=p_period and g.tier=u.tier and g.group_no=u.grp on conflict do nothing;
  insert into period_jobs(job_key,period_id,job_type) values(p_period||':activate',p_period,'activate_period') on conflict do nothing;
end $$;

create or replace function activate_period(p_period bigint) returns void language plpgsql as $$
declare v_users bigint; v_members bigint;
begin
  select count(*) into v_users from users;
  select count(*) into v_members from period_members where period_id=p_period;
  if v_users<>v_members then raise exception 'member_count_mismatch: users %, members %',v_users,v_members; end if;
  update periods set status='active' where id=p_period and status='preparing';
end $$;

create or replace function claim_period_job() returns table(id bigint,period_id bigint,job_type text,payload jsonb) language plpgsql as $$
begin
  -- 全局锁把所有 Worker 实例的数据库重任务总并发限制为 1。
  perform pg_advisory_xact_lock(861205);
  return query update period_jobs j set status='running',updated_at=now() where j.id=(select x.id from period_jobs x where (x.status='pending' and x.available_at<=now()) or (x.status='running' and x.updated_at<now()-interval '5 minutes') order by x.id for update skip locked limit 1) returning j.id,j.period_id,j.job_type,j.payload;
end $$;

create or replace function complete_period_job(p_job bigint) returns void language plpgsql as $$
declare v_period bigint; v_type text; v_remaining int;
begin
  update period_jobs set status='succeeded',last_error=null,updated_at=now() where id=p_job returning period_id,job_type into v_period,v_type;
  if v_type='settle_groups' then
    select count(*) into v_remaining from period_jobs where period_id=v_period and job_type='settle_groups' and status<>'succeeded';
    if v_remaining=0 then insert into period_jobs(job_key,period_id,job_type) values(v_period||':finalize',v_period,'finalize_settlement') on conflict do nothing; end if;
  end if;
  if v_type='apply_tier_groups' then
    select count(*) into v_remaining from period_jobs where period_id=v_period and job_type='apply_tier_groups' and status<>'succeeded';
    if v_remaining=0 then insert into period_jobs(job_key,period_id,job_type) values(v_period||':finish',v_period,'finish_settlement') on conflict do nothing; end if;
  end if;
  if v_type='dispatch_rewards' and exists(select 1 from reward_grants where source_period_id=v_period and status='pending') then
    insert into period_jobs(job_key,period_id,job_type) values(v_period||':dispatch:'||p_job,v_period,'dispatch_rewards') on conflict do nothing;
  end if;
end $$;
commit;
