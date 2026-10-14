param(
    [switch]$SkipNative,
    [switch]$SkipPreviews
)

$ErrorActionPreference = "Stop"
$root = Split-Path -Parent $PSScriptRoot
$arguments = @("$root\scripts\cross_platform_artifacts.py")
if ($SkipNative) {
    $arguments += "--skip-native"
}
if ($SkipPreviews) {
    $arguments += "--skip-previews"
}

& python @arguments
if ($LASTEXITCODE -ne 0) {
    exit $LASTEXITCODE
}
