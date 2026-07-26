package main

import (
	"codeswitch/services"
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	handled, err := runMaintenanceCommand(os.Args[1:], services.MigrateRequestLogIndexes)
	if err != nil {
		log.Fatalf("维护命令失败: %v", err)
	}
	if handled {
		log.Printf("request_log 索引和北京时间当天 30 分钟统计汇总迁移完成")
		return
	}

	runtime, err := newAppRuntime()
	if err != nil {
		if errors.Is(err, services.ErrRequestLogIndexMigrationRequired) {
			// systemd 使用 Restart=on-failure。正常退出可以避免漏做维护迁移时
			// 每几秒重启并重复触发数据库初始化。
			log.Printf("启动已停止，需要先执行数据库维护迁移: %v", err)
			return
		}
		log.Fatalf("启动服务失败: %v", err)
	}
	defer runtime.shutdown()

	server := newAdminServer(runtime)
	log.Printf("web admin listening on http://%s", runtime.adminAddr)
	if runtime.providerRelay != nil {
		log.Printf("provider relay listening on http://%s", runtime.providerRelay.Addr())
	}

	serverErr := make(chan error, 1)
	go func() {
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
		close(serverErr)
	}()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	select {
	case <-ctx.Done():
	case err := <-serverErr:
		if err != nil {
			log.Fatalf("web admin server failed: %v", err)
		}
		return
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Printf("web admin shutdown failed: %v", err)
	}
}

func runMaintenanceCommand(args []string, migrateRequestLogIndexes func() error) (bool, error) {
	if len(args) == 0 {
		return false, nil
	}
	if len(args) != 1 || args[0] != services.RequestLogIndexMigrationCommand {
		return true, fmt.Errorf(
			"unknown command %q; supported command: %s",
			args[0],
			services.RequestLogIndexMigrationCommand,
		)
	}
	return true, migrateRequestLogIndexes()
}
