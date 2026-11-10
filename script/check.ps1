param(
    [ValidateSet('All', 'Build', 'Format', 'FormatCheck', 'Vet', 'Test', 'Unit', 'Integration')]
    [string] $Task = 'All'
)

$ErrorActionPreference = 'Stop'
Push-Location (Split-Path $PSScriptRoot -Parent)
try {
    if ($Task -eq 'Format') {
        & gofmt -w .
        if ($LASTEXITCODE -ne 0) { throw 'gofmt failed' }
    }
    if ($Task -in @('All', 'FormatCheck')) {
        $unformatted = & gofmt -l .
        if ($LASTEXITCODE -ne 0) { throw 'gofmt failed' }
        if ($unformatted) { throw "Unformatted Go files:`n$($unformatted -join "`n")" }
    }
    if ($Task -in @('All', 'Vet')) {
        & go vet ./...
        if ($LASTEXITCODE -ne 0) { throw 'go vet failed' }
    }
    if ($Task -in @('All', 'Build')) {
        $extension = if ($IsWindows) { '.exe' } else { '' }
        & go build -o "gh-actions-archive-cache$extension" ./cmd/gh-actions-archive-cache
        if ($LASTEXITCODE -ne 0) { throw 'go build failed' }
    }
    if ($Task -in @('All', 'Test', 'Unit', 'Integration')) {
        $packages = @(switch ($Task) {
            'Unit' { './internal/...'; './cmd/...' }
            'Integration' { './test/integration/...' }
            default { './...' }
        })
        $env:CGO_ENABLED = '1'
        & go test -race -count=1 @packages
        if ($LASTEXITCODE -ne 0) { throw 'Go tests failed (race tests require a C compiler)' }
    }
}
finally {
    Pop-Location
}
