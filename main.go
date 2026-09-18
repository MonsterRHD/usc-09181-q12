// 制裁筛查案件台 —— 服务入口。
//
// 启动时从事件日志（默认 data/events.jsonl）重放恢复全部案件、名单版本与提醒状态；
// 事件仅追加且带哈希链，可通过 GET /v1/audit/replay 独立复核。
package main

import (
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"example.com/09181/q012/internal/httpapi"
	"example.com/09181/q012/internal/service"
	"example.com/09181/q012/internal/store"
)

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	logPath := os.Getenv("EVENT_LOG")
	if logPath == "" {
		logPath = "data/events.jsonl"
	}
	if dir := filepath.Dir(logPath); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			log.Fatalf("create event log directory %s: %v", dir, err)
		}
	}

	es, err := store.NewEventStore(logPath)
	if err != nil {
		log.Fatalf("open event log %s: %v", logPath, err)
	}

	cfg := service.Config{
		SLA:         durationEnv("SLA_DURATION", 24*time.Hour),
		WarningLead: durationEnv("SLA_WARNING_LEAD", 4*time.Hour),
	}
	svc, err := service.New(es, cfg)
	if err != nil {
		log.Fatalf("bootstrap service: %v", err)
	}

	srv := httpapi.NewServer(svc)
	log.Printf("sanctions screening case desk listening on :%s (event log: %s)", port, logPath)
	if err := http.ListenAndServe(":"+port, srv); err != nil {
		log.Fatal(err)
	}
}

func durationEnv(key string, fallback time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
		log.Printf("invalid %s=%q, using %s", key, v, fallback)
	}
	return fallback
}
