package engine

// 控制面地址注入（2026-09-29，集群内工作负载访问控制面的平台能力）：每个
// 任务 spec 恒注入两条 system env——
//
//	FLEETLY_CONTROL_GRPC_ADDR = <swarm advertise>:<gRPC 端口>
//	FLEETLY_CONTROL_TLS_NAME  = 平台 TLS 模式的证书校验名（TLS off 为空）
//
// 动机（T 线 torchwood 拆桥）：dispatcher 等「集群内程序化工作负载」需要
// 回拨控制面 gRPC；控制面是 host 网络容器（D-W5-4 回环消费面约束），Docker
// 禁止其加入 overlay——swarm DNS 无法给它服务名，swarm 也无 VPC DNS 可依。
// 平台因此把**自己已知的地址**在装配任务时物化进 env（exec relay 的
// FLEETLY_CONTROL_ADDR 同源先例）：值 = advertise（VPC 内网地址，流量不出
// 集群）+ 引擎自身 gRPC 端口，随环境自动正确，定义面（compose/平台 env）
// 零地址耦合。
//
// 消费契约（torchwood dispatcher 参考实现）：functions.fleetly.endpoint 显式
// 配置优先；为空时回落 FLEETLY_CONTROL_GRPC_ADDR，FLEETLY_CONTROL_TLS_NAME
// 非空即 TLS + ServerName 校验（system 根），为空 = TLS off 明文。
//
// 与 S3/DB 注入同族（s3inject/dbinject）但无条件：这不是用户声明的能力
// （无 label），是平台身份信息——键集恒定 = 快照与 desired-hash 确定；值非
// 敏感（内网地址+证书名）。键在 FLEETLY_ 保留名字空间（E_ENV_KEY_RESERVED
// 封死用户写），合并序 system 最高，用户不可覆盖。

import (
	"github.com/fleetlyrun/fleetly/internal/compose"
	"github.com/fleetlyrun/fleetly/internal/envlayer"
)

// controlPlaneSystemVars 是注入键的固定词表（两键恒注入——空值如实注入，
// 消费方按值裁决；s3SystemVars 同款纪律）。
func controlPlaneSystemVars(grpcAddr, tlsName string) []envlayer.PlatformVar {
	return []envlayer.PlatformVar{
		{Key: "FLEETLY_CONTROL_GRPC_ADDR", Value: grpcAddr, Source: string(envlayer.SourceSystem)},
		{Key: "FLEETLY_CONTROL_TLS_NAME", Value: tlsName, Source: string(envlayer.SourceSystem)},
	}
}

// resolveControlPlaneInjection 产出本次发布的控制面注入面：**每个服务**同
// 一组 system env（无条件注入）。ControlGRPCAddr 未装配（空）时返回 nil
// ——装配层未接线（旧装配/单测零 Config）则零行为变更，不注入半成品。
func (e *Engine) resolveControlPlaneInjection(spec *compose.Spec) map[string][]envlayer.PlatformVar {
	if e.cfg.ControlGRPCAddr == "" {
		return nil
	}
	vars := controlPlaneSystemVars(e.cfg.ControlGRPCAddr, e.cfg.ControlTLSName)
	out := make(map[string][]envlayer.PlatformVar, len(spec.Services))
	for i := range spec.Services {
		out[spec.Services[i].Name] = vars
	}
	return out
}
