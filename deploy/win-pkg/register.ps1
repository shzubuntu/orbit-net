# Orbit 自助开户: 填邀请码 + 服务器地址 -> 下载该服务器 CA -> 调公网注册 -> 生成本机 orbit-cli.yaml
# 用法(推荐, 不闪退): 在该文件所在目录打开 PowerShell, 执行:
#   Set-ExecutionPolicy -Scope Process Bypass -Force
#   .\register.ps1
# 或右键"以 PowerShell 运行"; 本脚本出错/结束时都会停住等待回车, 不会闪退。
#requires -Version 5.1
[Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12
$ErrorActionPreference = "Stop"

$log = Join-Path $PSScriptRoot "register-error.log"
function Die {
    param([string]$stage)
    $msg = "[ERR] $stage`n$($_.Exception.ToString())"
    Write-Host $msg -ForegroundColor Red
    try { ($msg) | Out-File -FilePath $log -Encoding utf8 -Append } catch {}
    Write-Host "错误详情已写入: $log" -ForegroundColor Yellow
    Read-Host "按回车退出"
    exit 1
}

Write-Host "== Orbit 自助开户 =="
Write-Host "本机: $([System.Environment]::MachineName)"

try {
    $srvHost = Read-Host "服务器地址(公共HTTPS入口, 形如 vpn.example.com:4431; 管理员提供)"
    if ([string]::IsNullOrWhiteSpace($srvHost)) { throw "服务器地址不能为空(找管理员要 公网IP:4431)" }
    $srvHost = $srvHost.Trim()
    if ($srvHost -match "^https?://") { $base = $srvHost.TrimEnd('/') }
    else { $base = "https://$srvHost" }

    $portPrompt = "数据面端口(默认 28443)"
    $dataPort = Read-Host $portPrompt
    if ([string]::IsNullOrWhiteSpace($dataPort)) { $dataPort = "28443" }
    $dataPort = $dataPort.Trim()

    $hostOnly = $srvHost
    if ($hostOnly -match "^(https?://)?([^:/]+)(:\d+)?") { $hostOnly = $Matches[2] }

    $invite = Read-Host "邀请码(由管理员发放)"
    if ([string]::IsNullOrWhiteSpace($invite)) { throw "邀请码不能为空" }
    $acctName = Read-Host "账户名(回车用默认 mm-$env:USERNAME)"
    if ([string]::IsNullOrWhiteSpace($acctName)) { $acctName = "mm-$env:USERNAME" }
    $devName  = Read-Host "设备名(回车用默认 $env:COMPUTERNAME)"
    if ([string]::IsNullOrWhiteSpace($devName))  { $devName = $env:COMPUTERNAME }

    # 服务器为自签定制 CA: 先取 CA 证书(注册/隧道共用同张私有 CA), 再做注册。
    # 链路保机制: HTTPS + 邀请码即闸; 管理员可用公网可信证书替换后移除 callback。
    [System.Net.ServicePointManager]::ServerCertificateValidationCallback = { $true }
    Write-Host "从 $base/ca.pem 下载服务器 CA 证书 ..."
    $caPath = Join-Path $PSScriptRoot "orbitd-cert.pem"
    Invoke-WebRequest -Uri "$base/ca.pem" -OutFile $caPath -UseBasicParsing -TimeoutSec 30

    $body = @{ invite_code = $invite; account_name = $acctName; device_name = $devName } | ConvertTo-Json
    Write-Host "正在注册 $base/api/v1/register ..."
    $r = Invoke-RestMethod -Method Post -Uri "$base/api/v1/register" `
        -ContentType "application/json" -Body $body -UseBasicParsing -TimeoutSec 30
} catch {
    Die "注册/下载CA失败"
}
if (-not $r.ok) { Write-Host "[ERR] 注册失败: $(ConvertTo-Json $r)"; Read-Host "按回车退出"; exit 3 }

Write-Host ""
Write-Host "注册成功: 账户 $($r.account_name) / 设备 $($r.device_id)"
Write-Host "设备令牌已写入配置(仅此一次展示, 请勿外泄): $($r.device_token)"

$yaml = @"
server_addr: $hostOnly`:$dataPort
ca_cert_path: C:\ProgramData\OrbitClient\orbitd-cert.pem
account: $($r.account_name)
device_id: $($r.device_id)
device_token: $($r.device_token)
hostname: $devName
tun_name: Orbit
mode: simple
egress: false
log_file: C:\ProgramData\OrbitClient\orbit-cli.log
log_max_bytes: 4194304
log_keep: 2
self_api_base: $base/api/v1
"@
Set-Content -Path "$PSScriptRoot\orbit-cli.yaml" -Value $yaml -Encoding ASCII
Write-Host ""
Write-Host "已生成 orbit-cli.yaml + orbitd-cert.pem (与 install.cmd 同目录)。下一步: 右键 install.cmd 以管理员身份运行安装并启动。"
Write-Host "如需出口上网(获得管理员授权后)可把 mode 改为 smart 并配置 rules 指向出口设备。"
Write-Host ""
Read-Host "完成, 按回车退出"