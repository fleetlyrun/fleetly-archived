// Containers 页测试（2026-09-29 IA 重设计）：运行实况投影的 UI 形态——
// 服务小结卡（声明 vs 实况水位成对、absent 警示色）、任务全量表（含历史
// 行 + desired 注记 + error 列）、空态、加载态（runtime 缺 services 字段
// 的响应如实按空集，不伪造清单）。

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { describe, expect, it, vi } from "vitest";

import { AppContainersPage } from "@/pages/AppContainersPage";
import { setToken } from "@/api/client";

const RUNTIME_FULL = {
  app: "demo",
  desired_deployment: "dep1",
  services: [
    {
      name: "fleetly-acme-prod-demo-web",
      service: "web",
      image: "nginx:1.27@sha256:aaa",
      mode: "replicated",
      declared_replicas: "2",
      actual_replicas: "1",
      update_state: "completed",
      tasks: [
        {
          id: "t-running",
          slot: 1,
          state: "running",
          desired_state: "running",
          image: "nginx:1.27@sha256:aaa",
          timestamp: "2026-09-29T10:00:00Z",
        },
        {
          id: "t-old",
          slot: 1,
          state: "shutdown",
          desired_state: "shutdown",
          image: "nginx:1.26@sha256:bbb",
          timestamp: "2026-09-29T09:00:00Z",
        },
        {
          id: "t-failed",
          slot: 1,
          state: "failed",
          desired_state: "running",
          error: "task: non-zero exit (1)",
          image: "nginx:1.27@sha256:aaa",
          timestamp: "2026-09-29T09:30:00Z",
        },
      ],
    },
  ],
};

function stubContainersFetch(runtime: unknown) {
  return vi.fn().mockImplementation((input: RequestInfo | URL) => {
    const url = String(input);
    if (url.endsWith("/runtime")) {
      return Promise.resolve({
        ok: true,
        status: 200,
        statusText: "",
        json: () => Promise.resolve(runtime),
      });
    }
    if (url.endsWith("/drift")) {
      return Promise.resolve({
        ok: true,
        status: 200,
        statusText: "",
        json: () => Promise.resolve({ app: "demo", drifted: false, services: [] }),
      });
    }
    if (url.endsWith("/auth/me")) {
      return Promise.resolve({
        ok: true,
        status: 200,
        statusText: "",
        json: () =>
          Promise.resolve({
            user: { id: "01U1", email: "u@t.test", is_platform_admin: false },
            teams: [
              { team_id: "01TEAM", team_slug: "acme", team_name: "Acme", role: "owner" },
            ],
            project_overrides: [],
          }),
      });
    }
    return Promise.resolve({
      ok: true,
      status: 200,
      statusText: "",
      json: () => Promise.resolve({}),
    });
  });
}

function renderContainers(app = "demo") {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return render(
    <MemoryRouter initialEntries={[`/apps/${app}/containers`]}>
      <QueryClientProvider client={client}>
        <Routes>
          <Route path="/apps/:name/containers" element={<AppContainersPage />} />
        </Routes>
      </QueryClientProvider>
    </MemoryRouter>,
  );
}

describe("AppContainersPage runtime projection", () => {
  it("renders service watermark (declared vs running) and every task row", async () => {
    setToken("flt_test");
    vi.stubGlobal("fetch", stubContainersFetch(RUNTIME_FULL));

    renderContainers();

    // 服务小结：水位成对（1 running / 声明 2），compose 服务名对位。
    await waitFor(() =>
      expect(screen.getByTestId("runtime-service-card").textContent).toContain("1 / 2"),
    );
    // 任务全量表：3 行（running + shutdown 历史 + failed），锚点逐行。
    await waitFor(() =>
      expect(screen.getAllByTestId("runtime-task-row")).toHaveLength(3),
    );
    // 下线任务的 desired 注记。
    expect(screen.getByText("desired: shutdown")).toBeInTheDocument();
    // 失败任务的 error 列逐字。
    expect(screen.getByText("task: non-zero exit (1)")).toBeInTheDocument();
  });

  it("marks absent services and renders the empty state on an empty union", async () => {
    setToken("flt_test");
    vi.stubGlobal(
      "fetch",
      stubContainersFetch({
        app: "demo",
        desired_deployment: "dep1",
        services: [
          {
            name: "fleetly-acme-prod-demo-web",
            service: "web",
            mode: "replicated",
            declared_replicas: "1",
            actual_replicas: "0",
            missing: true,
            tasks: [],
          },
        ],
      }),
    );

    const { container } = renderContainers();

    await waitFor(() =>
      expect(screen.getByTestId("runtime-service-card").textContent).toContain("0 / 1"),
    );
    // absent 服务无任务 → 任务空态带 Drift 指引（如实不冒充 running）。
    expect(screen.getByText("No tasks recorded")).toBeInTheDocument();
    // missing 服务卡的色点走红色语义（bg-red-500）。
    expect(container.querySelector('[data-testid="runtime-service-card"] .bg-red-500')).not.toBeNull();
  });

  it("renders the empty state when no services exist (never deployed)", async () => {
    setToken("flt_test");
    vi.stubGlobal("fetch", stubContainersFetch({ app: "demo" }));

    renderContainers();

    await screen.findByText("No managed services");
    // 空态指引点名 Deployments 与 cron 台账的去处。
    expect(screen.getByText(/Deploy a compose file/)).toBeInTheDocument();
  });
});
