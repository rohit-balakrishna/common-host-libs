// Copyright 2026 Hewlett Packard Enterprise Development LP

package linux

import (
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/hpe-storage/common-host-libs/model"
)

// fakeFileInfo is a minimal os.FileInfo whose only meaningful field is Name(),
// which is all GetNvmeDeviceFromNamespace consumes when iterating /dev entries.
type fakeFileInfo struct{ name string }

func (f fakeFileInfo) Name() string       { return f.name }
func (f fakeFileInfo) Size() int64        { return 0 }
func (f fakeFileInfo) Mode() os.FileMode  { return 0 }
func (f fakeFileInfo) ModTime() time.Time { return time.Time{} }
func (f fakeFileInfo) IsDir() bool        { return false }
func (f fakeFileInfo) Sys() any           { return nil }

// TestGetNvmeDeviceFromNamespace_SkipsUnreadableNguid is the ESC-17833 regression:
// the first NVMe namespace (e.g. a local boot drive like nvme0n1) has no readable
// nguid, and the target NVMe/TCP volume is on a later device (nvme1n1). Discovery
// must skip the unreadable device and still find the target instead of aborting.
func TestGetNvmeDeviceFromNamespace_SkipsUnreadableNguid(t *testing.T) {
	origReadDir := nvmeReadDir
	origReadNguid := readNvmeNamespaceNguid
	defer func() {
		nvmeReadDir = origReadDir
		readNvmeNamespaceNguid = origReadNguid
	}()

	const targetSerial = "60002ac00000003e0002ac940002b10c"

	nvmeReadDir = func(string) ([]os.FileInfo, error) {
		return []os.FileInfo{
			fakeFileInfo{name: "nvme0n1"}, // local boot drive: nguid read fails
			fakeFileInfo{name: "nvme1n1"}, // attached NVMe/TCP volume: matches
		}, nil
	}
	readNvmeNamespaceNguid = func(deviceName string) (string, error) {
		if deviceName == "nvme0n1" {
			return "", fmt.Errorf("open /sys/class/block/nvme0n1/subsystem/nvme0n1/nguid: no such file or directory")
		}
		// Return the (dashed) sysfs form; the function normalizes it.
		return "60002ac0-0000-003e-0002-ac940002b10c", nil
	}

	dev, err := GetNvmeDeviceFromNamespace(targetSerial)
	if err != nil {
		t.Fatalf("expected to find device on nvme1n1 despite nvme0n1 nguid failure, got error: %v", err)
	}
	if dev == nil {
		t.Fatalf("expected a device, got nil")
	}
	if dev.SerialNumber != targetSerial {
		t.Fatalf("expected serial %s, got %s", targetSerial, dev.SerialNumber)
	}
	if dev.Pathname != "/dev/nvme1n1" {
		t.Fatalf("expected /dev/nvme1n1, got %s", dev.Pathname)
	}
}

// TestGetNvmeDeviceFromNamespace_NotFound verifies a clean not-found error when no
// device matches (and one namespace has an unreadable nguid).
func TestGetNvmeDeviceFromNamespace_NotFound(t *testing.T) {
	origReadDir := nvmeReadDir
	origReadNguid := readNvmeNamespaceNguid
	defer func() {
		nvmeReadDir = origReadDir
		readNvmeNamespaceNguid = origReadNguid
	}()

	nvmeReadDir = func(string) ([]os.FileInfo, error) {
		return []os.FileInfo{
			fakeFileInfo{name: "nvme0n1"},
			fakeFileInfo{name: "nvme1n1"},
		}, nil
	}
	readNvmeNamespaceNguid = func(deviceName string) (string, error) {
		if deviceName == "nvme0n1" {
			return "", fmt.Errorf("no such file or directory")
		}
		return "aaaa-bbbb", nil
	}

	dev, err := GetNvmeDeviceFromNamespace("does-not-match")
	if err == nil {
		t.Fatalf("expected not-found error, got device %+v", dev)
	}
	if dev != nil {
		t.Fatalf("expected nil device, got %+v", dev)
	}
}

func setNvmeOptimizationTestSeams(t *testing.T) {
	t.Helper()
	origReadDir := nvmeControllerReadDir
	origReadFirstLine := nvmeControllerReadFirstLine
	origExec := nvmeExecCommandOutput
	origFindDevices := nvmeFindDevices
	origApplyTuning := nvmeApplyTcpTuning
	t.Cleanup(func() {
		nvmeControllerReadDir = origReadDir
		nvmeControllerReadFirstLine = origReadFirstLine
		nvmeExecCommandOutput = origExec
		nvmeFindDevices = origFindDevices
		nvmeApplyTcpTuning = origApplyTuning
	})
}

func TestGetLiveNvmeControllersForNQN(t *testing.T) {
	setNvmeOptimizationTestSeams(t)
	nvmeControllerReadDir = func(string) ([]os.FileInfo, error) {
		return []os.FileInfo{
			fakeFileInfo{name: "nvme0"},
			fakeFileInfo{name: "nvme1"},
			fakeFileInfo{name: "nvme2"},
		}, nil
	}
	nvmeControllerReadFirstLine = func(path string) (string, error) {
		values := map[string]string{
			"/sys/class/nvme/nvme0/subsysnqn": "nqn.target",
			"/sys/class/nvme/nvme0/state":     "live",
			"/sys/class/nvme/nvme0/address":   "traddr=10.0.0.1,trsvcid=4420",
			"/sys/class/nvme/nvme1/subsysnqn": "nqn.target",
			"/sys/class/nvme/nvme1/state":     "connecting",
			"/sys/class/nvme/nvme1/address":   "traddr=10.0.0.2,trsvcid=4420",
			"/sys/class/nvme/nvme2/subsysnqn": "nqn.other",
		}
		value, ok := values[path]
		if !ok {
			return "", fmt.Errorf("unexpected path %s", path)
		}
		return value, nil
	}

	addrs, err := getLiveNvmeControllersForNQN("nqn.target")
	if err != nil {
		t.Fatalf("getLiveNvmeControllersForNQN() error = %v", err)
	}
	if len(addrs) != 1 || !addrs["10.0.0.1"] {
		t.Fatalf("live addresses = %v, want only 10.0.0.1", addrs)
	}
}

func TestGetLiveNvmeControllersForNQNReadDirError(t *testing.T) {
	setNvmeOptimizationTestSeams(t)
	nvmeControllerReadDir = func(string) ([]os.FileInfo, error) {
		return nil, fmt.Errorf("sysfs unavailable")
	}

	addrs, err := getLiveNvmeControllersForNQN("nqn.target")
	if err == nil {
		t.Fatal("getLiveNvmeControllersForNQN() error = nil, want error")
	}
	if len(addrs) != 0 {
		t.Fatalf("live addresses = %v, want empty", addrs)
	}
}

func TestAllTargetPortalsLive(t *testing.T) {
	volume := &model.Volume{TargetAddress: " 10.0.0.1,10.0.0.2 "}
	if !allTargetPortalsLive(volume, map[string]bool{"10.0.0.1": true, "10.0.0.2": true}) {
		t.Fatal("allTargetPortalsLive() = false, want true")
	}
	if allTargetPortalsLive(volume, map[string]bool{"10.0.0.1": true}) {
		t.Fatal("allTargetPortalsLive() = true with missing portal, want false")
	}
}

// TestConnectNvmeTargetConnectsAllPortalsWhenNoneLive passes an empty liveAddrs,
// so ConnectNvmeTarget must attempt every portal. Deterministic on any host,
// real NVMe or none, since liveAddrs is now supplied by the caller rather than
// scanned from /sys/class/nvme internally.
func TestConnectNvmeTargetConnectsAllPortalsWhenNoneLive(t *testing.T) {
	originalExec := nvmeExecCommandOutput
	t.Cleanup(func() { nvmeExecCommandOutput = originalExec })
	var connectedAddresses []string
	nvmeExecCommandOutput = func(_ string, args []string) (string, int, error) {
		connectedAddresses = append(connectedAddresses, args[6])
		return "", 0, nil
	}

	err := ConnectNvmeTarget(&model.NvmeTarget{
		NQN:     "nqn.test-only.no-such-subsystem",
		Address: "10.0.0.1,10.0.0.2",
		Port:    "4420",
	}, nil)
	if err != nil {
		t.Fatalf("ConnectNvmeTarget() error = %v", err)
	}
	if fmt.Sprint(connectedAddresses) != "[10.0.0.1 10.0.0.2]" {
		t.Fatalf("connected addresses = %v, want both portals attempted", connectedAddresses)
	}
}

// TestConnectNvmeTargetSkipsConnectWhenAllPortalsLive verifies that, given a
// liveAddrs set already covering every portal, ConnectNvmeTarget returns
// success without executing a single "nvme connect".
func TestConnectNvmeTargetSkipsConnectWhenAllPortalsLive(t *testing.T) {
	originalExec := nvmeExecCommandOutput
	t.Cleanup(func() { nvmeExecCommandOutput = originalExec })
	execCount := 0
	nvmeExecCommandOutput = func(_ string, _ []string) (string, int, error) {
		execCount++
		return "", 0, nil
	}

	err := ConnectNvmeTarget(&model.NvmeTarget{
		NQN:     "nqn.test-only.all-live",
		Address: "10.0.0.1,10.0.0.2",
		Port:    "4420",
	}, map[string]bool{"10.0.0.1": true, "10.0.0.2": true})
	if err != nil {
		t.Fatalf("ConnectNvmeTarget() error = %v, want success when all portals already live", err)
	}
	if execCount != 0 {
		t.Fatalf("nvme connect exec count = %d, want 0 when all portals already live", execCount)
	}
}

// TestConnectNvmeTargetRejectsEmptyAddress ensures an empty/blank target
// address fails fast with a clear error instead of attempting "nvme connect"
// with a blank -a argument.
func TestConnectNvmeTargetRejectsEmptyAddress(t *testing.T) {
	originalExec := nvmeExecCommandOutput
	t.Cleanup(func() { nvmeExecCommandOutput = originalExec })
	execCount := 0
	nvmeExecCommandOutput = func(_ string, _ []string) (string, int, error) {
		execCount++
		return "", 0, nil
	}

	err := ConnectNvmeTarget(&model.NvmeTarget{NQN: "nqn.test-only.empty-address", Address: "  ", Port: "4420"}, nil)
	if err == nil {
		t.Fatal("ConnectNvmeTarget() error = nil, want error for empty target address")
	}
	if execCount != 0 {
		t.Fatalf("nvme connect exec count = %d, want 0 for empty target address", execCount)
	}
}

// TestConnectNvmeTargetDedupesPortals ensures a repeated IP in target.Address
// (e.g. a trailing comma or literal duplicate) is only attempted once.
func TestConnectNvmeTargetDedupesPortals(t *testing.T) {
	originalExec := nvmeExecCommandOutput
	t.Cleanup(func() { nvmeExecCommandOutput = originalExec })
	var attempted []string
	nvmeExecCommandOutput = func(_ string, args []string) (string, int, error) {
		attempted = append(attempted, args[6])
		return "", 0, nil
	}

	err := ConnectNvmeTarget(&model.NvmeTarget{NQN: "nqn.test-only.dup", Address: "10.0.0.1,10.0.0.1,", Port: "4420"}, nil)
	if err != nil {
		t.Fatalf("ConnectNvmeTarget() error = %v", err)
	}
	if fmt.Sprint(attempted) != "[10.0.0.1]" {
		t.Fatalf("attempted portals = %v, want a single deduped 10.0.0.1", attempted)
	}
}

// TestConnectNvmeTargetPassesCtrlLossTmo asserts that every "nvme connect"
// carries the default "-l 1800" so a lost I/O controller keeps retrying and
// self-reattaches after an array outage longer than the 600s kernel default (CON-4387).
func TestConnectNvmeTargetPassesCtrlLossTmo(t *testing.T) {
	t.Setenv(envNvmeCtrlLossTmo, "")
	originalExec := nvmeExecCommandOutput
	t.Cleanup(func() { nvmeExecCommandOutput = originalExec })
	var capturedArgs []string
	nvmeExecCommandOutput = func(_ string, args []string) (string, int, error) {
		capturedArgs = args
		return "", 0, nil
	}

	err := ConnectNvmeTarget(&model.NvmeTarget{NQN: "nqn.test-only.ctrl-loss", Address: "10.0.0.1", Port: "4420"}, nil)
	if err != nil {
		t.Fatalf("ConnectNvmeTarget() error = %v", err)
	}

	found := false
	for i, a := range capturedArgs {
		if a == "-l" {
			if i+1 >= len(capturedArgs) || capturedArgs[i+1] != defaultNvmeCtrlLossTmo {
				t.Fatalf("connect args = %v, want -l followed by %s", capturedArgs, defaultNvmeCtrlLossTmo)
			}
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("connect args = %v, want an explicit -l (ctrl-loss-tmo)", capturedArgs)
	}
}

// TestConnectNvmeTargetHonorsCtrlLossTmoEnv asserts NVME_CTRL_LOSS_TMO overrides
// the -l value passed to "nvme connect".
func TestConnectNvmeTargetHonorsCtrlLossTmoEnv(t *testing.T) {
	t.Setenv(envNvmeCtrlLossTmo, "1800")
	originalExec := nvmeExecCommandOutput
	t.Cleanup(func() { nvmeExecCommandOutput = originalExec })
	var capturedArgs []string
	nvmeExecCommandOutput = func(_ string, args []string) (string, int, error) {
		capturedArgs = args
		return "", 0, nil
	}

	if err := ConnectNvmeTarget(&model.NvmeTarget{NQN: "nqn.test-only.env", Address: "10.0.0.1", Port: "4420"}, nil); err != nil {
		t.Fatalf("ConnectNvmeTarget() error = %v", err)
	}
	for i, a := range capturedArgs {
		if a == "-l" {
			if i+1 >= len(capturedArgs) || capturedArgs[i+1] != "1800" {
				t.Fatalf("connect args = %v, want -l followed by 1800", capturedArgs)
			}
			return
		}
	}
	t.Fatalf("connect args = %v, want an explicit -l (ctrl-loss-tmo)", capturedArgs)
}

// TestGetNvmeCtrlLossTmo covers the env override, the default fallback, and the
// invalid-value fallback for NVME_CTRL_LOSS_TMO.
func TestGetNvmeCtrlLossTmo(t *testing.T) {
	cases := []struct {
		name string
		env  string
		set  bool
		want string
	}{
		{"unset uses default", "", false, defaultNvmeCtrlLossTmo},
		{"empty uses default", "", true, defaultNvmeCtrlLossTmo},
		{"valid seconds", "1800", true, "1800"},
		{"valid infinite", "-1", true, "-1"},
		{"valid zero", "0", true, "0"},
		{"non-integer falls back", "abc", true, defaultNvmeCtrlLossTmo},
		{"below range falls back", "-2", true, defaultNvmeCtrlLossTmo},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.set {
				t.Setenv(envNvmeCtrlLossTmo, tc.env)
			} else {
				t.Setenv(envNvmeCtrlLossTmo, "")
			}
			if got := getNvmeCtrlLossTmo(); got != tc.want {
				t.Fatalf("getNvmeCtrlLossTmo() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestHandleNvmeTcpDiscoveryNilVolume ensures a nil volume returns an error
// instead of panicking.
func TestHandleNvmeTcpDiscoveryNilVolume(t *testing.T) {
	if err := HandleNvmeTcpDiscovery(nil); err == nil {
		t.Fatal("HandleNvmeTcpDiscovery(nil) error = nil, want error")
	}
}
