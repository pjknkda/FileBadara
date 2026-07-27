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

while job=$(curl -fsS "$wait_url" 2>/dev/null); do
    [ -n "$job" ] || continue
    set -- $job
    log "upload token=$token file=\"$name\" offset=$2 status=start"
    # -C makes curl seek to the offset the server asked for, so a resumed
    # download never re-uploads the part the downloader already has.
    (
        if curl -fsS -C "$2" --upload-file "$file" "$1"; then
            log "upload token=$token file=\"$name\" offset=$2 status=done"
        else
            log "upload token=$token file=\"$name\" offset=$2 status=failed"
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

# Start-ThreadJob runs in this process, so an upload can report its own result
# as it happens. Start-Job would put it in a child whose console output is lost,
# leaving only the server's record of how the transfer ended.
$starter = "Start-Job"
if (Get-Command Start-ThreadJob -ErrorAction SilentlyContinue) { $starter = "Start-ThreadJob" }

$jobs = @()
while ($true) {
    $job = & curl.exe -fsS $waitUrl 2>$null
    if ($LASTEXITCODE -ne 0) { break }
    if ([string]::IsNullOrWhiteSpace($job)) { continue }

    $fields = $job.Trim() -split "\s+"
    Write-Log "upload token=$token file=""$($item.Name)"" offset=$($fields[1]) status=start"

    $jobs += & $starter -ScriptBlock {
        param($Path, $Url, $Offset, $Token, $Name)
        # -C seeks to the offset the server asked for, so a resumed download
        # never re-uploads the part the downloader already has.
        & curl.exe -fsS -C $Offset --upload-file $Path $Url
        $status = "done"
        if ($LASTEXITCODE -ne 0) { $status = "failed" }
        $now = (Get-Date).ToUniversalTime().ToString("yyyy-MM-ddTHH:mm:ssZ")
        [Console]::Error.WriteLine("$now upload token=$Token file=""$Name"" offset=$Offset status=$status")
    } -ArgumentList $full, $fields[0], $fields[1], $token, $item.Name
}

if ($jobs.Count -gt 0) {
    $jobs | Wait-Job | Receive-Job
    $jobs | Remove-Job
}
`

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

func powerShellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}
