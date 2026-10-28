//go:build darwin

package gateway

// applyGopsutilThermal reads °C from IOKit and, when that is unavailable,
// leaves the thermal pressure level (powermetrics) for the page to show.
func applyGopsutilThermal(m *HostMetrics) {
	if temp, source, ok := darwinCPUTempC(); ok {
		m.TempC = temp
		m.TempAvailable = true
		m.TempSource = source
	}
	if level, source, ok := darwinThermalPressure(); ok {
		m.ThermalPressure = level
		m.ThermalPressureAvailable = true
		m.ThermalPressureSource = source
	}
}

// applyGopsutilDiskTemps reads drive temperatures from SMART / NVMe.
func applyGopsutilDiskTemps(m *HostMetrics) { applyDiskTemps(m) }

// applyGopsutilFans reads the SMC fan keys.
func applyGopsutilFans(m *HostMetrics) { applyFanSpeeds(m) }

// applyGopsutilRestarts has no service manager to ask: the desktop app is
// launched by the user, not supervised by launchd or systemd.
func applyGopsutilRestarts(m *HostMetrics) {}
