\set ON_ERROR_STOP on

insert into users(id) select generate_series(1,100);

-- Bootstrap the first period, create two 50-person groups, and activate it.
select * from rollover_period('2026-08-10 12:00:00+08');
select create_period_groups((select id from periods where starts_at='2026-08-10 12:00:00+08'));
select activate_period((select id from periods where starts_at='2026-08-10 12:00:00+08'));

do $$
declare v_period bigint; v_groups bigint; v_members bigint;
begin
  select id into v_period from periods where starts_at='2026-08-10 12:00:00+08';
  select count(*) into v_groups from period_groups where period_id=v_period;
  select count(*) into v_members from period_members where period_id=v_period;
  if v_groups<>2 or v_members<>100 then raise exception 'bootstrap grouping failed: groups %, members %',v_groups,v_members; end if;
end $$;

-- Score API database contract: duration, idempotency, channel cap, and total cap.
do $$
declare v_result integer;
begin
  select add_score(1,'channel_a',100,'event-1',180,'2026-08-10 12:01:00+08') into v_result;
  if v_result<>100 then raise exception 'score not accepted'; end if;
  select add_score(1,'channel_a',100,'event-1',180,'2026-08-10 12:01:01+08') into v_result;
  if v_result<>0 then raise exception 'duplicate event was not idempotent'; end if;
  perform add_score(1,'channel_a',200,'event-2',180,'2026-08-10 12:04:00+08');
  perform add_score(1,'channel_b',200,'event-3',180,'2026-08-10 12:07:00+08');
  begin
    perform add_score(2,'channel_a',10,'event-short',179,'2026-08-10 12:01:00+08');
    raise exception 'short activity was accepted';
  exception when others then
    if sqlerrm='short activity was accepted' then raise; end if;
  end;
  begin
    perform add_score(1,'channel_c',1,'event-over-total',180,'2026-08-10 12:10:00+08');
    raise exception 'daily total overflow was accepted';
  exception when others then
    if sqlerrm='daily total overflow was accepted' then raise; end if;
  end;
end $$;

-- Give every user a deterministic score so settlement order is testable.
insert into period_scores(period_id,user_id,score)
select p.id,u.id,1000-u.id from periods p cross join users u
where p.starts_at='2026-08-10 12:00:00+08'
on conflict(period_id,user_id) do update set score=excluded.score;

-- Pick two users from the same group and force a score tie. The earlier
-- reached_at must win even when its user_id is larger.
do $$
declare v_period bigint; v_group bigint; v_early bigint; v_late bigint;
begin
  select id into v_period from periods where starts_at='2026-08-10 12:00:00+08';
  select group_id into v_group from period_members where period_id=v_period order by group_id limit 1;
  select max(user_id),min(user_id) into v_early,v_late from (select user_id from period_members where period_id=v_period and group_id=v_group order by user_id limit 2) x;
  update period_scores set score=2000,reached_at='2026-08-10 12:10:00+08' where period_id=v_period and user_id=v_early;
  update period_scores set score=2000,reached_at='2026-08-10 12:11:00+08' where period_id=v_period and user_id=v_late;
end $$;

-- Profile data may change, but the old period keeps its frozen stage.
update users set stage=2;

-- Prepare before noon, then perform the lightweight 12:00 rollover.
select prepare_next_period('2026-08-17 11:30:00+08');
select * from rollover_period('2026-08-17 12:00:00+08');

do $$
declare v_old bigint; v_new bigint; v_new_score_period bigint;
begin
  select id into v_old from periods where starts_at='2026-08-10 12:00:00+08';
  select id into v_new from periods where starts_at='2026-08-17 12:00:00+08';
  if (select status from periods where id=v_old)<>'settling' then raise exception 'old period not settling'; end if;
  if (select status from periods where id=v_new)<>'preparing' then raise exception 'new period not preparing'; end if;
  perform add_score(2,'channel_a',10,'new-period-event',180,'2026-08-17 12:00:00+08');
  select period_id into v_new_score_period from score_events where source_event_id='new-period-event';
  if v_new_score_period<>v_new then raise exception '12:00 event assigned to wrong period'; end if;
end $$;

-- Run settlement business functions. Tier 1 has two groups of 50, so 80 users promote.
select settle_period_tier((select id from periods where starts_at='2026-08-10 12:00:00+08'),tier)
from generate_series(1,5) tier;
select finalize_settlement((select id from periods where starts_at='2026-08-10 12:00:00+08'));

do $$
declare v_old bigint; v_settled bigint; v_tier2 bigint; v_tier1 bigint;
begin
  select id into v_old from periods where starts_at='2026-08-10 12:00:00+08';
  select count(*) into v_settled from settlements where period_id=v_old;
  select count(*) filter(where tier=2),count(*) filter(where tier=1) into v_tier2,v_tier1 from users;
  if v_settled<>100 then raise exception 'settlement count %, want 100',v_settled; end if;
  if v_tier2<>80 or v_tier1<>20 then raise exception 'tier result wrong: tier2 %, tier1 %',v_tier2,v_tier1; end if;
  if exists(select 1 from settlements where period_id=v_old and stage<>0) then raise exception 'settlement did not preserve frozen stage'; end if;
  if exists(select 1 from (select group_id,rank,reached_at,user_id,lag(reached_at) over(partition by group_id order by rank) previous_reached from settlements where period_id=v_old and final_score=2000) x where rank=2 and reached_at<previous_reached) then raise exception 'reached_at tie order is wrong'; end if;
end $$;

-- Generate each reached-tier reward once and dispatch all pending batches.
select create_reward_grants((select id from periods where starts_at='2026-08-10 12:00:00+08'));
select create_reward_grants((select id from periods where starts_at='2026-08-10 12:00:00+08'));
do $$
declare v_old bigint;
begin
  select id into v_old from periods where starts_at='2026-08-10 12:00:00+08';
  while exists(select 1 from reward_grants where source_period_id=v_old and status='pending') loop
    perform dispatch_reward_batch(v_old);
  end loop;
end $$;

do $$
declare v_grants bigint; v_deliveries bigint;
begin
  select count(*) into v_grants from reward_grants;
  select count(*) into v_deliveries from reward_deliveries;
  if v_grants<>180 or v_deliveries<>180 then raise exception 'reward result wrong: grants %, deliveries %',v_grants,v_deliveries; end if;
end $$;

-- Regroup using the settled tiers and expose the new leaderboard.
select create_period_groups((select id from periods where starts_at='2026-08-17 12:00:00+08'));
select activate_period((select id from periods where starts_at='2026-08-17 12:00:00+08'));

do $$
declare v_new bigint; v_members bigint; v_max_group bigint;
begin
  select id into v_new from periods where starts_at='2026-08-17 12:00:00+08';
  select count(*) into v_members from period_members where period_id=v_new;
  select max(c) into v_max_group from (select count(*) c from period_members where period_id=v_new group by group_id) x;
  if v_members<>100 or v_max_group>50 then raise exception 'regroup failed: members %, max group %',v_members,v_max_group; end if;
  if exists(select 1 from period_members where period_id=v_new and stage<>2) then raise exception 'new period did not freeze current stage'; end if;
  if (select status from periods where id=v_new)<>'active' then raise exception 'new period not active'; end if;
end $$;

select 'weekly contest integration workflow passed' as result;
