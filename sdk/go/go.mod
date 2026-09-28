module github.com/fleetlyrun/fleetly/sdk/go

go 1.26.6

// 对 genproto 的消费形态（IMPL-ARCH-K）：require 挂真实版本号
// genproto/v0.1.0——外部消费者（torchwood 等）import 本模块时只读
// require、忽略 replace，零伪版本在 proxy 上不存在（T2-3 被迫 vendored
// fork 的机械根因）。版本在 tag push（git tag genproto/v0.1.0）后对外可
// 解析；仓内构建全走 go.work workspace（版本被 use 覆盖，不联网解析），
// GOWORK=off 场景（license 扫描等）由下方 replace 兜底——replace 仅在本
// 模块为主模块时生效，对外无害，故保留。
replace github.com/fleetlyrun/fleetly/genproto => ../../genproto

require (
	github.com/fleetlyrun/fleetly/genproto v0.1.0
	google.golang.org/grpc v1.83.2
)

require (
	buf.build/gen/go/bufbuild/protovalidate/protocolbuffers/go v1.36.12-20260825204119-511051f7f437.2 // indirect
	github.com/grpc-ecosystem/grpc-gateway/v2 v2.29.0 // indirect
	golang.org/x/net v0.58.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
	golang.org/x/text v0.41.0 // indirect
	google.golang.org/genproto/googleapis/api v0.0.0-20260803160001-6ac0973c030d // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260729162451-8efbd57d26e0 // indirect
	google.golang.org/protobuf v1.36.12 // indirect
)
