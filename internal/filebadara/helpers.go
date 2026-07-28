package filebadara

import (
	"fmt"
	"net/http"
	"strings"
)

func (s *Server) handleShell(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}

	auth := ""
	if s.password != "" {
		auth = "-u upload"
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	fmt.Fprintf(w, shellHelper, shellQuote(s.baseURL(r)), auth)
}

func (s *Server) handlePowerShell(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}

	auth := ""
	if s.password != "" {
		auth = `, "-u", "upload"`
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	fmt.Fprintf(w, powerShellHelper, powerShellQuote(s.baseURL(r)), auth)
}

const shellHelper = `#!/bin/sh
set -eu

base=%s

if [ "$#" -ne 1 ] || [ ! -f "$1" ]; then
    echo "usage: curl -fsSL $base/sh | sh -s -- FILE" >&2
    exit 2
fi

file=$1
name=${file##*/}
size=$(wc -c < "$file")

# Diagnostics go to stderr so the download URL is the only thing on stdout.
log() {
    printf '%%s %%s\n' "$(date -u +%%Y-%%m-%%dT%%H:%%M:%%SZ)" "$1" >&2
}

# Neither URL can contain whitespace, so the shell can split the response.
set -- $(curl -fsS %s --data-urlencode "name=$name" --data-urlencode "size=$size" "$base/new")
download_url=$1
wait_url=$2

printf '%%s\n' "$download_url"

# The same token prefix the server logs, so the two sides can be lined up.
token=${download_url%%/*}
token=$(printf '%%.8s' "${token##*/}")

# The server answers with "URL OFFSET DOWNLOADER", none of which can contain
# whitespace, so the shell can split the line.
while job=$(curl -fsS "$wait_url" 2>/dev/null); do
    [ -n "$job" ] || continue
    set -- $job
    log "upload token=$token file=\"$name\" offset=$2 client=$3 status=start"
    # -C makes curl seek to the offset the server asked for, so a resumed
    # download never re-uploads the part the downloader already has.
    (
        if curl -fsS -C "$2" --upload-file "$file" "$1"; then
            log "upload token=$token file=\"$name\" offset=$2 client=$3 status=done"
        else
            log "upload token=$token file=\"$name\" offset=$2 client=$3 status=failed"
        fi
    ) &
done

wait
`

const powerShellHelper = `param([Parameter(Mandatory=$true)][string]$File)
$ErrorActionPreference = "Stop"
$base = %s

$full = (Resolve-Path -LiteralPath $File).Path
$item = Get-Item -LiteralPath $full
$args = @("-fsS", "--data-urlencode", "name=$($item.Name)", "--data-urlencode", "size=$($item.Length)"%s)

$response = & curl.exe @args "$base/new"
if ($LASTEXITCODE -ne 0) { throw "Failed to create transfer" }

$lines = @($response | Where-Object { $_ -ne "" })
$downloadUrl = $lines[0]
$waitUrl = $lines[1]
Write-Output $downloadUrl

# The same token prefix the server logs, so the two sides can be lined up.
$token = ($downloadUrl -split "/")[-2]
$token = $token.Substring(0, [Math]::Min(8, $token.Length))

# Diagnostics go to stderr so the download URL is the only thing on stdout.
# No backtick line continuation anywhere here: this file is a Go raw string.
function Write-Log($message) {
    $now = (Get-Date).ToUniversalTime().ToString("yyyy-MM-ddTHH:mm:ssZ")
    [Console]::Error.WriteLine("$now $message")
}

# One upload, run on a background thread of this same process so that it can
# report its own result to the console the moment it finishes. Start-Job would
# run it in a child process whose console output goes nowhere, which is why this
# does not use jobs at all: Start-ThreadJob only ships with PowerShell 7, and on
# Windows PowerShell the fallback silently swallowed every completion line.
$uploader = {
    param($Path, $Url, $Offset, $Token, $Name, $Client)
    # -C seeks to the offset the server asked for, so a resumed download
    # never re-uploads the part the downloader already has.
    & curl.exe -fsS -C $Offset --upload-file $Path $Url
    $status = "done"
    if ($LASTEXITCODE -ne 0) { $status = "failed" }
    $now = (Get-Date).ToUniversalTime().ToString("yyyy-MM-ddTHH:mm:ssZ")
    [Console]::Error.WriteLine("$now upload token=$Token file=""$Name"" offset=$Offset client=$Client status=$status")
}

# The server answers with "URL OFFSET DOWNLOADER".
$uploads = @()
while ($true) {
    $job = & curl.exe -fsS $waitUrl 2>$null
    if ($LASTEXITCODE -ne 0) { break }
    if ([string]::IsNullOrWhiteSpace($job)) { continue }

    $fields = $job.Trim() -split "\s+"
    Write-Log "upload token=$token file=""$($item.Name)"" offset=$($fields[1]) client=$($fields[2]) status=start"

    $shell = [PowerShell]::Create()
    [void]$shell.AddScript($uploader.ToString())
    [void]$shell.AddArgument($full)
    [void]$shell.AddArgument($fields[0])
    [void]$shell.AddArgument($fields[1])
    [void]$shell.AddArgument($token)
    [void]$shell.AddArgument($item.Name)
    [void]$shell.AddArgument($fields[2])
    $uploads += @{ Shell = $shell; Handle = $shell.BeginInvoke() }
}

foreach ($upload in $uploads) {
    [void]$upload.Shell.EndInvoke($upload.Handle)
    $upload.Shell.Dispose()
}
`

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

func powerShellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}
