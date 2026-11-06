# Dev-only MySQL instance for this project (port 3307).
#
# Why a second instance instead of the system MySQL80 service:
#   the root password of that service is owned by whoever installed it, and
#   development must not be blocked by a credential nobody wrote down.
#   This instance keeps its data under .dev/ (git-ignored), binds to 127.0.0.1 only,
#   and is removed entirely by `reset`.
#
# Usage:
#   .\scripts\dev-mysql.ps1 start     # init on first run, then boot and bootstrap
#   .\scripts\dev-mysql.ps1 stop
#   .\scripts\dev-mysql.ps1 status
#   .\scripts\dev-mysql.ps1 reset     # stop + delete .dev/ (full wipe)
#
# Connection string used by .env:
#   laborlaw:laborlaw_dev@tcp(127.0.0.1:3307)/laborlaw?parseTime=true&charset=utf8mb4&loc=UTC

param(
    [ValidateSet('start', 'stop', 'status', 'reset')]
    [string]$Action = 'start'
)

# Native tools (mysql, mysqld) write progress to stderr, and Windows PowerShell 5.1
# turns redirected stderr into a terminating error under 'Stop'. Every failure path
# below checks $LASTEXITCODE explicitly instead, which is also what CI does.
$ErrorActionPreference = 'Continue'

# Override with $env:MYSQL_HOME if MySQL is installed elsewhere.
$MysqlHome = if ($env:MYSQL_HOME) { $env:MYSQL_HOME } else { 'C:\Program Files\MySQL\MySQL Server 8.0' }
$Bin = Join-Path $MysqlHome 'bin'
$RepoRoot = Split-Path -Parent $PSScriptRoot
$BaseDir = Join-Path $RepoRoot '.dev'
$DataDir = Join-Path $BaseDir 'mysql-data'
$LogFile = Join-Path $BaseDir 'mysqld.log'
$Port = 3307
$DbName = 'laborlaw'
$AppUser = 'laborlaw'
$AppPwd = 'laborlaw_dev'

function Test-MySQLUp {
    & "$Bin\mysql.exe" -h 127.0.0.1 -P $Port -u root --connect-timeout=2 -e "SELECT 1" 2>$null | Out-Null
    return $LASTEXITCODE -eq 0
}
function Wait-MySQLUp {
    param([int]$TimeoutSeconds = 60)
    $deadline = (Get-Date).AddSeconds($TimeoutSeconds)
    while ((Get-Date) -lt $deadline) {
        if (Test-MySQLUp) { return $true }
        Start-Sleep -Milliseconds 500
    }
    return $false
}

switch ($Action) {
    'status' {
        if (Test-MySQLUp) { Write-Output "UP   127.0.0.1:$Port" } else { Write-Output "DOWN 127.0.0.1:$Port" }
        break
    }

    'stop' {
        if (-not (Test-MySQLUp)) {
            Write-Output "not running"
            break
        }
        & "$Bin\mysqladmin.exe" -h 127.0.0.1 -P $Port -u root shutdown
        Write-Output "stopped"
        break
    }

    'reset' {
        if (Test-MySQLUp) { & "$Bin\mysqladmin.exe" -h 127.0.0.1 -P $Port -u root shutdown }
        Start-Sleep -Seconds 2
        if (Test-Path $BaseDir) { Remove-Item -Recurse -Force $BaseDir }
        Write-Output "removed $BaseDir"
        break
    }

    'start' {
        if (-not (Test-Path $Bin)) { throw "MySQL binaries not found at $Bin (set MYSQL_HOME)" }

        if (-not (Test-Path $DataDir)) {
            Write-Output "initializing data directory $DataDir ..."
            New-Item -ItemType Directory -Force -Path $DataDir | Out-Null
            & "$Bin\mysqld.exe" --initialize-insecure --datadir="$DataDir" --basedir="$MysqlHome" --console 2>&1 |
                Out-File -FilePath (Join-Path $BaseDir 'init.log') -Encoding utf8
            if ($LASTEXITCODE -ne 0) {
                Get-Content (Join-Path $BaseDir 'init.log') -Tail 15 | ForEach-Object { Write-Output $_ }
                throw "mysqld --initialize-insecure failed with exit code $LASTEXITCODE"
            }
        }

        if (-not (Test-MySQLUp)) {
            Write-Output "starting mysqld on 127.0.0.1:$Port ..."
            # --log-error makes mysqld own its log file. Redirecting the child's stdout
            # here instead keeps a pipe open in the parent, which makes `start` look
            # hung until the server is killed.
            $proc = Start-Process -FilePath "$Bin\mysqld.exe" `
                -ArgumentList @("--datadir=$DataDir", "--basedir=`"$MysqlHome`"", "--port=$Port", "--bind-address=127.0.0.1", "--log-error=$LogFile") `
                -PassThru -WindowStyle Hidden
            $proc.Id | Set-Content -Path (Join-Path $BaseDir 'mysqld.pid')

            if (-not (Wait-MySQLUp)) {
                Write-Output "mysqld did not become ready; last log lines:"
                if (Test-Path $LogFile) { Get-Content $LogFile -Tail 15 }
                throw "mysqld failed to start"
            }
        }

        # Bootstrap: database + application user. Root stays passwordless but is bound
        # to loopback, so it is unreachable from the network.
        $bootstrap = @"
CREATE DATABASE IF NOT EXISTS $DbName CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci;
CREATE USER IF NOT EXISTS '$AppUser'@'%' IDENTIFIED BY '$AppPwd';
ALTER USER '$AppUser'@'%' IDENTIFIED BY '$AppPwd';
GRANT ALL PRIVILEGES ON $DbName.* TO '$AppUser'@'%';
FLUSH PRIVILEGES;
"@
        & "$Bin\mysql.exe" -h 127.0.0.1 -P $Port -u root -e $bootstrap
        if ($LASTEXITCODE -ne 0) { throw "bootstrap SQL failed with exit code $LASTEXITCODE" }

        Write-Output "ready: $AppUser@tcp(127.0.0.1:$Port)/$DbName"
        Write-Output "  data: $DataDir"
        Write-Output "  log : $LogFile"
        break
    }
}
