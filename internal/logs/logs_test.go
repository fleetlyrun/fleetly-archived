package logs

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/fleetlyrun/fleetly/internal/engine"
	"github.com/fleetlyrun/fleetly/internal/secrets"
	"github.com/fleetlyrun/fleetly/internal/state"
	"github.com/fleetlyrun/fleetly/internal/substrate"
)

// fakePort 是测试用底座端口：可编程的流产物。
type fakePort struct {
	mu       sync.Mutex
	services map[string][]string // app -> services
	lines    map[string][]substrate.LogLine
	// stuck 标记的流：StreamServiceLogs 返回永不发送也永不关闭的 channel
	//（MG-1 看门狗测试：模拟底座流挂死——同款缺陷的注入形态）。
	stuck map[string]bool
	// jobStates 是 app -> 一次性 job 服务实况投影（E5 Cron / DT-4 采集路径
	// 测试的发现面注入）。
	jobStates map[string][]engine.ServiceState
	// jobErr 非 nil 时 JobServiceStates 返回该错误（底座暂态注入）。
	jobErr error
	// taskStates 是任务服务实况投影（DT-5 任务日志面测试的发现面注入）。
	taskStates []engine.ServiceState
	// taskErr 非 nil 时 TaskServiceStates 返回该错误（底座暂态注入）。
	taskErr error
}

func newFakePort() *fakePort {
	return &fakePort{
		services:  map[string][]string{},
		lines:     map[string][]substrate.LogLine{},
		stuck:     map[string]bool{},
		jobStates: map[string][]engine.ServiceState{},
	}
}

func (f *fakePort) setJobServices(app string, states ...engine.ServiceState) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.jobStates[app] = states
}

func (f *fakePort) failJobDiscovery(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.jobErr = err
}

func (f *fakePort) JobServiceStates(_ context.Context, app string) ([]engine.ServiceState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.jobStates[app], f.jobErr
}

// TaskServiceStates 实现 logs.Port 的任务日志发现面（DT-5）。
func (f *fakePort) TaskServiceStates(_ context.Context) ([]engine.ServiceState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.taskStates, f.taskErr
}

func (f *fakePort) setApp(app string, services ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.services[app] = services
}

func (f *fakePort) emit(service string, lines ...substrate.LogLine) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lines[service] = append(f.lines[service], lines...)
}

func (f *fakePort) ManagedServiceProcesses(_ context.Context, app string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.services[app], nil
}

func (f *fakePort) StreamServiceLogs(_ context.Context, service string, since time.Time, _ bool) (<-chan substrate.LogLine, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.stuck[service] {
		// 挂死流：无生产者、永不关闭——消费者只能靠看门狗脱身。
		return make(chan substrate.LogLine), nil
	}
	out := make(chan substrate.LogLine)
	go func() {
		defer close(out)
		for _, l := range f.lines[service] {
			// 轮询游标语义：只投递 since 之后的行。零 At（时间戳不可解析
			// 形态）不参与过滤：真实 docker 侧按自身时间戳过滤，我侧
			// splitTimestamp 失败只是本地视图（fake 无法建模 docker 侧
			// 时间），按必达透传。
			if !since.IsZero() && !l.At.IsZero() && !l.At.After(since) {
				continue
			}
			out <- l
		}
	}()
	return out, nil
}

// newTestManager 构造接 fake 端口的 Manager（加速轮询、独立落盘目录、
// 真实 secrets.Box——脱敏链路端到端）。
func newTestManager(t *testing.T) (*Manager, *fakePort, *state.Store, *secrets.Box) {
	t.Helper()
	st := openStateStub(t)
	port := newFakePort()
	dir := t.TempDir()
	cfg := Config{Dir: filepath.Join(dir, "logs"), RetentionDays: 7, ScanIntervalMillis: 10, RingSize: 4}
	box, _, err := secrets.EnsureKey(filepath.Join(dir, "test.key"))
	if err != nil {
		t.Fatalf("EnsureKey: %v", err)
	}
	mg := NewManager(cfg, st, port, box, discardLogger())
	return mg, port, st, box
}

// openStateStub 打开测试用状态库（temp 目录独立，os.Args/CWD 不触碰）。
func openStateStub(t *testing.T) *state.Store {
	t.Helper()
	st, err := state.Open(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}
