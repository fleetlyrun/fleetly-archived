// 明文配置资源页（T 线 OT-3 / IMPL-T1-4 Console 面）：app 级 configs 的
// 读写面——列表（name/hash8/updated）+ 新增/编辑（覆盖即换版）+ 删除确认 +
// Get 明文查看（admin 门；与 GetEnv 同级信任面）。模式跟随
// AppSecretsPage（表 + 行内动作），差异 = 明文可回读、无加密话术。
//
// 角色门（前端体验门）：写面与明文查看 = admin+（canAdminResources）；
// 平台管理员资源面恒只读（P0-3 双门），说明行指向 CLI。

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { FileCode2, Eye, Pencil, Plus, RefreshCw, Trash2 } from "lucide-react";
import { useState, type FormEvent } from "react";
import { useParams } from "react-router-dom";

import {
  getConfig,
  listConfigs,
  removeConfig,
  setConfig,
} from "@/api/endpoints";
import { errorEnvelopeFrom } from "@/api/errors";
import type { ConfigView } from "@/api/types";
import { EnvelopeAlert } from "@/components/envelope-alert";
import { Button } from "@/components/ui/button";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { timeAgo } from "@/lib/utils";
import { useIsPlatformAdmin, useTeamCapabilities } from "@/lib/context";

const NAME_PATTERN = /^[A-Za-z0-9][A-Za-z0-9._-]*$/;

/** 查看明文对话框（Get；admin 门——按钮只在 canAdminResources 时渲染）。 */
function ViewConfigDialog({ app, name }: { app: string; name: string }) {
  const [open, setOpen] = useState(false);
  const query = useQuery({
    queryKey: ["config", app, name],
    queryFn: () => getConfig(app, name),
    enabled: open,
  });

  return (
    <>
      <Button
        variant="ghost"
        size="icon"
        className="h-7 w-7"
        aria-label={`View config ${name}`}
        data-testid="config-view-button"
        onClick={() => setOpen(true)}
      >
        <Eye aria-hidden className="h-3.5 w-3.5" />
      </Button>
      {open ? (
        <Dialog open onOpenChange={(v) => (v ? undefined : setOpen(false))}>
          <DialogContent data-testid="config-view-dialog" className="max-w-2xl">
            <DialogHeader>
              <DialogTitle>Config {name}</DialogTitle>
              <DialogDescription>
                Plaintext content (read back with admin scope — configs are not
                credentials; use secrets for values that must stay write-only).
              </DialogDescription>
            </DialogHeader>
            {query.isError ? (
              <EnvelopeAlert
                code={errorEnvelopeFrom(query.error).code}
                message={errorEnvelopeFrom(query.error).message}
                suggestion={errorEnvelopeFrom(query.error).suggestion}
              />
            ) : (
              <pre
                data-testid="config-view-content"
                className="max-h-96 overflow-auto rounded-md border bg-muted/40 p-3 font-mono text-xs whitespace-pre-wrap"
              >
                {query.isLoading ? "Loading…" : (query.data?.value ?? "")}
              </pre>
            )}
          </DialogContent>
        </Dialog>
      ) : null}
    </>
  );
}

/** 新增/编辑对话框（同名 Set = 覆盖即换版）。 */
function ConfigEditDialog({
  app,
  row,
  trigger,
}: {
  app: string;
  row?: ConfigView;
  trigger: React.ReactNode;
}) {
  const queryClient = useQueryClient();
  const [open, setOpen] = useState(false);
  const [name, setName] = useState(row?.name ?? "");
  const [value, setValue] = useState("");
  const [error, setError] = useState<ReturnType<typeof errorEnvelopeFrom> | null>(null);

  const mutation = useMutation({
    mutationFn: () => setConfig(app, name, value),
    onSuccess: () => {
      setOpen(false);
      setValue("");
      setError(null);
      void queryClient.invalidateQueries({ queryKey: ["configs", app] });
      void queryClient.invalidateQueries({ queryKey: ["config", app, name] });
    },
    onError: (err) => setError(errorEnvelopeFrom(err)),
  });

  function onSubmit(e: FormEvent) {
    e.preventDefault();
    if (NAME_PATTERN.test(name) && value) mutation.mutate();
  }

  return (
    <>
      <span onClick={() => setOpen(true)}>{trigger}</span>
      {open ? (
        <Dialog open onOpenChange={(v) => (v ? undefined : setOpen(false))}>
          <DialogContent data-testid="config-edit-dialog" className="max-w-2xl">
            <DialogHeader>
              <DialogTitle>{row ? `Edit config ${row.name}` : "New config"}</DialogTitle>
              <DialogDescription>
                Plaintext, versioned content mounted read-only by compose{" "}
                <code>configs:</code> on the next deploy. Saving the same name
                replaces the content — the referencing service picks it up (and
                rolls) on its next deploy. Directory trees should be baked into
                the image instead.
              </DialogDescription>
            </DialogHeader>
            <form className="space-y-3" onSubmit={onSubmit}>
              <div className="space-y-1.5">
                <Label htmlFor="config-name">Name</Label>
                <Input
                  id="config-name"
                  data-testid="config-name-input"
                  className="font-mono text-xs"
                  placeholder="app.yaml"
                  value={name}
                  disabled={row !== undefined}
                  onChange={(e) => setName(e.target.value)}
                />
              </div>
              <div className="space-y-1.5">
                <Label htmlFor="config-value">Content</Label>
                <textarea
                  id="config-value"
                  data-testid="config-value-input"
                  className="min-h-48 w-full rounded-md border bg-transparent p-2 font-mono text-xs shadow-sm"
                  placeholder="log_level: info"
                  value={value}
                  onChange={(e) => setValue(e.target.value)}
                />
              </div>
              {error ? (
                <EnvelopeAlert code={error.code} message={error.message} suggestion={error.suggestion} />
              ) : null}
              <DialogFooter>
                <Button
                  type="submit"
                  data-testid="config-save-submit"
                  disabled={!NAME_PATTERN.test(name) || !value || mutation.isPending}
                >
                  {mutation.isPending ? "Saving…" : row ? "Save config" : "Create config"}
                </Button>
              </DialogFooter>
            </form>
          </DialogContent>
        </Dialog>
      ) : null}
    </>
  );
}

function ConfigRow({
  app,
  row,
  canViewPlaintext,
}: {
  app: string;
  row: ConfigView;
  canViewPlaintext: boolean;
}) {
  const queryClient = useQueryClient();
  const [confirmOpen, setConfirmOpen] = useState(false);
  const [error, setError] = useState<ReturnType<typeof errorEnvelopeFrom> | null>(null);

  const removeMutation = useMutation({
    mutationFn: () => removeConfig(app, row.name ?? ""),
    onSuccess: () => {
      setError(null);
      setConfirmOpen(false);
      void queryClient.invalidateQueries({ queryKey: ["configs", app] });
    },
    onError: (err) => setError(errorEnvelopeFrom(err)),
  });

  return (
    <TableRow data-testid="config-row" data-name={row.name}>
      <TableCell className="font-mono text-xs">{row.name}</TableCell>
      <TableCell
        className="font-mono text-xs text-muted-foreground"
        title="sha256 prefix of the content — the swarm config object name embeds it, so a content change renames the reference and rolls the service"
      >
        {row.hash8}
      </TableCell>
      <TableCell className="whitespace-nowrap text-xs text-muted-foreground">
        updated {timeAgo(row.updated_at)}
      </TableCell>
      <TableCell className="text-right">
        <div className="flex items-center justify-end gap-1">
          {canViewPlaintext ? <ViewConfigDialog app={app} name={row.name ?? ""} /> : null}
          {canViewPlaintext ? (
            <ConfigEditDialog
              app={app}
              row={row}
              trigger={
                <Button
                  variant="ghost"
                  size="icon"
                  className="h-7 w-7"
                  aria-label={`Edit config ${row.name}`}
                  data-testid="config-edit-button"
                >
                  <Pencil aria-hidden className="h-3.5 w-3.5" />
                </Button>
              }
            />
          ) : null}
          {canViewPlaintext ? (
            <Button
              variant="ghost"
              size="icon"
              className="h-7 w-7 text-red-600 dark:text-red-400"
              aria-label={`Remove config ${row.name}`}
              data-testid="config-remove-button"
              onClick={() => setConfirmOpen(true)}
            >
              <Trash2 aria-hidden className="h-3.5 w-3.5" />
            </Button>
          ) : null}
        </div>
      </TableCell>
      {confirmOpen ? (
        <Dialog open onOpenChange={(v) => (v ? undefined : setConfirmOpen(false))}>
          <DialogContent data-testid="config-remove-dialog">
            <DialogHeader>
              <DialogTitle>Remove config {row.name}</DialogTitle>
              <DialogDescription>
                Services that still declare this config keep running, but their
                next deployment fails honestly with E_CONFIG_NOT_FOUND until the
                declaration is removed. Setting the same name again restores it.
              </DialogDescription>
            </DialogHeader>
            {error ? (
              <EnvelopeAlert code={error.code} message={error.message} suggestion={error.suggestion} />
            ) : null}
            <DialogFooter>
              <Button
                variant="destructive"
                data-testid="config-remove-submit"
                disabled={removeMutation.isPending}
                onClick={() => removeMutation.mutate()}
              >
                {removeMutation.isPending ? "Removing…" : "Remove config"}
              </Button>
            </DialogFooter>
          </DialogContent>
        </Dialog>
      ) : null}
    </TableRow>
  );
}

export function AppConfigsPage() {
  const { name = "" } = useParams();
  // 角色门（前端体验门，§3.2）：configs 写面与明文查看 = admin+（与 secrets
  // 同门；服务端硬门不变）。平台管理员资源面恒只读（P0-3 双门）——控件
  // 消失时以说明行明示原因，不做静默消失。
  const { canAdminResources } = useTeamCapabilities();
  const isPlatformAdmin = useIsPlatformAdmin();
  const query = useQuery({
    queryKey: ["configs", name],
    queryFn: () => listConfigs(name),
  });

  if (query.isError) {
    const envelope = errorEnvelopeFrom(query.error);
    return (
      <EnvelopeAlert
        code={envelope.code}
        message={envelope.message}
        suggestion={envelope.suggestion}
        docs={envelope.docs}
      />
    );
  }

  const rows = query.data?.configs ?? [];

  return (
    <Card data-testid="configs-page">
      <CardHeader className="flex-row items-center justify-between space-y-0 border-b pb-3">
        <div>
          <CardTitle className="flex items-center gap-2 text-sm font-semibold">
            <FileCode2 aria-hidden className="h-4 w-4 text-muted-foreground" />
            App configs
          </CardTitle>
          <CardDescription className="mt-1">
            Plaintext, versioned config files mounted read-only by compose{" "}
            <code>configs:</code> at explicit absolute paths (e.g.{" "}
            <code>/etc/app/config.yaml</code>). Content changes create a new
            content-addressed object, so the referencing service rolls on its
            next deploy. For a few runtime files only — directory trees belong
            in the image; credentials belong in secrets.
          </CardDescription>
        </div>
        <Button
          variant="ghost"
          size="icon"
          className="h-7 w-7"
          aria-label="Refresh configs"
          onClick={() => void query.refetch()}
        >
          <RefreshCw aria-hidden className="h-3.5 w-3.5" />
        </Button>
      </CardHeader>
      <CardContent className="divide-y pt-0">
        {canAdminResources ? (
          <section className="py-4">
            <ConfigEditDialog
              app={name}
              trigger={
                <Button size="sm" data-testid="config-new-button">
                  <Plus aria-hidden className="h-3.5 w-3.5" />
                  New config
                </Button>
              }
            />
          </section>
        ) : isPlatformAdmin ? (
          // P0-3：平台管理员资源面只读——说明行（CLI 有 configs set，文案
          // 如实指路）。
          <section className="py-4" data-testid="platform-readonly-note">
            <p className="text-sm text-muted-foreground">
              Platform administrators have read-only access to resources
              (separation of duties). Set or remove configs from the CLI with a
              machine token (<code>fleetly configs set</code>), or ask a team
              owner for a member role.
            </p>
          </section>
        ) : null}
        <section className="py-4">
          {rows.length === 0 ? (
            <p className="text-sm text-muted-foreground" data-testid="configs-empty">
              No configs stored.{" "}
              {canAdminResources
                ? "Create one above, declare it under the compose file's top-level "
                : "Declare it under the compose file's top-level "}
              <code>configs:</code> (external: true) and mount it from a service
              with <code>target</code>.
            </p>
          ) : (
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>Name</TableHead>
                  <TableHead>Content fingerprint</TableHead>
                  <TableHead>Updated</TableHead>
                  <TableHead />
                </TableRow>
              </TableHeader>
              <TableBody>
                {rows.map((r) => (
                  <ConfigRow key={r.name} app={name} row={r} canViewPlaintext={canAdminResources} />
                ))}
              </TableBody>
            </Table>
          )}
        </section>
      </CardContent>
    </Card>
  );
}
