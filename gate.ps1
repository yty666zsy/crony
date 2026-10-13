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
        $results.Add([pscustomobject]@{ Step = '评测回放（跳过：缺前置条件）'; Ok = $true })
    } else {
        Invoke-Gate 'go test -tags acceptance ./acceptance/ -count=1' {
            go test -tags acceptance ./acceptance/ -count=1
        }

        # 7) 评测回放（CI 门禁）：种子评测集对已录轨迹回放，断言"基线不红"。
        #
        # 为什么评测必须进门禁：评测台回答的是"这次改动有没有让历史场景变差"。
        # 只有每次改动都跑，它才是**回归基线**；只放在控制台上等人点，它只是一个演示页面。
        # 断言口径（与 AC-53 分工：AC-53 证接口自洽，这里证"基线不红"）：
        #   - failed == 0：种子集不得判红（有判红就把用例名与原因打出来）；
        #   - passed > 0：不能是"全跳过"——全跳过什么也没证明（跳过 ≠ 通过）；
        #   - passed+failed+skipped == cases 且 score == passed/(passed+failed)；
        #   - skipped 允许 > 0（环境里没有该前缀的已完成会话），但会显式提示。
        # 位置放在验收**之后**：验收里的 AC-55 会造"含写动作"的负向夹具跑一次判红的回放，
        # 本步再跑一次干净的，让库内"最近一次跑批"停在绿的状态
        # （放量判定取的就是最近一次跑批，否则一次验收就会让放量闸门长期拒绝加宽）。
        Invoke-Gate '评测回放（种子集零失败）' {
            $global:LASTEXITCODE = 0
            $base = $env:CRONY_ACC_BASE.TrimEnd('/')
            $headers = @{}
            $login = Invoke-RestMethod -Method Post -Uri "$base/login" -ContentType 'application/json' `
                -Body (@{ username = $env:CRONY_ACC_USER; password = $env:CRONY_ACC_PASS } | ConvertTo-Json)
            if ($login.code -ne 200) { throw "登录失败：code=$($login.code) msg=$($login.msg)" }
            $headers['Authorization'] = "Bearer $($login.data.token)"

            $sync = Invoke-RestMethod -Method Post -Uri "$base/ops/eval/seed/sync" `
                -ContentType 'application/json' -Headers $headers -Body '{}'
            if ($sync.code -ne 200) { throw "同步内置种子评测集失败：code=$($sync.code) msg=$($sync.msg)" }

            $run = Invoke-RestMethod -Method Post -Uri "$base/ops/eval/run" `
                -ContentType 'application/json' -Headers $headers -Body '{"trigger":"gate"}'
            if ($run.code -ne 200) { throw "回放跑批失败：code=$($run.code) msg=$($run.msg)" }
            $rep = $run.data.report
            if ($null -eq $rep) { throw '跑批没有返回报告（回放不构成回归证据）' }

            $cases = [int]$rep.cases
            $passed = [int]$rep.passed
            $failedN = [int]$rep.failed
            $skipped = [int]$rep.skipped
            Write-Host ("   回放 {0} 条样本：passed={1} failed={2} skipped={3} score={4} run_id={5}" -f `
                $cases, $passed, $failedN, $skipped, [math]::Round([double]$rep.score, 3), $rep.run_id) `
                -ForegroundColor DarkGray
            if ($skipped -gt 0) {
                Write-Host ("   注意：{0} 条样本被跳过（环境里没有对应前缀的已完成会话，跳过不算通过）" -f $skipped) -ForegroundColor Yellow
            }
            if ($passed -le 0) { throw '全部样本都被跳过：这次跑批不构成回归证据' }
            if ($failedN -ne 0) {
                $bad = @($rep.results | Where-Object { $_.status -eq 'failed' } |
                    ForEach-Object { "$($_.case_key)：$($_.reason)" })
                throw ("评测判红 {0} 条样本（改动让历史场景变差了）：{1}" -f $failedN, ($bad -join '；'))
            }
            if ($passed + $failedN + $skipped -ne $cases) {
                throw "报告计数不自洽：passed+failed+skipped=$($passed + $failedN + $skipped) ≠ cases=$cases"
            }
            $denom = $passed + $failedN
            $want = $passed / [double]$denom
            if ([math]::Abs([double]$rep.score - $want) -gt 1e-6) {
                throw "评分口径应为 passed/(passed+failed)=$([math]::Round($want, 6))（skipped 不进分母），实际 $($rep.score)"
            }
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
