package main

// fleetlyd schema-version —— 只读运维子命令（F5/S20：升级回退 schema 感知）。
//
// 背景：迁移只加法 + 回滚 = 恢复快照（架构 §2.8）意味着「新 daemon 已应用
// 迁移后回退旧二进制」时，旧二进制的启动高版本守卫（internal/state
// ensureMigrated）必然拒绝打开库——回退编排（deploy/upgrade.sh ⑧）需要
// 在拉起旧件**之前**拿到「DB 当前 schema 版本 vs 旧二进制支持上限」做比对，
// 给出可行动指引或从 pre_upgrade 快照自动恢复，而不是 start 失败后留下
// DEGRADED 现场让人猜。
//
// 用法：fleetlyd schema-version [-c config.yaml]
//
//	-c/--config 与主进程同语义（缺省回落内置默认配置）；本子命令不启动
//	任何服务、不写库（state.SchemaVersions 只读 goose 版本表）。
//
// 输出（key=value 行，供脚本 sed 定点提取；人读同形）：
//
//	db=9                          DB 当前 schema 版本（库/版本表缺失 = 0）
//	max=9                         本二进制内嵌迁移的最大版本（支持上限）
//	db_path=/var/lib/fleetly/fleetly.db
//	backup_root=/var/lib/fleetly/backups
//	key_path=/var/lib/fleetly/fleetly.key
//	binary_version=v0.1.0

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/spf13/viper"

	"github.com/fleetlyrun/fleetly/internal/runtime"
	"github.com/fleetlyrun/fleetly/internal/state"
)

// schemaVersionConfig 以主进程同款语义（viper + AppConfig）解析配置，取
// db_path/backup 根/密钥路径的派生结果（缺省回落链与 daemon 装配一致——
// DBPath/BackupRoot/KeyPath 单一事实源在 internal/runtime AppConfig）。
func schemaVersionConfig(path string) (*runtime.AppConfig, error) {
	v := viper.New()
	if path != "" {
		v.SetConfigFile(path)
	} else {
		v.AddConfigPath(".")
	}
	if err := v.ReadInConfig(); err != nil {
		// 与 lynx 同口径：搜索路径下无配置文件是可选配置，不报错；显式
		// 指定的 -c 文件缺失/解析错误是硬失败。
		var notFound viper.ConfigFileNotFoundError
		if path != "" || !errors.As(err, &notFound) {
			return nil, fmt.Errorf("read config: %w", err)
		}
	}
	var cfg runtime.AppConfig
	if err := v.Unmarshal(&cfg); err != nil {
		return nil, fmt.Errorf("unmarshal config into AppConfig: %w", err)
	}
	return &cfg, nil
}

// runSchemaVersion 实现 schema-version 子命令，返回进程退出码。
func runSchemaVersion(args []string) int {
	fs := flag.NewFlagSet("schema-version", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "config file (same semantics as the daemon; shorthand -c also accepted)")
	fs.StringVar(cfgPath, "c", "", "shorthand for -config")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintf(os.Stderr, "fleetlyd schema-version: %v\n", err)
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "fleetlyd schema-version: unexpected argument %q\n", fs.Arg(0))
		return 2
	}
	cfg, err := schemaVersionConfig(*cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fleetlyd schema-version: %v\n", err)
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	dbVersion, maxVersion, err := state.SchemaVersions(ctx, cfg.DBPath())
	if err != nil {
		fmt.Fprintf(os.Stderr, "fleetlyd schema-version: %v\n", err)
		return 1
	}
	fmt.Printf("db=%d\n", dbVersion)
	fmt.Printf("max=%d\n", maxVersion)
	fmt.Printf("db_path=%s\n", cfg.DBPath())
	fmt.Printf("backup_root=%s\n", cfg.BackupRoot())
	fmt.Printf("key_path=%s\n", cfg.KeyPath())
	fmt.Printf("binary_version=%s\n", version)
	return 0
}
