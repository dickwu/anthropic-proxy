package main

import (
	"context"
	"encoding/json"
	"flag"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:18083", "loopback listen address")
	upstream := flag.String("upstream", "https://api.anthropic.com", "fixed upstream")
	config := flag.String("config", "", "body policy JSON file")
	logPath := flag.String("log", "", "metadata-only JSON log")
	inspectPath := flag.String("inspect-socket", "", "private Unix socket for latest in-memory request")
	debugDir := flag.String("debug-dir", "", "private directory for redacted full request debug JSON")
	flag.Parse()
	host, _, err := net.SplitHostPort(*listen)
	if err != nil || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() {
		panic("listen address must be loopback")
	}
	policy := BodyPolicy{}
	if *config != "" {
		data, err := os.ReadFile(*config)
		if err != nil {
			panic(err)
		}
		if err := json.Unmarshal(data, &policy); err != nil {
			panic(err)
		}
	}
	writer := os.Stdout
	if *logPath != "" {
		file, err := os.OpenFile(*logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			panic(err)
		}
		defer file.Close()
		writer = file
	}
	logger := slog.New(slog.NewJSONHandler(writer, nil))
	if *debugDir != "" {
		if err := ensurePrivateDirectory(*debugDir); err != nil {
			panic(err)
		}
	}
	snapshots := &snapshotStore{}
	if *inspectPath != "" {
		listener, err := startInspector(*inspectPath, snapshots)
		if err != nil {
			panic(err)
		}
		defer listener.Close()
		defer os.Remove(*inspectPath)
	}
	handler, err := newProxy(*upstream, policy, logger, snapshots, os.Getenv("ANTHROPIC_PROBE_EXPECTED_KEY_SHA256"), *debugDir)
	if err != nil {
		panic(err)
	}
	server := &http.Server{Addr: *listen, Handler: handler, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 90 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		server.Shutdown(shutdown)
	}()
	logger.Info("proxy_started", "listen", *listen, "body_policy", policy)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		logger.Error("server_stopped", "error_type", "listen_error")
		os.Exit(1)
	}
}
