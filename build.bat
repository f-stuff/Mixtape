@echo off
setlocal
cd /d "%~dp0"
echo.
echo ==============================================
echo  MIXTAPE - BUILD WINDOWS APP
echo ==============================================
echo.
powershell.exe -NoProfile -ExecutionPolicy Bypass -File "%~dp0scripts\build.ps1" %*
set "RESULT=%ERRORLEVEL%"
echo.
if not "%RESULT%"=="0" (
  echo Build failed. Please see the error above.
) else (
  echo Build finished successfully.
)
echo.
if not defined CI pause
exit /b %RESULT%
