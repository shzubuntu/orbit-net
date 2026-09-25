#!/bin/bash
# orbit-cli Windows 客户端打包(自助开户版):
#   不再烤死演示账号/令牌; 新用户先跑 register.ps1 领配置, 再 install.cmd
#   交叉编译 console exe + 装配 + zip 到 /opt/orbit/www/
set -e
cd /root/project/orbit

rm -rf /tmp/winpkg && mkdir -p /tmp/winpkg

# 1. 交叉编译(默认 console 子系统; 禁止 -H windowsgui, 见 AGENTS 教训 9)
GOOS=windows GOARCH=amd64 go build -trimpath -ldflags "-s -w" -o /tmp/winpkg/orbit-cli.exe ./cmd/orbit-cli
cp /tmp/winpkg/orbit-cli.exe /tmp/winpkg/orbit-daemon.exe   # 守护用独立 exe 名, 见 admincmd.go:daemonExeName

# 3. GUI 托盘(也打进 zip): 优先同目录新版 orbit-cli.exe, 退回 C:\ProgramData\OrbitClient\
#    rsrc.syso = 内嵌 comctl32 v6 manifest + 应用图标(无则生成一次, 提交排除)
if [ ! -f cmd/orbit-gui/rsrc_windows.syso ]; then
  go run github.com/akavel/rsrc@latest \
    -manifest cmd/orbit-gui/app.manifest \
    -ico cmd/orbit-gui/assets/orbit.ico \
    -arch amd64 -o cmd/orbit-gui/rsrc_windows.syso
fi
GOOS=windows GOARCH=amd64 go build -trimpath -ldflags "-s -w -H windowsgui" -o /tmp/winpkg/orbit-gui.exe ./cmd/orbit-gui

# 4. 运行时依赖(证书由 register 阶段按服务器地址现场下载, 不再预置)
cp /opt/orbit/www/wintun.dll /tmp/winpkg/
cp /root/project/orbit/deploy/win-pkg/wintun-LICENSE.txt /tmp/winpkg/wintun-LICENSE.txt
cp /root/project/orbit/deps/wintun-NOTICE.md /tmp/winpkg/wintun-NOTICE.md

# 5. 自助开户脚本: 填邀请码 -> 公网注册 -> 生成 orbit-cli.yaml
cp /root/project/orbit/deploy/win-pkg/register.ps1 /tmp/winpkg/register.ps1

# 5. 双击一键安装(orbit-setup.bat): 自动升权 -> 填服务器地址+邀请码 -> exe注册开户(现场下载CA) -> 装 ProgramData -> 建任务自启
printf '%s\r\n' \
'@echo off' \
'chcp 65001 >nul' \
'cd /d "%~dp0"' \
'net session >nul 2>&1' \
'if errorlevel 1 (' \
'  echo 需要管理员权限,正在自动提升...' \
'  powershell -NoProfile -ExecutionPolicy Bypass -Command "Start-Process -FilePath ''%~f0'' -Verb RunAs -WorkingDirectory ''%~dp0''"' \
'  exit /b' \
')' \
'echo ===== Orbit 一键安装 =====' \
'if not exist "orbit-cli.exe" (echo [ERR] 缺少 orbit-cli.exe - 请先完整解压整个 zip 再双击本脚本 & pause & exit /b 1)' \
'if not exist "orbit-daemon.exe" (echo [ERR] 缺少 orbit-daemon.exe - 请先完整解压整个 zip 再运行本脚本 & pause & exit /b 1)' \
'if not exist "wintun.dll"    (echo [ERR] 缺少 wintun.dll    - 请先完整解压整个 zip 再双击本脚本 & pause & exit /b 1)' \
'set "SRV="' \
'set /p SRV=服务器地址(公共HTTPS入口, 如 vpn.example.com:4431; 管理员提供): ' \
'if "%SRV%"==""   (echo [ERR] 服务器地址不能为空 & pause & exit /b 1)' \
'set /p DPORT=数据面端口(默认28443): ' \
'if "%DPORT%"==""  set "DPORT=28443"' \
'set "HOST=%SRV%"' \
'set "WEBP=4431"' \
'for /f "tokens=1,2 delims=:" %%a in ("%SRV%") do (' \
'  set "HOST=%%a"' \
'  if not "%%b"=="" set "WEBP=%%b"' \
')' \
'set "INVITE="' \
'set /p INVITE=请输入邀请码(管理员发放): ' \
'if "%INVITE%"=="" (echo [ERR] 邀请码不能为空 & pause & exit /b 1)' \
'echo 正在注册开户...' \
'orbit-cli.exe register -invite "%INVITE%" -server "%HOST%:%DPORT%" -web "%HOST%:%WEBP%"' \
'if errorlevel 1 (echo [ERR] 注册失败,请重试或截图 & pause & exit /b 1)' \
'if not exist "orbit-cli.yaml"  (echo [ERR] 注册未生成 orbit-cli.yaml,异常终止 & pause & exit /b 1)' \
'if not exist "orbitd-cert.pem" (echo [ERR] 未下载到服务器CA orbitd-cert.pem & pause & exit /b 1)' \
'set APP=C:\ProgramData\OrbitClient' \
'mkdir "%APP%" 2>nul' \
'copy /y orbit-cli.exe "%APP%" >nul' \
'copy /y orbit-daemon.exe "%APP%" >nul' \
'copy /y wintun.dll "%APP%" >nul' \
'copy /y orbitd-cert.pem "%APP%" >nul' \
'copy /y orbit-cli.yaml "%APP%" >nul' \
'copy /y run-agent.cmd "%APP%" >nul' \
'copy /y uninstall.cmd "%APP%" >nul' \
'schtasks /create /tn OrbitClient /tr "C:\ProgramData\OrbitClient\run-agent.cmd" /sc onlogon /ru SYSTEM /rl HIGHEST /f' \
'schtasks /run /tn OrbitClient' \
'echo 安装完成并已启动. 自检: ipconfig /all 看 "Orbit" 网卡 10.0.0.x; 日志 C:\ProgramData\OrbitClient\orbit-cli.log' \
'pause' \
> /tmp/winpkg/orbit-setup.bat

# 6. 手动安装(install.cmd): 引号/重定向只写在文件里, 不在 schtasks 命令行
printf '%s\r\n' \
'@echo off' \
'setlocal' \
'cd /d "%~dp0"' \
'net session >nul 2>&1 || (echo Please run as Administrator (右键以管理员身份运行). & pause & exit /b 1)' \
'set APP=C:\ProgramData\OrbitClient' \
'if not exist "orbit-cli.exe"   (echo [ERR] orbit-cli.exe   missing - 请先完整解压整个 zip 再运行本脚本 & pause & exit /b 1)' \
'if not exist "orbit-daemon.exe" (echo [ERR] 缺少 orbit-daemon.exe - 请先完整解压整个 zip 再运行本脚本 & pause & exit /b 1)' \
'if not exist "wintun.dll"      (echo [ERR] wintun.dll      missing - 请先完整解压整个 zip 再运行本脚本 & pause & exit /b 1)' \
'if not exist "orbitd-cert.pem" (echo [ERR] orbitd-cert.pem missing - 请先完整解压整个 zip 再运行本脚本 & pause & exit /b 1)' \
'if not exist "orbit-cli.yaml"  (echo [ERR] orbit-cli.yaml  missing - 请先运行 register.ps1 领配置,再回来装. & pause & exit /b 1)' \
'if not exist "run-agent.cmd"   (echo [ERR] run-agent.cmd   missing - 请先完整解压整个 zip 再运行本脚本 & pause & exit /b 1)' \
'mkdir "%APP%" 2>nul' \
'copy /y orbit-cli.exe "%APP%" >nul' \
'copy /y orbit-daemon.exe "%APP%" >nul' \
'copy /y wintun.dll "%APP%" >nul' \
'copy /y orbitd-cert.pem "%APP%" >nul' \
'copy /y orbit-cli.yaml "%APP%" >nul' \
'copy /y run-agent.cmd "%APP%" >nul' \
'copy /y uninstall.cmd "%APP%" >nul' \
'schtasks /create /tn OrbitClient /tr "C:\ProgramData\OrbitClient\run-agent.cmd" /sc onlogon /ru SYSTEM /rl HIGHEST /f' \
'echo Installed. Starting now...' \
'schtasks /run /tn OrbitClient' \
'echo Adapter "Orbit" created and client connected. Logs: C:\ProgramData\OrbitClient\orbit-cli.log' \
'pause' \
> /tmp/winpkg/install.cmd

printf '%s\r\n' \
'@echo off' \
'cd /d "%~dp0"' \
'rem 守护用独立 exe 名: 若按 orbit-cli.exe 判定, GUI/终端里任何并发的' \
'rem orbit-cli status 调用都会被误认为"守护已在跑", 从而跳过真正拉起。' \
'tasklist /fi "imagename eq orbit-daemon.exe" 2>nul | find /i "orbit-daemon.exe" >nul && exit /b 0' \
'start "" /b "C:\ProgramData\OrbitClient\orbit-daemon.exe" -config "C:\ProgramData\OrbitClient\orbit-cli.yaml" 2>"C:\ProgramData\OrbitClient\orbit-daemon-crash.log"' \
'start "" "C:\ProgramData\OrbitClient\orbit-cli.exe" -config "C:\ProgramData\OrbitClient\orbit-cli.yaml"' \
> /tmp/winpkg/run-agent.cmd

# 一键卸载: 删任务 -> 杀进程 -> 清目录(双击或右键管理员运行)
printf '%s\r\n' \
'@echo off' \
'setlocal' \
'cd /d "%~dp0"' \
'net session >nul 2>&1 || (echo Please run as Administrator (右键以管理员身份运行). & pause & exit /b 1)' \
'schtasks /delete /tn OrbitClient /f >nul 2>&1' \
'schtasks /delete /tn OrbitClientRelink /f >nul 2>&1' \
'taskkill /f /im orbit-daemon.exe >nul 2>&1' \
'taskkill /f /im orbit-cli.exe >nul 2>&1' \
'ping 127.0.0.1 -n 3 >nul' \
'rd /s /q "C:\ProgramData\OrbitClient" >nul 2>&1' \
'if exist "C:\ProgramData\OrbitClient" (' \
'  ping 127.0.0.1 -n 3 >nul' \
'  taskkill /f /im orbit-cli.exe >nul 2>&1' \
'  rd /s /q "C:\ProgramData\OrbitClient" >nul 2>&1' \
')' \
'if exist "C:\ProgramData\OrbitClient" (echo UNINSTALL_FAIL: 无法删除 C:\ProgramData\OrbitClient & pause & exit /b 1)' \
'echo Orbit client uninstalled.' \
'pause' \
> /tmp/winpkg/uninstall.cmd

# 7. 自愈: 计划任务启动时检查 Orbit 网卡与进程, 掉了每小时拉起一次
printf '%s\r\n' \
'@echo off' \
'set APP=C:\ProgramData\OrbitClient' \
'schtasks /create /tn OrbitClientRelink /tr "C:\ProgramData\OrbitClient\relink.cmd" /sc hourly /mo 1 /ru SYSTEM /rl HIGHEST /f' \
'echo self-heal task installed.' \
'pause' \
> /tmp/winpkg/install-relink.cmd

printf '%s\r\n' \
'@echo off' \
'net session >nul 2>&1 || (echo need admin & exit /b 1)' \
'tasklist /fi "imagename eq orbit-daemon.exe" 2>nul | find /i "orbit-daemon.exe" >nul || (C:\ProgramData\OrbitClient\run-agent.cmd &)' \
> /tmp/winpkg/relink.cmd

# 8. README
printf '%s\r\n' \
'=== Orbit Client (Windows) ===' \
'1. [推荐] 双击 orbit-setup.bat -> 自动提升管理员 -> 输入服务器地址(如 vpn.example.com:4431)+邀请码 -> 自动下载服务器CA+注册开户+安装+启动, 一步到位' \
'2. [手动] 右键 register.ps1 以 PowerShell 运行 -> 填服务器地址+邀请码 -> 生成 orbit-cli.yaml+orbitd-cert.pem; 再右键 install.cmd 管理员运行' \
'3. 首次运行会注册 Wintun 驱动并创建名为 "Orbit" 的虚拟网卡(10.0.0.x)' \
'4. 开机自启: 已建计划任务 OrbitClient(onlogon, SYSTEM); 另跑 install-relink.cmd 可加每小时兜底自愈' \
'5. 出口上网: 向管理员申请授权, 授权后把 orbit-cli.yaml 的 mode 改 smart 并配置 rules' \
'6. 卸载: 右键 uninstall.cmd 管理员运行(删任务/杀进程/清目录)' \
'7. 图形面板 orbit-gui.exe: 与同 zip 的新版 orbit-cli.exe 放同一目录, 双击启动(托盘常驻);' \
'   可切模式/选出口/开自启/看用量/看日志; 写操作(改配置/重启/自启)会弹 UAC 管理员确认' \
'8. 老用户升级: 先停掉客户端, 再把新版 orbit-cli.exe 与 orbit-daemon.exe 一起复制到 C:\ProgramData\OrbitClient\ 覆盖, 最后启动 orbit-gui' \
'   (守护拆成独立 exe 名以免把并发的 orbit-cli status 调用误判为守护; 两个文件缺一不可)' \
'---' \
> /tmp/winpkg/README.txt

# 9. 打 zip 到下载目录
cd /tmp/winpkg
rm -f /opt/orbit/www/orbit-client.zip
zip -qr /opt/orbit/www/orbit-client.zip .
ls -l /opt/orbit/www/orbit-client.zip
echo "PKG READY"