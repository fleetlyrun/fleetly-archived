// 应用详情标题栏动作行测试（§5 app Stop/Start/Redeploy，databases 标题栏
// 同款）：按钮按角色门与挂起位投影（owner=developer+admin 全见；developer
// 只见 Redeploy；平台管理员全隐）；挂起态 Stop→Start 翻转 + Redeploy 禁用；
// 三个动作的载荷与失效面；错误信封原位渲染。渲染组件单挂——不经
// AppDetailLayout（那是路由壳，副作用面大）。

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { beforeEach, describe, expect, it, vi } from "vitest";

import { AppHeaderActions } from "@/components/app-header-actions";
import { TeamProjectProvider } from "@/lib/context";
import { setToken } from "@/api/client";

beforeEach(() => {
  window.localStorage.clear();
});

const CALLS: Array<{ url: string; method?: string; body?: unknown }> = [];

/** 动作行 stub：Me（角色开关）+ suspend/resume/rollback 三写面捕获。 */
function stubActions(role: string, isPlatformAdmin: boolean) {
  CALLS.length = 0;
  return vi.fn().mockImplementation((url: string, init?: { method?: string; body?: string }) => {
    const u = String(url);
    CALLS.push({ url: u, method: init?.method, body: init?.body ? JSON.parse(init.body) : undefined });
    if (u.endsWith("/auth/me")) {
      return Promise.resolve({
        ok: true, status: 200, statusText: "",
        json: () =>
          Promise.resolve({
            user: { id: "01U1", email: "f@t.test", is_platform_admin: isPlatformAdmin },
            teams: [{ team_id: "01TEAM", team_slug: "acme", team_name: "Acme", role }],
            project_overrides: [],
          }),
      });
    }
    if (init?.method === "POST" && u.endsWith("/suspend")) {
      return Promise.resolve({ ok: true, status: 200, statusText: "", json: () => Promise.resolve({ app: { name: "demo", suspended: true } }) });
    }
    if (init?.method === "POST" && u.endsWith("/resume")) {
      return Promise.resolve({ ok: true, status: 200, statusText: "", json: () => Promise.resolve({ app: { name: "demo", suspended: false }, deployment_id: "dep_start" }) });
    }
    if (init?.method === "POST" && u.endsWith("/rollbacks")) {
      if (CALLS.filter((c) => c.url.endsWith("/rollbacks")).length === 99) {
        return Promise.resolve({ ok: false, status: 409, statusText: "", json: () => Promise.resolve({ code: "E_APP_SUSPENDED", message: "app is suspended", suggestion: "resume first" }) });
      }
      return Promise.resolve({ ok: true, status: 200, statusText: "", json: () => Promise.resolve({ deployment_id: "dep_re", desired_hash: "h" }) });
    }
    return Promise.resolve({ ok: true, status: 200, statusText: "", json: () => Promise.resolve({}) });
  });
}

function renderActions(suspended: boolean) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <MemoryRouter>
      <QueryClientProvider client={client}>
        <TeamProjectProvider>
          <AppHeaderActions
            app={{ id: "a1", name: "demo", suspended: suspended ? true : undefined }}
          />
        </TeamProjectProvider>
      </QueryClientProvider>
    </MemoryRouter>,
  );
}

describe("AppHeaderActions projection", () => {
  it("owner sees Redeploy + Stop; running app has no Start", async () => {
    setToken("flt_test");
    vi.stubGlobal("fetch", stubActions("owner", false));
    renderActions(false);

    // Me 投影落地后按钮齐备（fail-open 窗口同样全开——稳态断言）。
    await waitFor(() => expect(screen.getByTestId("app-redeploy-button")).toBeEnabled());
    expect(screen.getByTestId("app-stop-button")).toBeEnabled();
    expect(screen.queryByTestId("app-start-button")).not.toBeInTheDocument();
  });

  it("suspended app flips Stop→Start and disables Redeploy (server refuses deploys while suspended)", async () => {
    setToken("flt_test");
    vi.stubGlobal("fetch", stubActions("owner", false));
    renderActions(true);

    await waitFor(() => expect(screen.getByTestId("app-start-button")).toBeEnabled());
    expect(screen.queryByTestId("app-stop-button")).not.toBeInTheDocument();
    expect(screen.getByTestId("app-redeploy-button")).toBeDisabled();
  });

  it("developer sees Redeploy only (Stop/Start are admin scope)", async () => {
    setToken("flt_test");
    vi.stubGlobal("fetch", stubActions("developer", false));
    renderActions(false);

    // Stop 消失 = Me 投影落地（fail-open 窗口关闭的稳态信号）——再断言
    // Redeploy 独存（deploy 面 developer 有）。
    await waitFor(() =>
      expect(screen.queryByTestId("app-stop-button")).not.toBeInTheDocument(),
    );
    expect(screen.getByTestId("app-redeploy-button")).toBeEnabled();
    expect(screen.queryByTestId("app-start-button")).not.toBeInTheDocument();
  });

  it("platform admin sees no action buttons (P0-3 resource face read-only)", async () => {
    setToken("flt_test");
    vi.stubGlobal("fetch", stubActions("owner", true));
    renderActions(false);

    // fail-open 窗口先过，Me 落地后能力门统一关门 → 全隐。
    await waitFor(() =>
      expect(screen.queryByTestId("app-redeploy-button")).not.toBeInTheDocument(),
    );
    expect(screen.queryByTestId("app-stop-button")).not.toBeInTheDocument();
    expect(screen.queryByTestId("app-start-button")).not.toBeInTheDocument();
  });
});

describe("AppHeaderActions mutations", () => {
  it("Stop POSTs /suspend; Start POSTs /resume; Redeploy POSTs /rollbacks with empty target (current revision replay)", async () => {
    setToken("flt_test");
    vi.stubGlobal("fetch", stubActions("owner", false));
    const user = userEvent.setup();

    const { unmount } = renderActions(false);
    await user.click(await screen.findByTestId("app-stop-button"));
    await waitFor(() => {
      const post = CALLS.find((c) => c.method === "POST" && c.url.endsWith("/v1/apps/demo/suspend"));
      expect(post).toBeTruthy();
    });
    unmount();

    CALLS.length = 0;
    const second = renderActions(true);
    await user.click(await screen.findByTestId("app-start-button"));
    await waitFor(() => {
      const post = CALLS.find((c) => c.method === "POST" && c.url.endsWith("/v1/apps/demo/resume"));
      expect(post).toBeTruthy();
    });
    second.unmount();

    CALLS.length = 0;
    const third = renderActions(false);
    const redeploy = await screen.findByTestId("app-redeploy-button");
    await waitFor(() => expect(redeploy).toBeEnabled());
    await user.click(redeploy);
    await waitFor(() => {
      const post = CALLS.find((c) => c.method === "POST" && c.url.endsWith("/v1/apps/demo/rollbacks"));
      expect(post).toBeTruthy();
      // 空 target = 保留窗最新成功版本（现行 active revision）——重放当前版。
      expect(post?.body).toEqual({});
    });
    third.unmount();
  });

  it("action failure renders the error envelope in place (no silent swallow)", async () => {
    setToken("flt_test");
    const fetchMock = stubActions("owner", false);
    const inner = fetchMock.getMockImplementation();
    vi.stubGlobal(
      "fetch",
      vi.fn().mockImplementation((url: string, init?: { method?: string; body?: string }) => {
        const u = String(url);
        if (init?.method === "POST" && u.endsWith("/suspend")) {
          return Promise.resolve({
            ok: false, status: 409, statusText: "",
            json: () => Promise.resolve({ code: "E_APP_SUSPENDED", message: "app is suspended", suggestion: "resume first" }),
          });
        }
        return inner?.(url, init);
      }),
    );
    const user = userEvent.setup();
    renderActions(false);

    await user.click(await screen.findByTestId("app-stop-button"));
    await waitFor(() => {
      const envelope = screen.getByTestId("error-envelope");
      expect(envelope).toHaveTextContent("E_APP_SUSPENDED");
      expect(envelope).toHaveTextContent("resume first");
    });
  });
});
