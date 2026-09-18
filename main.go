package main

import (
	"log"
	"net/http"
	"os"
)

// 制裁筛查案件台服务入口。
//
// EVENT_STORE_PATH 指向一个 JSON Lines 事件日志文件（追加写、启动时回放、带哈希链校验）；
// 未设置时使用内存存储，适合本地健康检查。敏感配置只从环境变量读取。
func main() {
	storePath := os.Getenv("EVENT_STORE_PATH")
	srv, err := newServer(storePath)
	if err != nil {
		log.Fatalf("初始化案件台失败: %v", err)
	}
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	log.Printf("制裁筛查案件台监听 :%s（事件存储: %q）", port, storePath)
	if err := http.ListenAndServe(":"+port, srv.routes()); err != nil {
		log.Fatal(err)
	}
}
