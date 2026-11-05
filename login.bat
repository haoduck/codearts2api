@echo off
chcp 65001 >nul
cd /d %~dp0

rem 本机登录（自动打开浏览器）
rem 服务器/无浏览器场景：在本行末尾加 -print-only 参数

echo === CodeArts Agent 登录 ===
echo 凭证将保存到 .\auths\codearts-{user_id}.json
echo.
rem bin/ 不入库（见 .gitignore），首次使用先编译
if not exist bin\codearts2api-login.exe (
  echo [build] 未找到 bin\codearts2api-login.exe，正在编译...
  if not exist bin mkdir bin
  go build -o bin\codearts2api-login.exe ./cmd/login
  if errorlevel 1 (
    echo [ERROR] 编译失败：请确认已安装 Go，并在项目根目录运行
    pause
    exit /b 1
  )
)

bin\codearts2api-login.exe -auth-dir ./auths %*
pause
