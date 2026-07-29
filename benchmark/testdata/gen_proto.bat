@ECHO OFF
SETLOCAL
PUSHD "%~dp0" || EXIT /B 1

SET "GOWORK=off"
IF "%PROTOC%" == "" SET "PROTOC=protoc"
FOR %%I IN ("%~dp0..\..") DO SET "REPO_ROOT=%%~fI"

FOR /F "delims=" %%I IN ('go tool -modfile="%REPO_ROOT%\tools\go.mod" -n protoc-gen-go') DO SET "PROTOC_GEN_GO=%%I"
IF NOT DEFINED PROTOC_GEN_GO (
  ECHO failed to resolve protoc-gen-go from tools\go.mod
  GOTO error
)

FOR /F "delims=" %%I IN ('"%PROTOC%" --version') DO SET "FOUND_PROTOC_VERSION=%%I"
IF NOT "%FOUND_PROTOC_VERSION%" == "libprotoc 35.1" (
  ECHO protoc 35.1 is required ^(found %FOUND_PROTOC_VERSION%^)
  GOTO error
)

"%PROTOC%" --plugin=protoc-gen-go="%PROTOC_GEN_GO%" --go_out=. --go_opt=paths=source_relative *.proto
SET "EXIT_CODE=%ERRORLEVEL%"
POPD
EXIT /B %EXIT_CODE%

:error
POPD
EXIT /B 1
