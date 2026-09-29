// 应用资源卡测试（E6 W5-S3）：opt-in 引导 + 开关（unset 态；开关写面 =
// requirePlatformWriteFace 平台管理员专属——平台管理员见钮可点、非平台
// 管理员原位说明）、诚实「采集中」态（组件未收敛不画空线）、on 态曲线
// 与每副本水位表（uPlot mock——jsdom 无 canvas）、错误信封。

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";

import { AppMetricsCard } from "@/components/app-metrics-card";
import { setToken } from "@/api/client";

vi.mock("uplot", () => ({
  default: class FakeUPlot {
    destroyed = false;
    constructor(_opts: unknown, _data: unknown, _el: HTMLElement) {}
    setSize() {}
    destroy() {
      this.destroyed = true;
    }
  },
}));

const ALL_READY = {
  mode: "on",
  mode_set: true,
  components: [
    { name: "fleetly-cadvisor", exists: true, image: "gcr.io/cadvisor/cadvisor:v0.55.1" },
    { name: "fleetly-node-exporter", exists: true, image: "prom/node-exporter:v1.12.1" },
    { name: "fleetly-victoriametrics", exists: true, image: "victoriametrics/victoria-metrics:v1.152.0" },
  ],
  nodes_reporting: 1,
  nodes_total: 1,
  retention_days: 14,
};

const UNSET_STATUS = {
  mode: "unset",
  mode_set: false,
  components: [],
  nodes_reporting: 0,
  nodes_total: 1,
  retention_days: 14,
};

/** /auth/me 投影（is_platform_admin 开关——写面门按服务端语义反转后钉双侧）。 */
function meResponse(isPlatformAdmin: boolean) {
  return jsonResponse({
    user: { id: "01U1", email: "f@t.test", is_platform_admin: isPlatformAdmin },
    teams: [
      { team_id: "01TEAM", team_slug: "acme", team_name: "Acme", role: "owner" },
    ],
    project_overrides: [],
  });
}

function jsonResponse(body: unknown, status = 200) {
  return Promise.resolve({
    ok: status >= 200 && status < 300,
    status,
    statusText: String(status),
    json: () => Promise.resolve(body),
  });
}

function renderCard(app = "demo") {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={client}>
      <AppMetricsCard app={app} />
    </QueryClientProvider>,
  );
}

describe("AppMetricsCard", () => {
  it("shows the opt-in guide with the toggle when metrics.mode is unset; the toggle PUTs mode=on", async () => {
    setToken("flt_test");
    const log: { url: string; method: string; body?: unknown }[] = [];
    vi.stubGlobal(
      "fetch",
      vi.fn().mockImplementation((input: RequestInfo | URL, init?: RequestInit) => {
        const url = String(input);
        log.push({ url, method: init?.method ?? "GET", body: init?.body });
        if (url.endsWith("/auth/me")) {
          return meResponse(true);
        }
        if (url.endsWith("/metrics/status")) {
          return jsonResponse(UNSET_STATUS);
        }
        return jsonResponse({});
      }),
    );
    renderCard();
    await screen.findByTestId("app-metrics-card");
    // 查询解析前卡先挂载（Loading 态）——等待数据到位再断言。
    await waitFor(() =>
      expect(screen.getByTestId("app-metrics-card").textContent).toContain("opt-in"),
    );
    // 未启用不触发任何指标查询。
    expect(log.filter((e) => e.url.includes("/metrics/search"))).toHaveLength(0);

    fireEvent.click(screen.getByTestId("metrics-mode-toggle"));
    await waitFor(() => {
      const put = log.find((e) => e.method === "PUT" && e.url.endsWith("/metrics/mode"));
      expect(put).toBeTruthy();
      expect(JSON.parse(String(put?.body))).toEqual({ mode: "on" });
    });
  });

  it("shows the platform-admin-required note instead of the toggle for non-admins (server gate is requirePlatformWriteFace)", async () => {
    setToken("flt_test");
    vi.stubGlobal(
      "fetch",
      vi.fn().mockImplementation((input: RequestInfo | URL) => {
        const url = String(input);
        if (url.endsWith("/auth/me")) return meResponse(false);
        if (url.endsWith("/metrics/status")) return jsonResponse(UNSET_STATUS);
        return jsonResponse({});
      }),
    );
    renderCard();
    await waitFor(() =>
      expect(screen.getByTestId("app-metrics-card").textContent).toContain("opt-in"),
    );
    const note = screen.getByTestId("metrics-mode-readonly-note");
    expect(note.textContent).toContain("Platform administrator required");
    expect(screen.queryByTestId("metrics-mode-toggle")).not.toBeInTheDocument();
  });

  it("shows the honest collecting state (no empty charts) while components converge", async () => {
    setToken("flt_test");
    const log: { url: string; method: string }[] = [];
    vi.stubGlobal(
      "fetch",
      vi.fn().mockImplementation((input: RequestInfo | URL, init?: RequestInit) => {
        const url = String(input);
        log.push({ url, method: init?.method ?? "GET" });
        if (url.endsWith("/metrics/status")) {
          return jsonResponse({
            ...ALL_READY,
            components: ALL_READY.components.map((c) => ({ ...c, exists: false })),
          });
        }
        return jsonResponse({});
      }),
    );
    renderCard();
    await screen.findByTestId("metrics-pending");
    expect(screen.queryByTestId("metrics-chart")).not.toBeInTheDocument();
    expect(log.filter((e) => e.url.includes("/metrics/search"))).toHaveLength(0);
  });

  it("renders charts and the per-replica watermark when the stack is ready", async () => {
    setToken("flt_test");
    vi.stubGlobal(
      "fetch",
      vi.fn().mockImplementation((input: RequestInfo | URL) => {
        const url = String(input);
        if (url.endsWith("/metrics/status")) return jsonResponse(ALL_READY);
        if (url.includes("/metrics/search")) {
          const q = decodeURIComponent(url);
          const taskDim = q.includes("container_label_com_docker_swarm_task_name");
          const rateDim = q.includes("rate(");
          return jsonResponse({
            series: [
              {
                metric: {
                  [taskDim ? "container_label_com_docker_swarm_task_name" : "container_label_com_docker_swarm_service_name"]:
                    taskDim ? "fleetly-demo-web.1.abc" : "fleetly-demo-web",
                },
                points: taskDim
                  ? rateDim
                    ? [{ t: 1, v: 0.125 }]
                    : [{ t: 1, v: 3 * 1024 * 1024 }]
                  : rateDim
                    ? [{ t: 1, v: 0.5 }]
                    : [{ t: 1, v: 8 * 1024 * 1024 }],
              },
            ],
          });
        }
        return jsonResponse({});
      }),
    );
    renderCard();
    await waitFor(() => expect(screen.getAllByTestId("metrics-chart").length).toBeGreaterThanOrEqual(2));
    // 副本水位：按 task 维度一行（CPU/Memory 列有值）。
    await waitFor(() => expect(screen.getByTestId("replicas-watermark").textContent).toContain("fleetly-demo-web.1.abc"));
    const watermark = screen.getByTestId("replicas-watermark").textContent ?? "";
    expect(watermark).toContain("0.125 CPU");
    expect(watermark).toContain("3.0 MiB");
  });

  it("surfaces the server envelope when the status query fails (403 scope)", async () => {
    setToken("flt_test");
    vi.stubGlobal(
      "fetch",
      vi.fn().mockImplementation(() =>
        Promise.resolve({
          ok: false,
          status: 403,
          statusText: "403",
          json: () =>
            Promise.resolve({ code: "", message: "token scope is insufficient", context: {} }),
        }),
      ),
    );
    renderCard();
    await waitFor(() => expect(screen.getByTestId("app-metrics-card").textContent).toContain("insufficient"));
  });
});
