module github.com/linguo2625469/workbuddy2api-panel

// 出货二进制用 Go 1.26.10（Dockerfile/CI），stdlib 漏洞数 0（1.22.5 时 govulncheck 报 69 条）。
// 此处写 1.26.0 而非 1.26.10：本地开发机无外网，装不了更新的补丁版，1.26.0 足够表达语言版本要求。
go 1.26.0

require github.com/redis/go-redis/v9 v9.18.0

require (
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/dgryski/go-rendezvous v0.0.0-20200823014737-9f7001d12a5f // indirect
	go.uber.org/atomic v1.11.0 // indirect
)
