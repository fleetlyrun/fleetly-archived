// ACME DNS-01 设置卡（B 线 W5 设计 §3，D-V3W5-3/D-V3W5-4，W5-S3）：DNS
// 服务商选择（none/dnspod/cloudflare）+ 凭证 write-only（留空 = 保留已存）
// + 探针（create→delete 真实 TXT）+ 通配证书开关 + 当前证书域集展示。
//
// 服务端契约（system.proto）：
//   - 读面脱敏：api_token 只回 fingerprint（sha256 前 8 hex），永无明文回填；
//   - 写面：api_token 留空 = 保留已存凭证（wildcard 切换不要求重录）；
//     provider=none 恒清空；
//   - 联动门：wildcard=true 须 provider ∈ {dnspod, cloudflare}（422
//     E_ACME_WILDCARD_REQUIRES_PROVIDER）且须 base_domain（409
//     E_ACME_WILDCARD_REQUIRES_BASE_DOMAIN）；
//   - 探针失败以 E_ACME_DNS_TEST_FAILED 信封报错（失败步进 context）；
//   - wildcard_domains = 服务端派生的通配期望域名集（与签发面同源）。

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Globe } from "lucide-react";
import { useEffect, useRef, useState } from "react";

import {
  getAcmeSettings,
  testDnsProvider,
  updateAcmeSettings,
} from "@/api/endpoints";
import { errorEnvelopeFrom } from "@/api/errors";
import type { ErrorEnvelope } from "@/api/errors";
import type { DnsProviderTestResult } from "@/api/types";
import {
  EnvelopeAlert,
  EnvelopeAlertFrom,
} from "@/components/envelope-alert";
import { Button } from "@/components/ui/button";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { formatTime } from "@/lib/utils";
import { useIsPlatformAdmin } from "@/lib/context";

const PROVIDER_OPTIONS = [
  { value: "none", label: "Not configured" },
  { value: "dnspod", label: "DNSPod" },
  { value: "cloudflare", label: "Cloudflare" },
];

/** 表单局部状态：凭证永不回填明文（输入框恒从空起）。 */
interface AcmeFormState {
  provider: string;
  token: string;
  wildcard: boolean;
}

/** 探针单步行（create/delete：ok + 耗时 + 失败原文——诚实契约可定位）。 */
function ProbeStepRow({
  step,
}: {
  step: { step?: string; ok?: boolean; duration_ms?: string; error?: string };
}) {
  return (
    <div className="flex items-center justify-between gap-3 text-xs">
      <span className="flex items-center gap-1.5">
        <span
          aria-hidden
          className={`h-2 w-2 rounded-full ${
            step.ok ? "bg-emerald-500" : "bg-red-500"
          }`}
        />
        <code>{step.step}</code>
      </span>
      <span className="flex items-center gap-2 text-muted-foreground">
        {step.error ? (
          <span className="max-w-[220px] truncate text-red-600 dark:text-red-400" title={step.error}>
            {step.error}
          </span>
        ) : null}
        {Number(step.duration_ms) > 0 ? `${step.duration_ms} ms` : null}
      </span>
    </div>
  );
}

export function AcmeSettingsCard() {
  const queryClient = useQueryClient();
  // 读写面整体 admin scope（scope.go SystemService/GetAcmeSettings 等三
  // RPC 登记）：读面 query 对非平台管理员关闭（enabled:false——否则每访
  // 必 403）；非平台管理员整卡替换为只读说明（2026-09-25 走查）。
  const isPlatformAdmin = useIsPlatformAdmin();
  const settingsQuery = useQuery({
    queryKey: ["system", "acme-settings"],
    queryFn: getAcmeSettings,
    enabled: isPlatformAdmin,
    retry: false,
  });

  const stored = settingsQuery.data?.settings;
  const [form, setForm] = useState<AcmeFormState>({
    provider: "none",
    token: "",
    wildcard: false,
  });
  // 服务端设置到达后同步一次表单（token 除外——永不回填）；用户已动手
  //（dirty）则不再回写——迟到的/轮询的重读不得覆盖编辑中的表单。
  const dirtyRef = useRef(false);
  useEffect(() => {
    if (stored && !dirtyRef.current) {
      setForm({
        // EmitUnpopulated=true 下未配置的 provider 显式输出 ""——`||` 让
        // 空串同样回落 none（`??` 只捕 null/undefined，捕不到零值显式形态）。
        provider: stored.dns_provider || "none",
        token: "",
        wildcard: stored.wildcard ?? false,
      });
    }
  }, [stored]);

  const [saveResult, setSaveResult] = useState<"saved" | null>(null);
  const [saveError, setSaveError] = useState<ErrorEnvelope | null>(null);
  const [testResult, setTestResult] = useState<DnsProviderTestResult | null>(null);
  const [testError, setTestError] = useState<ErrorEnvelope | null>(null);

  const set = (patch: Partial<AcmeFormState>) => {
    dirtyRef.current = true;
    setSaveResult(null);
    setForm((prev) => ({ ...prev, ...patch }));
  };

  const saveMutation = useMutation({
    mutationFn: () =>
      updateAcmeSettings({
        dns_provider: form.provider,
        // 留空 = 保留已存凭证（服务端语义）；provider=none 恒清空。
        api_token: form.token,
        wildcard: form.wildcard,
      }),
    onSuccess: (resp) => {
      setSaveError(null);
      setSaveResult("saved");
      dirtyRef.current = false; // 保存即新基线：表单随服务端响应重置。
      if (resp.settings) {
        setForm({
          // `||` 同上：空串显式形态回落 none（见挂载水合注释）。
          provider: resp.settings.dns_provider || "none",
          token: "",
          wildcard: resp.settings.wildcard ?? false,
        });
      }
      void queryClient.invalidateQueries({ queryKey: ["system", "acme-settings"] });
    },
    onError: (err) => {
      setSaveResult(null);
      setSaveError(errorEnvelopeFrom(err));
    },
  });

  const testMutation = useMutation({
    mutationFn: () =>
      // 候选凭证先测后存：任一字段非空即按候选测；全空 = 测已存凭证。
      testDnsProvider({
        dns_provider: form.provider === "none" ? "" : form.provider,
        api_token: form.token,
      }),
    onSuccess: (resp) => {
      setTestError(null);
      setTestResult(resp.result ?? null);
    },
    onError: (err) => {
      setTestResult(null);
      setTestError(errorEnvelopeFrom(err));
    },
  });

  const isNone = form.provider === "none";
  const providerConfigured = form.provider === "dnspod" || form.provider === "cloudflare";

  // 非平台管理员：整卡替换为只读说明（全部 hooks 之后条件返回）。
  if (!isPlatformAdmin) {
    return (
      <Card data-testid="acme-settings-card">
        <CardHeader className="flex-row items-center gap-2 space-y-0 border-b pb-3">
          <Globe aria-hidden className="h-4 w-4 text-muted-foreground" />
          <CardTitle className="text-sm font-semibold">Certificates (ACME)</CardTitle>
        </CardHeader>
        <CardContent className="space-y-4 pt-4">
          <p
            className="text-sm text-muted-foreground"
            data-testid="acme-settings-readonly-note"
          >
            Platform administrator required.
          </p>
        </CardContent>
      </Card>
    );
  }

  // saved 时间戳仅在非零值时渲染（proto 零值 Timestamp 序列化为 epoch
  // 字符串仍为真值——未保存过设置时会显示 "saved 1970/…"，2026-09-25
  // 审查 P2-3）：epoch 0 或不可解析一律视作未保存。
  const savedAtIso = stored?.updated_at ?? "";
  const savedAt = savedAtIso && new Date(savedAtIso).getTime() > 0 ? savedAtIso : "";

  return (
    <Card data-testid="acme-settings-card">
      <CardHeader className="flex-row items-center gap-2 space-y-0 border-b pb-3">
        <Globe aria-hidden className="h-4 w-4 text-muted-foreground" />
        <CardTitle className="text-sm font-semibold">Certificates (ACME)</CardTitle>
        <CardDescription className="ml-auto text-xs">
          Wildcard certificates via DNS-01{savedAt ? ` · saved ${formatTime(savedAt)}` : ""}
        </CardDescription>
      </CardHeader>
      <CardContent className="space-y-4 pt-4">
        {settingsQuery.isError ? (
          <EnvelopeAlertFrom envelope={errorEnvelopeFrom(settingsQuery.error)} />
        ) : null}

        <div className="space-y-1.5">
          <Label htmlFor="acme-provider">DNS provider</Label>
          <Select
            value={form.provider || "none"}
            onValueChange={(v) => set({ provider: v })}
          >
            <SelectTrigger id="acme-provider" className="w-64" data-testid="acme-provider-select">
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              {PROVIDER_OPTIONS.map((p) => (
                <SelectItem key={p.value} value={p.value}>
                  {p.label}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
          <p className="text-xs text-muted-foreground">
            The provider credentials answer DNS-01 challenges for the wildcard certificate
            (dnspod token form: <code>&lt;id&gt;,&lt;token&gt;</code>; cloudflare: a single API token
            with Zone.DNS edit).
          </p>
        </div>

        <div className="space-y-1.5">
          <Label htmlFor="acme-token">API token</Label>
          <Input
            id="acme-token"
            type="password"
            autoComplete="off"
            className="w-64 font-mono text-xs"
            disabled={isNone}
            data-testid="acme-token-input"
            placeholder={
              stored?.credentials_fingerprint
                ? `stored (fingerprint ${stored.credentials_fingerprint}) — write-only, never read back`
                : "write-only, never read back"
            }
            value={form.token}
            onChange={(e) => set({ token: e.target.value })}
          />
          <p className="text-xs text-muted-foreground" data-testid="acme-token-hint">
            {isNone
              ? "Select a provider to enter its API token."
              : stored?.credentials_fingerprint
                ? `A token is stored (fingerprint ${stored.credentials_fingerprint}). Leaving this blank keeps the stored token; switching to "Not configured" clears it.`
                : "Leaving this blank keeps any stored token (write-only; never read back)."}
          </p>
        </div>

        <label className="flex items-center gap-2 text-sm">
          <input
            type="checkbox"
            data-testid="acme-wildcard-toggle"
            className="h-4 w-4 accent-foreground"
            disabled={!providerConfigured || !stored?.base_domain}
            checked={providerConfigured && form.wildcard}
            onChange={(e) => set({ wildcard: e.target.checked })}
          />
          Wildcard certificate (<code>*.base_domain</code> signed via DNS-01; requires a
          provider and base_domain)
        </label>

        {/* 当前证书域集展示（服务端派生实值——与签发面同源）。 */}
        {stored?.wildcard && (stored?.wildcard_domains?.length ?? 0) > 0 ? (
          <div className="space-y-1 rounded-md border p-3" data-testid="acme-wildcard-domains">
            <div className="text-xs font-medium">
              Platform certificate domains (wildcard set)
            </div>
            <ul className="space-y-0.5">
              {(stored?.wildcard_domains ?? []).map((d) => (
                <li key={d}>
                  <code className="text-xs">{d}</code>
                </li>
              ))}
            </ul>
            <p className="text-xs text-muted-foreground">
              The wildcard certificate covers every app domain under the base domain via SNI;
              per-app HTTP-01 issuance stays for custom domains outside it.
            </p>
          </div>
        ) : null}

        {saveError ? (
          <EnvelopeAlertFrom envelope={saveError} />
        ) : null}
        {saveResult ? (
          <p className="text-xs text-emerald-600 dark:text-emerald-400">
            Settings saved. The platform certificate reconverges within a scan cycle when the
            wildcard mode changes.
          </p>
        ) : null}

        <div className="flex items-center gap-2">
          <Button
            type="button"
            variant="outline"
            size="sm"
            data-testid="acme-test-button"
            disabled={testMutation.isPending || isNone}
            onClick={() => {
              setSaveResult(null);
              testMutation.mutate();
            }}
          >
            Test provider
          </Button>
          <Button
            type="button"
            size="sm"
            disabled={saveMutation.isPending}
            onClick={() => {
              setTestResult(null);
              setTestError(null);
              saveMutation.mutate();
            }}
          >
            {saveMutation.isPending ? "Saving…" : "Save"}
          </Button>
        </div>

        {testError ? (
          <EnvelopeAlert
            code={testError.code}
            message={testError.message}
            suggestion={testError.suggestion}
            docs={testError.docs}
          />
        ) : null}
        {testResult ? (
          <div
            data-testid="acme-test-result"
            className="space-y-2 rounded-md border p-3"
          >
            <div className="flex items-center gap-2 text-xs font-medium">
              <span
                aria-hidden
                className={`h-2 w-2 rounded-full ${
                  testResult.ok ? "bg-emerald-500" : "bg-red-500"
                }`}
              />
              {testResult.ok ? "Provider OK" : `Probe failed${testResult.failed_step ? ` at step ${testResult.failed_step}` : ""}`}
              <code className="text-muted-foreground">{testResult.record_name}</code>
            </div>
            <div className="space-y-1">
              {(testResult.steps ?? []).map((s) => (
                <ProbeStepRow key={s.step} step={s} />
              ))}
            </div>
          </div>
        ) : null}
      </CardContent>
    </Card>
  );
}
