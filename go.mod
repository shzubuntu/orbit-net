module orbit

go 1.27

require (
	github.com/lxn/walk v0.0.0-20210112085537-c389da54e794
	github.com/lxn/win v0.0.0-20210218163916-a377121e959e
	golang.org/x/crypto v0.49.0
	golang.org/x/sys v0.43.0
	// Windows TUN(Wintun) 官方绑定; 运行时需 exe 同目录 wintun.dll
	golang.zx2c4.com/wintun v0.0.0-20230126152724-0fa3db229ce2
	gopkg.in/yaml.v3 v3.0.1
	// gVisor 用户态协议栈(B 端出口模式用): 2026-09 go 分支(含生成文件,可直接编译)。
	// 注意: 不要升到更新伪版本,内部 stack/bridge 包布局冲突会编译失败。
	gvisor.dev/gvisor v0.0.0-20260919055340-501da953ee38
)

require (
	github.com/google/btree v1.1.2 // indirect
	golang.org/x/exp v0.0.0-20250711185948-6ae5c78190dc // indirect
	golang.org/x/time v0.15.0 // indirect
	gopkg.in/Knetic/govaluate.v3 v3.0.0 // indirect
)
