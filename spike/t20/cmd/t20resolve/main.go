// Command t20resolve 是 IMPL-T2-0 spike ② 的解析探针：用生产代码
// internal/imageregistry 的解析客户端把 tag 引用解析为 manifest digest，
// 可选携带 Basic 凭证（自建 registry），并可按平台口径打印 X-Registry-Auth
// 编码（用于对照 swarm raft 存储面取证）。
//
// 用法（仓库根目录）：
//
//	go run ./spike/t20/cmd/t20resolve alpine:3.19
//	go run ./spike/t20/cmd/t20resolve -user u -pass p 10.240.0.50:5000/t20/private:1
//	go run ./spike/t20/cmd/t20resolve -user u -pass p -encode-auth-for 10.240.0.50:5000
package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/moby/moby/api/types/registry"

	"github.com/fleetlyrun/fleetly/internal/imageregistry"
)

func main() {
	username := flag.String("user", "", "registry username (optional)")
	password := flag.String("pass", "", "registry password (optional)")
	encodeHost := flag.String("encode-auth-for", "", "print the platform X-Registry-Auth blob for this host and exit")
	timeout := flag.Duration("timeout", 60*time.Second, "per-reference resolve budget")
	flag.Parse()

	if *encodeHost != "" {
		raw, err := json.Marshal(registry.AuthConfig{
			Username:      *username,
			Password:      *password,
			ServerAddress: *encodeHost,
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "encode auth: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("AUTH_BLOB %s\n", base64.URLEncoding.EncodeToString(raw))
		return
	}

	client := imageregistry.NewClient()
	var credentials *imageregistry.Credentials
	if *username != "" {
		credentials = &imageregistry.Credentials{Username: *username, Password: *password}
	}
	failed := false
	for _, raw := range flag.Args() {
		reference, err := imageregistry.Parse(raw)
		if err != nil {
			fmt.Printf("PARSE_FAIL %s: %v\n", raw, err)
			failed = true
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), *timeout)
		digest, err := client.Resolve(ctx, reference, credentials)
		cancel()
		if err != nil {
			fmt.Printf("RESOLVE_FAIL %s: %v\n", raw, err)
			failed = true
			continue
		}
		fmt.Printf("RESOLVED %s -> %s\n", raw, digest)
	}
	if failed {
		os.Exit(1)
	}
}
