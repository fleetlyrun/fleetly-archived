// degraded 一等 UI 测试（W5-S2，E6 设计 §3.3 挂账收口）：解释卡 degraded/
// ready 两态——degraded 时常驻解释卡（英文文案 = 对账发现 substrate 缺失
// 语义 + 该 app 事件流链接），ready 时不渲染；AppsPage 行内 compact 卡与
// AppOverview 全量卡两面覆盖；事件数据取 WatchEvents 窗口内最近因由事件。

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { describe, expect, it, vi } from "vitest";

import type { EventView } from "@/api/types";
import {
  DegradedExplanationCard,
  lastDegradedEvent,
} from "@/components/degraded-explanation-card";
import { AppsPage } from "@/pages/AppsPage";
import { AppOverviewPage } from "@/pages/AppOverviewPage";
import { setToken } from "@/api/client";

function renderWithRoutes(ui: React.ReactElement, route: string) {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return render(
    <MemoryRouter initialEntries={[route]}>
      <QueryClientProvider client={client}>
        <Routes>
          <Route path="/apps" element={<AppsPage />} />
          <Route path="/apps/:name" element={ui} />
        </Routes>
      </QueryClientProvider>
    </MemoryRouter>,
  );
}

function ev(partial: Partial<EventView>): EventView {
  return {
    seq: "1",
    at: "2026-09-21T08:00:00Z",
    name: "app.substrate_missing",
    subject: "app:demo",
    payload: "{}",
    ...partial,
  };
}

describe("DegradedExplanationCard unit", () => {
  it("renders the honest reconciliation copy with an event-stream link", () => {
    render(
      <MemoryRouter>
        <DegradedExplanationCard app="demo" event={ev({ seq: "7" })} />
      </MemoryRouter>,
    );
    const card = screen.getByTestId("degraded-explanation-card");
    // 英文文案：对账发现 substrate 缺失的语义。
    expect(card.textContent).toContain("Why is this app degraded?");
    expect(card.textContent).toContain("reconciliation");
    expect(card.textContent).toContain("substrate");
    // 指引链接 = 该 app 的事件流（EventsPage ?q= 深链）。
    const link = screen.getByRole("link");
    expect(link.getAttribute("href")).toBe(
      `/events?q=${encodeURIComponent("app:demo")}`,
    );
    // 事件窗口内的最近因由事件被展示。
    expect(card.textContent).toContain("app.substrate_missing");
  });

  it("renders without event data (stream window may be empty)", () => {
    render(
      <MemoryRouter>
        <DegradedExplanationCard app="demo" event={null} />
      </MemoryRouter>,
    );
    expect(screen.getByTestId("degraded-explanation-card")).toBeInTheDocument();
    expect(screen.getByRole("link").getAttribute("href")).toContain("/events?q=");
  });

  it("renders the compact row variant with the same anchor", () => {
    render(
      <MemoryRouter>
        <DegradedExplanationCard app="demo" compact />
      </MemoryRouter>,
    );
    expect(screen.getByTestId("degraded-explanation-card")).toBeInTheDocument();
    expect(screen.getByRole("link").getAttribute("href")).toContain("/events?q=");
  });

  it("picks the newest matching reason event for the app subject", () => {
    const events = [
      ev({ seq: "1", name: "app.degraded", subject: "app:demo" }),
      ev({ seq: "2", name: "deployment.succeeded", subject: "deployment:d1" }),
      ev({ seq: "3", name: "app.substrate_missing", subject: "app:other" }),
      ev({ seq: "4", name: "app.degraded", subject: "app:demo" }),
    ];
    expect(lastDegradedEvent("demo", events)?.seq).toBe("4");
    expect(lastDegradedEvent("ghost", events)).toBeNull();
  });
});

describe("DegradedExplanationCard surfaces", () => {
  it("AppsPage: degraded rows carry the inline card; running rows do not", async () => {
    setToken("flt_test");
    vi.stubGlobal(
      "fetch",
      vi.fn().mockResolvedValue({
        ok: true,
        status: 200,
        statusText: "",
        json: () =>
          Promise.resolve({
            apps: [
              { id: "a1", name: "web", lifecycle: "active", derived_state: "running" },
              { id: "a2", name: "api", lifecycle: "active", derived_state: "degraded" },
            ],
          }),
      }),
    );
    renderWithRoutes(<p>detail</p>, "/apps");

    await waitFor(() => screen.getByText("api"));
    expect(screen.getAllByTestId("degraded-explanation-card")).toHaveLength(1);
    // degraded 行有卡；running 行没有。
    const degradedRow = screen.getByText("api").closest("tr")!;
    expect(
      degradedRow.querySelector('[data-testid="degraded-explanation-card"]'),
    ).not.toBeNull();
    const runningRow = screen.getByText("web").closest("tr")!;
    expect(
      runningRow.querySelector('[data-testid="degraded-explanation-card"]'),
    ).toBeNull();
  });

  it("AppOverviewPage: ready state renders no card (zero extra streams)", async () => {
    setToken("flt_test");
    vi.stubGlobal("fetch", stubOverviewFetch("running"));
    renderWithRoutes(<AppOverviewPage />, "/apps/demo");

    await waitFor(() => screen.getByTestId("services-list"));
    expect(
      screen.queryByTestId("degraded-explanation-card"),
    ).not.toBeInTheDocument();
  });

  it("AppOverviewPage: degraded state shows the persistent explanation card", async () => {
    setToken("flt_test");
    vi.stubGlobal("fetch", stubOverviewFetch("degraded"));
    renderWithRoutes(<AppOverviewPage />, "/apps/demo");

    const card = await screen.findByTestId("degraded-explanation-card");
    expect(card.textContent).toContain("Why is this app degraded?");
    expect(card.textContent).toContain("substrate");
    // 卡内作用域查询：Overview 页面上还有 Deploy settings 卡的 terminal
    // 链接（§4.7），全局 getByRole("link") 不再唯一。
    expect(card.querySelector("a")?.getAttribute("href")).toContain("/events?q=");
  });
});

// stubOverviewFetch 是 AppOverviewPage 的 REST 桩（derived_state 可参数化
// ——degraded/ready 两态同一夹具）。事件流 URL 由桩按 JSON 兜底应答（无
// body → 流以错误收场 → 卡片退化为无事件形态，订阅随卸载清理）。
function stubOverviewFetch(derivedState: string) {
  return vi.fn().mockImplementation((input: RequestInfo | URL) => {
    const url = String(input);
    if (url.endsWith("/spec")) {
      return Promise.resolve({
        ok: true,
        status: 200,
        statusText: "",
        json: () =>
          Promise.resolve({
            compose: JSON.stringify({
              name: "demo",
              services: [{ name: "web", image: "nginx:1.27" }],
            }),
          }),
      });
    }
    if (url.includes("/cron-runs")) {
      return Promise.resolve({
        ok: true,
        status: 200,
        statusText: "",
        json: () => Promise.resolve({ app: "demo", runs: [] }),
      });
    }
    if (url.includes("/apps/demo/revisions")) {
      return Promise.resolve({
        ok: true,
        status: 200,
        statusText: "",
        json: () =>
          Promise.resolve({ revisions: [{ id: "rev1", status: "active" }] }),
      });
    }
    if (url.includes("/placement")) {
      return Promise.resolve({
        ok: true,
        status: 200,
        statusText: "",
        json: () => Promise.resolve({ volumes: [] }),
      });
    }
    // GET /apps/demo 与 /events/stream 兜底同形（流侧无 body 即失败收场）。
    return Promise.resolve({
      ok: true,
      status: 200,
      statusText: "",
      json: () =>
        Promise.resolve({
          id: "a1",
          name: "demo",
          lifecycle: "active",
          derived_state: derivedState,
        }),
    });
  });
}
