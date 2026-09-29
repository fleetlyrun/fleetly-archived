package dbtemplate

import "github.com/fleetlyrun/fleetly/internal/engine"

// EngineTemplatePort 实现 engine.DatabaseTemplatePort（fleetly.databases
// 引用面的物化键值与前缀的引擎消费口）。
//
// 2026-09-29 架构评审 C6 归位：原 runtime/provides.go 的装配层内联薄委托
// 搬入属主包——前缀与连接串键值的唯一单源本就在本包，适配随定义走；
// dbtemplate → engine 方向与 render.go 的 ServiceSpec 消费一致，engine
// 不 import dbtemplate。空结构体，方法委托包内单源。
type EngineTemplatePort struct{}

// EnvPrefix 实现 engine.DatabaseTemplatePort（前缀唯一单源）。
func (EngineTemplatePort) EnvPrefix(instance string) string { return EnvPrefix(instance) }

// ConnectionVars 实现 engine.DatabaseTemplatePort（连接串键值唯一单源）。
func (EngineTemplatePort) ConnectionVars(templateID, instance, password string) (map[string]string, error) {
	return ConnectionVars(templateID, instance, password)
}

// 编译期断言：满足引擎端口。
var _ engine.DatabaseTemplatePort = EngineTemplatePort{}
