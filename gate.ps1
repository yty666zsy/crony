#!/usr/bin/env pwsh
<#
.SYNOPSIS
  crony 一键门禁：格式 + 构建 + 静态检查 + 单元测试（可选：黑盒验收）。

.DESCRIPTION
  把"改完代码必须先过的几道关"固化成一个命令。它存在的理由是：只跑 `go test` 会漏掉三类真实问题——
    - gofmt：格式漂移（IDE 会顺手改，但没人记得查）；
    - `go vet -tags acceptance`：**普通 `go vet ./...` 不覆盖**带构建标签的验收代码，
      所以验收用例可以"编译不过却门禁全绿"；
    - build：只跑测试不代表所有 main 包都能编译。
  每一步都会打印 ok/FAIL，最后给一张汇总表，并以非 0 退出码表示"有步骤失败"（可直接接 CI）。

  机器相关的 Go 环境（GOPATH / GOMODCACHE / GOCACHE / GOTOOLCHAIN）与黑盒验收所需的
  CRONY_* 变量**不写进脚本**：如果仓库根存在 `.gate.env.ps1`（已在 .gitignore），脚本会自动
  dot-source 它。这样"门禁怎么跑"是共享的、"跑在哪台机器上"是本地的。

.PARAMETER Acceptance
  额外跑黑盒验收（`go test -tags acceptance`）。需要被测系统已在 `CRONY_ACC_BASE` 上运行，
  且 `CRONY_ACC_USER` / `CRONY_ACC_PASS` 已配置。缺这些前置条件时**跳过并写明原因**——
  按本项目的三态口径，"前置条件不存在、什么都没观测到"既不算通过也不算失败。

.PARAMETER NoTest
  只做构建与静态检查，跳过单元测试（赶时间时的子集门禁）。

.EXAMPLE
  pwsh -File gate.ps1
  pwsh -File gate.ps1 -Acceptance
  pwsh -File gate.ps1 -NoTest
#>
[CmdletBinding()]
param(
    [switch]$Acceptance,
    [switch]$NoTest
)

$ErrorActionPreference = 'Stop'
$root = $PSScriptRoot
Set-Location $root

# 本地环境（可选）：机器相关的 Go 缓存路径与验收变量放这里，不入 git。
$localEnv = Join-Path $root '.gate.env.ps1'
if (Test-Path $localEnv) {
    Write-Host "· 载入本地环境 $localEnv" -ForegroundColor DarkGray
    . $localEnv
}

# Go 源码根目录。gofmt 只扫这些，避免走进 console/node_modules。
$goDirs = @('admin', 'common', 'node', 'acceptance')

$results = [System.Collections.Generic.List[object]]::new()

function Invoke-Gate([string]$name, [scriptblock]$body) {
    Write-Host ''
    Write-Host "== $name" -ForegroundColor Cyan
    $ok = $true
    try {
        & $body
        if ($null -ne $LASTEXITCODE -and $LASTEXITCODE -ne 0) { $ok = $false }
    } catch {
        Write-Host "   $($_.Exception.Message)" -ForegroundColor Red
        $ok = $false
    }
    if ($ok) { Write-Host '   ok' -ForegroundColor Green } else { Write-Host '   FAIL' -ForegroundColor Red }
    $results.Add([pscustomobject]@{ Step = $name; Ok = $ok })
}

# 1) gofmt：列出即"需要修"（gofmt 自身退出码恒为 0，必须看输出）。
Invoke-Gate 'gofmt -l (格式)' {
    $out = & gofmt -l $goDirs
    if ($out) {
        Write-Host "   以下文件未 gofmt（修：go fmt $($goDirs -join ' ') 或 gofmt -w ...）：" -ForegroundColor Yellow
        $out | ForEach-Object { Write-Host "     $_" -ForegroundColor Yellow }
        $global:LASTEXITCODE = 1
    } else {
        $global:LASTEXITCODE = 0
    }
}

Invoke-Gate 'go build ./...' { go build ./... }
Invoke-Gate 'go vet ./...' { go vet ./... }
Invoke-Gate 'go vet -tags acceptance ./acceptance/' { go vet -tags acceptance ./acceptance/ }

if (-not $NoTest) {
    Invoke-Gate 'go test ./... -count=1' { go test ./... -count=1 }
}

if ($Acceptance) {
    $missing = @()
    foreach ($v in @('CRONY_ACC_BASE', 'CRONY_ACC_USER', 'CRONY_ACC_PASS')) {
        if (-not [Environment]::GetEnvironmentVariable($v)) { $missing += $v }
    }
    if ($missing.Count -gt 0) {
        Write-Host ''
        Write-Host "== 黑盒验收：跳过（缺 $($missing -join ', ')）" -ForegroundColor Yellow
        Write-Host '   前置条件不存在 = 没观测到任何东西，按三态口径既不算通过也不算失败。' -ForegroundColor DarkGray
        $results.Add([pscustomobject]@{ Step = 'acceptance（跳过：缺前置条件）'; Ok = $true })
    } else {
        Invoke-Gate 'go test -tags acceptance ./acceptance/ -count=1' {
            go test -tags acceptance ./acceptance/ -count=1
        }
    }
}

Write-Host ''
Write-Host '================ 门禁汇总 ================' -ForegroundColor White
foreach ($r in $results) {
    $mark = if ($r.Ok) { 'ok  ' } else { 'FAIL' }
    $color = if ($r.Ok) { 'Green' } else { 'Red' }
    Write-Host ('  [{0}] {1}' -f $mark, $r.Step) -ForegroundColor $color
}
$failed = @($results | Where-Object { -not $_.Ok })
if ($failed.Count -gt 0) {
    Write-Host "结果：失败 $($failed.Count) 步" -ForegroundColor Red
    exit 1
}
Write-Host '结果：全部通过' -ForegroundColor Green
exit 0
