// 路由总装：认证门卫三态（启动 Me 探测 splash → 未登录〔登录页/邀请页/
// 深链暂存〕→ 已登录应用面）+ 全局 401 监听（会话失效清本地态回登录页；
// 告警面收敛到登录页提交动作自身——P1-2 顺带，见 Gate 内注）。
//
// 路由级代码分割（2026-09-25 用户裁决「js 拆多文件缩短加载」）：登录/邀请
// 与应用壳（Layout）保持 eager（首屏与登录后落地最小串行依赖），其余页面
// 全部 lazy——单 bundle 时代整站 296KB gzip 首屏全量下载，慢链路 30-60s；
// 拆分后首屏只拉壳+当前路由（xterm/uplot 等只在所属路由 chunk 里按需取），
// vendor 再经 rolldown advancedChunks 稳定分组（跨版本缓存不失效）。

import { QueryClientProvider } from "@tanstack/react-query";
import { Loader2 } from "lucide-react";
import { lazy, useEffect } from "react";
import {
  BrowserRouter,
  Navigate,
  Route,
  Routes,
  useLocation,
} from "react-router-dom";

import { AuthProvider, useAuth } from "@/auth";
import { setUnauthorizedListener } from "@/api/client";
import { Layout } from "@/components/layout";
import { HomePage } from "@/pages/HomePage";
import { InvitePage } from "@/pages/InvitePage";
import { LoginPage } from "@/pages/LoginPage";
import { TeamProjectProvider } from "@/lib/context";
import { markSignedOut } from "@/lib/signed-out";
import { queryClient } from "@/query";

/** 路由页 lazy 装配（页面均命名导出——统一映射 default）。 */
const lazyPage = (load: () => Promise<{ [k: string]: unknown }>, name: string) =>
  lazy(() => load().then((m) => ({ default: m[name] as React.ComponentType })));

const AppsPage = lazyPage(() => import("@/pages/AppsPage"), "AppsPage");
const AppDetailLayout = lazyPage(() => import("@/pages/AppDetailLayout"), "AppDetailLayout");
const AppOverviewPage = lazyPage(() => import("@/pages/AppOverviewPage"), "AppOverviewPage");
const AppContainersPage = lazyPage(() => import("@/pages/AppContainersPage"), "AppContainersPage");
const AppDeploymentsPage = lazyPage(() => import("@/pages/AppDeploymentsPage"), "AppDeploymentsPage");
const AppBuildsPage = lazyPage(() => import("@/pages/AppBuildsPage"), "AppBuildsPage");
const AppLogsPage = lazyPage(() => import("@/pages/AppLogsPage"), "AppLogsPage");
const AppEnvPage = lazyPage(() => import("@/pages/AppEnvPage"), "AppEnvPage");
const AppSecretsPage = lazyPage(() => import("@/pages/AppSecretsPage"), "AppSecretsPage");
const AppConfigsPage = lazyPage(() => import("@/pages/AppConfigsPage"), "AppConfigsPage");
const AppDomainsPage = lazyPage(() => import("@/pages/AppDomainsPage"), "AppDomainsPage");
const AppTerminalPage = lazyPage(() => import("@/pages/AppTerminalPage"), "AppTerminalPage");
const DatabasesPage = lazyPage(() => import("@/pages/DatabasesPage"), "DatabasesPage");
const DatabaseDetailPage = lazyPage(() => import("@/pages/DatabaseDetailPage"), "DatabaseDetailPage");
const EventsPage = lazyPage(() => import("@/pages/EventsPage"), "EventsPage");
const PatPage = lazyPage(() => import("@/pages/PatPage"), "PatPage");
const ProjectsPage = lazyPage(() => import("@/pages/ProjectsPage"), "ProjectsPage");
const ProjectDetailPage = lazyPage(() => import("@/pages/ProjectDetailPage"), "ProjectDetailPage");
const SystemPage = lazyPage(() => import("@/pages/SystemPage"), "SystemPage");
const TeamPage = lazyPage(() => import("@/pages/TeamPage"), "TeamPage");
const TeamsPage = lazyPage(() => import("@/pages/TeamsPage"), "TeamsPage");
const AdminPage = lazyPage(() => import("@/pages/AdminPage"), "AdminPage");
const AuditPage = lazyPage(() => import("@/pages/AuditPage"), "AuditPage");

function RequireAuth({ children }: { children: React.ReactNode }) {
  const { authed } = useAuth();
  const location = useLocation();
  if (!authed) {
    return <Navigate to="/login" state={{ from: location.pathname }} replace />;
  }
  return <>{children}</>;
}

/** 启动 Me 探测进行中：占位 splash（避免登录页闪烁）。 */
function AuthSplash() {
  return (
    <div
      className="flex min-h-screen items-center justify-center bg-muted/30"
      data-testid="auth-splash"
    >
      <Loader2 aria-hidden className="h-6 w-6 animate-spin text-muted-foreground" />
    </div>
  );
}

/**
 * 匿名态路由：/login 直达登录页；邀请链接页可达（未登录分支——提示先
 * 登录/注册）；其余深链（/apps/...）经 Navigate 暂存 from（含查询串，
 * 邀请链接回跳依赖它）后落登录页，登录成功 navigate(from) 恢复。
 */
function AnonRoutes() {
  const location = useLocation();
  if (location.pathname === "/login") {
    return <LoginPage />;
  }
  return (
    <Routes>
      <Route path="/auth/invite" element={<InvitePage />} />
      <Route
        path="*"
        element={
          <Navigate
            to="/login"
            state={{ from: location.pathname + location.search }}
            replace
          />
        }
      />
    </Routes>
  );
}

/** 已登录应用面（含邀请页的已登录分支——进入即自动 accept）。 */
function AuthedRoutes() {
  return (
    // 团队/项目上下文（W2-S5 顶栏切换器的数据源与资源页收窄来源）只服务
    // 已登录面——匿名路由无需它。
    <TeamProjectProvider>
      <Routes>
        <Route element={<RequireAuth><Layout /></RequireAuth>}>
          <Route path="/" element={<HomePage />} />
          <Route path="/apps" element={<AppsPage />} />
          <Route path="/apps/:name" element={<AppDetailLayout />}>
            <Route index element={<AppOverviewPage />} />
            {/* Containers 紧随 Overview（2026-09-29 IA 裁决：运行真相第二
                优先——对齐 dokploy 把 Containers 放中部显眼位）。 */}
            <Route path="containers" element={<AppContainersPage />} />
            <Route path="deployments" element={<AppDeploymentsPage />} />
            <Route path="builds" element={<AppBuildsPage />} />
            <Route path="logs" element={<AppLogsPage />} />
            <Route path="env" element={<AppEnvPage />} />
            <Route path="secrets" element={<AppSecretsPage />} />
            <Route path="configs" element={<AppConfigsPage />} />
            <Route path="domains" element={<AppDomainsPage />} />
            <Route path="terminal" element={<AppTerminalPage />} />
          </Route>
          <Route path="/databases" element={<DatabasesPage />} />
          <Route path="/databases/:name" element={<DatabaseDetailPage />} />
          <Route path="/pat" element={<PatPage />} />
          <Route path="/system" element={<SystemPage />} />
          <Route path="/events" element={<EventsPage />} />
          <Route path="/teams" element={<TeamsPage />} />
          <Route path="/teams/:teamId" element={<TeamPage />} />
          <Route path="/projects" element={<ProjectsPage />} />
          <Route path="/projects/:projectId" element={<ProjectDetailPage />} />
          <Route path="/admin/users" element={<AdminPage />} />
          <Route path="/admin/audit" element={<AuditPage />} />
          <Route path="/auth/invite" element={<InvitePage />} />
          <Route path="*" element={<Navigate to="/" replace />} />
        </Route>
      </Routes>
    </TeamProjectProvider>
  );
}

function Gate() {
  const { status, clearSession } = useAuth();

  useEffect(() => {
    // App 级 401 监听：任意资源请求被拒（会话/令牌失效或被吊销）→ 清本地
    // 凭据回登录页（服务端 cookie 由失效响应自身清，不另发注销请求）。告警
    // 面不在此设（P1-2 顺带收敛）：登录页错误态归提交动作自身——全局 401
    // 只负责弹回 + 置一次性中性提示标志（登录页渲染「You have been signed
    // out.」muted 条；主动登出走用户菜单的 logout 路径，不置位、无提示）。
    setUnauthorizedListener(() => {
      markSignedOut();
      clearSession();
    });
    return () => setUnauthorizedListener(null);
  }, [clearSession]);

  return (
    <QueryClientProvider client={queryClient}>
      {status === "loading" ? (
        <AuthSplash />
      ) : status === "anon" ? (
        <AnonRoutes />
      ) : (
        <AuthedRoutes />
      )}
    </QueryClientProvider>
  );
}

export function App() {
  return (
    <AuthProvider>
      <BrowserRouter basename="/ui">
        <Gate />
      </BrowserRouter>
    </AuthProvider>
  );
}
