begin;

create table users (
  id bigint primary key,
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

create table periods (
  id bigint generated always as identity primary key,
  starts_at timestamptz not null unique,
  ends_at timestamptz not null unique,
  status text not null check (status in ('active','settling','finished')),
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
  primary key(period_id,user_id), unique(group_id,user_id)
);
create table period_scores (
  period_id bigint not null references periods(id), user_id bigint not null references users(id),
  score bigint not null default 0 check(score>=0), primary key(period_id,user_id)
);
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
create table settlements (
  period_id bigint not null, user_id bigint not null, group_id bigint not null,
  rank integer not null, old_tier smallint not null, new_tier smallint not null,
  final_score bigint not null, promoted boolean not null,
  primary key(period_id,user_id)
);
create table reward_claims (
  user_id bigint not null references users(id), tier smallint not null references tier_rules(tier),
  reward_snapshot jsonb not null, claimed_at timestamptz not null default now(),
  primary key(user_id,tier)
);

create or replace function add_score(p_user bigint,p_channel text,p_points int,p_source_event text,p_duration_seconds int,p_now timestamptz) returns int language plpgsql as $$
declare v_period bigint; v_channel_limit int; v_min_duration int; v_channel_used int; v_total_used int;
begin
  if p_points <= 0 then raise exception 'invalid_points'; end if;
  if exists(select 1 from score_events where source_event_id=p_source_event) then return 0; end if;
  select daily_limit,min_duration_seconds into v_channel_limit,v_min_duration from channel_rules where channel=p_channel;
  if not found then raise exception 'invalid_channel'; end if;
  if p_duration_seconds<v_min_duration then raise exception 'duration_not_enough'; end if;
  select id into v_period from periods where status='active' and starts_at<=p_now and p_now<ends_at for update;
  if not found then raise exception 'no_active_period'; end if;
  perform pg_advisory_xact_lock(p_user);
  select coalesce(score,0) into v_channel_used from daily_scores where user_id=p_user and score_date=(p_now at time zone 'Asia/Shanghai')::date and channel=p_channel;
  select coalesce(sum(score),0) into v_total_used from daily_scores where user_id=p_user and score_date=(p_now at time zone 'Asia/Shanghai')::date;
  if v_channel_used+p_points>v_channel_limit then raise exception 'channel_daily_limit_exceeded'; end if;
  if v_total_used+p_points>500 then raise exception 'total_daily_limit_exceeded'; end if;
  insert into daily_scores values(p_user,(p_now at time zone 'Asia/Shanghai')::date,p_channel,p_points) on conflict(user_id,score_date,channel) do update set score=daily_scores.score+excluded.score;
  insert into period_scores values(v_period,p_user,p_points) on conflict(period_id,user_id) do update set score=period_scores.score+excluded.score;
  insert into score_events values(p_source_event,p_user,v_period,p_channel,p_points,p_duration_seconds,p_now);
  return p_points;
end $$;

create or replace function start_weekly_period(p_now timestamptz) returns bigint language plpgsql as $$
declare v_start timestamptz; v_period bigint;
begin
  v_start := date_trunc('week',p_now at time zone 'Asia/Shanghai') at time zone 'Asia/Shanghai' + interval '12 hours';
  if p_now<v_start then v_start:=v_start-interval '7 days'; end if;
  insert into periods(starts_at,ends_at,status) values(v_start,v_start+interval '7 days','active') on conflict(starts_at) do update set starts_at=excluded.starts_at returning id into v_period;
  insert into period_groups(period_id,tier,group_no) select v_period,tier,grp from (select tier,ceil(row_number() over(partition by tier order by id)::numeric/50)::int grp from users) x group by tier,grp on conflict do nothing;
  insert into period_members(period_id,group_id,user_id,tier) select v_period,g.id,u.id,u.tier from (select id,tier,ceil(row_number() over(partition by tier order by id)::numeric/50)::int grp from users) u join period_groups g on g.period_id=v_period and g.tier=u.tier and g.group_no=u.grp on conflict do nothing;
  return v_period;
end $$;

create or replace function settle_weekly_period(p_now timestamptz) returns bigint language plpgsql as $$
declare v_period bigint;
begin
  select id into v_period from periods where ends_at<=p_now and status in('active','settling') order by ends_at desc limit 1 for update;
  if not found then raise exception 'no_period_to_settle'; end if;
  update periods set status='settling' where id=v_period;
  insert into settlements(period_id,user_id,group_id,rank,old_tier,new_tier,final_score,promoted)
  select period_id,user_id,group_id,rn,old_tier,case when promoted then least(5,old_tier+1) else greatest(1,old_tier-1) end,score,promoted from (
    select x.*,rn<=ceil(cnt*promotion_percent/100.0) promoted from (
      select m.period_id,m.user_id,m.group_id,m.tier old_tier,coalesce(s.score,0) score,r.promotion_percent,
      row_number() over(partition by m.group_id order by coalesce(s.score,0) desc,m.user_id) rn,count(*) over(partition by m.group_id) cnt
      from period_members m left join period_scores s using(period_id,user_id) join tier_rules r on r.tier=m.tier where m.period_id=v_period
    ) x
  ) y on conflict(period_id,user_id) do nothing;
  update users u set tier=s.new_tier,highest_tier=greatest(u.highest_tier,s.new_tier) from settlements s where s.period_id=v_period and s.user_id=u.id;
  update periods set status='finished' where id=v_period;
  return v_period;
end $$;
commit;
