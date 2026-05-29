package main

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"github.com/getlantern/systray"
	"golang.org/x/sys/windows"
)

const (
	appName = "Lenovo Fan Control Go"

	fanNormal = 0
	fanFast   = 1

	ioctlFanControl = 0x831020C0

	defaultFastMs = 8090
	defaultGapMs  = 10090

	modeNormal = iota
	modePulse
	modeThermal

	thermalTriggerC  = 70.0
	tempPollInterval = time.Minute
)

//go:embed res/icon.ico
var iconData []byte

type config struct {
	FastMs     int `json:"fast_ms"`
	PulseGapMs int `json:"pulse_gap_ms"`
}

type controller struct {
	mu     sync.RWMutex
	fastMs int
	gapMs  int
	mode   int
	stopCh chan struct{}
	tempC  float64
	tempOK bool
}

func newController(cfg config) *controller {
	fastMs := cfg.FastMs
	if fastMs <= 0 {
		fastMs = defaultFastMs
	}
	gapMs := cfg.PulseGapMs
	if gapMs < 0 {
		gapMs = defaultGapMs
	}
	return &controller{
		fastMs: fastMs,
		gapMs:  gapMs,
	}
}

func (c *controller) startPulse() {
	c.mu.Lock()
	if c.mode == modePulse {
		c.mu.Unlock()
		return
	}
	c.stopLocked()
	stopCh := make(chan struct{})
	c.stopCh = stopCh
	c.mode = modePulse
	c.mu.Unlock()

	go c.pulseLoop(stopCh)
}

func (c *controller) startThermal() {
	c.mu.Lock()
	if c.mode == modeThermal {
		c.mu.Unlock()
		return
	}
	c.stopLocked()
	stopCh := make(chan struct{})
	c.stopCh = stopCh
	c.mode = modeThermal
	c.mu.Unlock()

	go c.thermalLoop(stopCh)
}

func (c *controller) stopControl() {
	c.mu.Lock()
	c.stopLocked()
	c.mu.Unlock()
	_ = fanControl(fanNormal)
}

func (c *controller) stopLocked() {
	if c.stopCh != nil {
		close(c.stopCh)
		c.stopCh = nil
	}
	c.mode = modeNormal
}

func (c *controller) setGap(gapMs int) {
	c.mu.Lock()
	c.gapMs = gapMs
	fastMs := c.fastMs
	c.mu.Unlock()

	_ = saveConfig(config{FastMs: fastMs, PulseGapMs: gapMs})
}

func (c *controller) setTemp(tempC float64, ok bool) {
	c.mu.Lock()
	c.tempC = tempC
	c.tempOK = ok
	c.mu.Unlock()
}

func (c *controller) state() (fastMs int, gapMs int, mode int, tempC float64, tempOK bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.fastMs, c.gapMs, c.mode, c.tempC, c.tempOK
}

func (c *controller) pulseLoop(stopCh <-chan struct{}) {
	defer fanControl(fanNormal)

	for {
		if shouldStop(stopCh) {
			return
		}
		_ = fanControl(fanFast)

		fastMs, gapMs, _, _, _ := c.state()
		if waitOrStop(stopCh, time.Duration(fastMs)*time.Millisecond) {
			return
		}

		_ = fanControl(fanNormal)
		if waitOrStop(stopCh, time.Duration(gapMs)*time.Millisecond) {
			return
		}
	}
}

func (c *controller) thermalLoop(stopCh <-chan struct{}) {
	defer fanControl(fanNormal)

	for {
		if shouldStop(stopCh) {
			return
		}

		tempC, ok := readTemperatureC()
		c.setTemp(tempC, ok)

		if ok && tempC >= thermalTriggerC {
			_ = fanControl(fanFast)
			if waitOrStop(stopCh, time.Duration(defaultFastMs)*time.Millisecond) {
				return
			}
			_ = fanControl(fanNormal)
		}

		if waitOrStop(stopCh, tempPollInterval) {
			return
		}
	}
}

func shouldStop(stopCh <-chan struct{}) bool {
	select {
	case <-stopCh:
		return true
	default:
		return false
	}
}

func waitOrStop(stopCh <-chan struct{}, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-stopCh:
		return true
	case <-timer.C:
		return false
	}
}

func fanControl(mode uint32) error {
	handle, err := windows.CreateFile(
		windows.StringToUTF16Ptr(`\\.\EnergyDrv`),
		windows.GENERIC_WRITE,
		0,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL,
		0,
	)
	if err != nil {
		return fmt.Errorf("open EnergyDrv failed: %w", err)
	}
	defer windows.CloseHandle(handle)

	// 与原 C 版保持一致：[6, 1, mode] 写入联想电源驱动。
	in := [3]uint32{6, 1, mode}
	var bytesReturned uint32
	err = windows.DeviceIoControl(
		handle,
		ioctlFanControl,
		(*byte)(unsafe.Pointer(&in[0])),
		uint32(unsafe.Sizeof(in)),
		nil,
		0,
		&bytesReturned,
		nil,
	)
	if err != nil {
		return fmt.Errorf("DeviceIoControl failed: %w", err)
	}
	return nil
}

func readTemperatureC() (float64, bool) {
	cmd := exec.Command(
		"powershell",
		"-NoProfile",
		"-ExecutionPolicy", "Bypass",
		"-Command",
		"$v=(Get-CimInstance -ClassName Win32_PerfFormattedData_Counters_ThermalZoneInformation -ErrorAction SilentlyContinue | Select-Object -First 1 -ExpandProperty Temperature); if ($null -ne $v) { [math]::Round(($v - 273.15), 1) }",
	)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	out, err := cmd.Output()
	if err != nil {
		return 0, false
	}
	text := strings.TrimSpace(string(out))
	if text == "" {
		return 0, false
	}
	text = strings.ReplaceAll(text, ",", ".")
	tempC, err := strconv.ParseFloat(text, 64)
	if err != nil {
		return 0, false
	}
	return tempC, true
}

func configPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	dir = filepath.Join(dir, "LenovoFanControlGo")
	return filepath.Join(dir, "config.json"), nil
}

func loadConfig() config {
	path, err := configPath()
	if err != nil {
		return config{FastMs: defaultFastMs, PulseGapMs: defaultGapMs}
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return config{FastMs: defaultFastMs, PulseGapMs: defaultGapMs}
	}
	if err != nil {
		return config{FastMs: defaultFastMs, PulseGapMs: defaultGapMs}
	}
	var cfg config
	if json.Unmarshal(data, &cfg) != nil {
		return config{FastMs: defaultFastMs, PulseGapMs: defaultGapMs}
	}
	if cfg.FastMs <= 0 {
		cfg.FastMs = defaultFastMs
	}
	if cfg.PulseGapMs < 0 {
		cfg.PulseGapMs = defaultGapMs
	}
	return cfg
}

func saveConfig(cfg config) error {
	path, err := configPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

type gapPreset struct {
	title string
	value int
	item  *systray.MenuItem
}

func main() {
	cfg := loadConfig()
	ctrl := newController(cfg)
	systray.Run(func() { onReady(ctrl) }, func() { ctrl.stopControl() })
}

func onReady(ctrl *controller) {
	systray.SetIcon(iconData)
	systray.SetTitle(appName)
	systray.SetTooltip(appName)

	status := systray.AddMenuItem("状态：脉冲模式", "当前风扇控制状态")
	status.Disable()

	mPulse := systray.AddMenuItem("脉冲模式", "高转 8090 毫秒 / 正常停顿 循环")
	mThermal := systray.AddMenuItem("温控模式", "温度 >= 70°C 时高转 8090 毫秒，然后恢复正常")
	mNormal := systray.AddMenuItem("正常模式", "停止脉冲并恢复正常控制")
	systray.AddSeparator()

	presets := []*gapPreset{
		{title: "正常停顿 2000 毫秒", value: 2000},
		{title: "正常停顿 10090 毫秒", value: 10090},
		{title: "正常停顿 30000 毫秒", value: 30000},
	}
	for _, preset := range presets {
		preset.item = systray.AddMenuItem(preset.title, "设置正常转速停顿时间")
	}

	systray.AddSeparator()
	mQuit := systray.AddMenuItem("退出", "退出并恢复正常控制")

	updateTemperature(ctrl)
	updateMenu(ctrl, status, mPulse, mThermal, mNormal, presets)
	ctrl.startThermal()
	updateMenu(ctrl, status, mPulse, mThermal, mNormal, presets)

	go func() {
		ticker := time.NewTicker(tempPollInterval)
		defer ticker.Stop()
		for range ticker.C {
			updateTemperature(ctrl)
			updateMenu(ctrl, status, mPulse, mThermal, mNormal, presets)
		}
	}()

	go func() {
		for {
			select {
			case <-mPulse.ClickedCh:
				ctrl.startPulse()
				updateMenu(ctrl, status, mPulse, mThermal, mNormal, presets)

			case <-mThermal.ClickedCh:
				ctrl.startThermal()
				updateMenu(ctrl, status, mPulse, mThermal, mNormal, presets)

			case <-mNormal.ClickedCh:
				ctrl.stopControl()
				updateMenu(ctrl, status, mPulse, mThermal, mNormal, presets)

			case <-mQuit.ClickedCh:
				ctrl.stopControl()
				systray.Quit()
				return
			}
		}
	}()

	for _, preset := range presets {
		p := preset
		go func() {
			for range p.item.ClickedCh {
				ctrl.setGap(p.value)
				updateMenu(ctrl, status, mPulse, mThermal, mNormal, presets)
			}
		}()
	}
}

func updateTemperature(ctrl *controller) {
	tempC, ok := readTemperatureC()
	ctrl.setTemp(tempC, ok)
}

func updateMenu(ctrl *controller, status, mPulse, mThermal, mNormal *systray.MenuItem, presets []*gapPreset) {
	fastMs, gapMs, mode, tempC, tempOK := ctrl.state()
	tempText := "温度：读取失败"
	if tempOK {
		tempText = fmt.Sprintf("温度：%.1f°C", tempC)
	}
	systray.SetTooltip(fmt.Sprintf("%s\n%s", appName, tempText))

	switch mode {
	case modePulse:
		status.SetTitle(fmt.Sprintf("状态：脉冲模式 | %s | 高转 %d 毫秒 / 正常停顿 %d 毫秒", tempText, fastMs, gapMs))
		mPulse.Check()
		mThermal.Uncheck()
		mNormal.Uncheck()
	case modeThermal:
		status.SetTitle(fmt.Sprintf("状态：温控模式 | %s | 70°C 以上高转 %d 毫秒", tempText, defaultFastMs))
		mPulse.Uncheck()
		mThermal.Check()
		mNormal.Uncheck()
	default:
		status.SetTitle(fmt.Sprintf("状态：正常模式 | %s", tempText))
		mPulse.Uncheck()
		mThermal.Uncheck()
		mNormal.Check()
	}

	for _, preset := range presets {
		if preset.value == gapMs {
			preset.item.Check()
		} else {
			preset.item.Uncheck()
		}
	}
}
