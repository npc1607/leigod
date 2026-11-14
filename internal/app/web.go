package app

import (
	"context"
	"fmt"

	"leigod/internal/assets"
	"leigod/internal/monitor"
	"leigod/internal/webui"
)

// RunWeb 启动只读状态面板（默认 http://127.0.0.1:7788）。
// 普通用户即可运行；以 root 运行可获得更完整的命名空间内信息。
func (a *App) RunWeb(ctx context.Context) error {
	paths := assets.ResolvePaths(a.Cfg.InstallDir)

	mon := monitor.New(monitor.Config{
		LogPath:     paths.LogDir + "/acc_PC.log",
		Namespace:   a.Cfg.Namespace,
		ServiceName: ServiceName,
		RouteTable:  a.Cfg.RouteTable,
		InstallDir:  a.Cfg.InstallDir,
		StateDir:    a.Cfg.StateDir,
		WebPort:     a.Cfg.WebPort,
		WebListen:   a.Cfg.WebListen,
	})

	srv := &webui.Server{
		Mon:    mon,
		Listen: a.Cfg.WebListen,
		Logf:   a.logf,
	}

	fmt.Fprintf(a.Out, "状态面板: http://%s  （Ctrl+C 退出）\n", a.Cfg.WebListen)
	return srv.Start(ctx)
}
