package cmd

// backups 命令（T2.22 备份基线读面/手动触发）：经 SDK 消费平台的
// SystemService.ListBackups（台账只读）与 TriggerBackup（手动触发一次
// 热备快照，同步返回落账后的台账行）。
//
// 诚实契约的消费口径：verify_status=failed 的行是红色告警面——list 如实
// 渲染失败行与原因；create 在 verify 失败时以非零退出码 + 错误原文返回
// （升级编排的 pre_upgrade 快照依赖此语义阻断）。

import (
	"context"
	"flag"
	"fmt"
	"strings"

	"github.com/lynx-go/commands"

	serverv1 "github.com/fleetlyrun/fleetly/genproto/fleetly/server/v1"
)

// ── fleetly backups ──────────────────────────────────────────────────────────

// backupsCmd 是外层动词 `backups`：分发 list/create。
type backupsCmd struct {
	sub *commands.App
}

func newBackupsCmd() *backupsCmd {
	sub := commands.New()
	sub.Register(&backupsListCmd{}, &backupsCreateCmd{})
	sub.VerbTitle = "backups subcommands:"
	return &backupsCmd{sub: sub}
}

func (c *backupsCmd) Name() string { return "backups" }
func (c *backupsCmd) Synopsis() string {
	return "control-plane state backups (hot snapshots with verified read-back)"
}
func (c *backupsCmd) Usage() string { return "backups <list|create> [flags]" }

func (c *backupsCmd) SetFlags(_ *flag.FlagSet) {}

func (c *backupsCmd) Run(ctx context.Context, env *commands.Environment, args []string) error {
	if len(args) == 0 {
		return &commands.UsageError{Usage: c.Usage(), Err: fmt.Errorf("missing subcommand (list|create)")}
	}
	return subDispatchUsage(c, c.sub, ctx, env, args)
}

// backupView 是台账行的机器/人读共用形态（E3-3：上传结论三面随行——
// upload_status/uploaded_at/upload_error；uploaded_at 为空 = 从未尝试）。
type backupView struct {
	ID           string `json:"id"`
	Kind         string `json:"kind"`
	Path         string `json:"path"`
	SHA256       string `json:"sha256"`
	SizeBytes    int64  `json:"size_bytes"`
	VerifyStatus string `json:"verify_status"`
	Error        string `json:"error,omitempty"`
	CreatedAt    string `json:"created_at"`
	UploadStatus string `json:"upload_status"`
	UploadedAt   string `json:"uploaded_at,omitempty"`
	UploadError  string `json:"upload_error,omitempty"`
}

// backupsListCmd 实现 `fleetly backups list`：台账倒序列表。
type backupsListCmd struct {
	jsonOut bool
	conn    connFlags
}

func (c *backupsListCmd) Name() string { return "list" }
func (c *backupsListCmd) Synopsis() string {
	return "list state backup ledger rows (verified and failed)"
}
func (c *backupsListCmd) Usage() string {
	return "backups list [--addr <host:port>] [--token <tok>] [--json]"
}

func (c *backupsListCmd) SetFlags(fs *flag.FlagSet) {
	c.conn.register(fs)
	fs.BoolVar(&c.jsonOut, "json", false, "output machine-readable JSON")
}

func (c *backupsListCmd) Run(ctx context.Context, env *commands.Environment, args []string) error {
	if len(args) != 0 {
		return &commands.UsageError{Usage: c.Usage(), Err: fmt.Errorf("expected 0 arguments, got %d", len(args))}
	}
	return c.conn.withClient(func(cl *fleetlyClient) error {
		resp, err := cl.System().ListBackups(ctx, &serverv1.ListBackupsRequest{})
		if err != nil {
			return err
		}
		views := make([]backupView, 0, len(resp.GetBackups()))
		for _, b := range resp.GetBackups() {
			views = append(views, backupView{
				ID: b.GetId(), Kind: b.GetKind(), Path: b.GetPath(),
				SHA256: b.GetSha256(), SizeBytes: b.GetSizeBytes(),
				VerifyStatus: b.GetVerifyStatus(), Error: b.GetError(),
				CreatedAt: tstampRFC3339(b.GetCreatedAt()),
				// 上传结论三面（E3-3）：none = 未上传（s3.mode=unset 合法态）。
				UploadStatus: b.GetUploadStatus(),
				UploadedAt:   tstampRFC3339(b.GetUploadedAt()),
				UploadError:  b.GetUploadError(),
			})
		}
		if c.jsonOut {
			return writeJSON(env.Stdout, map[string]any{"backups": views})
		}
		var b strings.Builder
		b.WriteString("state backups (hot snapshots; manifest + sha256 verified read-back)\n")
		if len(views) == 0 {
			b.WriteString("  (no backups recorded yet — daily loop runs at daemon start; `fleetly backups create` forces one)\n")
		}
		for _, v := range views {
			fmt.Fprintf(&b, "  %s %s %s %d bytes sha256:%.12s %s",
				v.ID, v.Kind, v.CreatedAt, v.SizeBytes, v.SHA256, v.VerifyStatus)
			if v.Error != "" {
				fmt.Fprintf(&b, " error:%s", v.Error)
			}
			fmt.Fprintf(&b, " upload:%s", v.UploadStatus)
			if v.UploadedAt != "" {
				fmt.Fprintf(&b, " uploaded_at:%s", v.UploadedAt)
			}
			if v.UploadError != "" {
				fmt.Fprintf(&b, " upload_error:%s", v.UploadError)
			}
			b.WriteString("\n")
		}
		_, err = fmt.Fprint(env.Stdout, b.String())
		return err
	})
}

// backupsCreateCmd 实现 `fleetly backups create [--kind <k>]`：手动触发
// 一次热备快照（同步；verify 失败 → 退出 1，错误原文带出——升级编排的
// pre_upgrade 快照走同一入口）。本 RPC 的客户端 deadline 经长预算覆盖放
// 宽至 16min（conn.go：服务端合法耗时 = 快照 5min + 上传 10min——长上传
// 不再被 30s 缺省掐死；Ctrl-C 仍即时取消，服务端 WithoutCancel 下备份本
// 体不受客户端断连影响）。
type backupsCreateCmd struct {
	kind    string
	jsonOut bool
	conn    connFlags
}

func (c *backupsCreateCmd) Name() string { return "create" }
func (c *backupsCreateCmd) Synopsis() string {
	return "trigger a state backup now (waits for verified read-back and the remote upload verdict)"
}
func (c *backupsCreateCmd) Usage() string {
	return "backups create [--kind manual|pre_upgrade] [--addr <host:port>] [--token <tok>] [--json]"
}

func (c *backupsCreateCmd) SetFlags(fs *flag.FlagSet) {
	c.conn.register(fs)
	fs.StringVar(&c.kind, "kind", "manual", "backup kind (manual|pre_upgrade; daily/post_deploy are system-triggered)")
	fs.BoolVar(&c.jsonOut, "json", false, "output machine-readable JSON")
}

func (c *backupsCreateCmd) Run(ctx context.Context, env *commands.Environment, args []string) error {
	if len(args) != 0 {
		return &commands.UsageError{Usage: c.Usage(), Err: fmt.Errorf("expected 0 arguments, got %d", len(args))}
	}
	return c.conn.withClient(func(cl *fleetlyClient) error {
		resp, err := cl.System().TriggerBackup(ctx, &serverv1.TriggerBackupRequest{Kind: c.kind})
		if err != nil {
			return err
		}
		b := resp.GetBackup()
		view := backupView{
			ID: b.GetId(), Kind: b.GetKind(), Path: b.GetPath(),
			SHA256: b.GetSha256(), SizeBytes: b.GetSizeBytes(),
			VerifyStatus: b.GetVerifyStatus(), Error: b.GetError(),
			CreatedAt:    tstampRFC3339(b.GetCreatedAt()),
			UploadStatus: b.GetUploadStatus(),
			UploadedAt:   tstampRFC3339(b.GetUploadedAt()),
			UploadError:  b.GetUploadError(),
		}
		if c.jsonOut {
			return writeJSON(env.Stdout, map[string]any{"backup": view})
		}
		_, err = fmt.Fprintf(env.Stdout, "backup %s created (kind=%s, %d bytes, sha256 %.12s, verify=%s, upload=%s)\n",
			view.ID, view.Kind, view.SizeBytes, view.SHA256, view.VerifyStatus, view.UploadStatus)
		if err == nil && view.UploadError != "" {
			_, err = fmt.Fprintf(env.Stdout, "upload failed: %s\n", view.UploadError)
		}
		return err
	})
}

// 编译期断言：嵌套动词与子命令实现 commands.Command/Flagged 契约。
var (
	_ commands.Command = &backupsCmd{}
	_ commands.Flagged = &backupsCmd{}
	_ commands.Command = &backupsListCmd{}
	_ commands.Flagged = &backupsListCmd{}
	_ commands.Command = &backupsCreateCmd{}
	_ commands.Flagged = &backupsCreateCmd{}
)
