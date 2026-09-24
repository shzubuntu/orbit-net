// orbitd 服务端: 账户/设备控制面 + 管理 API + 数据面中继(M1)。
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"gopkg.in/yaml.v3"

	"orbit/internal/rollinglog"
	"orbit/internal/server"
	"orbit/internal/version"
)

func main() {
	// 子命令分发: orbitd genconfig ...
	if len(os.Args) > 1 && os.Args[1] == "genconfig" {
		if err := runGenconfig(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "genconfig:", err)
			os.Exit(1)
		}
		return
	}

	var cfgPath string
	var showVer bool
	flag.StringVar(&cfgPath, "config", "", "path to orbitd.yaml (required)")
	flag.BoolVar(&showVer, "version", false, "print version and exit")
	flag.Parse()

	if showVer {
		fmt.Println(version.Version)
		return
	}
	if cfgPath == "" {
		log.Fatal("usage: orbitd -config orbitd.yaml")
	}

	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		log.Fatalf("read config: %v", err)
	}
	var cfg server.Config
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		log.Fatalf("parse config: %v", err)
	}
	if cfg.Admin.Addr == "" {
		cfg.Admin.Addr = "127.0.0.1:18444" // 管理 API 仅本机/内网可达
	}

	if cfg.LogFile != "" {
		lm, err := rollinglog.Setup(cfg.LogFile, cfg.LogMaxBytes, cfg.LogKeep)
		if err != nil {
			log.Fatalf("log setup: %v", err)
		}
		log.SetOutput(lm)
	}

	s, err := server.New(cfg)
	if err != nil {
		log.Fatalf("init: %v", err)
	}
	log.Printf("orbitd %s starting (admin=%s data=%s public=%s subnet=%s)",
		version.Version, cfg.Admin.Addr, cfg.ListenAddr, cfg.Public.Addr, cfg.NetworkCIDR())

	// 管理 API(goroutine)、公共 HTTPS(可选)与数据面(前台)并行
	go func() {
		if err := s.ListenAndServe(); err != nil {
			log.Fatalf("admin api: %v", err)
		}
	}()
	if cfg.Public.Addr != "" {
		go func() {
			if err := s.ListenAndServePublic(); err != nil {
				log.Fatalf("public api: %v", err)
			}
		}()
	}

	done := make(chan error, 1)
	go func() { done <- s.Serve() }()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	select {
	case sig := <-sig:
		log.Printf("signal %v, shutting down", sig)
		_ = s.Close()
		<-done
	case err := <-done:
		if err != nil {
			log.Fatalf("data plane: %v", err)
		}
	}
	log.Printf("orbitd stopped")
}
