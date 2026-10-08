@echo off
rem Use the same locked generator pipeline in Git Bash on Windows.
bash "%~dp0../../scripts/generate-protos.sh"
exit /b %ERRORLEVEL%
