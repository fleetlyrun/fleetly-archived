// Package objectstore 是平台对 S3 兼容端点的唯一客户端面（E3 对象存储专项
// 设计 §2.1，D-S3-1）：minio-go/v7 的封闭封装，全仓库只有本包 import
// minio-go；其余代码（备份上传轨/凭证注入/RustFS 管理器）一律经本包消费 S3。
//
// 操作面 = 平台实际用到的封闭集（操作面即 conformance 面，§2.7）：
// EnsureBucket（幂等建桶）/Put/Get/Stat/Delete/List（前缀列举）。
// 不做 presign/multipart/生命周期等大面——需要新操作时先扩本包再扩
// conformance，不在包外直接持有 minio 客户端。
//
// TestConnection 探针（诚实契约 §2.1）：put→get→delete 一枚探针对象并
// 比对写入字节——测试通过 = 「能认证、能写、能读回」，不是 TCP 探活；
// 失败步与底层错误摘要以结构化结果返回（endpoint 回显、secret 不回显）。
package objectstore

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/oklog/ulid/v2"
)

// Endpoint 是 S3 兼容端点描述符（设计 §2.1）。URL 必须含 scheme
// （https://s3.amazonaws.com 或 http://rustfs:9000）；PathStyle=true 用于
// RustFS/MinIO 类自建端点（IP/自定义主机名只能 path-style），AWS 等虚拟
// 主机式端点为 false。SecretKey 明文只在内存存活，持久层走 envelope
// （internal/state，密文落库），本包不落盘、不打日志。
type Endpoint struct {
	URL       string
	Region    string
	Bucket    string
	AccessKey string
	SecretKey string
	PathStyle bool
}

// Validate 校验端点形状（New 与调用方保存/探测前的最小前哨）：URL 必须
// 可解析、scheme ∈ {http, https}、host 非空、bucket 非空。凭证不在此强校验
// ——缺凭证应走真实 403 探针路径如实失败（「能认证」是探测语义的一部分），
// 不在客户端伪造判定。
func (e Endpoint) Validate() error {
	if strings.TrimSpace(e.URL) == "" {
		return fmt.Errorf("objectstore: endpoint url is empty")
	}
	u, err := url.Parse(e.URL)
	if err != nil {
		return fmt.Errorf("objectstore: parse endpoint url: %w", err)
	}
	switch u.Scheme {
	case "http", "https":
	default:
		return fmt.Errorf("objectstore: endpoint url scheme %q not in {http, https}", u.Scheme)
	}
	if u.Host == "" {
		return fmt.Errorf("objectstore: endpoint url %s has no host", e.URL)
	}
	if strings.TrimSpace(e.Bucket) == "" {
		return fmt.Errorf("objectstore: bucket is empty")
	}
	return nil
}

// Client 是单端点的 S3 客户端（minio-go 封装）。零值不可用；经 New 构造。
type Client struct {
	ep  Endpoint
	cli *minio.Client
}

// New 构造端点客户端（形状校验 fail-fast；连接惰性建立）。transport 用
// minio-go 缺省——所有调用携带 ctx，超时/取消由调用方预算决定。
func New(_ context.Context, ep Endpoint) (*Client, error) {
	if err := ep.Validate(); err != nil {
		return nil, err
	}
	u, err := url.Parse(ep.URL)
	if err != nil {
		return nil, fmt.Errorf("objectstore: parse endpoint url: %w", err)
	}
	lookup := minio.BucketLookupDNS
	if ep.PathStyle {
		lookup = minio.BucketLookupPath
	}
	cli, err := minio.New(u.Host, &minio.Options{
		Creds:        credentials.NewStaticV4(ep.AccessKey, ep.SecretKey, ""),
		Secure:       u.Scheme == "https",
		Region:       ep.Region,
		BucketLookup: lookup,
	})
	if err != nil {
		return nil, fmt.Errorf("objectstore: construct s3 client: %w", err)
	}
	return &Client{ep: ep, cli: cli}, nil
}

// EnsureBucket 幂等建桶（设计 §2.1：启用面自动建桶，存在即 no-op）。
// region 语义：虚拟主机式端点（AWS）透传 Region 生成 LocationConstraint；
// path-style 自建端点不带（自建 S3 对 region 约束支持参差，空 region 恒
// 可接受）。
func (c *Client) EnsureBucket(ctx context.Context) error {
	exists, err := c.cli.BucketExists(ctx, c.ep.Bucket)
	if err != nil {
		return fmt.Errorf("objectstore: probe bucket %s: %w", c.ep.Bucket, err)
	}
	if exists {
		return nil
	}
	region := ""
	if !c.ep.PathStyle {
		region = c.ep.Region
	}
	if err := c.cli.MakeBucket(ctx, c.ep.Bucket, minio.MakeBucketOptions{Region: region}); err != nil {
		return fmt.Errorf("objectstore: create bucket %s: %w", c.ep.Bucket, err)
	}
	return nil
}

// Put 写入对象（data 全量字节；contentType 缺省回落 application/octet-
// stream）。小对象单 PUT——测试/探针对象保持 KB 级（multipart 面不在
// 操作面内）。
func (c *Client) Put(ctx context.Context, key string, data []byte, contentType string) error {
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	_, err := c.cli.PutObject(ctx, c.ep.Bucket, key, strings.NewReader(string(data)), int64(len(data)),
		minio.PutObjectOptions{ContentType: contentType})
	if err != nil {
		return fmt.Errorf("objectstore: put %s: %w", key, err)
	}
	return nil
}

// ObjectInfo 是对象元数据只读投影（不外漏 minio 类型——D-S3-1 封闭面）。
type ObjectInfo struct {
	Key          string
	Size         int64
	ETag         string
	LastModified time.Time
}

// Get 读回对象全量字节。
func (c *Client) Get(ctx context.Context, key string) ([]byte, error) {
	obj, err := c.cli.GetObject(ctx, c.ep.Bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, fmt.Errorf("objectstore: get %s: %w", key, err)
	}
	defer func() { _ = obj.Close() }()
	data, err := io.ReadAll(obj)
	if err != nil {
		return nil, fmt.Errorf("objectstore: read %s: %w", key, err)
	}
	return data, nil
}

// Stat 取对象元数据。
func (c *Client) Stat(ctx context.Context, key string) (ObjectInfo, error) {
	info, err := c.cli.StatObject(ctx, c.ep.Bucket, key, minio.StatObjectOptions{})
	if err != nil {
		return ObjectInfo{}, fmt.Errorf("objectstore: stat %s: %w", key, err)
	}
	return ObjectInfo{Key: info.Key, Size: info.Size, ETag: info.ETag, LastModified: info.LastModified}, nil
}

// Delete 删除对象。
func (c *Client) Delete(ctx context.Context, key string) error {
	if err := c.cli.RemoveObject(ctx, c.ep.Bucket, key, minio.RemoveObjectOptions{}); err != nil {
		return fmt.Errorf("objectstore: delete %s: %w", key, err)
	}
	return nil
}

// List 前缀列举（递归；返回按 minio-go 列举序）。列举通道内的错误以
// error 显式上抛，不静默截断（诚实契约：部分列举结果不可冒充全量）。
func (c *Client) List(ctx context.Context, prefix string) ([]ObjectInfo, error) {
	var out []ObjectInfo
	for info := range c.cli.ListObjects(ctx, c.ep.Bucket, minio.ListObjectsOptions{Prefix: prefix, Recursive: true}) {
		if info.Err != nil {
			return nil, fmt.Errorf("objectstore: list prefix %s: %w", prefix, info.Err)
		}
		out = append(out, ObjectInfo{Key: info.Key, Size: info.Size, ETag: info.ETag, LastModified: info.LastModified})
	}
	return out, nil
}

// ProbeStepName 是探针步骤词表（诚实契约：失败步可定位到 put/get/delete）。
const (
	ProbeStepPut    = "put"
	ProbeStepGet    = "get"
	ProbeStepDelete = "delete"
)

// ProbeStep 是单步探测结果（耗时 + 底层错误摘要）。
type ProbeStep struct {
	Step     string
	OK       bool
	Duration time.Duration
	// Err 是失败步的底层错误摘要（错误链文本；不含 secret——minio-go
	// 错误不回显凭证，本包不拼装请求材料进错误）。
	Err string
}

// ProbeResult 是探针结构化结果：endpoint 回显脱敏（secret 不回显）、各步
// 耗时、失败步。OK=false 时 FailedStep 指向首个失败步。
type ProbeResult struct {
	EndpointURL string
	Region      string
	Bucket      string
	PathStyle   bool
	OK          bool
	FailedStep  string
	Steps       []ProbeStep
}

// probeObjectPrefix 是探针对象键前缀（设计 §2.1：fleetly-probe-<ULID>）。
const probeObjectPrefix = "fleetly-probe-"

// TestConnection 执行探针（设计 §2.1 诚实契约）：put→get→delete 一枚
// 探针对象（fleetly-probe-<ULID>，内容含 ULID 字节），get 回读逐字节比对
// ——「能认证/能写/能读回」三段全部走真实 S3 往返。任何一步失败即短路
// 收口，结果携带失败步与底层错误摘要；本函数不返回 error（失败是结果，
// 不是传输异常——调用方按 OK 字段裁决）。
func TestConnection(ctx context.Context, ep Endpoint) ProbeResult {
	res := ProbeResult{EndpointURL: ep.URL, Region: ep.Region, Bucket: ep.Bucket, PathStyle: ep.PathStyle}
	id := ulid.Make()
	key := probeObjectPrefix + id.String()
	// 探针内容 = ULID 本体（回读比对对象；KB 级小对象单 PUT）。
	payload := []byte(id.String())

	run := func(step string, fn func() error) bool {
		start := time.Now()
		err := fn()
		ps := ProbeStep{Step: step, OK: err == nil, Duration: time.Since(start)}
		if err != nil {
			ps.Err = err.Error()
			res.FailedStep = step
		}
		res.Steps = append(res.Steps, ps)
		return err == nil
	}

	cli, err := New(ctx, ep)
	if err != nil {
		// 端点形状非法（URL 无 scheme 等）：探测未起步即失败，失败步记
		// put（首个真实往返步）——形状问题在错误摘要里如实可见。
		res.FailedStep = ProbeStepPut
		res.Steps = append(res.Steps, ProbeStep{Step: ProbeStepPut, OK: false, Err: err.Error()})
		return res
	}
	if !run(ProbeStepPut, func() error { return cli.Put(ctx, key, payload, "text/plain") }) {
		return res
	}
	if !run(ProbeStepGet, func() error {
		got, err := cli.Get(ctx, key)
		if err != nil {
			return err
		}
		if string(got) != string(payload) {
			return fmt.Errorf("probe object readback mismatch: wrote %d bytes, read %d bytes", len(payload), len(got))
		}
		return nil
	}) {
		// 读回失败也尝试清场（探针对象可能已写入）——清场失败只追加记录，
		// 不改变失败结论。
		_ = cli.Delete(ctx, key)
		return res
	}
	if !run(ProbeStepDelete, func() error { return cli.Delete(ctx, key) }) {
		return res
	}
	res.OK = true
	return res
}
