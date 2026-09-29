// 用户菜单测试（v0.3 RBAC W1；M9-9 口径沿袭）：
// - Me 投影展示：显示名/邮箱、平台管理员标志、所属团队与角色（W1 只读）；
// - Sign out 在清凭据的同时清空 react-query 缓存——下一个会话（换人）不
//   得复用上一个会话的服务端状态；注销请求打到 POST /v1/auth/logout；
// - Sign out all devices 走 POST /v1/auth/logout-all；
// - Bearer 身份指示（P1-4）：读到 API token → 面板顶部身份说明条；纯会话
//   cookie → 不渲染。

import { QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { beforeEach, describe, expect, it, vi } from "vitest";

import { Layout } from "@/components/layout";
import { getToken, setToken } from "@/api/client";
import { AuthProvider } from "@/auth";
import { queryClient } from "@/query";

function jsonResponse(status: number, body: unknown) {
  return {
    ok: status >= 200 && status < 300,
    status,
    statusText: "",
    json: () => Promise.resolve(body),
  };
}

const ME = {
  user: {
    id: "u1",
    email: "op@example.com",
    display_name: "Operator",
    is_platform_admin: true,
  },
  teams: [
    { team_id: "t1", team_slug: "acme", team_name: "Acme", role: "owner" },
    { team_id: "t2", team_slug: "globex", team_name: "Globex", role: "developer" },
  ],
};

function stubFetch() {
  return vi.fn((url: unknown) => {
    const u = String(url);
    if (u.includes("/auth/me")) return Promise.resolve(jsonResponse(200, ME));
    if (u.includes("/system/status")) return Promise.resolve(jsonResponse(200, { version: "0.3.0" }));
    if (u.includes("/auth/logout-all")) return Promise.resolve(jsonResponse(200, { sessions_revoked: "3" }));
    if (u.includes("/auth/logout")) return Promise.resolve(jsonResponse(200, {}));
    return Promise.resolve(jsonResponse(404, { message: `unmocked ${u}` }));
  });
}

function renderLayout() {
  return render(
    <MemoryRouter initialEntries={["/"]}>
      <AuthProvider>
        <QueryClientProvider client={queryClient}>
          <Routes>
            <Route element={<Layout />}>
              <Route path="/" element={<p>home-body</p>} />
            </Route>
          </Routes>
        </QueryClientProvider>
      </AuthProvider>
    </MemoryRouter>,
  );
}

beforeEach(() => {
  queryClient.clear();
  setToken("");
});

describe("UserMenu Me projection", () => {
  it("shows identity, platform-admin flag and read-only teams", async () => {
    vi.stubGlobal("fetch", stubFetch());
    renderLayout();
    // 会话面拉 Me 后菜单按钮以显示名呈现。
    await waitFor(() => expect(screen.getByTestId("user-menu")).toHaveTextContent("Operator"));

    const user = userEvent.setup();
    await user.click(screen.getByTestId("user-menu"));

    expect(screen.getByTestId("user-menu-name")).toHaveTextContent("Operator");
    expect(screen.getByTestId("user-menu-email")).toHaveTextContent("op@example.com");
    expect(screen.getByTestId("user-platform-admin")).toHaveTextContent("Platform admin");
    const teams = screen.getByTestId("user-teams");
    expect(teams).toHaveTextContent("Acme");
    expect(teams).toHaveTextContent("owner");
    expect(teams).toHaveTextContent("Globex");
    expect(teams).toHaveTextContent("developer");
    // 纯会话 cookie（无 token）：Bearer 身份指示条不渲染。
    expect(screen.queryByTestId("user-menu-token-note")).not.toBeInTheDocument();
  });
});

describe("UserMenu API-token indicator (review P1-4)", () => {
  it("notes when the session acts via an API token", async () => {
    vi.stubGlobal("fetch", stubFetch());
    setToken("flt_operator_a");
    renderLayout();
    await waitFor(() => expect(screen.getByTestId("user-menu")).toBeInTheDocument());
    const user = userEvent.setup();
    await user.click(screen.getByTestId("user-menu"));
    expect(screen.getByTestId("user-menu-token-note")).toHaveTextContent(
      "Acting via API token — session cookie is not used.",
    );
  });
});

describe("UserMenu sign out (M9-9 cache isolation)", () => {
  it("clears the react-query cache along with credentials and calls POST /auth/logout", async () => {
    const fetchMock = stubFetch();
    vi.stubGlobal("fetch", fetchMock);
    setToken("flt_operator_a");
    // 预置缓存：上一个会话的应用列表与部署历史。
    queryClient.setQueryData(["apps"], { apps: [{ name: "secret-app" }] });
    queryClient.setQueryData(["deployments", "secret-app"], { deployments: [] });
    expect(queryClient.getQueryData(["apps"])).toBeTruthy();

    renderLayout();
    await waitFor(() => expect(screen.getByTestId("user-menu")).toBeInTheDocument());
    const user = userEvent.setup();
    await user.click(screen.getByTestId("user-menu"));
    await user.click(screen.getByTestId("logout"));

    // 凭据、缓存与远端会话一并清空。
    expect(getToken()).toBe("");
    expect(queryClient.getQueryData(["apps"])).toBeUndefined();
    expect(queryClient.getQueryData(["deployments", "secret-app"])).toBeUndefined();
    await waitFor(() => {
      expect(fetchMock.mock.calls.some(([u]) => String(u).endsWith("/v1/auth/logout"))).toBe(true);
    });
    expect(fetchMock.mock.calls.some(([u]) => String(u).includes("logout-all"))).toBe(false);
  });

  it("signs out of all devices via POST /auth/logout-all", async () => {
    const fetchMock = stubFetch();
    vi.stubGlobal("fetch", fetchMock);
    setToken("flt_operator_a");

    renderLayout();
    await waitFor(() => expect(screen.getByTestId("user-menu")).toBeInTheDocument());
    const user = userEvent.setup();
    await user.click(screen.getByTestId("user-menu"));
    await user.click(screen.getByTestId("logout-all"));

    expect(getToken()).toBe("");
    await waitFor(() => {
      expect(fetchMock.mock.calls.some(([u]) => String(u).endsWith("/v1/auth/logout-all"))).toBe(true);
    });
  });
});
