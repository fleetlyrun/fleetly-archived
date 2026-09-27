// 项目网卡测试（T 线 OT-1 / IMPL-T15-1）：参与状态展示（attached/detached +
// 项目网名 + 项目限定形链接）、角色门（admin+ 见写控件；developer 见角色
// 说明行；平台管理员见只读说明且无控件）、attach/detach 走 RPC（POST/DELETE
// /apps/<id>/project-network，detach 经确认对话框）。

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { describe, expect, it, vi } from "vitest";

import { setToken } from "@/api/client";
import { AppProjectNetworkCard } from "@/components/app-project-network-card";
import { TeamProjectProvider } from "@/lib/context";

type Call = { url: string; method: string; body: unknown };

function stubFetch(opts: { role: string; platformAdmin?: boolean }) {
  const calls: Call[] = [];
  const fetchMock = vi.fn().mockImplementation((input: RequestInfo | URL, init?: RequestInit) => {
    const url = String(input);
    const method = init?.method ?? "GET";
    let body: unknown = null;
    if (typeof init?.body === "string") {
      try {
        body = JSON.parse(init.body);
      } catch {
        body = init.body;
      }
    }
    calls.push({ url, method, body });
    const json = (payload: unknown) =>
      Promise.resolve({ ok: true, status: 200, statusText: "", json: () => Promise.resolve(payload) });
    if (url.endsWith("/auth/me")) {
      return json({
        user: { id: "01U1", email: "f@t.test", is_platform_admin: opts.platformAdmin === true },
        teams: [{ team_id: "01TEAM", team_slug: "acme", team_name: "Acme", role: opts.role }],
        project_overrides: [],
      });
    }
    if (url.endsWith("/projects")) {
      return json({ projects: [] });
    }
    if (url.endsWith("/project-network") && method === "POST") {
      return json({
        membership: { app: "demo", attached: true, changed: true, status: "rolling" },
      });
    }
    if (url.endsWith("/project-network") && method === "DELETE") {
      return json({
        membership: { app: "demo", attached: false, changed: true, status: "rolling" },
      });
    }
    return json({});
  });
  return { fetchMock, calls };
}

const APP = {
  id: "01APP00000000000000000000A",
  name: "demo",
  team_slug: "acme",
  project_slug: "default",
  project_id: "01PROJ0000000000000000000A",
  project_network_attached: false,
};

function renderCard(app: Record<string, unknown>) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <MemoryRouter>
      <QueryClientProvider client={client}>
        <TeamProjectProvider>
          <AppProjectNetworkCard app={app} />
        </TeamProjectProvider>
      </QueryClientProvider>
    </MemoryRouter>,
  );
}

describe("AppProjectNetworkCard", () => {
  it("detached + admin：展示缺省态与项目链接，attach 走 POST", async () => {
    setToken("flt_test");
    const { fetchMock, calls } = stubFetch({ role: "admin" });
    vi.stubGlobal("fetch", fetchMock);

    renderCard(APP);

    expect(screen.getByTestId("project-network-badge")).toHaveTextContent("detached");
    expect(screen.getByTestId("project-network-project-link")).toHaveTextContent("acme/default");
    expect(screen.getByTestId("project-network-project-link")).toHaveAttribute(
      "href",
      "/projects/01PROJ0000000000000000000A",
    );

    const user = userEvent.setup();
    await user.click(screen.getByTestId("project-network-attach"));
    await waitFor(() => {
      expect(
        calls.some(
          (c) => c.method === "POST" && c.url.endsWith("/apps/01APP00000000000000000000A/project-network"),
        ),
      ).toBe(true);
    });
  });

  it("attached + admin：展示项目网名；detach 经确认对话框走 DELETE", async () => {
    setToken("flt_test");
    const { fetchMock, calls } = stubFetch({ role: "admin" });
    vi.stubGlobal("fetch", fetchMock);

    renderCard({
      ...APP,
      project_network_attached: true,
      project_network: "fleetly-project-01PROJ0000000000000000000A",
    });

    expect(screen.getByTestId("project-network-badge")).toHaveTextContent("attached");
    expect(screen.getByTestId("project-network-name")).toHaveTextContent(
      "fleetly-project-01PROJ0000000000000000000A",
    );

    const user = userEvent.setup();
    await user.click(screen.getByTestId("project-network-detach"));
    expect(screen.getByTestId("project-network-detach-dialog")).toBeInTheDocument();
    // 未确认前不发请求。
    expect(calls.some((c) => c.method === "DELETE")).toBe(false);
    await user.click(screen.getByTestId("project-network-detach-confirm"));
    await waitFor(() => {
      expect(
        calls.some(
          (c) => c.method === "DELETE" && c.url.endsWith("/apps/01APP00000000000000000000A/project-network"),
        ),
      ).toBe(true);
    });
  });

  it("developer：只读参与状态 + 角色说明行，无写控件", async () => {
    setToken("flt_test");
    const { fetchMock } = stubFetch({ role: "developer" });
    vi.stubGlobal("fetch", fetchMock);

    renderCard(APP);

    expect(screen.getByTestId("project-network-badge")).toHaveTextContent("detached");
    // Me 投影异步到达前能力门开（OPEN 兜底）——等待角色收敛后再断负面。
    const note = await screen.findByTestId("project-network-role-note");
    expect(note).toHaveTextContent(/team admin or\s+owner/);
    await waitFor(() => {
      expect(screen.queryByTestId("project-network-attach")).not.toBeInTheDocument();
      expect(screen.queryByTestId("project-network-detach")).not.toBeInTheDocument();
    });
  });

  it("平台管理员：只读说明行（职责分离），无写控件", async () => {
    setToken("flt_test");
    const { fetchMock } = stubFetch({ role: "owner", platformAdmin: true });
    vi.stubGlobal("fetch", fetchMock);

    renderCard(APP);

    const note = await screen.findByTestId("project-network-readonly-note");
    expect(note).toHaveTextContent(/read-only access/);
    await waitFor(() => {
      expect(screen.queryByTestId("project-network-attach")).not.toBeInTheDocument();
      expect(screen.queryByTestId("project-network-detach")).not.toBeInTheDocument();
    });
  });
});
