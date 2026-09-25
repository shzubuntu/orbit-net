// orbit-cli 命令行客户端: 设备握手 → TUN 桥接 + 智能/全局模式出口。
// 断线自动指数退避重连(2s→60s 封顶+抖动)。
package main

import (
	"crypto/tls"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"

	"orbit/internal/client"
	"orbit/internal/protocol"
	"orbit/internal/version"
)

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "register":
			os.Exit(runRegister(os.Args[2:]))
		case "devices":
			os.Exit(runDevices(os.Args[2:]))
		case "status":
			os.Exit(runStatus(os.Args[2:]))
		case "set":
			os.Exit(runSet(os.Args[2:]))
		case "autostart":
			os.Exit(runAutostart(os.Args[2:]))
		case "usage":
			os.Exit(runUsage(os.Args[2:]))
		}
	}
	var cfgPath string
	var showVer bool
	flag.StringVar(&cfgPath, "config", "", "path to orbit-cli.yaml (required)")
	flag.BoolVar(&showVer, "version", false, "print version and exit")
	flag.Parse()

	if showVer {
		fmt.Println(version.Version)
		return
	}
	if cfgPath == "" {
		log.Fatal("usage: orbit-cli -config orbit-cli.yaml | orbit-cli register -invite <code>")
	}

	cfg, err := client.Load(cfgPath)
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	if cfg.Hostname == "" {
		host, _ := os.Hostname()
		cfg.Hostname = host
	}
	lm, err := client.SetupLog(cfg)
	if err != nil {
		log.Fatalf("log setup: %v", err)
	}
	if lm != nil {
		log.SetOutput(lm)
	}

	c, err := client.New(cfg)
	if err != nil {
		log.Fatalf("init: %v", err)
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	stop := make(chan struct{})
	go func() {
		<-sig
		log.Printf("signal, exiting")
		_ = c.Close()
		close(stop)
	}()

	backoff := 2 * time.Second
	const max = 60 * time.Second
	for {
		// Run 返回 nil = 断线走完清理, 同样需要重连; 信号处理里 Close 后走 stop 通道退出。
		// safeRun 兜底: 任何 Run panic(如 wintun 会话竞态)记入滚动日志后继续退避重连,
		// 不让客户端"静默死亡"留下一个没有保活进程的空壳。
		err := safeRun(c)
		if err != nil {
			log.Printf("orbit-cli: %v (reconnect in %s)", err, backoff)
		}
		// 信号已到: Run 已走完 cleanup(closeCh 驱动), 直接退出。
		// 绝不能在这之前 os.Exit——cleanup 没完成会导致 tun fd 泄漏,
		// 接口滞留在内核, 后续重连 TUNSETIFF 永远 EBUSY(实机踩过)。
		select {
		case <-stop:
			return
		default:
		}
		time.Sleep(backoff + time.Duration(rand.Intn(500))*time.Millisecond)
		if backoff < max {
			backoff *= 2
			if backoff > max {
				backoff = max
			}
		}
	}
}

// safeRun 捕获 Run 的 panic 转成 error(栈写入滚动日志)。
func safeRun(c *client.Client) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("Panic in Run: %v\n%s", r, debug.Stack())
		}
	}()
	return c.Run()
}

// ---- 自助开户: orbit-cli register -invite <code> [-account x] [-device x] [-out yaml] ----

type regResp struct {
	OK          bool   `json:"ok"`
	AccountID   string `json:"account_id"`
	AccountName string `json:"account_name"`
	DeviceID    string `json:"device_id"`
	DeviceToken string `json:"device_token"`
	Tier        string `json:"tier"`
	Error       string `json:"error"`
}

func runRegister(args []string) int {
	fs := flag.NewFlagSet("register", flag.ExitOnError)
	invite := fs.String("invite", "", "邀请码(必填)")
	acct := fs.String("account", "", "账户名(默认 mm-<系统用户名>)")
	dev := fs.String("device", "", "设备名(默认主机名)")
	out := fs.String("out", "orbit-cli.yaml", "输出配置文件路径")
	server := fs.String("server", "47.101.198.197:28443", "数据面地址 host:port(写入 server_addr)")
	web := fs.String("web", "", "公共 HTTPS 入口(如 vpn.example.com:4431; 填了会自动下载该服务器 ca.pem 并以之注册)")
	endpoint := fs.String("endpoint", "", "注册接口地址(空则默认 <web>/api/v1/register)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *invite == "" {
		fmt.Fprintln(os.Stderr, "[ERR] 请用 -invite 提供邀请码")
		fs.Usage()
		return 2
	}
	if *acct == "" {
		*acct = "mm-" + currentUser()
	}
	if *dev == "" {
		*dev, _ = os.Hostname()
	}
	webBase := strings.TrimRight(*web, "/")
	if webBase == "" {
		webBase = "https://demo.orbit-net.local:4431"
	} else if !strings.HasPrefix(webBase, "http") {
		webBase = "https://" + webBase
	}
	if *endpoint == "" {
		*endpoint = webBase + "/api/v1/register"
	}

	body, _ := json.Marshal(map[string]string{
		"invite_code":  *invite,
		"account_name": *acct,
		"device_name":  *dev,
	})
	// 注册端证书为定制 CA(公网 IP 证书), MVP 跳过链校验保机密性; 正式对外开放换公网证书后移除。
	httpc := &http.Client{
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}},
		Timeout:   30 * time.Second,
	}
	// 先取服务器 CA(登录链路同证书, 可含自签 CA)
	caPath := defaultCACertPath()
	if caResp, err := httpc.Get(webBase + "/ca.pem"); err == nil {
		if caBody, rerr := io.ReadAll(io.LimitReader(caResp.Body, 1<<20)); rerr == nil && len(caBody) > 0 {
			if perr := os.WriteFile(filepath.Join(filepath.Dir(*out), "orbitd-cert.pem"), caBody, 0o600); perr == nil {
				caPath = filepath.Join(filepath.Dir(*out), "orbitd-cert.pem")
			}
		}
		_ = caResp.Body.Close()
	}
	resp, err := httpc.Post(*endpoint, "application/json", strings.NewReader(string(body)))
	if err != nil {
		fmt.Fprintf(os.Stderr, "[ERR] 注册请求失败: %v\n", err)
		return 1
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var r regResp
	if err := json.Unmarshal(raw, &r); err != nil {
		fmt.Fprintf(os.Stderr, "[ERR] 服务端返回异常: HTTP %d %s\n", resp.StatusCode, string(raw))
		return 1
	}
	if !r.OK || r.DeviceToken == "" {
		fmt.Fprintf(os.Stderr, "[ERR] 注册被拒(HTTP %d): %s\n", resp.StatusCode, string(raw))
		return 1
	}

	cfg := client.Config{
		ServerAddr:  *server,
		CACertPath:  caPath,
		SelfAPIBase: webBase + "/api/v1",
		Account:     r.AccountName,
		DeviceID:    r.DeviceID,
		DeviceToken: r.DeviceToken,
		Hostname:    *dev,
		TunName:     defaultTunName(),
		Mode:        protocol.ModeSimple,
		LogFile:     defaultLogPath(),
		LogMaxBytes: 4194304,
		LogKeep:     2,
	}
	ys, err := yaml.Marshal(&cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[ERR] 生成配置失败: %v\n", err)
		return 1
	}
	if err := os.WriteFile(*out, ys, 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "[ERR] 写配置失败: %v\n", err)
		return 1
	}

	fmt.Println("注册成功: 账户", r.AccountName, "/ 设备", r.DeviceID)
	fmt.Printf("设备令牌(仅此一次展示,请勿外泄): %s\n", r.DeviceToken)
	fmt.Printf("已生成 %s, 下一步运行同目录 install.cmd/install.sh 安装并启动。\n", *out)
	return 0
}

func currentUser() string {
	if runtime.GOOS == "windows" {
		if u := os.Getenv("USERNAME"); u != "" {
			return u
		}
	}
	if u := os.Getenv("USER"); u != "" {
		return u
	}
	return "user"
}

func defaultCACertPath() string {
	if runtime.GOOS == "windows" {
		return `C:\ProgramData\OrbitClient\orbitd-cert.pem`
	}
	return "/opt/orbit-client/orbitd-cert.pem"
}

func defaultTunName() string {
	if runtime.GOOS == "windows" {
		return "Orbit"
	}
	return "orbit0"
}

func defaultLogPath() string {
	if runtime.GOOS == "windows" {
		return `C:\ProgramData\OrbitClient\orbit-cli.log`
	}
	return "/var/log/orbit-cli.log"
}

// ---- 设备自助管理: orbit-cli devices <list|rename|revoke|token|usage> [-config yaml] ----

// devicesBase 自助 API 入口: 优先取配置 self_api_base(register 时写入), 空则回落默认。
// 注意默认值是演示服务器, 自托管请务必在配置里写 self_api_base。
const defaultSelfAPIBase = "https://47.101.198.197/orbit/api/v1"

func selfAPIBase(cfg *client.Config) string {
	if cfg.SelfAPIBase != "" {
		return strings.TrimRight(cfg.SelfAPIBase, "/")
	}
	return defaultSelfAPIBase
}

func runDevices(args []string) int {
	fs := flag.NewFlagSet("devices", flag.ExitOnError)
	action := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		action = args[0]
		args = args[1:]
	}
	cfgPath := fs.String("config", deviceDefaultConfig(), "orbit-cli.yaml 路径(取 device_id/device_token)")
	devID := fs.String("device", "", "目标设备 id(默认调用者自己)")
	name := fs.String("name", "", "改名用新名字(仅 rename)")
	fs.Parse(args)

	cfg, err := client.Load(*cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[ERR] 读配置 %s: %v\n", *cfgPath, err)
		return 1
	}
	base := selfAPIBase(cfg)

	switch action {
	case "list":
		return doDeviceGet(*cfgPath, base+"/devices", "设备列表")
	case "usage":
		return doDeviceGet(*cfgPath, base+"/usage", "用量")
	case "rename":
		return doDevicePost(*cfgPath, base+"/devices/rename",
			map[string]string{"device_id": *devID, "name": *name}, "改名")
	case "revoke":
		if *devID == "" {
			fmt.Fprintln(os.Stderr, "[ERR] revoke 需要 -device <id>")
			return 2
		}
		return doDevicePost(*cfgPath, base+"/devices/revoke",
			map[string]string{"device_id": *devID}, "吊销")
	case "token":
		return doDevicePost(*cfgPath, base+"/devices/rotate-token",
			map[string]string{"device_id": *devID}, "轮换")
	default:
		fmt.Fprintln(os.Stderr, "用法: orbit-cli devices <list|usage|rename|revoke|token> [-config orbit-cli.yaml] [-device <id>] [-name <新名>]")
		return 2
	}
}

func deviceDefaultConfig() string {
	if runtime.GOOS == "windows" {
		return `C:\ProgramData\OrbitClient\orbit-cli.yaml`
	}
	return "/opt/orbit-client/orbit-cli.yaml"
}

// doDeviceGet 自助 GET 请求(设备 token 鉴权)。
func doDeviceGet(cfgPath, url, label string) int {
	cfg, err := client.Load(cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[ERR] 读配置 %s: %v\n", cfgPath, err)
		return 1
	}
	resp, raw, ok := apiDo("GET", url, cfg, nil)
	if !ok {
		fmt.Fprintf(os.Stderr, "[ERR] %s失败(HTTP %d): %s\n", label, resp.StatusCode, string(raw))
		return 1
	}
	fmt.Println(string(raw))
	return 0
}

// doDevicePost 自助 POST 请求(送体 + headers)。
func doDevicePost(cfgPath, url string, body map[string]string, label string) int {
	cfg, err := client.Load(cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[ERR] 读配置 %s: %v\n", cfgPath, err)
		return 1
	}
	b, _ := json.Marshal(body)
	resp, raw, ok := apiDo("POST", url, cfg, b)
	if !ok {
		fmt.Fprintf(os.Stderr, "[ERR] %s失败(HTTP %d): %s\n", label, resp.StatusCode, string(raw))
		return 1
	}
	switch label {
	case "轮换":
		var r struct {
			OK          bool   `json:"ok"`
			DeviceID    string `json:"device_id"`
			DeviceToken string `json:"device_token"`
		}
		_ = json.Unmarshal(raw, &r)
		fmt.Printf("新令牌(仅此一次展示,请勿外泄): %s\n", r.DeviceToken)
	case "改名":
		var r struct {
			OK       bool   `json:"ok"`
			DeviceID string `json:"device_id"`
			Name     string `json:"name"`
		}
		_ = json.Unmarshal(raw, &r)
		fmt.Printf("已改名: %s → %s\n", r.DeviceID, r.Name)
	default:
		fmt.Printf("%s成功: %s\n", label, string(raw))
	}
	return 0
}

// apiDo 带设备-token 鉴权的通用请求; 返回 响应/原文/是否 2xx。
func apiDo(method, url string, cfg *client.Config, body []byte) (*http.Response, []byte, bool) {
	return apiDoTT(method, url, cfg, body, 30*time.Second)
}

// apiDoTT 同 apiDo, 但可指定超时; 供 status 等短轮询用(服务器不可达时不被拖 30s)。
func apiDoTT(method, url string, cfg *client.Config, body []byte, timeout time.Duration) (*http.Response, []byte, bool) {
	httpc := &http.Client{
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}},
		Timeout:   timeout,
	}
	var rd io.Reader
	if body != nil {
		rd = strings.NewReader(string(body))
	}
	req, _ := http.NewRequest(method, url, rd)
	req.Header.Set("X-Orbit-Device", cfg.DeviceID)
	req.Header.Set("X-Orbit-Token", cfg.DeviceToken)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := httpc.Do(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[ERR] 请求失败: %v\n", err)
		return nil, nil, false
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp, raw, resp.StatusCode >= 200 && resp.StatusCode < 300
}
