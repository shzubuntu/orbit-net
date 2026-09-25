// 管理子命令(orbit-cli status/set/autostart/usage):
//   供 orbit-gui 托盘与运维脚本共用的幂等底层 —— 改配置/重启进程/建删计划任务
//   都在这一层完成, GUI 只负责展示与调起(写入类操作经 ShellExecute runas 提权)。
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"orbit/internal/client"
	"orbit/internal/protocol"
)

// ---- 公共小工具 ----

// printJSON 把结构输出为单行 JSON(供 GUI 解析)。
func printJSON(v interface{}) {
	b, err := json.Marshal(v)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[ERR] json: %v\n", err)
		os.Exit(1)
	}
	fmt.Println(string(b))
}

// procInfo 运行中的 orbit-cli 进程快照(排除自身 PID)。
type procInfo struct {
	PID  int
	Path string
}

// runningDaemons 列出其它 orbit-cli 进程。
func runningDaemons() []procInfo {
	var out []procInfo
	if runtime.GOOS == "windows" {
		raw, err := exec.Command("tasklist", "/fi", "imagename eq orbit-cli.exe", "/fo", "csv", "/nh").Output()
		if err != nil {
			return out
		}
		for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
			if !strings.Contains(line, "orbit-cli") {
				continue
			}
			// CSV: "orbit-cli.exe","PID","Console","Session#","Mem"
			parts := strings.Split(line, ",")
			if len(parts) < 2 {
				continue
			}
			pid, err := strconv.Atoi(strings.Trim(parts[1], `" `))
			if err != nil || pid == os.Getpid() {
				continue
			}
			out = append(out, procInfo{PID: pid})
		}
		return out
	}
	// linux: /proc 扫 comm
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return out
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid == os.Getpid() {
			continue
		}
		comm, err := os.ReadFile(filepath.Join("/proc", e.Name(), "comm"))
		if err != nil || strings.TrimSpace(string(comm)) != "orbit-cli" {
			continue
		}
		out = append(out, procInfo{PID: pid})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PID < out[j].PID })
	return out
}

// taskState Windows 计划任务查询: 返回 (存在, 状态, 运行命令)。
func taskState(name string) (bool, string, string) {
	if runtime.GOOS != "windows" {
		return false, "", ""
	}
	raw, err := exec.Command("schtasks", "/query", "/tn", name, "/fo", "list", "/v").Output()
	if err != nil {
		return false, "", ""
	}
	state, run := "", ""
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "Status:") {
			state = strings.TrimSpace(strings.TrimPrefix(line, "Status:"))
		} else if strings.HasPrefix(line, "Task To Run:") {
			run = strings.TrimSpace(strings.TrimPrefix(line, "Task To Run:"))
		}
	}
	return true, state, run
}

// adapterInfo 查询 TUN 网卡状态; 返回 (up?, ipWithPrefix, 描述)。
func adapterInfo(tunName string) (bool, string, string) {
	if tunName == "" {
		tunName = "Orbit"
	}
	if runtime.GOOS == "windows" {
		ps := fmt.Sprintf("$a=Get-NetIPAdapter -Name '%s' -ErrorAction SilentlyContinue; if($a){$aa=Get-NetIPAddress -InterfaceIndex $a.InterfaceIndex -AddressFamily IPv4 -ErrorAction SilentlyContinue; Write-Output ('STATUS='+$a.Status); if($aa){Write-Output ('IP='+$aa.IPAddress+'/'+$aa.PrefixLength)}}", tunName)
		raw, err := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command", ps).Output()
		if err != nil {
			return false, "", fmt.Sprintf("probe err: %v", err)
		}
		up, ip := false, ""
		for _, line := range strings.Split(string(raw), "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "STATUS=") {
				up = strings.Contains(line, "Up")
			} else if strings.HasPrefix(line, "IP=") {
				ip = strings.TrimPrefix(line, "IP=")
			}
		}
		if !up && ip == "" {
			return false, "", "adapter not present"
		}
		return up, ip, "wintun"
	}
	raw, err := exec.Command("ip", "-brief", "addr", "show", tunName).Output()
	if err != nil {
		return false, "", "adapter not present"
	}
	fields := strings.Fields(string(raw))
	if len(fields) < 3 {
		return false, "", "adapter not present"
	}
	return strings.Contains(fields[1], "UP"), fields[2], fields[1]
}

// logTail 读日志最后 n 行。
func logTail(path string, n int) []string {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || st.Size() == 0 {
		return nil
	}
	buf := make([]byte, int64(n)<<9) // 每行按最多 512B 估算
	if st.Size() < int64(len(buf)) {
		buf = make([]byte, st.Size())
	}
	off := int64(0)
	if st.Size() > int64(len(buf)) {
		off = st.Size() - int64(len(buf))
	}
	if _, err := f.ReadAt(buf, off); err != nil && off == 0 {
		return nil
	}
	lines := strings.Split(string(buf), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	for i := range lines {
		lines[i] = strings.TrimRight(lines[i], "\r")
	}
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// selfAPIBaseCli 兼容 main.go 里的 selfAPIBase。
func selfAPIBaseCli(cfg *client.Config) string {
	if cfg.SelfAPIBase != "" {
		return strings.TrimRight(cfg.SelfAPIBase, "/")
	}
	// ServerAddr 形如 host:28443(已含数据面端口), 必须先剥离端口再拼管理面基址,
	// 否则得到 host:28443:4431 的非法 URL(NewRequest 解析失败)。
	host := cfg.ServerAddr
	if h, _, err := net.SplitHostPort(cfg.ServerAddr); err == nil {
		host = h
	}
	if strings.Contains(host, ":") && !strings.HasPrefix(host, "[") {
		host = "[" + host + "]"
	}
	return fmt.Sprintf("https://%s:4431/api/v1", host)
}

// ---- status ----

func runStatus(args []string) int {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	cfgPath := fs.String("config", deviceDefaultConfig(), "orbit-cli.yaml 路径")
	_ = fs.Parse(args)

	type taskInfo struct {
		On    bool   `json:"on"`
		State string `json:"state,omitempty"`
		Run   string `json:"run,omitempty"`
	}
	st := map[string]interface{}{
		"ok":          true,
		"config_path": *cfgPath,
		"installed":   false,
		"errors":      []string{},
	}

	cfg, err := client.Load(*cfgPath)
	if err != nil {
		st["errors"] = append(st["errors"].([]string), fmt.Sprintf("config: %v", err))
	} else {
		st["installed"] = true
		st["server_addr"] = cfg.ServerAddr
		st["account"] = cfg.Account
		st["hostname"] = cfg.Hostname
		st["device_id"] = cfg.DeviceID
		st["tun_name"] = cfg.TunName
		st["mode"] = string(cfg.Mode)
		st["egress"] = cfg.Egress
		st["exit_device"] = cfg.ExitDevice
		st["keep_local"] = cfg.KeepLocal
	}

	// 守护进程
	if daemons := runningDaemons(); len(daemons) > 0 {
		st["daemon_running"] = true
		pids := []int{}
		for _, d := range daemons {
			pids = append(pids, d.PID)
		}
		st["daemon_pid"] = pids
	} else {
		st["daemon_running"] = false
	}

	// 网卡
	if cfg != nil {
		if up, ip, desc := adapterInfo(cfg.TunName); up || ip != "" {
			st["adapter"] = map[string]interface{}{"up": up, "ip": ip, "desc": desc}
		} else {
			st["adapter"] = nil
			if desc != "" && !strings.Contains(desc, "not present") {
				st["errors"] = append(st["errors"].([]string), desc)
			}
		}
	}

	// 自启任务(Windows)
	if runtime.GOOS == "windows" {
		ti := func(name string) taskInfo {
			on, state, run := taskState(name)
			if !on {
				return taskInfo{On: false}
			}
			return taskInfo{On: true, State: state, Run: run}
		}
		st["autostart"] = map[string]taskInfo{
			"main":   ti("OrbitClient"),
			"relink": ti("OrbitClientRelink"),
		}
	} else {
		on := fileExists("/etc/systemd/system/orbit-cli.service") || fileExists("/usr/lib/systemd/system/orbit-cli.service")
		st["autostart"] = map[string]interface{}{
			"main":   map[string]interface{}{"on": on},
			"relink": map[string]interface{}{"on": false},
		}
	}

	// 日志尾巴
	if cfg != nil && cfg.LogFile != "" {
		st["log_tail"] = logTail(cfg.LogFile, 15)
	}

	// 出口候选(同账户可用出口, 供 GUI 下拉; 服务器不可达时不阻塞)
	if cfg != nil {
		st["devices"] = fetchDevices(cfg, 4*time.Second)
	}

	printJSON(st)
	return 0
}

type deviceInfo struct {
	DeviceID     string `json:"device_id"`
	Name         string `json:"name"`
	EgressEnable bool   `json:"egress_enabled"`
	Online       bool   `json:"online"`
}

// fetchDevices 取同账户设备(自助接口), 失败返回 nil。
func fetchDevices(cfg *client.Config, timeout time.Duration) []deviceInfo {
	resp, raw, ok := apiDoTT("GET", selfAPIBaseCli(cfg)+"/devices", cfg, nil, timeout)
	if !ok {
		_ = resp
		return nil
	}
	var out []deviceInfo
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil
	}
	return out
}

// ---- set ----

func runSet(args []string) int {
	fs := flag.NewFlagSet("set", flag.ExitOnError)
	cfgPath := fs.String("config", deviceDefaultConfig(), "orbit-cli.yaml 路径")
	mode := fs.String("mode", "", "simple|smart|global (空=不改)")
	egress := fs.Bool("egress", false, "本机开放为出口 (显式传 true/false 才生效)")
	exit := fs.String("exit", "", "global/smart 默认出口 (传空清除)")
	keepLocal := fs.String("keep-local", "", "global 排除网段, 逗号分隔 (传空清除)")
	noRestart := fs.Bool("no-restart", false, "只改写配置不重启守护进程")
	fs.Parse(args)

	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })

	cfg, err := client.Load(*cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[ERR] 读配置 %s: %v\n", *cfgPath, err)
		return 1
	}
	if set["mode"] {
		if *mode == "" {
			fmt.Fprintln(os.Stderr, "[ERR] -mode 不能为空串")
			return 2
		}
		switch protocol.Mode(*mode) {
		case protocol.ModeSimple, protocol.ModeSmart, protocol.ModeGlobal:
			cfg.Mode = protocol.Mode(*mode)
		default:
			fmt.Fprintf(os.Stderr, "[ERR] 非法 mode=%q (simple|smart|global)\n", *mode)
			return 2
		}
	}
	if set["egress"] {
		cfg.Egress = *egress
	}
	if set["exit"] {
		cfg.ExitDevice = *exit
	}
	if set["keep-local"] {
		cfg.KeepLocal = splitList(*keepLocal)
	}
	if err := cfg.Validate(); err != nil {
		fmt.Fprintf(os.Stderr, "[ERR] %v\n", err)
		return 2
	}
	if err := cfg.Save(*cfgPath); err != nil {
		fmt.Fprintf(os.Stderr, "[ERR] 写配置: %v\n", err)
		return 1
	}
	if !*noRestart {
		if err := restartDaemon(); err != nil {
			fmt.Fprintf(os.Stderr, "[WARN] %v\n", err)
		}
	}
	fmt.Printf("applied: mode=%s egress=%v exit=%q keep_local=%v\n",
		cfg.Mode, cfg.Egress, cfg.ExitDevice, cfg.KeepLocal)
	return 0
}

// splitList 逗号分隔 → 去空白列表。
func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// restartDaemon: 停其它 orbit-cli → 优先经计划任务(Windows)重启到 SYSTEM 上下文,
// 无任务则后台自拉起。
func restartDaemon() error {
	for _, d := range runningDaemons() {
		if runtime.GOOS == "windows" {
			_ = exec.Command("taskkill", "/f", "/pid", strconv.Itoa(d.PID)).Run()
		} else {
			_ = exec.Command("kill", strconv.Itoa(d.PID)).Run()
		}
	}
	time.Sleep(800 * time.Millisecond) // 等 wintun 清场, 防 TUNSETIFF EBUSY

	if runtime.GOOS == "windows" {
		if on, _, _ := taskState("OrbitClient"); on {
			if err := exec.Command("schtasks", "/run", "/tn", "OrbitClient").Run(); err == nil {
				return nil
			}
		}
	}
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("restart: cannot resolve self: %v", err)
	}
	cmd := exec.Command(exe, "-config", deviceDefaultConfig())
	cmd.SysProcAttr = &syscall.SysProcAttr{}
	setDetached(cmd.SysProcAttr)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("restart: start daemon: %v", err)
	}
	return nil
}

// ---- autostart ----

func runAutostart(args []string) int {
	if len(args) >= 1 && args[0] == "status" {
		on, state, run := taskState("OrbitClient")
		rOn, rState, rRun := taskState("OrbitClientRelink")
		printJSON(map[string]interface{}{
			"main":   map[string]interface{}{"on": on, "state": state, "run": run},
			"relink": map[string]interface{}{"on": rOn, "state": rState, "run": rRun},
		})
		return 0
	}
	if len(args) < 2 {
		fmt.Fprintln(os.Stderr, "用法: orbit-cli autostart <main|relink> <on|off> | autostart status")
		return 2
	}
	target, op := args[0], args[1]
	name := "OrbitClient"
	switch target {
	case "relink":
		name = "OrbitClientRelink"
	case "main", "on":
		name = "OrbitClient"
	default:
		fmt.Fprintf(os.Stderr, "[ERR] 未知目标 %q (main|relink)\n", target)
		return 2
	}
	switch op {
	case "on":
		cmdline := []string{"schtasks", "/create", "/tn", name, "/tr", taskScriptPath(name), "/sc", taskSchedule(name), "/ru", "SYSTEM", "/rl", "HIGHEST", "/f"}
		if err := exec.Command(cmdline[0], cmdline[1:]...).Run(); err != nil {
			fmt.Fprintf(os.Stderr, "[ERR] 建任务 %s 失败: %v\n", name, err)
			return 1
		}
		fmt.Printf("autostart %s on\n", name)
	case "off":
		if err := exec.Command("schtasks", "/delete", "/tn", name, "/f").Run(); err != nil {
			fmt.Fprintf(os.Stderr, "[ERR] 删任务 %s 失败: %v\n", name, err)
			return 1
		}
		fmt.Printf("autostart %s off\n", name)
	default:
		fmt.Fprintf(os.Stderr, "[ERR] 未知操作 %q (on|off)\n", op)
		return 2
	}
	return 0
}

// taskScriptPath/taskSchedule: 任务映射(Windows 专有, 非 Windows 走自拉起)。
func taskScriptPath(name string) string {
	base := `C:\ProgramData\OrbitClient`
	if name == "OrbitClientRelink" {
		return filepath.Join(base, "relink.cmd")
	}
	return filepath.Join(base, "run-agent.cmd")
}

func taskSchedule(name string) string {
	if name == "OrbitClientRelink" {
		return "hourly"
	}
	return "onlogon"
}

// ---- usage ----

func runUsage(args []string) int {
	fs := flag.NewFlagSet("usage", flag.ExitOnError)
	cfgPath := fs.String("config", deviceDefaultConfig(), "orbit-cli.yaml 路径")
	fs.Parse(args)
	cfg, err := client.Load(*cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[ERR] 读配置 %s: %v\n", *cfgPath, err)
		return 1
	}
	_, raw, ok := apiDo("GET", selfAPIBaseCli(cfg)+"/usage", cfg, nil)
	if !ok {
		fmt.Fprintf(os.Stderr, "[ERR] usage 请求失败\n")
		return 1
	}
	var pretty map[string]interface{}
	if err := json.Unmarshal(raw, &pretty); err == nil {
		b, _ := json.MarshalIndent(pretty, "", "  ")
		fmt.Println(string(b))
		return 0
	}
	fmt.Println(string(raw))
	return 0
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}