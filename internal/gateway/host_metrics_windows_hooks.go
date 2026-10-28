//go:build windows

package gateway

// Windows exposes thermal data only through WMI (MSAcpi_ThermalZoneTemperature),
// which most desktops and every Hyper-V / cloud VM leave unimplemented; the
// seconds spent querying it on each poll would buy nothing. These hooks keep
// the shared collector identical on both gopsutil platforms.

func applyGopsutilThermal(m *HostMetrics)   {}
func applyGopsutilDiskTemps(m *HostMetrics) {}
func applyGopsutilFans(m *HostMetrics)      {}
func applyGopsutilRestarts(m *HostMetrics)  {}
