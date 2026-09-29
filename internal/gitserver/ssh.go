package gitserver

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"time"

	gossh "golang.org/x/crypto/ssh"
)

// SSH git 面（T2.19）：golang.org/x/crypto/ssh 承载（原间接依赖提升为
// 直接依赖——非新引入第三方框架）。
//
// 认证 = 平台管理的 git 公钥：PublicKeyCallback 按 SHA256 指纹查 git_keys
// 表（指纹 = gossh.FingerprintSHA256，ssh-keygen -lf 同格式）；查无此键
// 一律拒绝。
//
// 协议 = exec 白名单：session 的 exec 请求限定
//
//	git-receive-pack '<app>.git'   （push；bare 仓库懒创建 + 钩子生成）
//	git-upload-pack '<app>.git'    （clone/fetch）
//
// app 名经 ValidAppName 严格校验（路径注入防线）；git 直接 exec（无
// shell），命令形态解析拒绝任何其他词形。
//
// 连接治理（E6，S19）：git 子进程挂连接级 ctx——总预算 10min + 连接以
// 任何原因关闭（客户端中途断开/网络失联）即取消，exec.CommandContext
// 杀进程回收；WaitDelay 兜底 I/O 挂死后的 Wait 阻塞。

// E6（S19）连接治理常量。
const (
	// gitSessionBudget 是单 SSH 连接的总预算（v0.1 常量：10 分钟覆盖最慢
	// 的常规 clone/fetch；超出即取消——挂死传输不得常驻。连接内多 session
	// 共享预算：git ssh 形态一连接一命令，实际不放大）。
	gitSessionBudget = 10 * time.Minute
	// gitWaitDelay 是 cmd.Wait 的兜底上限（Go 1.20+）：ctx 取消杀进程后，
	// 若 stdio 对端（已失联的 channel）仍挂住管道副本，Wait 至多再等这么
	// 久即返回——防 I/O 挂死导致 Wait 永久阻塞。
	gitWaitDelay = 30 * time.Second
)

// gitBin 是 git 可执行名（E6 测试注入缝：假 git 进程驱动「ctx 取消 →
// 限时回收」断言；生产恒 "git"）。
var gitBin = "git"

// ListenAndServe 监听 SSH git 面（阻塞至 ctx 取消或监听错误）；启用态由
// lynx.Service 壳（cmd/fleetlyd）驱动。
func (s *GitTriggers) ListenAndServe(ctx context.Context, addr string) error {
	signer, err := s.ensureHostKey(ctx)
	if err != nil {
		return err
	}
	serverConfig := &gossh.ServerConfig{}
	// 安全评审注记：PublicKeyCallback 返回错误即拒绝该公钥；拒绝文案不
	// 泄漏注册表信息（unknown key 统一措辞）。SystemLog 对失败尝试输出
	// 对端地址——保留默认即可。
	serverConfig.PublicKeyCallback = func(meta gossh.ConnMetadata, key gossh.PublicKey) (*gossh.Permissions, error) {
		fingerprint := gossh.FingerprintSHA256(key)
		keyRow, err := s.st.GetGitKeyByFingerprint(ctx, fingerprint)
		if err != nil {
			s.log.Warn("gitserver: ssh auth rejected", "remote", meta.RemoteAddr().String(),
				"fingerprint", fingerprint)
			return nil, fmt.Errorf("unknown public key")
		}
		perms := &gossh.Permissions{Extensions: map[string]string{"fingerprint": fingerprint}}
		// W2 §2.3 push 署名用户：key 属主非空（用户自服务注册）→ 经
		// Permissions 扩展随连接传递，push 路径注入钩子环境变量入审计
		// actor；存量无主键（user_id NULL）不带该扩展——审计 actor 落
		// system 原口径（迁移口径，如实报告）。
		if keyRow.UserID != "" {
			perms.Extensions["push_user"] = keyRow.UserID
		}
		return perms, nil
	}
	serverConfig.AddHostKey(signer)

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("gitserver: listen %s: %w", addr, err)
	}
	s.log.Info("gitserver: ssh git endpoint listening", "addr", ln.Addr().String())
	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return fmt.Errorf("gitserver: accept: %w", err)
		}
		go s.handleConn(ctx, conn, serverConfig)
	}
}

// handleConn 完成一次 SSH 握手并服务其会话（每连接独立 goroutine）。
func (s *GitTriggers) handleConn(ctx context.Context, conn net.Conn, config *gossh.ServerConfig) {
	defer func() { _ = conn.Close() }()
	sconn, chans, reqs, err := gossh.NewServerConn(conn, config)
	if err != nil {
		return
	}
	defer func() { _ = sconn.Close() }()
	// E6：连接级 ctx——总预算 + 连接关闭即取消；exec 的 git 子进程挂在该
	// ctx 上（CommandContext 杀进程），不再依赖客户端体面退出。
	connCtx, cancel := connContext(ctx, sconn)
	defer cancel()
	go func() {
		for req := range reqs {
			// 全局请求（无 v0.1 语义）一律拒绝。
			_ = req.Reply(false, nil)
		}
	}()
	for newChannel := range chans {
		if newChannel.ChannelType() != "session" {
			_ = newChannel.Reject(gossh.UnknownChannelType, "unsupported channel type")
			continue
		}
		ch, requests, err := newChannel.Accept()
		if err != nil {
			continue
		}
		go s.handleSession(connCtx, sconn, ch, requests)
	}
}

// connContext 构造连接级 ctx（E6）：WithTimeout 包上层 ctx（单连接总
// 预算），并监听 sconn.Wait——连接以任何原因关闭（客户端中途断开/网络
// 失联/对端退出）即 cancel。这是「客户端断开 → git 子进程限时回收」的
// 取消源；Wait 在 sconn.Close（handleConn 的 defer）后必然返回，监听
// goroutine 不泄漏。
func connContext(ctx context.Context, sconn *gossh.ServerConn) (context.Context, context.CancelFunc) {
	connCtx, cancel := context.WithTimeout(ctx, gitSessionBudget)
	go func() {
		_ = sconn.Wait()
		cancel()
	}()
	return connCtx, cancel
}

// handleSession 服务一个 session 通道：只认 exec 请求（env/pty 等一律
// 拒绝——请求面即攻击面）。
func (s *GitTriggers) handleSession(ctx context.Context, sconn *gossh.ServerConn, ch gossh.Channel, requests <-chan *gossh.Request) {
	defer func() { _ = ch.Close() }()
	for req := range requests {
		switch req.Type {
		case "exec":
			var payload struct{ Command string }
			if err := gossh.Unmarshal(req.Payload, &payload); err != nil {
				_ = req.Reply(false, nil)
				return
			}
			_ = req.Reply(true, nil)
			s.execGitCommand(ctx, sconn, ch, payload.Command)
			return
		case "env":
			_ = req.Reply(false, nil)
		default:
			if req.WantReply {
				_ = req.Reply(false, nil)
			}
		}
	}
}

// execGitCommand 解析白名单命令并 exec 系统 git（stdio 直连通道）。exit
// status 随通道回执；未知命令回 127。
func (s *GitTriggers) execGitCommand(ctx context.Context, sconn *gossh.ServerConn, ch gossh.Channel, command string) {
	sub, app, err := parseGitCommand(command)
	if err != nil {
		s.log.Warn("gitserver: rejected git command", "remote", sconn.RemoteAddr().String(),
			"command", truncate(command, 120), "error", err.Error())
		_, _ = fmt.Fprintf(ch.Stderr(), "fleetly: %v\r\n", err)
		sendExitStatus(ch, 127)
		return
	}
	repoPath := s.repoPath(app)
	if sub == "receive-pack" {
		// push 路径：bare 仓库懒创建 + post-receive 钩子在位。
		if _, _, err := s.EnsureBareRepo(ctx, app); err != nil {
			s.log.Warn("gitserver: ensure bare repo failed", "app", app, "error", err.Error())
			_, _ = fmt.Fprintf(ch.Stderr(), "fleetly: repository init failed: %v\r\n", err)
			sendExitStatus(ch, 1)
			return
		}
	}
	gitArgs := []string{sub, repoPath}
	cmd := newGitCommand(ctx, gitArgs...)
	if sub == "receive-pack" {
		// W2 §2.3：push 路径把 SSH 认证回调解析的署名用户注入钩子环境
		//（post-receive 继承 git 子进程环境，回调 payload 随行透传——
		// push 审计 actor 署名）。值经 pushUserEnvValue 白名单化（ULID
		// 词表）——环境变量与 JSON 载荷双面防注入；无主键/克隆路径不注
		// 入（缺省变量，钩子按空处理）。
		if sconn.Permissions != nil {
			if v := pushUserEnvValue(sconn.Permissions.Extensions["push_user"]); v != "" {
				cmd.Env = append(os.Environ(), "FLEETLY_PUSH_USER="+v)
			}
		}
	}
	cmd.Stdin = ch
	cmd.Stdout = ch
	cmd.Stderr = ch.Stderr()
	if err := cmd.Start(); err != nil {
		_, _ = fmt.Fprintf(ch.Stderr(), "fleetly: exec git: %v\r\n", err)
		sendExitStatus(ch, 127)
		return
	}
	waitErr := cmd.Wait()
	code := 0
	if waitErr != nil {
		code = 1
		var exitErr *exec.ExitError
		if errors.As(waitErr, &exitErr) {
			code = exitErr.ExitCode()
			if code < 0 {
				code = 1
			}
		}
	}
	s.log.Info("gitserver: git command finished", "app", app, "command", sub, "exit", code)
	sendExitStatus(ch, code)
}

// newGitCommand 构造受连接级 ctx 治理的 git 子进程（E6）：CommandContext
// （ctx 取消——连接断开/超时预算耗尽——即杀进程）+ WaitDelay（I/O 挂死
// 后 Wait 的兜底回收上限）。
func newGitCommand(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, gitBin, args...) //nolint:gosec // G204：子命令为白名单词形、路径经 app 名严格校验（E6 gitBin 为测试注入缝）
	cmd.WaitDelay = gitWaitDelay
	return cmd
}

// pushUserEnvValue 白名单化 push 署名用户（ULID 词表 [A-Za-z0-9]）：环境
// 变量与钩子 JSON 载荷双面防注入；含任何其他字符的值整体丢弃（fail-
// closed——属主 id 由平台写入通道生成，异形值只可能是异常态）。
func pushUserEnvValue(v string) string {
	if v == "" {
		return ""
	}
	for _, r := range v {
		if (r < '0' || r > '9') && (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') {
			return ""
		}
	}
	return v
}

// parseGitCommand 解析 SSH exec 命令形态：
//
//	git-receive-pack '<app>.git' | git-upload-pack '<app>.git'
//
// 引号可选（引号必须成对闭合）；路径允许一个前导斜杠（ssh:// URL 形态
// git 原样发送 /<app>.git——归一后校验），其余含路径分隔符、..、空白或
// 词形非法的形态一律拒绝。app 名去 .git 后缀后经 ValidAppName 严格校验
// ——白名单即防线（最终仓库路径由服务端以 <root>/<app>.git 构造，客户端
// 传值只决定 app 名）。
func parseGitCommand(command string) (sub, app string, err error) {
	fields := strings.TrimSpace(command)
	sub, rest, found := strings.Cut(fields, " ")
	if !found || rest == "" {
		return "", "", fmt.Errorf("malformed command")
	}
	if sub != "git-receive-pack" && sub != "git-upload-pack" {
		return "", "", fmt.Errorf("command not allowed")
	}
	repo := strings.TrimSpace(rest)
	if len(repo) >= 2 && (repo[0] == '\'' || repo[0] == '"') {
		q := repo[0]
		if repo[len(repo)-1] != q {
			return "", "", fmt.Errorf("unbalanced quotes")
		}
		repo = repo[1 : len(repo)-1]
	}
	repo = strings.TrimPrefix(repo, "/")
	if repo == "" || strings.ContainsAny(repo, "/\\ \t") || strings.Contains(repo, "..") {
		return "", "", fmt.Errorf("malformed repository path")
	}
	app = strings.TrimSuffix(repo, ".git")
	if !ValidAppName(app) {
		return "", "", fmt.Errorf("invalid repository name %q", app)
	}
	return strings.TrimPrefix(sub, "git-"), app, nil
}

// sendExitStatus 回执 exit-status 并关闭写侧（SSH 协议约定）。
func sendExitStatus(ch gossh.Channel, code int) {
	payload := gossh.Marshal(struct{ Status uint32 }{Status: uint32(code)}) //nolint:gosec // G115：exit code 经上文归一到 0..255
	if _, err := ch.SendRequest("exit-status", false, payload); err != nil {
		return
	}
	_ = ch.CloseWrite()
}

// truncate 是日志字段截断（防超长命令刷屏）。
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
