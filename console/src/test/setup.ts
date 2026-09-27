import "@testing-library/jest-dom/vitest";

// jsdom 缺少的少量 API（TextDecoder 流式解码在 node 环境由 polyfill 提供）。
import { afterEach } from "vitest";
import { cleanup } from "@testing-library/react";

// uPlot（E6 W5-S3 metrics-chart 依赖）在模块加载期读 matchMedia 计算像素
// 比率——jsdom 未实现，导入即抛（连累存量页面测试）。此处给最小桩：
// 图表渲染路径在测试里走 canvas 空实现/断言锚点与数据形态，不触 DPI 逻辑。
if (typeof window !== "undefined" && typeof window.matchMedia !== "function") {
  Object.defineProperty(window, "matchMedia", {
    writable: true,
    value: (query: string) => ({
      matches: false,
      media: query,
      onchange: null,
      addListener: () => {},
      removeListener: () => {},
      addEventListener: () => {},
      removeEventListener: () => {},
      dispatchEvent: () => false,
    }),
  });
}

// ResizeObserver 同为 jsdom 缺失面（metrics-chart 宽度自适应依赖——观察者
// 在测试中无操作即可；尺寸驱动的断言由 metrics-chart.test 自注入假件）。
if (typeof globalThis.ResizeObserver !== "function") {
  class SetupResizeObserverStub {
    observe() {}
    unobserve() {}
    disconnect() {}
  }
  (globalThis as { ResizeObserver?: unknown }).ResizeObserver = SetupResizeObserverStub;
}

// scrollIntoView 同属 jsdom 缺失面（radix Select 键盘导航高亮候选时调用
// ——空实现即可：测试断言选项存在与选中结果，不关心滚动位置）。
if (typeof Element !== "undefined" && typeof Element.prototype.scrollIntoView !== "function") {
  Element.prototype.scrollIntoView = () => {};
}

afterEach(() => {
  cleanup();
  window.localStorage.clear();
  window.sessionStorage.clear();
});
