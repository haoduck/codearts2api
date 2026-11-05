@echo off
chcp 65001 >nul
cd /d %~dp0

rem 从 .env 读取 CA2A_API_KEY 等环境变量
if exist .env (
  for /f "usebackq tokens=1,* delims==" %%a in (".env") do (
    if not "%%a"=="" if not "%%b"=="" set "%%a=%%b"
  )
)

if "%CA2A_API_KEY%"=="" (
  echo [ERROR] CA2A_API_KEY 未设置，请检查 .env 文件
  pause
  exit /b 1
)

rem bin/ 不入库（见 .gitignore），首次使用先编译
if not exist bin\codearts2api.exe (
  echo [build] 未找到 bin\codearts2api.exe，正在编译...
  if not exist bin mkdir bin
  go build -o bin\codearts2api.exe ./cmd/server
  if errorlevel 1 (
    echo [ERROR] 编译失败：请确认已安装 Go，并在项目根目录运行
    pause
    exit /b 1
  )
)

echo Starting codearts2api on http://127.0.0.1:7866 ...
echo WebUI: http://127.0.0.1:7866/   API Key: %CA2A_API_KEY%
echo.
bin\codearts2api.exe -config config.json
pause
