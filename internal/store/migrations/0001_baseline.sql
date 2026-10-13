-- 0001_baseline.sql — TinyCDI v1 baseline schema.
--
-- Generated from pg_dump --schema-only of a database migrated through the
-- pre-1.0 migrations 001…024, then hand-cleaned. There is no upgrade path
-- from pre-1.0 databases: v1 installs start from this file. Columns that
-- were nullable only for rows written by earlier builds are NOT NULL here
-- (connection_lease/launch_ticket.portal_session_digest). The remaining
-- nullable columns carry meaning: NULL is "not bound yet", "no stream
-- owner", "no sealer", "follow the template" or "zero policy".
--
-- schema_migrations is created by the runner (internal/store/migrate.go).

CREATE TABLE IF NOT EXISTS workspaces (
    id text NOT NULL,
    tenant_id text NOT NULL,
    owner_subject text DEFAULT ''::text NOT NULL,
    request_id text NOT NULL,
    intent_revision bigint DEFAULT 0 NOT NULL,
    state text DEFAULT 'active'::text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    deleted_at timestamp with time zone,
    name text DEFAULT ''::text NOT NULL,
    owner_issuer text DEFAULT ''::text NOT NULL,
    owner_sub text DEFAULT ''::text NOT NULL,
    template jsonb,
    data_policy text DEFAULT 'Ephemeral'::text NOT NULL,
    desired_state text DEFAULT 'Stopped'::text NOT NULL,
    runtime_generation bigint DEFAULT 0 NOT NULL,
    phase text DEFAULT 'Pending'::text NOT NULL,
    failure_reason text,
    retained_data_ref text,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT workspaces_pkey PRIMARY KEY (id),
    CONSTRAINT workspaces_request_id_key UNIQUE (request_id),
    CONSTRAINT workspaces_data_policy_check CHECK ((data_policy = ANY (ARRAY['Ephemeral'::text, 'Retain'::text]))),
    CONSTRAINT workspaces_desired_state_check CHECK ((desired_state = ANY (ARRAY['Running'::text, 'Stopped'::text]))),
    CONSTRAINT workspaces_intent_revision_check CHECK ((intent_revision >= 0)),
    CONSTRAINT workspaces_runtime_generation_check CHECK ((runtime_generation >= 0)),
    CONSTRAINT workspaces_state_check CHECK ((state = ANY (ARRAY['active'::text, 'deleted'::text])))
);
CREATE UNIQUE INDEX IF NOT EXISTS workspaces_owner_name ON workspaces USING btree (tenant_id, owner_subject, name) WHERE ((state = 'active'::text) AND (name <> ''::text));

CREATE TABLE IF NOT EXISTS connection_lease (
    id text NOT NULL,
    workspace_id text NOT NULL,
    tenant_id text NOT NULL,
    principal_subject text NOT NULL,
    runtime_generation bigint NOT NULL,
    runtime_uid text NOT NULL,
    fencing_version bigint NOT NULL,
    gateway_id text NOT NULL,
    state text DEFAULT 'active'::text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    expires_at timestamp with time zone NOT NULL,
    last_renewed_at timestamp with time zone,
    closed_at timestamp with time zone,
    session_digest bytea,
    stream_epoch bigint DEFAULT 0 NOT NULL,
    stream_owner_tab text,
    stream_owner_epoch bigint,
    portal_session_digest bytea NOT NULL,
    CONSTRAINT connection_lease_pkey PRIMARY KEY (id),
    CONSTRAINT connection_lease_workspace_id_fkey FOREIGN KEY (workspace_id) REFERENCES workspaces(id),
    CONSTRAINT connection_lease_fencing_version_check CHECK ((fencing_version > 0)),
    CONSTRAINT connection_lease_state_check CHECK ((state = ANY (ARRAY['active'::text, 'superseded'::text, 'revoked'::text, 'expired'::text])))
);
CREATE UNIQUE INDEX IF NOT EXISTS connection_lease_one_active ON connection_lease USING btree (workspace_id) WHERE (state = 'active'::text);
CREATE INDEX IF NOT EXISTS connection_lease_portal_session ON connection_lease USING btree (portal_session_digest);
CREATE INDEX IF NOT EXISTS connection_lease_principal_active ON connection_lease USING btree (tenant_id, principal_subject) WHERE (state = 'active'::text);
CREATE UNIQUE INDEX IF NOT EXISTS connection_lease_session_digest ON connection_lease USING btree (session_digest) WHERE ((session_digest IS NOT NULL) AND (state = 'active'::text));
CREATE INDEX IF NOT EXISTS connection_lease_session_digest_any ON connection_lease USING btree (session_digest) WHERE (session_digest IS NOT NULL);
CREATE INDEX IF NOT EXISTS connection_lease_workspace ON connection_lease USING btree (workspace_id);

CREATE TABLE IF NOT EXISTS idempotency (
    tenant_id text NOT NULL,
    key text NOT NULL,
    op text DEFAULT 'create'::text NOT NULL,
    request_hash bytea NOT NULL,
    request_id text NOT NULL,
    result jsonb,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT idempotency_pkey PRIMARY KEY (tenant_id, key)
);

CREATE TABLE IF NOT EXISTS launch_ticket (
    ticket_hash bytea NOT NULL,
    workspace_id text NOT NULL,
    tenant_id text NOT NULL,
    principal_subject text NOT NULL,
    runtime_generation bigint NOT NULL,
    runtime_uid text NOT NULL,
    audience text NOT NULL,
    takeover boolean DEFAULT false NOT NULL,
    request_id text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    expires_at timestamp with time zone NOT NULL,
    consumed_at timestamp with time zone,
    revoked_at timestamp with time zone,
    clipboard_policy text,
    portal_session_digest bytea NOT NULL,
    session_policy text,
    CONSTRAINT launch_ticket_pkey PRIMARY KEY (ticket_hash),
    CONSTRAINT launch_ticket_workspace_id_fkey FOREIGN KEY (workspace_id) REFERENCES workspaces(id)
);
CREATE INDEX IF NOT EXISTS launch_ticket_portal_session ON launch_ticket USING btree (portal_session_digest);
CREATE INDEX IF NOT EXISTS launch_ticket_principal_outstanding ON launch_ticket USING btree (tenant_id, principal_subject) WHERE ((consumed_at IS NULL) AND (revoked_at IS NULL));
CREATE INDEX IF NOT EXISTS launch_ticket_workspace ON launch_ticket USING btree (workspace_id);

CREATE TABLE IF NOT EXISTS outbox_intent (
    seq bigserial NOT NULL,
    workspace_id text NOT NULL,
    tenant_id text NOT NULL,
    revision bigint NOT NULL,
    kind text NOT NULL,
    request_id text NOT NULL,
    payload jsonb,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    dispatched_at timestamp with time zone,
    CONSTRAINT outbox_intent_pkey PRIMARY KEY (seq),
    CONSTRAINT outbox_intent_workspace_id_revision_key UNIQUE (workspace_id, revision),
    CONSTRAINT outbox_intent_workspace_id_fkey FOREIGN KEY (workspace_id) REFERENCES workspaces(id),
    CONSTRAINT outbox_intent_kind_check CHECK ((kind = ANY (ARRAY['create'::text, 'start'::text, 'stop'::text, 'delete'::text]))),
    CONSTRAINT outbox_intent_revision_check CHECK ((revision > 0))
);
CREATE INDEX IF NOT EXISTS outbox_intent_pending ON outbox_intent USING btree (workspace_id, revision) WHERE (dispatched_at IS NULL);

-- platform_meta.session_epoch binds every session row to this install: the
-- epoch is minted here and rotated by the restore procedure, so a session
-- captured in a backup cannot be resurrected from a dump.
CREATE TABLE IF NOT EXISTS platform_meta (
    key text NOT NULL,
    value text NOT NULL,
    CONSTRAINT platform_meta_pkey PRIMARY KEY (key)
);
INSERT INTO platform_meta (key, value)
VALUES ('session_epoch', gen_random_uuid()::text)
ON CONFLICT (key) DO NOTHING;

CREATE TABLE IF NOT EXISTS principal_directory (
    tenant_id text NOT NULL,
    owner_ref text NOT NULL,
    subject text NOT NULL,
    display_name text DEFAULT ''::text NOT NULL,
    email text DEFAULT ''::text NOT NULL,
    last_login_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT principal_directory_pkey PRIMARY KEY (tenant_id, owner_ref)
);

CREATE TABLE IF NOT EXISTS quota_reservation (
    workspace_id text NOT NULL,
    tenant_id text NOT NULL,
    running_slots bigint NOT NULL,
    cpu_millis bigint NOT NULL,
    memory_bytes bigint NOT NULL,
    disk_bytes bigint NOT NULL,
    state text DEFAULT 'held'::text NOT NULL,
    release_proof text,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    released_at timestamp with time zone,
    restart_slots bigint,
    restart_cpu_millis bigint,
    restart_memory_bytes bigint,
    CONSTRAINT quota_reservation_pkey PRIMARY KEY (workspace_id),
    CONSTRAINT quota_reservation_workspace_id_fkey FOREIGN KEY (workspace_id) REFERENCES workspaces(id),
    CONSTRAINT quota_reservation_cpu_millis_check CHECK ((cpu_millis >= 0)),
    CONSTRAINT quota_reservation_disk_bytes_check CHECK ((disk_bytes >= 0)),
    CONSTRAINT quota_reservation_memory_bytes_check CHECK ((memory_bytes >= 0)),
    CONSTRAINT quota_reservation_restart_cpu_millis_check CHECK ((restart_cpu_millis >= 0)),
    CONSTRAINT quota_reservation_restart_memory_bytes_check CHECK ((restart_memory_bytes >= 0)),
    CONSTRAINT quota_reservation_restart_slots_check CHECK ((restart_slots >= 0)),
    CONSTRAINT quota_reservation_running_slots_check CHECK ((running_slots >= 0)),
    CONSTRAINT quota_reservation_state_check CHECK ((state = ANY (ARRAY['held'::text, 'released'::text])))
);
CREATE INDEX IF NOT EXISTS quota_reservation_held_tenant ON quota_reservation USING btree (tenant_id) WHERE (state = 'held'::text);

CREATE TABLE IF NOT EXISTS rate_limit_window (
    route text NOT NULL,
    bucket_key text NOT NULL,
    window_start timestamp with time zone NOT NULL,
    count bigint NOT NULL,
    CONSTRAINT rate_limit_window_pkey PRIMARY KEY (route, bucket_key, window_start)
);
CREATE INDEX IF NOT EXISTS rate_limit_window_expiry ON rate_limit_window USING btree (window_start);

CREATE TABLE IF NOT EXISTS retained_data (
    id text NOT NULL,
    tenant_id text NOT NULL,
    owner_subject text NOT NULL,
    state text DEFAULT 'Retained'::text NOT NULL,
    transition_seq bigint DEFAULT 0 NOT NULL,
    pvc_namespace text NOT NULL,
    pvc_name text NOT NULL,
    pvc_uid text NOT NULL,
    source_workspace_id text NOT NULL,
    source_workspace_name text DEFAULT ''::text NOT NULL,
    consuming_workspace_id text,
    runtime text NOT NULL,
    size_bytes bigint NOT NULL,
    retained_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    purged_at timestamp with time zone,
    CONSTRAINT retained_data_pkey PRIMARY KEY (id),
    CONSTRAINT retained_data_consuming_workspace_id_fkey FOREIGN KEY (consuming_workspace_id) REFERENCES workspaces(id),
    CONSTRAINT retained_data_runtime_check CHECK ((runtime = ANY (ARRAY['LinuxContainer'::text, 'WindowsVM'::text]))),
    CONSTRAINT retained_data_size_bytes_check CHECK ((size_bytes >= 0)),
    CONSTRAINT retained_data_state_check CHECK ((state = ANY (ARRAY['Retained'::text, 'Attaching'::text, 'Attached'::text, 'Purging'::text, 'Purged'::text]))),
    CONSTRAINT retained_data_transition_seq_check CHECK ((transition_seq >= 0))
);
CREATE UNIQUE INDEX IF NOT EXISTS retained_data_consumer ON retained_data USING btree (consuming_workspace_id) WHERE (consuming_workspace_id IS NOT NULL);
CREATE INDEX IF NOT EXISTS retained_data_owner ON retained_data USING btree (tenant_id, owner_subject) WHERE (state <> 'Purged'::text);
CREATE UNIQUE INDEX IF NOT EXISTS retained_data_pvc ON retained_data USING btree (pvc_namespace, pvc_uid);

CREATE TABLE IF NOT EXISTS retained_nonce_key (
    id boolean DEFAULT true NOT NULL,
    key bytea NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT retained_nonce_key_pkey PRIMARY KEY (id),
    CONSTRAINT retained_nonce_key_id_check CHECK (id),
    CONSTRAINT retained_nonce_key_key_check CHECK ((octet_length(key) >= 32))
);

CREATE TABLE IF NOT EXISTS sessions (
    id text NOT NULL,
    issuer text NOT NULL,
    subject text NOT NULL,
    tenant_id text NOT NULL,
    groups jsonb DEFAULT '[]'::jsonb NOT NULL,
    created_at timestamp with time zone NOT NULL,
    last_seen_at timestamp with time zone NOT NULL,
    expires_at timestamp with time zone,
    epoch text DEFAULT ''::text NOT NULL,
    display_name text DEFAULT ''::text NOT NULL,
    email text DEFAULT ''::text NOT NULL,
    id_token text,
    CONSTRAINT sessions_pkey PRIMARY KEY (id)
);
CREATE INDEX IF NOT EXISTS sessions_expires_at ON sessions USING btree (expires_at) WHERE (expires_at IS NOT NULL);
CREATE INDEX IF NOT EXISTS sessions_issuer_subject ON sessions USING btree (issuer, subject);

CREATE TABLE IF NOT EXISTS stop_intent (
    id bigserial NOT NULL,
    workspace_id text NOT NULL,
    runtime_generation bigint NOT NULL,
    reason text NOT NULL,
    deadline timestamp with time zone NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    drained_at timestamp with time zone,
    CONSTRAINT stop_intent_pkey PRIMARY KEY (id),
    CONSTRAINT stop_intent_workspace_id_runtime_generation_reason_key UNIQUE (workspace_id, runtime_generation, reason),
    CONSTRAINT stop_intent_workspace_id_fkey FOREIGN KEY (workspace_id) REFERENCES workspaces(id),
    CONSTRAINT stop_intent_reason_check CHECK ((reason = ANY (ARRAY['idle_timeout'::text, 'disconnect_timeout'::text, 'max_duration'::text, 'requested'::text])))
);
CREATE INDEX IF NOT EXISTS stop_intent_undrained ON stop_intent USING btree (workspace_id) WHERE (drained_at IS NULL);

CREATE TABLE IF NOT EXISTS tenant_quota (
    tenant_id text NOT NULL,
    max_running_slots bigint NOT NULL,
    max_cpu_millis bigint NOT NULL,
    max_memory_bytes bigint NOT NULL,
    max_disk_bytes bigint NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT tenant_quota_pkey PRIMARY KEY (tenant_id),
    CONSTRAINT tenant_quota_max_cpu_millis_check CHECK ((max_cpu_millis >= 0)),
    CONSTRAINT tenant_quota_max_disk_bytes_check CHECK ((max_disk_bytes >= 0)),
    CONSTRAINT tenant_quota_max_memory_bytes_check CHECK ((max_memory_bytes >= 0)),
    CONSTRAINT tenant_quota_max_running_slots_check CHECK ((max_running_slots >= 0))
);

-- NULL policy columns mean "follow the template".
CREATE TABLE IF NOT EXISTS tenant_session_policy (
    tenant_id text NOT NULL,
    clipboard text,
    file_transfer text,
    file_max_bytes bigint,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT tenant_session_policy_pkey PRIMARY KEY (tenant_id),
    CONSTRAINT tenant_session_policy_clipboard_check CHECK ((clipboard = ANY (ARRAY['Disabled'::text, 'Send'::text, 'Receive'::text, 'Bidirectional'::text]))),
    CONSTRAINT tenant_session_policy_file_max_bytes_check CHECK (((file_max_bytes IS NULL) OR (file_max_bytes > 0))),
    CONSTRAINT tenant_session_policy_file_transfer_check CHECK ((file_transfer = ANY (ARRAY['Disabled'::text, 'Upload'::text, 'Download'::text, 'Bidirectional'::text])))
);

CREATE TABLE IF NOT EXISTS tenant_user_limit_default (
    tenant_id text NOT NULL,
    max_running bigint NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT tenant_user_limit_default_pkey PRIMARY KEY (tenant_id),
    CONSTRAINT tenant_user_limit_default_max_running_check CHECK ((max_running >= 0))
);

CREATE TABLE IF NOT EXISTS user_session_limit (
    tenant_id text NOT NULL,
    owner_subject text NOT NULL,
    max_running bigint NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT user_session_limit_pkey PRIMARY KEY (tenant_id, owner_subject),
    CONSTRAINT user_session_limit_max_running_check CHECK ((max_running >= 0))
);

CREATE TABLE IF NOT EXISTS workspace_activity (
    workspace_id text NOT NULL,
    runtime_generation bigint NOT NULL,
    last_input_at timestamp with time zone,
    connected_at timestamp with time zone,
    disconnected_since timestamp with time zone,
    open_streams integer DEFAULT 0 NOT NULL,
    updated_at timestamp with time zone NOT NULL,
    CONSTRAINT workspace_activity_pkey PRIMARY KEY (workspace_id, runtime_generation),
    CONSTRAINT workspace_activity_workspace_id_fkey FOREIGN KEY (workspace_id) REFERENCES workspaces(id),
    CONSTRAINT workspace_activity_open_streams_check CHECK ((open_streams >= 0))
);

CREATE TABLE IF NOT EXISTS workspace_revocation (
    workspace_id text NOT NULL,
    runtime_generation bigint NOT NULL,
    revoked_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT workspace_revocation_pkey PRIMARY KEY (workspace_id, runtime_generation),
    CONSTRAINT workspace_revocation_workspace_id_fkey FOREIGN KEY (workspace_id) REFERENCES workspaces(id)
);

CREATE TABLE IF NOT EXISTS audit_event (
    id           bigserial PRIMARY KEY,
    tenant       text        NOT NULL,
    actor_ref    text        NOT NULL,
    action       text        NOT NULL,
    target_kind  text        NOT NULL DEFAULT '',
    target_id    text        NOT NULL DEFAULT '',
    outcome      text        NOT NULL CHECK (outcome IN ('success','denied','error')),
    error_code   text        NOT NULL DEFAULT '',
    request_id   text        NOT NULL DEFAULT '',
    details      jsonb       NOT NULL DEFAULT '{}'::jsonb,
    created_at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS audit_event_tenant_created_idx ON audit_event (tenant, created_at DESC, id DESC);
CREATE INDEX IF NOT EXISTS audit_event_created_idx ON audit_event (created_at);

CREATE TABLE IF NOT EXISTS session_file_transfer (
    id                  text        PRIMARY KEY,
    workspace_id        text        NOT NULL,
    tenant              text        NOT NULL,
    principal_issuer    text        NOT NULL,
    principal_subject   text        NOT NULL,
    runtime_generation  bigint      NOT NULL,
    runtime_uid         text        NOT NULL,
    direction           text        NOT NULL CHECK (direction IN ('upload','download')),
    path                text        NOT NULL,
    declared_bytes      bigint      NOT NULL CHECK (declared_bytes >= 0),
    bytes_done          bigint      NOT NULL DEFAULT 0,
    status              text        NOT NULL DEFAULT 'pending'
                         CHECK (status IN ('pending','claimed','completed','failed','expired')),
    resolved_policy     text        NOT NULL,
    created_at          timestamptz NOT NULL DEFAULT now(),
    expires_at          timestamptz NOT NULL,
    claimed_at          timestamptz,
    completed_at        timestamptz
);
CREATE INDEX IF NOT EXISTS session_file_transfer_ws_idx ON session_file_transfer (workspace_id, created_at DESC);
CREATE INDEX IF NOT EXISTS session_file_transfer_expiry_idx ON session_file_transfer (status, expires_at);
