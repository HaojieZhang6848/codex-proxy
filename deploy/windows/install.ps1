<#
.SYNOPSIS
    Installs codex-proxy as a Windows service.

.DESCRIPTION
    Run this from the unpacked release directory in an elevated PowerShell:

        .\install.ps1                 install or upgrade, then start
        .\install.ps1 -Uninstall      stop and remove the service
        .\install.ps1 -Uninstall -Purge
                                      also delete the installation directory

    The service is registered as:
        "<InstallDir>\codex-proxy.exe" run -config "<InstallDir>\config.yaml"

    Logs go to the Windows Event Log (Event Viewer -> Windows Logs ->
    Application, source "codex-proxy") unless log.file is set in the
    configuration file.
#>
#Requires -RunAsAdministrator
[CmdletBinding()]
param(
    [string]$InstallDir = (Join-Path $env:ProgramFiles 'codex-proxy'),
    [string]$ConfigPath,
    [switch]$Uninstall,
    [switch]$Purge,
    [switch]$NoStart
)

$ErrorActionPreference = 'Stop'

$ServiceName = 'codex-proxy'
$DisplayName = 'Codex Proxy'
$Description = 'Forwards Codex Responses API requests to configured upstream models.'

function Write-Step($message) { Write-Host "==> $message" -ForegroundColor Cyan }
function Write-Warn($message) { Write-Host "warning: $message" -ForegroundColor Yellow }

$ScriptDir = Split-Path -Parent $MyInvocation.MyCommand.Path
if (-not $ConfigPath) { $ConfigPath = Join-Path $InstallDir 'config.yaml' }
$ExePath = Join-Path $InstallDir 'codex-proxy.exe'

function Test-ServiceExists {
    return [bool](Get-Service -Name $ServiceName -ErrorAction SilentlyContinue)
}

function Stop-AndRemoveService {
    if (-not (Test-ServiceExists)) { return }
    Write-Step "stopping $ServiceName"
    Stop-Service -Name $ServiceName -Force -ErrorAction SilentlyContinue
    $service = Get-CimInstance Win32_Service -Filter "Name='$ServiceName'" -ErrorAction SilentlyContinue
    if ($service -and $service.ProcessId -gt 0) {
        Wait-Process -Id $service.ProcessId -Timeout 30 -ErrorAction SilentlyContinue
    }
    Write-Step 'removing the service registration'
    & sc.exe delete $ServiceName | Out-Null
    Start-Sleep -Seconds 1
}

function Uninstall-CodexProxy {
    Stop-AndRemoveService
    if (Test-Path $ExePath) {
        Write-Step "removing $ExePath"
        Remove-Item -LiteralPath $ExePath -Force
    }
    if ($Purge -and (Test-Path $InstallDir)) {
        Write-Step "removing $InstallDir"
        Remove-Item -LiteralPath $InstallDir -Recurse -Force
    } else {
        Write-Host "configuration in $InstallDir was kept (use -Purge to delete it)"
    }
    if ([System.Diagnostics.EventLog]::SourceExists($ServiceName)) {
        Remove-EventLog -Source $ServiceName -ErrorAction SilentlyContinue
    }
    Write-Step 'uninstalled'
}

function Install-CodexProxy {
    $source = Join-Path $ScriptDir 'codex-proxy.exe'
    if (-not (Test-Path $source)) {
        throw "codex-proxy.exe not found next to this script; run it from the unpacked release directory"
    }

    Stop-AndRemoveService

    Write-Step "installing to $InstallDir"
    New-Item -ItemType Directory -Path $InstallDir -Force | Out-Null
    Copy-Item -LiteralPath $source -Destination $ExePath -Force

    if (Test-Path $ConfigPath) {
        Write-Step "keeping the existing $ConfigPath"
    } else {
        Write-Step "installing $ConfigPath"
        Copy-Item -LiteralPath (Join-Path $ScriptDir 'config.example.yaml') -Destination $ConfigPath -Force
        Write-Warn "edit $ConfigPath before the proxy can serve requests: it still contains environment variable placeholders"
    }

    # Makes the event source available so log entries show up in Event Viewer.
    if (-not [System.Diagnostics.EventLog]::SourceExists($ServiceName)) {
        try {
            New-EventLog -LogName Application -Source $ServiceName
        } catch {
            Write-Warn "could not register the '$ServiceName' event log source: $($_.Exception.Message)"
        }
    }

    Write-Step 'registering the service'
    $binaryPath = '"{0}" run -config "{1}"' -f $ExePath, $ConfigPath
    New-Service -Name $ServiceName `
        -BinaryPathName $binaryPath `
        -DisplayName $DisplayName `
        -Description $Description `
        -StartupType Automatic | Out-Null

    # Restart three times, then give up, with the counter reset after a day.
    & sc.exe failure $ServiceName reset= 86400 actions= restart/5000/restart/15000/restart/60000 | Out-Null

    if ($NoStart) { return }

    Write-Step "starting $ServiceName"
    Start-Service -Name $ServiceName
    Start-Sleep -Seconds 2
    $service = Get-Service -Name $ServiceName
    if ($service.Status -eq 'Running') {
        Write-Step "$ServiceName is running"
    } else {
        Write-Warn "$ServiceName is $($service.Status); check the Event Log (source '$ServiceName')"
    }
    Write-Host ''
    Write-Host "configuration: $ConfigPath"
    Write-Host "status:        Get-Service $ServiceName"
    Write-Host "logs:          Get-EventLog -LogName Application -Source $ServiceName -Newest 50"
}

if ($Uninstall) { Uninstall-CodexProxy } else { Install-CodexProxy }
