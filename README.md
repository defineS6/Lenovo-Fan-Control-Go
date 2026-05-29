# Lenovo Fan Control Go

一个用于联想笔记本的 Windows 托盘风扇控制小工具。

本项目通过 `\\.\EnergyDrv` 调用 `Lenovo ACPI-Compliant Virtual Power Controller` 驱动，触发联想风扇除尘/高转动作。默认启动为温控模式。

## 功能

- 托盘运行，退出时恢复正常风扇控制
- 显示 Windows Thermal Zone 温度
- 温控模式：每 1 分钟读取一次温度，温度 `>= 70°C` 时高转 `8090ms` 后恢复正常
- 脉冲模式：按 `高转 8090ms / 正常停顿 PulseGapMs` 循环
- `PulseGapMs` 预设：`2000`、`10090`、`30000`

## 构建

```powershell
go build -ldflags "-H windowsgui" -o bin\LenovoFanControlGo.exe .
```

控制台排错版本：

```powershell
go build -o bin\LenovoFanControlGo-console.exe .
```

## 说明

使用前请确认系统已安装 `Lenovo ACPI-Compliant Virtual Power Controller` 驱动。不同机型的 EC/固件行为不同，风扇动作可能不是连续满速。

使用风险自担。
