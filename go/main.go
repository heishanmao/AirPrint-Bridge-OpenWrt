package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

const version = "1.1.0"

func main() {
	var (
		port       = flag.Int("port", 0, "override listen port (0 = UCI)")
		configPath = flag.String("config", uciPath, "UCI config path")
		logPath    = flag.String("log", "", "log file (default from config, /var/log/airprint/proxy.log)")
		history    = flag.String("history", "", "history file (default from config)")
		noNotify   = flag.Bool("no-notify", false, "disable push notifications")
		noRecover  = flag.Bool("no-recover", false, "disable automatic queue recovery")
		noGuard    = flag.Bool("no-guard", false, "disable the printer-offline submission guard (testing only)")
		debug      = flag.Bool("debug", false, "verbose logging")
		showVer    = flag.Bool("version", false, "print version and exit")
	)
	flag.Parse()
	if *showVer {
		fmt.Println(version)
		return
	}
	settings := loadSettings(*configPath)
	if *port > 0 {
		settings.ListenPort = *port
	}
	if *debug {
		settings.Debug = true
	}
	logFile := *logPath
	if logFile == "" {
		logFile = "/var/log/airprint/proxy.log"
	}
	historyFile := *history
	if historyFile == "" {
		historyFile = settings.HistoryFile
	}
	logger := newLogger(logFile, settings.Debug)
	defer logger.Close()
	logger.Info("airprint-proxy %s starting, queue=%s printer=%s dpi=%d port=%d",
		version, settings.Queue, settings.Printer.URI, settings.DPI, settings.ListenPort)

	bridge := newBridge(settings, logger, historyFile, logFile, *noNotify, *noRecover, *noGuard)
	srv := &http.Server{
		Addr:              fmt.Sprintf(":%d", settings.ListenPort),
		Handler:           &proxyServer{b: bridge},
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
		<-sig
		logger.Info("airprint-proxy stopping")
		bridge.Stop()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}()
	fmt.Printf("airprint-proxy %s listening on :%d pid=%d\n", version, settings.ListenPort, os.Getpid())
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		logger.Warning("server failed: %v", err)
		os.Exit(1)
	}
	logger.Info("airprint-proxy stopped")
}
