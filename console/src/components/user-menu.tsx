// 用户菜单（顶栏，设计 §7）：当前身份投影（GET /v1/auth/me——邮箱 / 显示
// 名 / 平台管理员标志 / 所属团队与角色 W1 只读展示）+ 退出（POST
// /v1/auth/logout）/ 全部退出（POST /v1/auth/logout-all）。会话失效的 401
// 经 api 层全局处置回登录页，本组件不重复处置。

import { useQuery } from "@tanstack/react-query";
import { KeyRound, LogOut, Users } from "lucide-react";
import { useEffect, useRef, useState } from "react";
import { useNavigate } from "react-router-dom";

import { me } from "@/api/endpoints";
import { getToken } from "@/api/client";
import { useAuth } from "@/auth";
import { Button } from "@/components/ui/button";
import { queryClient } from "@/query";

export function UserMenu() {
  const { logout, logoutAll } = useAuth();
  const navigate = useNavigate();
  const [open, setOpen] = useState(false);
  const rootRef = useRef<HTMLDivElement | null>(null);

  // Bearer 身份指示（P1-4，2026-09-25 审查 §3）：读到 API token 即按 token
  // 凭据形态操作（Bearer 优先不回落，会话 cookie 不参与）——审计与排障都
  // 需要知道这一点。只在面板打开的渲染时机求值（getToken 非响应式；开合
  // 菜单即重渲染，足够诚实且零订阅成本）。
  const viaApiToken = open && getToken() !== "";

  // Me 投影（登录后拉取）：staleTime 内不重复请求——启动探测已验证会话，
  // 这里取展示数据。
  const { data } = useQuery({
    queryKey: ["auth", "me"],
    queryFn: () => me(),
    staleTime: 5 * 60 * 1000,
    refetchOnWindowFocus: false,
  });

  const user = data?.user;
  const label = user?.display_name || user?.email || "Account";
  const initial = (label[0] ?? "?").toUpperCase();

  // 点击面板外关闭（轻量实现：fixed 背板会拦布局，改用 document 监听）。
  useEffect(() => {
    if (!open) return;
    function onPointerDown(e: PointerEvent) {
      if (rootRef.current && e.target instanceof Node && !rootRef.current.contains(e.target)) {
        setOpen(false);
      }
    }
    document.addEventListener("pointerdown", onPointerDown);
    return () => document.removeEventListener("pointerdown", onPointerDown);
  }, [open]);

  async function signOut(all: boolean) {
    setOpen(false);
    // 登出同时清 react-query 缓存（M9-9）：上一身份的 apps/部署等服务端
    // 状态不得泄给下一个会话（换人后直接复用旧缓存会闪现他人数据）。
    queryClient.clear();
    if (all) {
      await logoutAll();
    } else {
      await logout();
    }
    navigate("/login");
  }

  return (
    <div ref={rootRef} className="relative">
      <Button
        variant="ghost"
        size="sm"
        className="gap-2 px-2"
        data-testid="user-menu"
        aria-haspopup="menu"
        aria-expanded={open}
        onClick={() => setOpen((v) => !v)}
      >
        <span
          aria-hidden
          className="flex h-6 w-6 items-center justify-center rounded-full bg-primary text-[11px] font-bold text-primary-foreground"
        >
          {initial}
        </span>
        <span className="hidden max-w-[140px] truncate sm:inline">{label}</span>
      </Button>
      {open ? (
        <div
          role="menu"
          aria-label="Account"
          data-testid="user-menu-panel"
          className="absolute right-0 top-full z-30 mt-2 w-64 rounded-md border bg-background p-2 shadow-md"
        >
          {/* Bearer 身份指示条置顶（P1-4）：token 认证生效时明示凭据形态——
              否则 403「scope insufficient」之类文案解释不了身份从何而来。 */}
          {viaApiToken ? (
            <div
              data-testid="user-menu-token-note"
              className="mb-1 rounded-md bg-amber-500/10 px-2 py-1.5 text-[11px] leading-snug text-amber-800 dark:text-amber-300"
            >
              Acting via API token — session cookie is not used.
            </div>
          ) : null}
          <div className="space-y-0.5 px-2 py-1.5">
            <div className="truncate text-sm font-medium" data-testid="user-menu-name">
              {user?.display_name || user?.email || "Signed in"}
            </div>
            {user?.email ? (
              <div className="truncate text-xs text-muted-foreground" data-testid="user-menu-email">
                {user.email}
              </div>
            ) : null}
            {user?.is_platform_admin ? (
              <span
                data-testid="user-platform-admin"
                className="mt-1 inline-flex items-center rounded bg-amber-500/15 px-1.5 py-0.5 text-[10px] font-semibold uppercase tracking-wide text-amber-700 dark:text-amber-400"
              >
                Platform admin
              </span>
            ) : null}
          </div>
          {(data?.teams?.length ?? 0) > 0 ? (
            <div className="border-t px-2 py-1.5" data-testid="user-teams">
              <div className="flex items-center gap-1 pb-1 text-[10px] font-semibold uppercase tracking-widest text-muted-foreground/70">
                <Users aria-hidden className="h-3 w-3" />
                Teams
              </div>
              <ul className="space-y-0.5">
                {(data?.teams ?? []).map((t) => (
                  <li
                    key={t.team_id}
                    className="flex items-center justify-between gap-2 text-xs"
                    data-testid="user-team"
                  >
                    <span className="truncate">{t.team_name || t.team_slug}</span>
                    <span className="shrink-0 text-muted-foreground">{t.role}</span>
                  </li>
                ))}
              </ul>
            </div>
          ) : null}
          <div className="mt-1 space-y-0.5 border-t pt-1">
            {/* PAT 自服务页入口（设计 §7 用户菜单「PAT 页」）。 */}
            <Button
              variant="ghost"
              size="sm"
              role="menuitem"
              className="w-full justify-start"
              data-testid="user-menu-pat"
              onClick={() => {
                setOpen(false);
                navigate("/pat");
              }}
            >
              <KeyRound aria-hidden className="h-4 w-4" />
              Personal access tokens
            </Button>
            <Button
              variant="ghost"
              size="sm"
              role="menuitem"
              className="w-full justify-start"
              data-testid="logout"
              onClick={() => signOut(false)}
            >
              <LogOut aria-hidden className="h-4 w-4" />
              Sign out
            </Button>
            <Button
              variant="ghost"
              size="sm"
              role="menuitem"
              className="w-full justify-start text-muted-foreground"
              data-testid="logout-all"
              onClick={() => signOut(true)}
            >
              <LogOut aria-hidden className="h-4 w-4" />
              Sign out all devices
            </Button>
          </div>
        </div>
      ) : null}
    </div>
  );
}
