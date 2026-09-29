// Compose 可视化测试（2026-09-29 IA 重设计 §4.4「实际生效的 compose 文
// 件」）：Overview 卡渲染 active revision 快照（pretty-print 原文 + Copy
// 载荷）、无 active revision 空态、ComposeDialog 按部署 revision 取快照。

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";

import { AppComposeCard, ComposeDialog } from "@/components/app-compose-card";
import { setToken } from "@/api/client";

const COMPOSE_RAW = JSON.stringify({
  name: "demo",
  services: [{ name: "web", image: "nginx:1.27", env: ["FOO=sha256:deadbeef"] }],
});

function jsonResponse(body: unknown, ok = true, status = 200) {
  return {
    ok,
    status,
    statusText: ok ? "OK" : "Error",
    json: () => Promise.resolve(body),
  };
}

function stubComposeFetch(opts: { revisions?: unknown; spec?: unknown } = {}) {
  return vi.fn().mockImplementation((input: RequestInfo | URL) => {
    const url = String(input);
    if (url.includes("/revisions") && !url.endsWith("/spec")) {
      return jsonResponse(
        opts.revisions ?? { revisions: [{ id: "rev1", seq: 7, status: "active" }] },
      );
    }
    if (url.endsWith("/spec")) {
      return jsonResponse(opts.spec ?? { revision_id: "rev1", seq: 7, compose: COMPOSE_RAW });
    }
    return jsonResponse({});
  });
}

function renderCard(app = "demo") {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return render(
    <QueryClientProvider client={client}>
      <AppComposeCard app={app} />
    </QueryClientProvider>,
  );
}

describe("AppComposeCard", () => {
  it("renders the active revision snapshot verbatim with a copy action", async () => {
    setToken("flt_test");
    const writeText = vi.fn().mockResolvedValue(undefined);
    Object.defineProperty(navigator, "clipboard", {
      value: { writeText },
      configurable: true,
    });
    vi.stubGlobal("fetch", stubComposeFetch());

    renderCard();

    // 快照原文渲染（pretty-print 含镜像与服务名；env 是 key:hash 形态——
    // 值明文结构性不在快照中）。
    const viewer = await screen.findByTestId("compose-viewer");
    expect(viewer.textContent).toContain('"name": "demo"');
    expect(viewer.textContent).toContain("nginx:1.27");

    // Copy 把 pretty-print 后的快照写剪贴板。
    fireEvent.click(screen.getByTestId("compose-copy"));
    await waitFor(() => expect(writeText).toHaveBeenCalled());
    expect(writeText.mock.calls[0]?.[0]).toContain("nginx:1.27");
  });

  it("renders the empty state when no active revision exists", async () => {
    setToken("flt_test");
    vi.stubGlobal(
      "fetch",
      stubComposeFetch({ revisions: { revisions: [] } }),
    );

    renderCard();

    await screen.findByText("No compose deployed yet");
    expect(screen.queryByTestId("compose-viewer")).not.toBeInTheDocument();
  });
});

describe("ComposeDialog", () => {
  it("fetches the deployment revision spec and renders it", async () => {
    setToken("flt_test");
    vi.stubGlobal("fetch", stubComposeFetch());

    const client = new QueryClient({
      defaultOptions: { queries: { retry: false } },
    });
    render(
      <QueryClientProvider client={client}>
        <ComposeDialog
          app="demo"
          deploymentId="dep-abcdef123456"
          revisionId="rev1"
          open
          onOpenChange={() => undefined}
        />
      </QueryClientProvider>,
    );

    const viewer = await screen.findByTestId("compose-viewer");
    expect(viewer.textContent).toContain("nginx:1.27");
    // 对话框标题携带部署短 id（历史回看语境）。
    expect(screen.getByText(/deployment dep-abcdef12/)).toBeInTheDocument();
  });
});
