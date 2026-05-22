# mm_gpu_devices

## Purpose
Persistent catalog of GPUs known to the scheduler. Refreshed on boot from
`GPU_SCHEDULER_DEVICES` and/or `nvidia-smi`. Provides a stable
`scheduler_device_key` that GPU job rows reference.

## Primary key
`gpu_device_id` (uuid).

## Unique key
`scheduler_device_key` — e.g. `cuda:0`, `vulkan:1`, `ollama:0`.

## Key columns
- `backend` (`cuda|vulkan|ollama|cpu|unknown`).
- `device_index`, `pci_bus_id`, `name`.
- `total_memory_mb`, `free_memory_mb`.
- `capabilities` — `jsonb` (compute caps, supported task types).
- `last_seen_at` — last refresh time.

## Indexes
- PK on `gpu_device_id`.
- Unique on `scheduler_device_key`.
- `(backend)`.

## Writers
- `internal/services/gpu_scheduler` on boot/refresh.

## Readers
- The scheduler itself; ops dashboards.

## Retention
Indefinite.

## Migration history
- `20260520001` — initial creation.
