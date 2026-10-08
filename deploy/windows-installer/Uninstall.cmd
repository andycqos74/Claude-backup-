@echo off
title Central Backup Agent - Uninstaller

net session >nul 2>&1
if %errorlevel% neq 0 (
    echo Requesting administrator rights...
    powershell -NoProfile -Command "Start-Process -Verb RunAs -FilePath '%~f0'"
    exit /b
)

set "APPDIR=%ProgramFiles%\BackupAgent"
echo Removing the Central Backup Agent service...
if exist "%APPDIR%\backup-agent.exe" (
    "%APPDIR%\backup-agent.exe" uninstall
) else (
    echo (agent not found in %APPDIR% - nothing to remove)
)
echo.
echo The service has been removed. Credentials in
echo   C:\ProgramData\BackupAgent
echo were kept; delete that folder manually for a full wipe.
echo The client still exists on the server until you delete it there.
echo.
pause
