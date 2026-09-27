package database

// 引擎适配器（managed-databases §2.6 EngineAdapter 的 S5 落地）：每引擎的
// 备份/回读校验/原地恢复命令词表 + 一次性 job 载荷拼装。执行体 = 平台
// dbtools 镜像的一次性 Swarm job（D-DB-6「执行体」行：replicated-job 钉
// 绑定节点、库凭据与 restic 目标经 env 注入——本文件拼装载荷并兑现接口；
// 编排/台账/事件在 backup.go/restore.go/upgrade.go）。
//
// 明文纪律（负面测试钉死）：密码只进 job env（PGPASSWORD / REDISCLI_AUTH /
// MYSQL_PWD / MONGO_PASSWORD / RESTIC_PASSWORD / AWS_*）——命令词表零密码
// （Mongo 的 URI 消费 ${MONGO_PASSWORD} 运行时展开，引用不落字面量）；PG
// 恢复走 unix socket trust（官方镜像 pg_hba 对 local 全 trust）故恢复命令
// 零凭据；MySQL 恢复走 --skip-grant-tables socket-only 临时实例、Mongo 恢
// 复走免认证 loopback 临时实例（PG 同暴露类，见 restoreMySQLJobScript/
// restoreMongoJobScript 注）；restic 输出只含路径与字节量。凭据明文字段
//（dbtemplate.BackupInput 等）只存活于内存链。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/fleetlyrun/fleetly/internal/dbtemplate"
	"github.com/fleetlyrun/fleetly/internal/naming"
	"github.com/fleetlyrun/fleetly/internal/state"
)

// DefaultDatabaseToolsImage 是库备份/恢复/校验一次性 job 的平台镜像（E4
// D-DB-6：引擎工具 + restic，digest 钉定随平台 release——zot 同款双锚纪
// 律：tag 保留可读性、digest 为准，多架构 index 摘要 amd64/arm64 通吃）。
// S2 v0.2.x 重发为 debian/glibc 基底（deploy/Dockerfile.dbtools：基底 =
// postgres:16 与 dbtemplate.DefaultPostgresImage 同一钉定 digest，redis-cli
// 取自 debian 版 redis:7，restic 静态二进制照旧）——恢复单 job 的前提
// （本引擎与 dbtools 跨 libc 的 musl/glibc 重放风险随基底一致而消除）。
//
// **IMPL-DB-0（多 PG 大版本工具面）**：镜像重建为**单镜像双工具面**——
// postgres:16 基底自带 16 工具链，另从 postgres:18（DB-1 的 vanilla 引擎
// 同 digest）COPY 18 工具链到 Debian 版本分区路径（/usr/lib/postgresql/18
// + /usr/share/postgresql/18 + 缺失 soname），job 内按实例数据目录 major
// 以**显式绝对路径**选工具（见 pgToolDir 注：两代并存后 /usr/bin 的
// pg_wrapper 会把裸名解析为最新 major）。工具面版本纪律 = 与实例数据目录
// 同 major（pg_dump 拒更高 major 服务器、pg_restore 拒更高 major 归档、
// 临时 postgres 拒异 major 数据目录——一手实证见 docs/plan/
// 2026-09-26-torchwood-line-impl.md §4「IMPL-DB-0 方案可行性审查」）。
//
// 供应链：CI 首推 2026-09-23（run 35797985743），digest 已钉（多架构 index，
// buildx imagetools 独立解析）——中间态豁免已摘除，与平台其余镜像同构。
// **发布记录（IMPL-DB-0，2026-09-27）**：PG16+PG18 双工具面镜像经
// dbtools.yml dispatch 发布（run 36303530093，tag v0.3.1-dbtools.1；cosign
// keyless 签名 + 验签门随工作流）——三锚同批回填 = 本常量 +
// e2e/databases.sh DBTOOLS_IMG + 台账 docs/runbooks/image-prepull.md #22。
// 工具面：PG16 16.15 + PG18 18.6（版本分区路径，按 major 显式选取）、
// redis-cli、restic 0.19.1、mysql/mongo 工具面（与引擎逐位同版）。重建随
// 平台 release 由 .github/workflows/dbtools.yml 承载。
//
// **IMPL-DB-1（percona 发行版工具面）**：deploy/Dockerfile.dbtools 已增
// percona PG18 完整工具面（/usr/pgsql-18 的 bin/share/lib，与引擎镜像同
// digest）与 ICU 67 缺口闭包——percona 条目的 dump/verify/restore 全部
// 走 /usr/pgsql-18/bin（pgToolDir 按发行版选择；恢复重放的扩展文件面
// 随发行版整体承载）。**发布挂账**：不 commit/push 约束下 CI 无法构建
// 新内容（同 DB-0 兜底），重发（建议 tag v0.3.1-dbtools.2）后三锚回填
// = 本常量 + e2e/databases.sh DBTOOLS_IMG + 台账 #25 的 digest 列；回填
// 前 percona-postgresql-18 的 job 以「镜像缺该发行版工具面」fail-loud
// （缺面前置在 restorePostgresJobScript 首步）。
const DefaultDatabaseToolsImage = "ghcr.io/fleetlyrun/dbtools:v0.3.1-dbtools.1@sha256:c6cafbc303415f2df88410599ff5e28e97e7dcdc0fb9fb79adba4bddb1720382"

// 备份计划平台缺省（§5.4 配置键 databases.backup_*；实例 settings 零值
// 字段回落——平台缺省只在此处为常量，不进 config.yaml：备份计划属实例
// settings 资源面，S2 已受理展示）。
const (
	// DefaultBackupIntervalHours 是计划备份间隔（小时）。
	DefaultBackupIntervalHours = 24
	// DefaultBackupKeep 是保留份数。
	DefaultBackupKeep = 7
	// DefaultBackupHourUTC 是每日备份窗起点（UTC 小时）。
	DefaultBackupHourUTC = 3
)

// 一次性工具 job 的单步预算（平台常量，无配置面——cron 看门狗缺省同纪
// 律）：覆盖镜像分发 + 工具执行；job ctx 超预算即诚实判败（事件/事件面
// 台账承载结论）。
const (
	backupJobTimeout  = 10 * time.Minute // pg_dump/--rdb 导出 + restic 入库（首传含镜像拉取）
	verifyJobTimeout  = 5 * time.Minute  // restic dump 回读 + 引擎级头校验
	restoreJobTimeout = 15 * time.Minute // 停库重放（temp postgres 起停 + pg_restore）
	pruneJobTimeout   = 5 * time.Minute  // restic forget --prune（路径过滤）
	// upgradeWatchWindow 是升级健康门观察窗（provisioning 健康门预算同口
	// 径——start_period 30s + 探测窗；超窗判败 → digest 归位）。
	upgradeWatchWindow = 5 * time.Minute
	// upgradeOpTimeout 是升级编排整体预算（备份门 + 受控重建观察 + 归位
	// 余量——异步编排 ctx 与 Stop 排水的上界护栏）。
	upgradeOpTimeout = 30 * time.Minute
)

// repo 内路径约定（§2.6 备份目标行：命名空间 db/<instance>/；与控制面快
// 照同 repo——D-DB-6「独立 repo 被否」）。restic forget 的路径过滤与恢复
// 的 restic dump 都以该路径寻址。
const (
	pgBackupFilename    = "db.dump"
	redisBackupFilename = "dump.rdb"
	mysqlBackupFilename = "db.sql"     // mysqldump 文本导出（v0.3 W4 D-W4-3）
	mongoBackupFilename = "db.archive" // mongodump --archive --gzip 归档
)

// pgToolDir 返回模板对应 PG 工具面二进制目录（dbtools 镜像契约；**按发行版
// 与 major 双维选择**——IMPL-DB-1 起）：
//   - vanilla（官方镜像/Debian pgdg 布局）：/usr/lib/postgresql/<major>/bin
//     （版本分区目录，两代并存——见 deploy/Dockerfile.dbtools 头注）；
//   - percona（Percona 发行版面）：/usr/pgsql-<major>/bin——与 percona 引擎
//     镜像逐位同版；发行版差异面（扩展文件/pkglib 等）随该面整体承载，
//     恢复重放的 CREATE EXTENSION 才能落地（percona 自带 pgvector 及
//     pg_cron/pg_stat_monitor 等，vanilla 面缺面即硬失败——一手证据见
//     docs/plan/2026-09-26-torchwood-line-impl.md §4「IMPL-DB-1 方案
//     可行性审查（2026-09-27 续）」）。
//
// **工具面版本纪律：与实例数据目录同 major**（IMPL-DB-0 一手实证）：
//   - pg_dump 必须 ≥ 服务器 major（16 导 18 服务器 `aborting because of
//     server version mismatch`）；
//   - pg_restore 必须 ≥ dump 产出 major（16 读 18 归档 `unsupported version
//     (1.16) in file header`）；
//   - 临时恢复实例 postgres 必须 = 数据目录 major（异 major `database files
//     are incompatible with server`）。
//
// **必须显式绝对路径**：vanilla 面两代并存后 /usr/bin 的 postgresql-common
// pg_wrapper 会把裸名 pg_dump/psql/pg_restore/pg_isready 解析为**最新**
// major（实测 18），percona 面 /usr/bin 是 alternatives 符号链接——裸名不是
// 确定性来源。未知/缺失 major 与未知发行版诚实报错（不静默回落别代或
// 别发行版工具）。
func pgToolDir(tpl dbtemplate.Template) (string, error) {
	if tpl.Engine != dbtemplate.EnginePostgres {
		return "", fmt.Errorf("database: template %q is not a postgres engine (no pg tool face)", tpl.ID)
	}
	if tpl.Major <= 0 {
		return "", fmt.Errorf("database: template %q has no postgres major (pg tool face cannot be selected)", tpl.ID)
	}
	switch tpl.Distribution {
	case dbtemplate.DistributionVanilla:
		return fmt.Sprintf("/usr/lib/postgresql/%d/bin", tpl.Major), nil
	case dbtemplate.DistributionPercona:
		return fmt.Sprintf("/usr/pgsql-%d/bin", tpl.Major), nil
	default:
		return "", fmt.Errorf("database: template %q has unknown distribution %q (pg tool face cannot be selected)", tpl.ID, tpl.Distribution)
	}
}

// backupFilename 取模板对应的导出文件名（repo 内路径 = db/<instance>/<文件>）。
// 分派轴 = 引擎族（同族发行版/大版本共享——IMPL-DB-0）。
func backupFilename(tpl dbtemplate.Template) (string, error) {
	switch tpl.Engine {
	case dbtemplate.EnginePostgres:
		return pgBackupFilename, nil
	case dbtemplate.EngineRedis:
		return redisBackupFilename, nil
	case dbtemplate.EngineMySQL:
		return mysqlBackupFilename, nil
	case dbtemplate.EngineMongo:
		return mongoBackupFilename, nil
	default:
		return "", fmt.Errorf("database: template %q has no backup adapter", tpl.ID)
	}
}

// resticCmd 是带寻址形态的 restic 命令前缀（path-style → 扩展选项
// `-o s3.bucket-lookup=path`——rustfs 与 path_style=true 的 external 端点
// 必需；statebackup 上传轨同口径的 job 内形态）。
func resticCmd(pathStyle bool) string {
	if pathStyle {
		return "restic -o s3.bucket-lookup=path"
	}
	return "restic"
}

// backupJobScript 拼装备份命令（流式导出 | restic 入库；--json 产出
// summary 供 snapshot id / total_bytes 提取）：
//
//	PG     pg_dump -Fc（逻辑备份，运行中一致性；工具面按模板 major 显式
//	       绝对路径选取）| restic backup --stdin
//	Redis  redis-cli --rdb /dev/stdout（RDB 流式）| restic backup --stdin
//	MySQL  mysqldump --single-transaction --databases（InnoDB 一致性快照，
//	       D-W4-3；**不带 --source-data=2**——复制坐标需 RELOAD/FLUSH_TABLES
//	       特权，fleetly 用户（官方入口仅授 db.* ALL）跑必败且失败被管道
//	       掩蔽为「仅头部假快照」，W4-S4 真机实证后裁撤；坐标平台不消费）
//	       | restic backup --stdin
//	Mongo  mongodump --archive --gzip（归档流，D-W4-3）| restic backup --stdin
//
// /dev/stdout 而非字面 "-"：redis-cli 的 --rdb 接收**文件名**参数，设备
// 文件是管道流的可靠形态（alpine 容器内恒存在）。
// 明文纪律：MySQL/Mongo 凭据经 MYSQL_PWD/MONGO_PASSWORD env——Mongo 的
// URI 以 ${MONGO_PASSWORD} 引用展开（字面量不进 job spec；运行时 shell
// 展开与本仓 Redis 健康门 env 引用同暴露类——mongodump 无原生凭据 env，
// 该形态是命令词表零明文的唯一解）。
func backupJobScript(tpl dbtemplate.Template, in dbtemplate.BackupInput) ([]string, error) {
	filename, err := backupFilename(tpl)
	if err != nil {
		return nil, err
	}
	repoPath := "db/" + in.Instance + "/" + filename
	var export string
	switch tpl.Engine {
	case dbtemplate.EnginePostgres:
		dir, err := pgToolDir(tpl)
		if err != nil {
			return nil, err
		}
		export = fmt.Sprintf("%s/pg_dump -h %s -U fleetly -d %s -Fc",
			dir, in.Instance, dbtemplate.DatabaseName(in.Instance))
	case dbtemplate.EngineRedis:
		export = fmt.Sprintf("redis-cli -h %s --no-auth-warning --rdb /dev/stdout", in.Instance)
	case dbtemplate.EngineMySQL:
		// --databases：导出自带 CREATE DATABASE + USE——恢复重放前置 DROP
		// DATABASE 后裸重放必须有库名锚（W4-S2 容器内实证抓出）。不带
		// --source-data=2（特权不符+管道掩蔽假快照，W4-S4 真机裁撤——见
		// backupJobScript 头注）。
		export = fmt.Sprintf("mysqldump -h %s -u fleetly --single-transaction --databases %s",
			in.Instance, dbtemplate.DatabaseName(in.Instance))
	case dbtemplate.EngineMongo:
		// authSource=admin：官方入口把 initdb root 恒建于 admin 库（设计
		// managed-databases §8.1 实现注记）——URI 认证库名随其固定。
		export = fmt.Sprintf(`mongodump --uri "mongodb://fleetly:${MONGO_PASSWORD}@%s:27017/%s?authSource=admin" --archive --gzip`,
			in.Instance, dbtemplate.DatabaseName(in.Instance))
	default:
		return nil, fmt.Errorf("database: template %q has no backup adapter", tpl.ID)
	}
	return []string{"sh", "-c",
		fmt.Sprintf("%s | %s backup --stdin --stdin-filename %s --json",
			export, resticCmd(in.S3PathStyle), repoPath)}, nil
}

// verifyJobScript 拼装回读校验命令（§2.6 Verify 契约——「备份假成功」零
// 容忍的引擎级实现）：restic 读回快照 + 引擎级校验——
//
//	PG     restic dump > /tmp/v.dump && pg_restore --list（exit 0 = 归档有效；
//	       工具面按模板 major 显式绝对路径选取）
//	Redis  restic dump | head -c 5 == "REDIS"（RDB magic）
//	MySQL  restic dump | head -c 32 含 "MySQL dump"（mysqldump 文件头魔术串）
//	Mongo  restic dump | head -c 2 == gzip magic 1f 8b（--gzip 归档流头）
func verifyJobScript(tpl dbtemplate.Template, in dbtemplate.BackupOutcome) ([]string, error) {
	filename, err := backupFilename(tpl)
	if err != nil {
		return nil, err
	}
	repoPath := "db/" + in.Instance + "/" + filename
	switch tpl.Engine {
	case dbtemplate.EnginePostgres:
		dir, err := pgToolDir(tpl)
		if err != nil {
			return nil, err
		}
		return []string{"sh", "-c",
			fmt.Sprintf("%s dump %s %s > /tmp/v.dump && %s/pg_restore --list /tmp/v.dump > /dev/null",
				resticCmd(in.S3PathStyle), in.SnapshotID, repoPath, dir)}, nil
	case dbtemplate.EngineRedis:
		return []string{"sh", "-c",
			fmt.Sprintf("%s dump %s %s | head -c 5 | grep -q REDIS",
				resticCmd(in.S3PathStyle), in.SnapshotID, repoPath)}, nil
	case dbtemplate.EngineMySQL:
		// 双门：文件头魔术串 + **CREATE DATABASE 在场**——W4-S4 真机实证
		// 「仅头部假快照」（mysqldump 失败被管道掩蔽）能过头部门，恢复
		// 重放空内容即删库；内容门让假快照在 verify 期显性失败（防御纵深
		// ——根因已在备份词表修复，此处是检测面兜底）。
		return []string{"sh", "-c",
			fmt.Sprintf(`%s dump %s %s > /tmp/v.dump && head -c 32 /tmp/v.dump | grep -q "MySQL dump" && grep -q "CREATE DATABASE" /tmp/v.dump`,
				resticCmd(in.S3PathStyle), in.SnapshotID, repoPath)}, nil
	case dbtemplate.EngineMongo:
		// gzip 头两字节 0x1f 0x8b：od 十六进制化后比对（busybox/coreutils
		// 双兼容形态，dbtools 内 od 恒在）。
		return []string{"sh", "-c",
			fmt.Sprintf(`%s dump %s %s | head -c 2 | od -An -tx1 | tr -d ' \n' | grep -q 1f8b`,
				resticCmd(in.S3PathStyle), in.SnapshotID, repoPath)}, nil
	default:
		return nil, fmt.Errorf("database: template %q has no verify adapter", tpl.ID)
	}
}

// restoreJobScript 拼装原地恢复命令（停库重放；实例服务已 scale 0、job 钉
// 绑定节点挂数据卷 rw——「远端 local 卷不可经 manager 读」约束下的唯一
// 执行位置）。PG = **单 job**：dbtools 自 v0.2.1-dbtools.1 起是 debian/
// glibc 基底（postgres:16，与 dbtemplate.DefaultPostgresImage 同一钉定
// digest——引擎二进制与 dbtools 内的工具逐位同源），restic 取回快照与
// 临时实例重放在同一 job 内完成；W4 时代的双 job（dbtools 只取 dump 落卷
// + 引擎镜像起临时 postgres 重放）随 musl/glibc 跨 libc 重放风险的消除
// 而回退（编排更短、job 数减半、少一次镜像分发）。Redis 无引擎参与，同
// 样单 job（restoreRedisFetchScript）。
//
// 保留的防御（W4-S6 实测链逐条延续，单 job 下语义不变）：
//   - 快照 dump 先落卷根暂存文件再重放（材料落盘可对账；暂存文件重放后
//     清场——残留只会误导人工排查）；
//   - 降权 uid 从数据目录属主探测（dbtools 的 postgres uid 与卷上文件属
//     主对齐以数据目录为准，gosu/su-exec 均接受数字 uid）；
//   - 临时实例拉起带 kill -0 看护重启（scale 0 与 job 之间无任务全停等待
//     窗，旧引擎下线期一次性起动必失败）；
//   - pg_ctl 停临时实例与起动同 uid（root 形态失败会经 set -e 误判重放
//     失败）。
const replayDumpFilename = "fleetly-replay.dump"

func restorePostgresJobScript(tpl dbtemplate.Template, in dbtemplate.RestoreInput) ([]string, error) {
	filename, err := backupFilename(tpl)
	if err != nil {
		return nil, err
	}
	dir, err := pgToolDir(tpl)
	if err != nil {
		return nil, err
	}
	repoPath := "db/" + in.Instance + "/" + filename
	dumpPath := in.VolumeTarget + "/" + replayDumpFilename
	db := dbtemplate.DatabaseName(in.Instance)
	// PGDATA 自挂载点参数化（IMPL-DB-0）：PG16 现值 /var/lib/postgresql/data/
	// pgdata 逐字不变；发行版按条目携带的原生挂载点自然成立（percona
	// /data/db → /data/db/pgdata）。工具二进制按模板 major 显式绝对路径
	// （pgToolDir 注：裸名会被 pg_wrapper 解析为最新 major）。
	pgData := in.VolumeTarget + "/pgdata"
	script := strings.Join([]string{
		"set -e",
		// 工具面版本纪律前置（IMPL-DB-0）：镜像必须携带与数据目录同 major
		// 的工具链——缺面即在此点名退败（免看护窗耗尽后才以「未就绪」
		// 误报；镜像版本纪律见 deploy/Dockerfile.dbtools 头注）。
		fmt.Sprintf(`test -x %s/postgres || { echo "dbtools image lacks the postgres %d tool face (%s) — the tool face must match the data directory major" >&2; exit 66; }`, dir, tpl.Major, dir),
		// ① 取回快照落卷根暂存文件（restic 材料在 env）。
		fmt.Sprintf("%s dump %s %s > %s",
			resticCmd(in.S3PathStyle), in.SnapshotID, repoPath, dumpPath),
		// ② 临时实例重放（停库重放本体）。
		`if command -v gosu >/dev/null 2>&1; then PRIVDROP="gosu"; elif command -v su-exec >/dev/null 2>&1; then PRIVDROP="su-exec"; else echo "no privilege-drop tool in job image" >&2; exit 64; fi`,
		`PGDATA=` + pgData + `; export PGDATA`,
		// 降权 uid 从数据目录属主探测（gosu/su-exec 均接受数字 uid——
		// dbtools 的 postgres passwd 条目与卷上文件属主一致，探测只是
		// 免假设的收口）。
		`PGUID=$(stat -c %u "$PGDATA")`,
		`$PRIVDROP "$PGUID" ` + dir + `/postgres &`,
		`PGPID=$!`,
		`i=0`,
		`until ` + dir + `/pg_isready -h /var/run/postgresql -U fleetly >/dev/null 2>&1; do`,
		`  i=$((i+1))`,
		`  if [ "$i" -gt 90 ]; then echo "temporary postgres did not become ready" >&2; exit 65; fi`,
		`  if ! kill -0 "$PGPID" 2>/dev/null; then sleep 2; $PRIVDROP "$PGUID" ` + dir + `/postgres & PGPID=$!; fi`,
		`  sleep 1`,
		`done`,
		fmt.Sprintf(`%s/psql -h /var/run/postgresql -U fleetly -d postgres -v ON_ERROR_STOP=1 -c 'DROP DATABASE IF EXISTS "%s";' -c 'CREATE DATABASE "%s";'`, dir, db, db),
		fmt.Sprintf(`%s/pg_restore -h /var/run/postgresql -U fleetly -d "%s" --no-owner %s`, dir, db, dumpPath),
		// 停临时实例与起动同 uid（pg_ctl 拒以 root 运行——`set -e` 下
		// 根形态失败会让成功的重放被误判为失败）。
		`$PRIVDROP "$PGUID" ` + dir + `/pg_ctl -D "$PGDATA" -m fast stop`,
		// ③ 暂存 dump 清场（卷根文件不属集群数据）。
		fmt.Sprintf(`rm -f %s`, dumpPath),
	}, "\n")
	return []string{"sh", "-c", script}, nil
}

// pruneJobScript 拼装保留对齐命令（forget 以本实例的 repo 路径过滤——共
// 享 repo 下无过滤的 forget 会波及全部快照〔控制面备份与他实例〕；路径
// 过滤使 keep-last N 恰为本实例的保留策略，§2.6「prune 沿用台账保留期
// 删除语义」）。
func pruneJobScript(tpl dbtemplate.Template, in dbtemplate.BackupOutcome, keep int) ([]string, error) {
	filename, err := backupFilename(tpl)
	if err != nil {
		return nil, err
	}
	repoPath := "db/" + in.Instance + "/" + filename
	return []string{"sh", "-c",
		fmt.Sprintf("%s forget --keep-last %d --path %s --prune",
			resticCmd(in.S3PathStyle), keep, repoPath)}, nil
}

// ── dbtemplate.EngineAdapter 的 Manager 兑现（接口非装饰——编排紧邻底
//    座，兑现点即 Manager 方法；编排入口在 backup.go/restore.go）────────

// 编译期契约钉：Manager 兑现 §2.6 EngineAdapter 全接口（Backup/Restore/
// Verify/RotateCredential——S1 钉接口、S5 兑现）。
var _ dbtemplate.EngineAdapter = (*Manager)(nil)

// Backup 逻辑备份（dbtemplate.EngineAdapter 契约）：一次性 job 流式导出 →
// restic 入库（repo 内路径 db/<instance>/）。失败/无快照 id 都是诚实错误
// ——调用方（编排层）落 db.backup_failed 事件，无台账行。
func (m *Manager) Backup(ctx context.Context, in dbtemplate.BackupInput) (dbtemplate.BackupOutcome, error) {
	tpl, err := dbtemplate.Get(in.TemplateID)
	if err != nil {
		return dbtemplate.BackupOutcome{}, fmt.Errorf("database: template %q has no backup adapter: %w", in.TemplateID, err)
	}
	script, err := backupJobScript(tpl, in)
	if err != nil {
		return dbtemplate.BackupOutcome{}, err
	}
	net, err := naming.DBNetworkName(in.TeamSlug, in.PrjSlug, in.Instance)
	if err != nil {
		return dbtemplate.BackupOutcome{}, err
	}
	outcome, err := m.runToolsJob(ctx, toolsJobInput{
		instance: in.Instance,
		purpose:  "backup",
		script:   script,
		env:      toolsJobEnv(in.Password, in.Repository, in.ResticPassword, in.S3AccessKeyID, in.S3SecretKey, in.S3Region),
		networks: jobNetworks(in.AttachRustfsNetwork, net),
		timeout:  backupJobTimeout,
		bindNode: in.BindNodeID,
		teamSlug: in.TeamSlug,
		prjSlug:  in.PrjSlug,
	})
	if err != nil {
		return dbtemplate.BackupOutcome{}, err
	}
	if !outcome.Success() {
		return dbtemplate.BackupOutcome{}, fmt.Errorf("backup job failed: %s", jobFailureText(outcome))
	}
	snap, size := parseResticSummary(outcome.Stdout)
	if snap == "" {
		return dbtemplate.BackupOutcome{}, errors.New("restic backup produced no snapshot id (summary message missing)")
	}
	return m.outcomeContext(in, snap, size), nil
}

// Verify 回读校验（dbtemplate.EngineAdapter 契约）：restic 读回 + 引擎级
// 头校验。exit 0 = 有效；其余（含超预算）= 校验失败（红色告警面）。
func (m *Manager) Verify(ctx context.Context, in dbtemplate.BackupOutcome) error {
	tpl, err := dbtemplate.Get(in.TemplateID)
	if err != nil {
		return fmt.Errorf("database: template %q has no verify adapter: %w", in.TemplateID, err)
	}
	script, err := verifyJobScript(tpl, in)
	if err != nil {
		return err
	}
	net, err := naming.DBNetworkName(in.TeamSlug, in.PrjSlug, in.Instance)
	if err != nil {
		return err
	}
	outcome, err := m.runToolsJob(ctx, toolsJobInput{
		instance: in.Instance,
		purpose:  "verify",
		script:   script,
		env:      toolsJobEnv("", in.Repository, in.ResticPassword, in.S3AccessKeyID, in.S3SecretKey, in.S3Region),
		networks: jobNetworks(in.AttachRustfsNetwork, net),
		timeout:  verifyJobTimeout,
		bindNode: in.BindNodeID,
		teamSlug: in.TeamSlug,
		prjSlug:  in.PrjSlug,
	})
	if err != nil {
		return err
	}
	if !outcome.Success() {
		return fmt.Errorf("verify job failed: %s", jobFailureText(outcome))
	}
	return nil
}

// Restore 原地恢复（dbtemplate.EngineAdapter 契约）：纯执行体——scale 0/
// 事件/失败口径归 restore.go 编排；本方法只跑恢复 job。PG = 单 dbtools job
// （restic 取回 + 临时 postgres 重放同 job——v0.2.1-dbtools.1 起基底与引
// 擎同源 glibc，跨 libc 重放风险消除，见 restorePostgresJobScript 注）；
// Redis = 单 dbtools job（RDB 落卷）。
func (m *Manager) Restore(ctx context.Context, in dbtemplate.RestoreInput) error {
	tpl, err := dbtemplate.Get(in.TemplateID)
	if err != nil {
		return fmt.Errorf("database: template %q has no restore adapter: %w", in.TemplateID, err)
	}
	net, err := naming.DBNetworkName(in.TeamSlug, in.PrjSlug, in.Instance)
	if err != nil {
		return err
	}
	materials := toolsJobEnv("", in.Repository, in.ResticPassword, in.S3AccessKeyID, in.S3SecretKey, in.S3Region)
	mounts := []JobMount{{VolumeName: in.VolumeName, Target: in.VolumeTarget, ReadOnly: false}}
	nets := jobNetworks(in.AttachRustfsNetwork, net)
	var script []string
	switch tpl.Engine {
	case dbtemplate.EnginePostgres:
		script, err = restorePostgresJobScript(tpl, in)
	case dbtemplate.EngineRedis:
		// Redis：fetch 即重放完成（RDB 落卷 + AOF 目录清除在 fetch script
		// 的卷内收尾——dbtools 与 redis 引擎镜像同为 glibc 可执行面）。
		script, err = restoreRedisFetchScript(tpl, in)
	case dbtemplate.EngineMySQL:
		// MySQL：临时 mysqld 起于数据卷重放（D-W4-3，restoreMySQLJobScript）。
		script, err = restoreMySQLJobScript(tpl, in)
	case dbtemplate.EngineMongo:
		// MongoDB：临时 mongod 起于数据卷重放（D-W4-3，restoreMongoJobScript）。
		script, err = restoreMongoJobScript(tpl, in)
	default:
		return fmt.Errorf("database: template %q has no restore adapter", in.TemplateID)
	}
	if err != nil {
		return err
	}
	outcome, err := m.runToolsJob(ctx, toolsJobInput{
		instance: in.Instance,
		purpose:  "restore",
		script:   script,
		env:      materials,
		networks: nets,
		mounts:   mounts,
		timeout:  restoreJobTimeout,
		bindNode: in.BindNodeID,
		teamSlug: in.TeamSlug,
		prjSlug:  in.PrjSlug,
	})
	if err != nil {
		return err
	}
	if !outcome.Success() {
		return fmt.Errorf("restore job failed: %s", jobFailureText(outcome))
	}
	return nil
}

// restoreRedisFetchScript 是 Redis 的单 job 恢复命令（RDB 落卷 + AOF 目录
// 清除——下次启动按 RDB 装载；无引擎参与，dbtools 内 restic 取回即完成）。
func restoreRedisFetchScript(tpl dbtemplate.Template, in dbtemplate.RestoreInput) ([]string, error) {
	filename, err := backupFilename(tpl)
	if err != nil {
		return nil, err
	}
	repoPath := "db/" + in.Instance + "/" + filename
	script := strings.Join([]string{
		"set -e",
		fmt.Sprintf("%s dump %s %s > /data/dump.rdb", resticCmd(in.S3PathStyle), in.SnapshotID, repoPath),
		"rm -rf /data/appendonlydir",
	}, "\n")
	return []string{"sh", "-c", script}, nil
}

// replaySQLFilename / replayArchiveFilename 是恢复材料在卷根的暂存文件名
// （材料落盘可对账；重放后清场——残留只会误导人工排查，PG 同纪律）。
const (
	replaySQLFilename     = "fleetly-replay.sql"
	replayArchiveFilename = "fleetly-replay.archive"
)

// restoreMySQLJobScript 是 MySQL 的单 job 恢复命令（D-W4-3：停库重放 =
// 快照落卷根暂存 → 临时 mysqld 起于数据卷 → `mysql < dump.sql` 重放 →
// 关停清场）。与 PG 恢复同构的防御面：
//   - `--skip-grant-tables`：免认证重放（卷内 fleetly 密码可能是备份时刻
//     旧值，恢复前置态不依赖它）；该旗标自动蕴含 `--skip-networking`（8.0+
//     文档语义）——临时实例 socket-only，与 PG「unix socket trust」同暴露
//     类：无网络监听、容器内瞬态、不进实例共享网；
//   - 降权 uid 从数据目录属主探测（mysqld 拒以 root 运行——gosu/su-exec
//     接受数字 uid，PG 同款探测收口）；
//   - 临时实例拉起带 kill -0 看护重启（旧引擎下线期一次性起动可能失败）；
//   - `mysqladmin shutdown` 与起动同 uid（root 形态在 `set -e` 下会让成
//     功的重放被误判失败）。
//   - 暂存 dump 落卷根（datadir 根散文件不被 mysqld 当作数据库扫描），
//     重放后清场。
//
// 重放语义：mysqldump 导出自带 `CREATE DATABASE IF NOT EXISTS` + `USE`
// （按库导出的官方形态），前置 DROP DATABASE 保证幂等重放（重放即回到备
// 份时刻——半程失败由编排层保持实例停止的既有口径承载）。
func restoreMySQLJobScript(tpl dbtemplate.Template, in dbtemplate.RestoreInput) ([]string, error) {
	filename, err := backupFilename(tpl)
	if err != nil {
		return nil, err
	}
	repoPath := "db/" + in.Instance + "/" + filename
	dumpPath := in.VolumeTarget + "/" + replaySQLFilename
	sock := "/run/mysqld/mysqld.sock"
	db := dbtemplate.DatabaseName(in.Instance)
	script := strings.Join([]string{
		"set -e",
		// ① 取回快照落卷根暂存文件（restic 材料在 env）。
		fmt.Sprintf("%s dump %s %s > %s",
			resticCmd(in.S3PathStyle), in.SnapshotID, repoPath, dumpPath),
		// ② 临时实例重放（停库重放本体）。
		`if command -v gosu >/dev/null 2>&1; then PRIVDROP="gosu"; elif command -v su-exec >/dev/null 2>&1; then PRIVDROP="su-exec"; else echo "no privilege-drop tool in job image" >&2; exit 64; fi`,
		`MYUID=$(stat -c %u ` + in.VolumeTarget + `)`,
		`mkdir -p /run/mysqld && chown "$MYUID" /run/mysqld`,
		fmt.Sprintf(`$PRIVDROP "$MYUID" mysqld --skip-grant-tables --skip-networking --socket=%s --datadir=%s &`, sock, in.VolumeTarget),
		`MYPID=$!`,
		`i=0`,
		fmt.Sprintf(`until mysqladmin --socket=%s ping >/dev/null 2>&1; do`, sock),
		`  i=$((i+1))`,
		`  if [ "$i" -gt 90 ]; then echo "temporary mysqld did not become ready" >&2; exit 65; fi`,
		fmt.Sprintf(`  if ! kill -0 "$MYPID" 2>/dev/null; then sleep 2; $PRIVDROP "$MYUID" mysqld --skip-grant-tables --skip-networking --socket=%s --datadir=%s & MYPID=$!; fi`, sock, in.VolumeTarget),
		`  sleep 1`,
		`done`,
		"mysql --socket=" + sock + " -e 'DROP DATABASE IF EXISTS `" + db + "`;'",
		fmt.Sprintf(`mysql --socket=%s < %s`, sock, dumpPath),
		fmt.Sprintf(`$PRIVDROP "$MYUID" mysqladmin --socket=%s shutdown`, sock),
		// ③ 暂存 dump 清场（卷根文件不属引擎数据）。
		fmt.Sprintf(`rm -f %s`, dumpPath),
	}, "\n")
	return []string{"sh", "-c", script}, nil
}

// restoreMongoJobScript 是 MongoDB 的单 job 恢复命令（D-W4-3：停库重放 =
// 快照落卷根暂存 → 临时 mongod 起于数据卷 → `mongorestore --archive
// --gzip --drop` 重放 → 关停清场）。防御面与 MySQL 脚本同构：
//   - 临时 mongod 不带 --auth（授权是进程旗标非卷内持久态——本地全权 +
//     `--bind_ip 127.0.0.1` 锁回环，与 PG socket trust 同暴露类）；
//   - 降权 uid 从数据目录属主探测（mongod 不建议以 root 运行——同款探测
//     收口）；
//   - kill -0 看护重启 + `mongosh … shutdown` 优雅关停（连接随关停断开，
//     `|| true` 收尾）+ wait 进程退出（WiredTiger 检查点落盘后 job 才收）。
func restoreMongoJobScript(tpl dbtemplate.Template, in dbtemplate.RestoreInput) ([]string, error) {
	filename, err := backupFilename(tpl)
	if err != nil {
		return nil, err
	}
	repoPath := "db/" + in.Instance + "/" + filename
	archivePath := in.VolumeTarget + "/" + replayArchiveFilename
	script := strings.Join([]string{
		"set -e",
		// ① 取回快照落卷根暂存文件（restic 材料在 env）。
		fmt.Sprintf("%s dump %s %s > %s",
			resticCmd(in.S3PathStyle), in.SnapshotID, repoPath, archivePath),
		// ② 临时实例重放（停库重放本体）。
		`if command -v gosu >/dev/null 2>&1; then PRIVDROP="gosu"; elif command -v su-exec >/dev/null 2>&1; then PRIVDROP="su-exec"; else echo "no privilege-drop tool in job image" >&2; exit 64; fi`,
		`MUID=$(stat -c %u ` + in.VolumeTarget + `)`,
		fmt.Sprintf(`$PRIVDROP "$MUID" mongod --dbpath %s --bind_ip 127.0.0.1 --port 27017 &`, in.VolumeTarget),
		`MOPID=$!`,
		`i=0`,
		`until mongosh --quiet --host 127.0.0.1 --port 27017 --eval "db.adminCommand('ping')" >/dev/null 2>&1; do`,
		`  i=$((i+1))`,
		`  if [ "$i" -gt 90 ]; then echo "temporary mongod did not become ready" >&2; exit 65; fi`,
		fmt.Sprintf(`  if ! kill -0 "$MOPID" 2>/dev/null; then sleep 2; $PRIVDROP "$MUID" mongod --dbpath %s --bind_ip 127.0.0.1 --port 27017 & MOPID=$!; fi`, in.VolumeTarget),
		`  sleep 1`,
		`done`,
		fmt.Sprintf(`mongorestore --host 127.0.0.1 --port 27017 --archive=%s --gzip --drop`, archivePath),
		`mongosh --quiet --host 127.0.0.1 --port 27017 --eval "db.adminCommand({shutdown: 1})" >/dev/null 2>&1 || true`,
		`wait "$MOPID" 2>/dev/null || true`,
		// ③ 暂存 archive 清场（卷根文件不属引擎数据）。
		fmt.Sprintf(`rm -f %s`, archivePath),
	}, "\n")
	return []string{"sh", "-c", script}, nil
}

// RotateCredential 引擎侧热轮换（dbtemplate.EngineAdapter 契约——薄委托
// rotate.go 的既有原语，接口非装饰）：PG = 一次性容器 ALTER USER；MySQL =
// 一次性容器 ALTER USER（fleetly@'%'）；Mongo = 一次性容器 updateUser
// （admin 库）；Redis = 无引擎侧动作（spec 启动参数投递，收敛 duty 按哈
// 希差换挂）。
func (m *Manager) RotateCredential(ctx context.Context, in dbtemplate.RotateInput) error {
	inst, err := m.store.GetDatabaseInstanceByName(ctx, in.Instance)
	if err != nil {
		return err
	}
	old, err := m.decryptCredential(&inst)
	if err != nil {
		return err
	}
	tpl, err := dbtemplate.Get(inst.Template)
	if err != nil {
		return err
	}
	switch tpl.Engine {
	case dbtemplate.EnginePostgres:
		return m.rotatePostgresCredential(ctx, &inst, tpl.Image, old, in.NewPassword)
	case dbtemplate.EngineMySQL:
		return m.rotateMySQLCredential(ctx, &inst, tpl.Image, old, in.NewPassword)
	case dbtemplate.EngineMongo:
		return m.rotateMongoCredential(ctx, &inst, tpl.Image, old, in.NewPassword)
	case dbtemplate.EngineRedis:
		return nil // 无引擎侧动作（凭据 = spec 启动参数，rotate.go 编排承载）
	default:
		return fmt.Errorf("database: template %q has no rotation adapter", inst.Template)
	}
}

// ── job 载荷归一 ─────────────────────────────────────────────────────────────

// toolsJobInput 是工具 job 的内部载荷（adapter 各方法 → runToolsJob 的归
// 一入口）。
type toolsJobInput struct {
	instance string
	purpose  string
	script   []string
	env      []string
	networks []string
	mounts   []JobMount
	timeout  time.Duration
	bindNode string
	// teamSlug/prjSlug 是归属 slug（fleetly.db label 值 = 三段限定形——
	// 跨项目同名实例的清场选择器不互撞，v0.3 流标签口径）。
	teamSlug string
	prjSlug  string
}

// restic 同仓写锁互斥的有界重试（W4-S6 e2e 实测）：库备份与控制面状态备
// 份共享同一 restic repo，而引擎的 post_deploy 钩子会在每次应用部署成功后
// 触发一次状态备份——与手动/计划库备份天然并发，先到者持独占写锁，后到者
// 以「unable to create lock in backend」退败。restic 无内建等锁，job 侧以
// 撞锁文本为条件做有界退避重试（12 × 10s ≈ 2 分钟容忍窗，落在各步预算内
// ——backupJobTimeout 等常量本就为「镜像分发 + 工具执行」留了余量）；非
// 撞锁失败零重试（诚实失败原则：真失败快速红，不烧预算）。
const (
	repoLockRetryAttempts = 12
	repoLockRetryDelay    = 10 * time.Second
)

// isRepoLockConflict 报告一次 job 产出是否为 restic 撞锁退败（重试判据；
// 文本来自 restic exit_error 的稳定措辞）。
func isRepoLockConflict(out JobRunOutcome, err error) bool {
	return strings.Contains(jobFailureText(out), "unable to create lock")
}

// runToolsJob 构造并执行一次工具 job（命名/label/env 公共面在此归一；
// HOME=/tmp 兜底 restic 的缓存目录解析——job 以任意用户身份运行时
// os.UserCacheDir 的 HOME 依赖不作假设；凭据材料在 env 由调用方注入，
// 本函数零展开）。撞 restic 互斥写锁时有界重试（见 repoLockRetryAttempts）。
func (m *Manager) runToolsJob(ctx context.Context, in toolsJobInput) (JobRunOutcome, error) {
	jctx, cancel := context.WithTimeout(ctx, in.timeout)
	defer cancel()
	env := append([]string{"HOME=/tmp"}, in.env...)

	run := func() (JobRunOutcome, error) {
		name, err := naming.DBJobName(in.instance, in.purpose, ulid.Make().String())
		if err != nil {
			return JobRunOutcome{}, err
		}
		// 放置钉定 = 平台节点身份 label 约束（node.labels.fleetly.node-id ==
		// <平台ID n_<ULID>>；placement.ConstraintFor 同公式的就地形态——
		// database 不反依赖 placement。W4-S6 e2e 实测修正：此前误用
		// `node.id == <平台ID>`——swarm node.id 是引擎侧节点 ID，与平台 ID
		// 恒不相等，job 永远 PENDING（"scheduling constraints not satisfied"）。
		// fake 底座不校验约束真实性，单测抓不到——真机闭环兜住的典型）。
		return m.docker.JobRun(jctx, JobRunInput{
			Name:        name,
			Image:       DefaultDatabaseToolsImage,
			Cmd:         in.script,
			Env:         env,
			Networks:    in.networks,
			Mounts:      in.mounts,
			Constraints: []string{"node.labels." + state.LabelNodeID + " == " + in.bindNode},
			Labels: map[string]string{
				state.LabelManaged: state.ManagedLabelValue,
				// 归属锚 = 三段限定形（跨项目同名实例不互撞；v0.3 流标签口径）。
				state.LabelDatabase: qualifiedOf(in.teamSlug, in.prjSlug, in.instance),
			},
		})
	}

	out, err := run()
	for attempt := 1; err == nil && !out.Success() && isRepoLockConflict(out, err) && attempt < repoLockRetryAttempts; attempt++ {
		m.log.Info("database: tools job hit a restic repo lock (concurrent control-plane backup); retrying",
			"instance", in.instance, "purpose", in.purpose, "attempt", attempt)
		select {
		case <-jctx.Done():
			return out, err
		case <-time.After(repoLockRetryDelay):
		}
		out, err = run()
	}
	return out, err
}

// toolsJobEnv 组装工具 job 的 env 集（凭据材料只进 env；KEY 集恒定——
// 空值省略，命令词表与 env 键集一一对应）。RESTIC_REPOSITORY 由编排层解析
// 的 repo 目标携带——W4-S6 e2e 实测修正：此前漏设，restic 以
// 「Please specify repository location」诚实退败（词表与 env 键集声称
// 一一对应，缺这一键 = 备份/校验/恢复永远无 repo 可寻址；fake 底座不看
// env 真实性，单测抓不到）。MYSQL_PWD/MONGO_PASSWORD 是 v0.3 W4 新引擎
// 的凭据消费键（mysqldump 原生读 MYSQL_PWD；mongodump 无原生 env，消费
// 形态 = 命令词表内 ${MONGO_PASSWORD} 的 shell 展开——引用不落字面量）。
func toolsJobEnv(enginePassword, resticRepository, resticPassword, accessKey, secretKey, region string) []string {
	var env []string
	if enginePassword != "" {
		env = append(env,
			"PGPASSWORD="+enginePassword,
			"REDISCLI_AUTH="+enginePassword,
			"MYSQL_PWD="+enginePassword,
			"MONGO_PASSWORD="+enginePassword,
		)
	}
	if resticRepository != "" {
		env = append(env, "RESTIC_REPOSITORY="+resticRepository)
	}
	env = append(env,
		"RESTIC_PASSWORD="+resticPassword,
		"AWS_ACCESS_KEY_ID="+accessKey,
		"AWS_SECRET_ACCESS_KEY="+secretKey,
	)
	if region != "" {
		env = append(env, "AWS_DEFAULT_REGION="+region)
	}
	return env
}

// jobNetworks 组装 job 网络挂接（rustfs 模式追加 fleetly-rustfs-net——托
// 管端点 http://rustfs:9000 只在该网可解析；实例共享网络由编排层以首元素
// 携带——备份/恢复都要以实例别名可达）。
func jobNetworks(attachRustfs bool, instanceNet string) []string {
	out := []string{instanceNet}
	if attachRustfs {
		out = append(out, state.RustfsNetworkName)
	}
	return out
}

// outcomeContext 把备份执行上下文随行到产出（Verify 的影子作业材料——
// 同 repo/节点/网络/材料重放）。
func (m *Manager) outcomeContext(in dbtemplate.BackupInput, snap string, size int64) dbtemplate.BackupOutcome {
	return dbtemplate.BackupOutcome{
		SnapshotID:          snap,
		SizeBytes:           size,
		Instance:            in.Instance,
		TemplateID:          in.TemplateID,
		TeamSlug:            in.TeamSlug,
		PrjSlug:             in.PrjSlug,
		BindNodeID:          in.BindNodeID,
		Repository:          in.Repository,
		ResticPassword:      in.ResticPassword,
		S3AccessKeyID:       in.S3AccessKeyID,
		S3SecretKey:         in.S3SecretKey,
		S3Region:            in.S3Region,
		S3PathStyle:         in.S3PathStyle,
		AttachRustfsNetwork: in.AttachRustfsNetwork,
	}
}

// qualifiedOf 是库实例三段限定形的本地出口（state.DatabaseInstance.
// QualifiedName 同式——adapters 层的 tools job 载荷只有 slug 散字段）。
func qualifiedOf(team, prj, instance string) string {
	return team + "/" + prj + "/" + instance
}

// jobFailureText 归一 job 失败诊断（任务 Err + 退出码 + 尾部输出摘要——
// 命令词表保证无凭据，文本再经编排层 scrub 兜底；单行化截断防失控）。
func jobFailureText(o JobRunOutcome) string {
	parts := []string{}
	if o.Err != "" {
		parts = append(parts, o.Err)
	}
	if o.ExitCode > 0 {
		parts = append(parts, fmt.Sprintf("exit code %d", o.ExitCode))
	}
	if tail := strings.TrimSpace(jobOutputTail(o.Stdout)); tail != "" {
		parts = append(parts, "output tail: "+tail)
	}
	if len(parts) == 0 {
		return "job ended without a task verdict"
	}
	return singleLine(strings.Join(parts, "; "))
}

// jobOutputTail 取输出尾行（最后一条非空行，256 字节——错误摘要在尾部；
// restic --json 的 summary/错误行是最后的结构化行）。
func jobOutputTail(out string) string {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		if len(line) > 256 {
			line = line[:256]
		}
		return line
	}
	return ""
}

// resticSummary 是 restic backup --json 的 summary 消息（restic 0.19
// scripting 契约：message_type=summary、snapshot_id/total_bytes 在快照创
// 建成功时非空）。
type resticSummary struct {
	MessageType string `json:"message_type"`
	SnapshotID  string `json:"snapshot_id"`
	TotalBytes  int64  `json:"total_bytes"`
}

// parseResticSummary 从 restic backup --json 输出提取本次快照 id 与字节量
// （逐行 JSON：取 message_type=summary 的行；无 → 空串）。
func parseResticSummary(output string) (string, int64) {
	var snap string
	var size int64
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var s resticSummary
		if json.Unmarshal([]byte(line), &s) != nil {
			continue
		}
		if s.MessageType == "summary" && s.SnapshotID != "" {
			snap = s.SnapshotID
			size = s.TotalBytes
		}
	}
	return snap, size
}
