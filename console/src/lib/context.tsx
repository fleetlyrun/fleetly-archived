// 团队/项目上下文（v0.3 W2-S5，rbac-teams 设计 §7「顶栏：团队切换器 +
// 项目切换器」）：当前选择持久化 localStorage（键名沿仓内
// fleetly.console.* 前缀惯例）；项目选择随团队切换而失效（跨队项目无
// 意义）。资源页经 useProjectContext().projectRef 携带 `team/prj` 收窄
// （服务端 ?project= 语义，apps.proto/databases.proto ListXxxRequest）；
// 跨团队展示一律限定形 team/prj（D-W0-9）。
//
// 门语义：本模块派生的角色能力（useTeamCapabilities）只是前端体验门——
// viewer 隐藏写按钮等；服务端角色门（W2-S4 ResolvePermission）才是硬门。
// W3-S3 起能力判定按「当前项目覆写行优先于团队角色」（§3.3 B 形，双向
// 生效——Me.project_overrides 投影 W3-S2 已供数）：选中项目命中覆写行即
// 用覆写角色渲染按钮；未选项目时按团队角色。

import { useQuery } from "@tanstack/react-query";
import {
  createContext,
  useCallback,
  useContext,
  useMemo,
  useState,
  type ReactNode,
} from "react";

import { listProjects, me } from "@/api/endpoints";
import type { ProjectOverrideMembership, ProjectView, TeamMembership } from "@/api/types";

const CONTEXT_STORAGE_KEY = "fleetly.console.context";

type StoredContext = { team: string | null; project: string | null };

function loadStoredContext(): StoredContext {
  try {
    const raw = localStorage.getItem(CONTEXT_STORAGE_KEY);
    if (raw) {
      const parsed = JSON.parse(raw) as Partial<StoredContext>;
      return {
        team: typeof parsed.team === "string" ? parsed.team : null,
        project: typeof parsed.project === "string" ? parsed.project : null,
      };
    }
  } catch {
    // 存储不可用/形态漂移——回落无选择（全量可见集）。
  }
  return { team: null, project: null };
}

export interface TeamProjectContextValue {
  /** 我所在团队（Me 投影；平台管理员 = 全部团队只读）。 */
  teams: TeamMembership[];
  /** 可见项目全集（团队选择后的子集由 selectedTeam 过滤）。 */
  projects: ProjectView[];
  /** 我的项目角色覆写行（Me 投影——能力判定按覆写优先，W3-S3 消费）。 */
  projectOverrides: ProjectOverrideMembership[];
  /** 当前选中团队（slug 匹配；null = 未收窄）。 */
  selectedTeamSlug: string | null;
  /** 当前选中项目 slug（队内唯一；null = 未收窄——跨队随团队切换失效）。 */
  selectedProjectSlug: string | null;
  /** 当前选中项目的归属团队 slug（限定形第一段；无选择 = ""）。 */
  projectRef: string;
  /** 限定形 app 引用（有项目上下文 = team/prj/app，否则原名透传）。 */
  qualifyApp(name: string): string;
  setTeamSlug(slug: string | null): void;
  setProjectSlug(slug: string | null): void;
}

const noop = () => {};

/** 无 Provider 时的中性缺省（全量可见、零收窄）——直连页面测试复用。 */
const NeutralContext: TeamProjectContextValue = {
  teams: [],
  projects: [],
  projectOverrides: [],
  selectedTeamSlug: null,
  selectedProjectSlug: null,
  projectRef: "",
  qualifyApp: (name: string) => name,
  setTeamSlug: noop,
  setProjectSlug: noop,
};

const TeamProjectContext = createContext<TeamProjectContextValue>(NeutralContext);

// Me 投影查询（provider / useTeamCapabilities / useIsPlatformAdmin 三处
// 同键共享缓存——同 key 去重，不会多发请求；选项必须一致地静默失败）。
// 导出给「挂载即拉、且按能力门收窄」的读面做 Me 就绪门控：能力门 fail-open
// 窗口（Me 未达）内不发起注定 403 的请求——只看 canAdminResources 会在
// fail-open 期误发（2026-09-29 Deployments 页 webhook URL 行实证）。
export function useMeQuery() {
  return useQuery({
    queryKey: ["auth", "me"],
    queryFn: () => me(),
    staleTime: 5 * 60 * 1000,
    refetchOnWindowFocus: false,
    retry: false,
  });
}

export function TeamProjectProvider({ children }: { children: ReactNode }) {
  const [selected, setSelected] = useState<StoredContext>(loadStoredContext);

  // Me 投影（与 user-menu 同键共享缓存）+ 可见项目集。静默失败 = 上下文
  // 降级为无选择（资源页全量可见集）——列表读面失败不阻塞页面。
  const meQuery = useMeQuery();
  const projectsQuery = useQuery({
    queryKey: ["projects", "context"],
    queryFn: () => listProjects(),
    staleTime: 60_000,
    refetchOnWindowFocus: false,
    retry: false,
  });

  const teams = useMemo(() => meQuery.data?.teams ?? [], [meQuery.data]);
  const projects = useMemo(() => projectsQuery.data?.projects ?? [], [projectsQuery.data]);
  const projectOverrides = useMemo(
    () => meQuery.data?.project_overrides ?? [],
    [meQuery.data],
  );

  // 持久化落点：setter 内直写（不经 effect—— setState-in-effect 纪律）。
  const persist = useCallback((next: StoredContext) => {
    try {
      localStorage.setItem(CONTEXT_STORAGE_KEY, JSON.stringify(next));
    } catch {
      // 忽略：持久化失败只影响下次启动的记忆，不影响会话内行为。
    }
  }, []);

  const setTeamSlug = useCallback(
    (slug: string | null) => {
      // 切团队即失效项目选择（项目 slug 只在队内唯一）。
      const next = { team: slug, project: null };
      setSelected(next);
      persist(next);
    },
    [persist],
  );

  const setProjectSlug = useCallback(
    (slug: string | null) => {
      setSelected((prev) => {
        const next = { ...prev, project: slug };
        persist(next);
        return next;
      });
    },
    [persist],
  );

  // 团队选择漂移守卫：持久化的 slug 不在可见集（被移出团队/存储串台）→
  // 渲染期派生归位（不改持久化层——重新可见时记忆仍成立）。
  const effective = useMemo<StoredContext>(() => {
    if (selected.team && teams.length > 0 && !teams.some((t) => t.team_slug === selected.team)) {
      return { team: null, project: null };
    }
    return selected;
  }, [selected, teams]);

  const value = useMemo<TeamProjectContextValue>(() => {
    const selectedProject =
      effective.team && effective.project
        ? (projects.find(
            (p) => p.team_slug === effective.team && p.slug === effective.project,
          ) ?? null)
        : null;
    return {
      teams,
      projects,
      projectOverrides,
      selectedTeamSlug: effective.team,
      selectedProjectSlug: effective.project,
      projectRef:
        effective.team && selectedProject ? `${effective.team}/${selectedProject.slug}` : "",
      qualifyApp: (name: string) =>
        effective.team && selectedProject
          ? `${effective.team}/${selectedProject.slug}/${name}`
          : name,
      setTeamSlug,
      setProjectSlug,
    };
  }, [teams, projects, projectOverrides, effective, setTeamSlug, setProjectSlug]);

  return <TeamProjectContext.Provider value={value}>{children}</TeamProjectContext.Provider>;
}

export function useProjectContext(): TeamProjectContextValue {
  return useContext(TeamProjectContext);
}

// ── 角色能力（§3.2 矩阵的前端投影；仅体验门）────────────────────────────

/** 四档团队角色的蕴含排序（owner > admin > developer > viewer）。 */
const ROLE_RANK: Record<string, number> = {
  viewer: 0,
  developer: 1,
  admin: 2,
  owner: 3,
};

export function roleAtLeast(role: string | null | undefined, min: string): boolean {
  if (!role) return false;
  return (ROLE_RANK[role] ?? -1) >= (ROLE_RANK[min] ?? 99);
}

export interface TeamCapabilities {
  /** 我的团队角色（无团队上下文 = null——能力全关，服务端亦拒）。 */
  role: string | null;
  /** read 面：所有角色恒真（viewer 起步）。 */
  canRead: boolean;
  /** deploy 面：developer+（部署/回滚/env 写/cron/终端）；平台管理员恒 false。 */
  canDeploy: boolean;
  /** admin 面：admin+（app 删除/env 明文/secrets/构建/库生命周期）；平台管理员恒 false。 */
  canAdminResources: boolean;
  /** owner 面：成员/角色/邀请/项目创建与删除（身份面——平台管理员按角色照常）。 */
  canManageTeam: boolean;
  /** 邀请面：owner/admin（§3.1；所邀角色 ≤ 自身由服务端把关）——身份面。 */
  canInvite: boolean;
}

/**
 * 能力全开（Me 投影不可得时的缺省——见 useTeamCapabilities 头注 fail-open
 * 口径）。平台管理员**不得**落到这里：双门判定先于本缺省（同函数内）。
 */
const OPEN_CAPABILITIES: TeamCapabilities = {
  role: null,
  canRead: true,
  canDeploy: true,
  canAdminResources: true,
  canManageTeam: true,
  canInvite: true,
};

export function capabilitiesForRole(role: string | null | undefined): TeamCapabilities {
  return {
    role: role ?? null,
    canRead: roleAtLeast(role, "viewer"),
    canDeploy: roleAtLeast(role, "developer"),
    canAdminResources: roleAtLeast(role, "admin"),
    canManageTeam: roleAtLeast(role, "owner"),
    canInvite: roleAtLeast(role, "admin"),
  };
}

/**
 * 当前选中团队（及项目）的能力视图（未选团队时回落「我所在唯一团队」——
 * 单队用户（个人队 owner）零选择也有正确能力；多队未选 = 无能力，促显式
 * 选择）。
 *
 * 覆写优先（W3-S3 消费 Me.project_overrides，rbac-teams §3.3 B 形）：选中
 * 项目命中覆写行即用覆写角色——双向生效（升权者见写按钮、降权者隐藏）；
 * 行匹配按 team_id + prj_slug（覆写只对团队成员存在，owner 恒无行——服务
 * 端保证）。未选项目时按团队角色。
 *
 * fail-open 口径：Me 投影不可得（teams 空——接口失败/测试直挂页面）时
 * 返回能力全开。前端门只是体验优化，服务端角色门（ResolvePermission）
 * 才是硬门；无角色信息就隐藏写按钮只会制造「按钮无故消失」的错觉，方向
 * 性错误——如实全开，让 403 信封说话。
 *
 * 双门模型（P0-3，2026-09-25 审查 docs/reports/2026-09-25-console-ui-review.md
 * §2；服务端裁决 = internal/api/ownership.go ResolvePermission：平台管理员
 * 无条件 levelRead，即使他是团队 owner）：能力分两个平面——
 *   - 资源面（canDeploy / canAdminResources，写；canRead 恒真）：平台管理
 *     员**一律 false**——服务端 403 硬拒，前端再全开就是「按钮可见、点了
 *     必炸」（这正是 P0-3）；
 *   - 身份面（canManageTeam / canInvite，建队/建项目/邀请/成员管理/PAT/
 *     用户管理）：服务端**不禁**平台管理员（实测 founder 建队建项目发邀请
 *     均成功）——保持按团队角色解析，不因平台身份缩权。
 * fail-open 与双门的关系：fail-open 只在 Me 投影不可得时生效；Me 可得且
 * is_platform_admin=true 时**必须先于 fail-open 关门**（平台管理员在 Me
 * 投影里常以「全部团队」形态出现、teams 非空，但即便 teams 空——无团队
 * 平台管理员——也不得落 OPEN_CAPABILITIES，服务端同样是硬拒）。
 */
export function useTeamCapabilities(): TeamCapabilities {
  const { teams, projectOverrides, selectedTeamSlug, selectedProjectSlug } =
    useProjectContext();
  const { data: meData } = useMeQuery();
  const isPlatformAdmin = meData?.user?.is_platform_admin === true;
  const role = useMemo(() => {
    let membership: TeamMembership | null = null;
    if (selectedTeamSlug) {
      membership = teams.find((t) => t.team_slug === selectedTeamSlug) ?? null;
    } else if (teams.length === 1) {
      membership = teams[0];
    }
    if (!membership) return null;
    // 覆写行优先于团队角色（仅选中项目时判定；team_id + prj_slug 命中）。
    if (selectedProjectSlug) {
      const override = projectOverrides.find(
        (o) =>
          o.team_id === membership?.team_id && o.prj_slug === selectedProjectSlug,
      );
      if (override) return override.role ?? null;
    }
    return membership.role ?? null;
  }, [teams, projectOverrides, selectedTeamSlug, selectedProjectSlug]);
  // 双门第一判（先于 fail-open——见头注）：平台管理员资源面硬关、身份面
  // 按角色。canRead 强制恒真（无角色时 capabilitiesForRole 会给 false，
  // 但平台管理员对全部资源只读是服务端保证的）。
  if (isPlatformAdmin) {
    return {
      ...capabilitiesForRole(role),
      canRead: true,
      canDeploy: false,
      canAdminResources: false,
    };
  }
  if (teams.length === 0) return OPEN_CAPABILITIES;
  return capabilitiesForRole(role);
}

/** 平台管理员标志（Me 投影；透明度/平台管理面的导航与页面门）。 */
export function useIsPlatformAdmin(): boolean {
  const { data } = useMeQuery();
  return data?.user?.is_platform_admin === true;
}
