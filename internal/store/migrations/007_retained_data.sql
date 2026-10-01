-- 007_retained_data.sql — retained-disk inventory
-- (design §5). Applies after 001–006; create it with the existing runner:
-- each file migrates in its own transaction via schema_migrations.
--
-- Model:
--   * One row per retained dataset. Dataset identity is
--     (pvc_namespace, pvc_uid) + source_workspace_id — NEVER a bare PVC
--     name, which is reusable and spoofable. The PVC's metadata
--     (labels/annotations stamped by the operator's retention step) is the
--     source of truth; this table is the transactional index the API
--     state machine runs on and is rebuildable from PVC metadata after
--     API DB loss (see internal/operator/retention.go).
--   * State machine: Retained -> Attaching -> Attached and
--     Retained -> Purging -> Purged. Transitions are conditional updates
--     (UPDATE ... WHERE state = 'Retained') inside the SAME transaction as
--     the quota reservation and the outbox intent, so attach and purge
--     exclude each other and a disk has at most one consumer. transition_seq
--     is bumped on every state change and is the epoch the purge
--     confirmation nonce is bound to (nonce = MAC over
--     id|state|transition_seq|caller — stateless, single-use because a
--     transition invalidates it).
--   * Quota accounting: the source workspace's quota_reservation row stays
--     'held' for the disk_bytes of the retained disk until the volume is
--     actually deleted (Purge reaches 'Purged'). Attach re-keys that held
--     disk reservation to the consuming workspace so disk quota moves with
--     the disk and is never double-counted. Compute reservations follow the
--     normal per-workspace lifecycle.
--   * The backend NEVER deletes the PVC itself: 'Purging' records an
--     outbox intent; actual volume deletion is the operator's job, and only
--     its completion proof advances the record to 'Purged'.

CREATE TABLE retained_data (
    id                      text PRIMARY KEY,              -- rd_ public ID
    tenant_id               text NOT NULL,
    owner_subject           text NOT NULL,                 -- iss|sub of the retaining owner
    state                   text NOT NULL DEFAULT 'Retained'
                            CHECK (state IN ('Retained', 'Attaching', 'Attached', 'Purging', 'Purged')),
    transition_seq          bigint NOT NULL DEFAULT 0
                            CHECK (transition_seq >= 0),   -- bumped on every state change; nonce epoch

    -- Dataset identity (see header): PVC UID pins the object; name is
    -- display/debug only and may be reused by unrelated volumes.
    pvc_namespace           text NOT NULL,
    pvc_name                text NOT NULL,
    pvc_uid                 text NOT NULL,

    source_workspace_id     text NOT NULL,                 -- workspaceUID the disk was retained from
    source_workspace_name   text NOT NULL DEFAULT '',      -- display name at retain time
    consuming_workspace_id  text REFERENCES workspaces (id), -- set while Attaching/Attached

    runtime                 text NOT NULL
                            CHECK (runtime IN ('LinuxContainer', 'WindowsVM')),
    size_bytes              bigint NOT NULL CHECK (size_bytes >= 0),

    retained_at             timestamptz NOT NULL DEFAULT now(),
    updated_at              timestamptz NOT NULL DEFAULT now(),
    purged_at               timestamptz
);

-- One record per physical dataset, forever (a PVC UID cannot recur).
CREATE UNIQUE INDEX retained_data_pvc
    ON retained_data (pvc_namespace, pvc_uid);

-- Owner inventory reads exclude finished purges.
CREATE INDEX retained_data_owner
    ON retained_data (tenant_id, owner_subject) WHERE state <> 'Purged';

-- A consuming workspace claims at most one retained disk and a disk has at
-- most one consumer; the conditional updates enforce this transactionally,
-- the index is the last line of defense.
CREATE UNIQUE INDEX retained_data_consumer
    ON retained_data (consuming_workspace_id)
    WHERE consuming_workspace_id IS NOT NULL;
