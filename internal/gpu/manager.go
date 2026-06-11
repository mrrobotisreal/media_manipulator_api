// Package gpu wraps the existing process-wide scheduler in
// internal/services with persistence (mm_gpu_devices, mm_gpu_jobs) and
// Prometheus instrumentation. Existing call sites continue to use the
// services scheduler directly; this package provides a higher-level
// Acquire/Release pair for new call sites that need telemetry.
package gpu

import (
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
	"github.com/mrrobotisreal/media_manipulator_api/internal/logger"
	"github.com/mrrobotisreal/media_manipulator_api/internal/metrics"
	"github.com/mrrobotisreal/media_manipulator_api/internal/telemetry"
)

// TaskType is one of the modeled GPU workloads.
type TaskType string

const (
	TaskWhisper    TaskType = "whisper"
	TaskRealESRGAN TaskType = "realesrgan"
	TaskVLM        TaskType = "vlm"
	TaskOllama     TaskType = "ollama"
	TaskRembg      TaskType = "rembg"
	TaskDemucs     TaskType = "demucs"
	TaskDeepFilter TaskType = "deepfilter"
	// TaskRestoreSR covers the PyTorch video-restoration models (SwinIR, HAT,
	// BasicVSR++, RVRT, VRT); realesrgan-ncnn runs keep using TaskRealESRGAN.
	TaskRestoreSR TaskType = "restore_sr"
	TaskOther     TaskType = "other"
)

// Device represents a single GPU known to the scheduler.
type Device struct {
	Key           string // e.g. cuda:0
	Backend       string // cuda | vulkan | ollama | cpu | unknown
	Index         int
	PCIBusID      string
	Name          string
	TotalMemoryMB int64
	FreeMemoryMB  int64
}

// Manager wraps the lower-level scheduler with persistence + metrics.
type Manager struct {
	Cfg     *config.Config
	Store   *telemetry.Store
	Metrics *metrics.Registry
	Logger  *slog.Logger

	mu      sync.Mutex
	devices map[string]Device
}

// NewManager constructs the manager and persists discovered devices.
func NewManager(cfg *config.Config, store *telemetry.Store, m *metrics.Registry, l *slog.Logger) *Manager {
	if l == nil {
		l = slog.Default()
	}
	mgr := &Manager{Cfg: cfg, Store: store, Metrics: m, Logger: l, devices: map[string]Device{}}
	mgr.discover()
	return mgr
}

// Enabled reports whether the manager has any devices configured.
func (m *Manager) Enabled() bool { return m != nil && m.Cfg.GPUSchedulerEnabled && len(m.devices) > 0 }

// Devices returns a snapshot of discovered devices.
func (m *Manager) Devices() []Device {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Device, 0, len(m.devices))
	for _, d := range m.devices {
		out = append(out, d)
	}
	return out
}

// Lease represents one acquired GPU slot. Callers must Release exactly once.
type Lease struct {
	manager   *Manager
	device    Device
	task      TaskType
	tool      string
	jobID     string
	requestID string
	acquired  time.Time
	released  bool
	auditID   string
}

// Acquire blocks until a device matching task constraints is available.
// When the manager is disabled, it returns a Lease bound to a synthetic
// "cpu" device so callers can keep their code path.
func (m *Manager) Acquire(ctx context.Context, task TaskType, tool, jobID, requestID string) (*Lease, error) {
	if m == nil || !m.Cfg.GPUSchedulerEnabled {
		return &Lease{manager: m, task: task, tool: tool, jobID: jobID, requestID: requestID, acquired: time.Now()}, nil
	}
	// Simplified leasing — we currently rely on the OS + nvidia-smi probe
	// in services.GPUScheduler to do actual capacity checking. This
	// manager exists to write rows / record metrics. Pick the operator
	// default device for the task type.
	dev := m.pickDevice(task)
	lease := &Lease{
		manager:   m,
		device:    dev,
		task:      task,
		tool:      tool,
		jobID:     jobID,
		requestID: requestID,
		acquired:  time.Now(),
	}
	if m.Store != nil {
		acquiredAt := lease.acquired
		auditID := m.Store.InsertGPUJob(ctx, telemetry.GPUJobInsert{
			JobID:        jobID,
			RequestID:    requestID,
			Tool:         tool,
			TaskType:     string(task),
			SchedulerKey: dev.Key,
			AcquiredAt:   &acquiredAt,
			Status:       "running",
		})
		lease.auditID = auditID
	}
	logger.FromContext(ctx).Info("gpu acquired", "task", string(task), "device", dev.Key, "tool", tool)
	return lease, nil
}

// Device returns the device assigned to this lease.
func (l *Lease) Device() Device { return l.device }

// Release records the run on Prometheus + Postgres.
func (l *Lease) Release(ctx context.Context, err error) {
	if l == nil || l.released {
		return
	}
	l.released = true
	released := time.Now()
	runMS := int(released.Sub(l.acquired) / time.Millisecond)
	status := "completed"
	if err != nil {
		status = "failed"
	}
	if l.manager != nil {
		if l.manager.Metrics != nil {
			l.manager.Metrics.GPURun(string(l.task), l.device.Key, released.Sub(l.acquired))
		}
		if l.manager.Store != nil {
			errMsg := ""
			if err != nil {
				errMsg = err.Error()
			}
			l.manager.Store.InsertGPUJob(ctx, telemetry.GPUJobInsert{
				JobID:        l.jobID,
				RequestID:    l.requestID,
				Tool:         l.tool,
				TaskType:     string(l.task),
				SchedulerKey: l.device.Key,
				AcquiredAt:   &l.acquired,
				ReleasedAt:   &released,
				RunMS:        runMS,
				Status:       status,
				ErrorMessage: errMsg,
			})
		}
		logger.FromContext(ctx).Info("gpu released",
			"task", string(l.task), "device", l.device.Key, "tool", l.tool,
			"runMs", runMS, "status", status)
	}
}

// --- device discovery ------------------------------------------------------

func (m *Manager) discover() {
	if !m.Cfg.GPUSchedulerEnabled {
		return
	}
	added := 0
	for _, raw := range m.Cfg.GPUSchedulerDevices {
		dev, ok := parseDeviceSpec(raw)
		if !ok {
			m.Logger.Warn("ignoring malformed GPU_SCHEDULER_DEVICES entry", "raw", raw)
			continue
		}
		m.devices[dev.Key] = dev
		added++
	}
	if added == 0 {
		for _, dev := range probeNvidiaSmi() {
			m.devices[dev.Key] = dev
			added++
		}
	}
	if added == 0 {
		// Fall back to operator-configured indexes from existing env.
		if m.Cfg.AICUDAGPU >= 0 {
			d := Device{
				Key:     fmt.Sprintf("cuda:%d", m.Cfg.AICUDAGPU),
				Backend: "cuda",
				Index:   m.Cfg.AICUDAGPU,
			}
			m.devices[d.Key] = d
			added++
		}
		if m.Cfg.AIVulkanGPU >= 0 {
			d := Device{
				Key:     fmt.Sprintf("vulkan:%d", m.Cfg.AIVulkanGPU),
				Backend: "vulkan",
				Index:   m.Cfg.AIVulkanGPU,
			}
			m.devices[d.Key] = d
			added++
		}
	}
	for _, d := range m.devices {
		if m.Store != nil {
			m.Store.UpsertGPUDevice(context.Background(), telemetry.GPUDeviceUpsert{
				SchedulerKey:  d.Key,
				Backend:       d.Backend,
				DeviceIndex:   d.Index,
				PCIBusID:      d.PCIBusID,
				Name:          d.Name,
				TotalMemoryMB: d.TotalMemoryMB,
				FreeMemoryMB:  d.FreeMemoryMB,
			})
		}
	}
	m.Logger.Info("gpu manager discovered devices", "count", added)
}

// pickDevice chooses a sensible default device per task type, prefering
// operator overrides.
func (m *Manager) pickDevice(task TaskType) Device {
	pref := ""
	switch task {
	case TaskWhisper:
		pref = m.Cfg.GPUSchedulerDefaultWhisperDevice
	case TaskRealESRGAN:
		pref = m.Cfg.GPUSchedulerDefaultRealESRGANDevice
		if pref == "" {
			pref = fmt.Sprintf("vulkan:%d", m.Cfg.AIVulkanGPU)
		}
	case TaskVLM, TaskOllama:
		pref = m.Cfg.GPUSchedulerDefaultVLMDevice
	case TaskRestoreSR:
		// PyTorch restoration models are pinned to the big-VRAM card.
		pref = fmt.Sprintf("cuda:%d", m.Cfg.AIRestoreCUDAGPU)
	}
	if pref != "" {
		if d, ok := m.devices[pref]; ok {
			return d
		}
	}
	// Fall back to first CUDA device with the most memory.
	var best Device
	for _, d := range m.devices {
		if d.Backend == "cuda" && d.TotalMemoryMB >= best.TotalMemoryMB {
			best = d
		}
	}
	if best.Key != "" {
		return best
	}
	for _, d := range m.devices {
		return d
	}
	return Device{Key: "cpu:0", Backend: "cpu"}
}

// parseDeviceSpec parses "cuda:0:RTX5060Ti:8192" → Device.
// Format: backend:index[:name[:memMB]].
func parseDeviceSpec(raw string) (Device, bool) {
	parts := strings.Split(raw, ":")
	if len(parts) < 2 {
		return Device{}, false
	}
	backend := strings.ToLower(strings.TrimSpace(parts[0]))
	idx, err := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err != nil {
		return Device{}, false
	}
	d := Device{
		Key:     backend + ":" + strconv.Itoa(idx),
		Backend: backend,
		Index:   idx,
	}
	if len(parts) >= 3 {
		d.Name = strings.TrimSpace(parts[2])
	}
	if len(parts) >= 4 {
		if m, err := strconv.ParseInt(strings.TrimSpace(parts[3]), 10, 64); err == nil {
			d.TotalMemoryMB = m
		}
	}
	return d, true
}

// probeNvidiaSmi shells out to nvidia-smi for device discovery. Failures
// (no GPU host, no nvidia-smi installed) are non-fatal.
func probeNvidiaSmi() []Device {
	if _, err := exec.LookPath("nvidia-smi"); err != nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "nvidia-smi",
		"--query-gpu=index,name,pci.bus_id,memory.total,memory.free",
		"--format=csv,noheader,nounits")
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	var devices []Device
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		parts := strings.Split(line, ",")
		if len(parts) < 5 {
			continue
		}
		idx, _ := strconv.Atoi(strings.TrimSpace(parts[0]))
		total, _ := strconv.ParseInt(strings.TrimSpace(parts[3]), 10, 64)
		free, _ := strconv.ParseInt(strings.TrimSpace(parts[4]), 10, 64)
		devices = append(devices, Device{
			Key:           "cuda:" + strconv.Itoa(idx),
			Backend:       "cuda",
			Index:         idx,
			Name:          strings.TrimSpace(parts[1]),
			PCIBusID:      strings.TrimSpace(parts[2]),
			TotalMemoryMB: total,
			FreeMemoryMB:  free,
		})
	}
	return devices
}
