@echo off
setlocal enabledelayedexpansion
title Central Backup Agent - Installer

rem --- self-elevate to Administrator (needed to install the service) ---
net session >nul 2>&1
if %errorlevel% neq 0 (
    echo Requesting administrator rights...
    powershell -NoProfile -Command "Start-Process -Verb RunAs -FilePath '%~f0'"
    exit /b
)

set "APPDIR=%ProgramFiles%\BackupAgent"
set "SRCEXE=%~dp0backup-agent.exe"
set "DSTEXE=%APPDIR%\backup-agent.exe"

echo ============================================
echo    Central Backup Agent - Installer
echo ============================================
echo.

if not exist "%SRCEXE%" (
    echo ERROR: backup-agent.exe was not found next to this installer.
    echo Make sure you extracted the whole folder before running it.
    goto end
)

echo Installing to: %APPDIR%
if not exist "%APPDIR%" mkdir "%APPDIR%"

rem Remove any previous service so re-running cleanly updates an existing install.
if exist "%DSTEXE%" "%DSTEXE%" service uninstall >nul 2>&1

copy /y "%SRCEXE%" "%DSTEXE%" >nul
if %errorlevel% neq 0 (
    echo ERROR: could not copy the agent into %APPDIR%.
    goto end
)
rem Clear the mark-of-the-web so the service can start without prompts.
powershell -NoProfile -Command "Unblock-File -LiteralPath '%DSTEXE%'" >nul 2>&1

echo.
echo Setting up the agent...
rem A server-downloaded installer already carries the server address, token
rem and fingerprint, so this one step is all that is needed.
"%DSTEXE%" install
if %errorlevel% equ 0 goto success

echo.
echo This agent needs enrollment details from the server.
echo   ( Server GUI  -^>  Clients  -^>  Enroll new client )
echo.
set "SRV="
set "TOK="
set "FP="
set "NM="
set /p "SRV=Server URL (e.g. https://backup.example.com:8443): "
set /p "TOK=Enrollment token: "
set /p "FP=Server fingerprint (optional, press Enter to skip): "
set /p "NM=Client name (optional, press Enter to use the hostname): "
"%DSTEXE%" install --server "%SRV%" --token "%TOK%" --fingerprint "%FP%" --name "%NM%"
if %errorlevel% equ 0 goto success

echo.
echo Installation did not complete. Please re-check the details and try again.
goto end

:success
echo.
echo ============================================
echo    Done. The Central Backup Agent is installed
echo    and running as a Windows service.
echo    It should appear under Clients in the server
echo    GUI within a few seconds.
echo ============================================

:end
echo.
pause
