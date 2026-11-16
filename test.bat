@echo off
setlocal
call "%~dp0build.bat" -TestsOnly %*
exit /b %ERRORLEVEL%
