# smoke-upload.ps1 -- end-to-end upload smoke test.
#
# ASCII only on purpose: this machine's PowerShell 5.1 parses a BOM-less UTF-8
# script as GBK, and any non-ASCII byte breaks the parser. Keep comments English.
#
# Why a fresh random PDF every run: store.GetByDigest returns the old record for
# an already-uploaded file and skips parsing entirely. With a fixed fixture a
# completely broken parser still answers 200, and the smoke test lies. The random
# token inside the page text makes sha256 unique every single run.
#
# Usage (service must be running):
#   powershell -ExecutionPolicy Bypass -File scripts/smoke-upload.ps1

$ErrorActionPreference = 'Stop'

$base = 'http://127.0.0.1:8080/api/v1/files'

function Fail {
    param([string]$Message, [string]$Body)
    Write-Host ''
    Write-Host ("FAIL: " + $Message) -ForegroundColor Red
    if ($Body) {
        Write-Host '---- response body ----' -ForegroundColor Yellow
        Write-Host $Body
        Write-Host '-----------------------' -ForegroundColor Yellow
    }
    exit 1
}

if (-not (Get-Command curl.exe -ErrorAction SilentlyContinue)) {
    Write-Host 'FAIL: curl.exe not found in PATH' -ForegroundColor Red
    exit 1
}

# ---------------------------------------------------------------- build a PDF
$stamp = Get-Date -Format 'yyyyMMddHHmmssfff'
$token = [guid]::NewGuid().ToString('N').Substring(0, 12)
$pdfPath = Join-Path $env:TEMP ("smoke-" + $stamp + ".pdf")

# One page, one line of text. The token is the only part that changes per run.
$stream = 'BT /F1 12 Tf 20 100 Td (Smoke ' + $token + ') Tj ET'
$streamLen = [Text.Encoding]::ASCII.GetByteCount($stream)

$pdf = New-Object System.Text.StringBuilder
[void]$pdf.Append("%PDF-1.4`n")
[void]$pdf.Append("1 0 obj<</Type/Catalog/Pages 2 0 R>>endobj`n")
[void]$pdf.Append("2 0 obj<</Type/Pages/Kids[3 0 R]/Count 1>>endobj`n")
[void]$pdf.Append("3 0 obj<</Type/Page/Parent 2 0 R/MediaBox[0 0 200 200]/Contents 4 0 R/Resources<</Font<</F1 5 0 R>>>>>>endobj`n")
[void]$pdf.Append("4 0 obj<</Length " + $streamLen + ">>stream`n")
[void]$pdf.Append($stream + "`n")
[void]$pdf.Append("endstream`nendobj`n")
[void]$pdf.Append("5 0 obj<</Type/Font/Subtype/Type1/BaseFont/Helvetica>>endobj`n")
[void]$pdf.Append("trailer<</Root 1 0 R>>`n%%EOF`n")

[IO.File]::WriteAllBytes($pdfPath, [Text.Encoding]::ASCII.GetBytes($pdf.ToString()))
$sha = (Get-FileHash -Algorithm SHA256 -Path $pdfPath).Hash.ToLower()
Write-Host ("[smoke] pdf   : " + $pdfPath)
Write-Host ("[smoke] token : " + $token)
Write-Host ("[smoke] sha256: " + $sha.Substring(0, 16) + "...")

# ------------------------------------------------------------ 1st upload: parse
Write-Host '[smoke] 1/4 upload (expect code=0, page_count>=1)'
$body1 = (& curl.exe -s -S -X POST -F ("file=@" + $pdfPath) $base 2>&1) -join "`n"
if (-not $body1) { Fail 'empty response from POST /files (service down?)' $body1 }

try {
    $j1 = $body1 | ConvertFrom-Json
} catch {
    Fail 'response is not JSON' $body1
}

if ($j1.code -ne 0) { Fail ("code = " + $j1.code + " (want 0)") $body1 }
if ($null -eq $j1.data) { Fail 'response has no data' $body1 }
if ([int]$j1.data.page_count -lt 1) {
    Fail ("page_count = " + $j1.data.page_count + " (want >= 1): parser returned no pages") $body1
}
$id1 = [string]$j1.data.id
if ($id1.Length -ne 26) { Fail ("id length = " + $id1.Length + " (want 26)") $body1 }
Write-Host ("[smoke] ok    : id=" + $id1 + " page_count=" + $j1.data.page_count + " confidence=" + $j1.data.confidence)

# ------------------------------------------- 2nd upload: cache hit must be same
Write-Host '[smoke] 2/4 upload same file again (expect same data.id = digest cache)'
$body2 = (& curl.exe -s -S -X POST -F ("file=@" + $pdfPath) $base 2>&1) -join "`n"
try {
    $j2 = $body2 | ConvertFrom-Json
} catch {
    Fail 'second upload: response is not JSON' $body2
}
if ($j2.code -ne 0) { Fail ("second upload code = " + $j2.code) $body2 }
$id2 = [string]$j2.data.id
if ($id2 -ne $id1) {
    Fail ("second upload returned id " + $id2 + " but first was " + $id1 + " (digest cache broken)") $body2
}
Write-Host ("[smoke] ok    : same id " + $id2 + " (cache hit)")

# ------------------------------------------------ 3rd step: pages shape (DTO)
Write-Host '[smoke] 3/4 GET /files/<id>/pages (expect snake_case keys)'
$pagesUrl = $base + '/' + $id1 + '/pages'
$body3 = (& curl.exe -s -S $pagesUrl 2>&1) -join "`n"
if (-not $body3) { Fail 'empty response from GET /pages' $body3 }

try {
    $j3 = $body3 | ConvertFrom-Json
} catch {
    Fail 'pages: response is not JSON' $body3
}
if ($j3.code -ne 0) { Fail ("pages code = " + $j3.code) $body3 }

if ($body3 -notmatch '"file_id"') { Fail 'pages body has no "file_id" key' $body3 }
if ($body3 -notmatch '"bbox"') { Fail 'pages body has no "bbox" key' $body3 }
# Guard against a DTO regression: serialising port.ParsedDoc directly leaks the
# Go field names (FileID / BBox / CharStart), and the frontend silently gets undefined.
# -cmatch, not -match: PowerShell's -match is case-insensitive, so 'BBox' would
# also match the correct lowercase "bbox" and the check would always fail.
if ($body3 -cmatch 'FileID') { Fail 'pages body leaked Go field name FileID' $body3 }
if ($body3 -cmatch 'BBox') { Fail 'pages body leaked Go field name BBox' $body3 }
if ($j3.data.pages.Count -lt 1) { Fail 'pages array is empty' $body3 }

$blockCount = 0
foreach ($p in $j3.data.pages) { $blockCount += $p.blocks.Count }
if ($blockCount -lt 1) { Fail 'no text blocks returned' $body3 }
Write-Host ("[smoke] ok    : pages=" + $j3.data.pages.Count + " blocks=" + $blockCount + " first_text=" + $j3.data.pages[0].blocks[0].text)

# ----------------------------------------------------------------- teardown
Remove-Item -LiteralPath $pdfPath -Force -ErrorAction SilentlyContinue
Write-Host '[smoke] 4/4 PASS'
exit 0
