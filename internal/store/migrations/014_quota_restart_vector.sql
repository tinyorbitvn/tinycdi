-- 014_quota_restart_vector.sql — remember the compute vector of a stopped
-- Retain workspace whose reservation became a disk-only hold.
--
-- Quota model: running slots, CPU and memory are held exactly while a runtime
-- incarnation exists; disk is held while the volume exists. When a Retain
-- workspace stops and the pod is proven gone, its reservation row stays held
-- for the disk but its compute columns are zeroed. The three nullable columns
-- below keep the vector the workspace was admitted with so a start can
-- re-acquire exactly it. NULL means "no stored vector" (every other row).
-- Additive and replayable; no backfill.
ALTER TABLE quota_reservation
    ADD COLUMN IF NOT EXISTS restart_slots        bigint CHECK (restart_slots >= 0),
    ADD COLUMN IF NOT EXISTS restart_cpu_millis   bigint CHECK (restart_cpu_millis >= 0),
    ADD COLUMN IF NOT EXISTS restart_memory_bytes bigint CHECK (restart_memory_bytes >= 0);
