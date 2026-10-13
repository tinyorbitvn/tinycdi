CREATE TABLE public.audit_event (
    id bigint NOT NULL,
    tenant text NOT NULL,
    actor_ref text NOT NULL,
    action text NOT NULL,
    target_kind text DEFAULT ''::text NOT NULL,
    target_id text DEFAULT ''::text NOT NULL,
    outcome text NOT NULL,
    error_code text DEFAULT ''::text NOT NULL,
    request_id text DEFAULT ''::text NOT NULL,
    details jsonb DEFAULT '{}'::jsonb NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT audit_event_outcome_check CHECK ((outcome = ANY (ARRAY['success'::text, 'denied'::text, 'failure'::text])))
);

CREATE SEQUENCE public.audit_event_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

ALTER SEQUENCE public.audit_event_id_seq OWNED BY public.audit_event.id;

CREATE TABLE public.connection_lease (
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
    CONSTRAINT connection_lease_fencing_version_check CHECK ((fencing_version > 0)),
    CONSTRAINT connection_lease_state_check CHECK ((state = ANY (ARRAY['active'::text, 'superseded'::text, 'revoked'::text, 'expired'::text])))
);

CREATE TABLE public.idempotency (
    tenant_id text NOT NULL,
    key text NOT NULL,
    op text DEFAULT 'create'::text NOT NULL,
    request_hash bytea NOT NULL,
    request_id text NOT NULL,
    result jsonb,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);

CREATE TABLE public.launch_ticket (
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
    session_policy text
);

CREATE TABLE public.outbox_intent (
    seq bigint NOT NULL,
    workspace_id text NOT NULL,
    tenant_id text NOT NULL,
    revision bigint NOT NULL,
    kind text NOT NULL,
    request_id text NOT NULL,
    payload jsonb,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    dispatched_at timestamp with time zone,
    CONSTRAINT outbox_intent_kind_check CHECK ((kind = ANY (ARRAY['create'::text, 'start'::text, 'stop'::text, 'delete'::text]))),
    CONSTRAINT outbox_intent_revision_check CHECK ((revision > 0))
);

CREATE SEQUENCE public.outbox_intent_seq_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

ALTER SEQUENCE public.outbox_intent_seq_seq OWNED BY public.outbox_intent.seq;

CREATE TABLE public.platform_meta (
    key text NOT NULL,
    value text NOT NULL
);

CREATE TABLE public.principal_directory (
    tenant_id text NOT NULL,
    owner_ref text NOT NULL,
    subject text NOT NULL,
    display_name text DEFAULT ''::text NOT NULL,
    email text DEFAULT ''::text NOT NULL,
    last_login_at timestamp with time zone DEFAULT now() NOT NULL
);

CREATE TABLE public.quota_reservation (
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
    CONSTRAINT quota_reservation_cpu_millis_check CHECK ((cpu_millis >= 0)),
    CONSTRAINT quota_reservation_disk_bytes_check CHECK ((disk_bytes >= 0)),
    CONSTRAINT quota_reservation_memory_bytes_check CHECK ((memory_bytes >= 0)),
    CONSTRAINT quota_reservation_restart_cpu_millis_check CHECK ((restart_cpu_millis >= 0)),
    CONSTRAINT quota_reservation_restart_memory_bytes_check CHECK ((restart_memory_bytes >= 0)),
    CONSTRAINT quota_reservation_restart_slots_check CHECK ((restart_slots >= 0)),
    CONSTRAINT quota_reservation_running_slots_check CHECK ((running_slots >= 0)),
    CONSTRAINT quota_reservation_state_check CHECK ((state = ANY (ARRAY['held'::text, 'released'::text])))
);

CREATE TABLE public.rate_limit_window (
    route text NOT NULL,
    bucket_key text NOT NULL,
    window_start timestamp with time zone NOT NULL,
    count bigint NOT NULL
);

CREATE TABLE public.retained_data (
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
    CONSTRAINT retained_data_runtime_check CHECK ((runtime = ANY (ARRAY['LinuxContainer'::text, 'WindowsVM'::text]))),
    CONSTRAINT retained_data_size_bytes_check CHECK ((size_bytes >= 0)),
    CONSTRAINT retained_data_state_check CHECK ((state = ANY (ARRAY['Retained'::text, 'Attaching'::text, 'Attached'::text, 'Purging'::text, 'Purged'::text]))),
    CONSTRAINT retained_data_transition_seq_check CHECK ((transition_seq >= 0))
);

CREATE TABLE public.retained_nonce_key (
    id boolean DEFAULT true NOT NULL,
    key bytea NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT retained_nonce_key_id_check CHECK (id),
    CONSTRAINT retained_nonce_key_key_check CHECK ((octet_length(key) >= 32))
);

CREATE TABLE public.schema_migrations (
    version integer NOT NULL,
    name text NOT NULL,
    applied_at timestamp with time zone DEFAULT now() NOT NULL
);

CREATE TABLE public.session_file_transfer (
    id text NOT NULL,
    workspace_id text NOT NULL,
    tenant text NOT NULL,
    principal_issuer text NOT NULL,
    principal_subject text NOT NULL,
    runtime_generation bigint NOT NULL,
    runtime_uid text NOT NULL,
    direction text NOT NULL,
    path text NOT NULL,
    declared_bytes bigint NOT NULL,
    bytes_done bigint DEFAULT 0 NOT NULL,
    status text DEFAULT 'pending'::text NOT NULL,
    resolved_policy text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    expires_at timestamp with time zone NOT NULL,
    claimed_at timestamp with time zone,
    completed_at timestamp with time zone,
    CONSTRAINT session_file_transfer_declared_bytes_check CHECK ((declared_bytes >= 0)),
    CONSTRAINT session_file_transfer_direction_check CHECK ((direction = ANY (ARRAY['upload'::text, 'download'::text]))),
    CONSTRAINT session_file_transfer_status_check CHECK ((status = ANY (ARRAY['pending'::text, 'claimed'::text, 'completed'::text, 'failed'::text, 'expired'::text])))
);

CREATE TABLE public.sessions (
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
    id_token text
);

CREATE TABLE public.stop_intent (
    id bigint NOT NULL,
    workspace_id text NOT NULL,
    runtime_generation bigint NOT NULL,
    reason text NOT NULL,
    deadline timestamp with time zone NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    drained_at timestamp with time zone,
    CONSTRAINT stop_intent_reason_check CHECK ((reason = ANY (ARRAY['idle_timeout'::text, 'disconnect_timeout'::text, 'max_duration'::text, 'requested'::text])))
);

CREATE SEQUENCE public.stop_intent_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

ALTER SEQUENCE public.stop_intent_id_seq OWNED BY public.stop_intent.id;

CREATE TABLE public.tenant_quota (
    tenant_id text NOT NULL,
    max_running_slots bigint NOT NULL,
    max_cpu_millis bigint NOT NULL,
    max_memory_bytes bigint NOT NULL,
    max_disk_bytes bigint NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT tenant_quota_max_cpu_millis_check CHECK ((max_cpu_millis >= 0)),
    CONSTRAINT tenant_quota_max_disk_bytes_check CHECK ((max_disk_bytes >= 0)),
    CONSTRAINT tenant_quota_max_memory_bytes_check CHECK ((max_memory_bytes >= 0)),
    CONSTRAINT tenant_quota_max_running_slots_check CHECK ((max_running_slots >= 0))
);

CREATE TABLE public.tenant_session_policy (
    tenant_id text NOT NULL,
    clipboard text,
    file_transfer text,
    file_max_bytes bigint,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT tenant_session_policy_clipboard_check CHECK ((clipboard = ANY (ARRAY['Disabled'::text, 'Send'::text, 'Receive'::text, 'Bidirectional'::text]))),
    CONSTRAINT tenant_session_policy_file_max_bytes_check CHECK (((file_max_bytes IS NULL) OR (file_max_bytes > 0))),
    CONSTRAINT tenant_session_policy_file_transfer_check CHECK ((file_transfer = ANY (ARRAY['Disabled'::text, 'Upload'::text, 'Download'::text, 'Bidirectional'::text])))
);

CREATE TABLE public.tenant_user_limit_default (
    tenant_id text NOT NULL,
    max_running bigint NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT tenant_user_limit_default_max_running_check CHECK ((max_running >= 0))
);

CREATE TABLE public.user_session_limit (
    tenant_id text NOT NULL,
    owner_subject text NOT NULL,
    max_running bigint NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT user_session_limit_max_running_check CHECK ((max_running >= 0))
);

CREATE TABLE public.workspace_activity (
    workspace_id text NOT NULL,
    runtime_generation bigint NOT NULL,
    last_input_at timestamp with time zone,
    connected_at timestamp with time zone,
    disconnected_since timestamp with time zone,
    open_streams integer DEFAULT 0 NOT NULL,
    updated_at timestamp with time zone NOT NULL,
    CONSTRAINT workspace_activity_open_streams_check CHECK ((open_streams >= 0))
);

CREATE TABLE public.workspace_revocation (
    workspace_id text NOT NULL,
    runtime_generation bigint NOT NULL,
    revoked_at timestamp with time zone DEFAULT now() NOT NULL
);

CREATE TABLE public.workspaces (
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
    CONSTRAINT workspaces_data_policy_check CHECK ((data_policy = ANY (ARRAY['Ephemeral'::text, 'Retain'::text]))),
    CONSTRAINT workspaces_desired_state_check CHECK ((desired_state = ANY (ARRAY['Running'::text, 'Stopped'::text]))),
    CONSTRAINT workspaces_intent_revision_check CHECK ((intent_revision >= 0)),
    CONSTRAINT workspaces_runtime_generation_check CHECK ((runtime_generation >= 0)),
    CONSTRAINT workspaces_state_check CHECK ((state = ANY (ARRAY['active'::text, 'deleted'::text])))
);

ALTER TABLE ONLY public.audit_event ALTER COLUMN id SET DEFAULT nextval('public.audit_event_id_seq'::regclass);

ALTER TABLE ONLY public.outbox_intent ALTER COLUMN seq SET DEFAULT nextval('public.outbox_intent_seq_seq'::regclass);

ALTER TABLE ONLY public.stop_intent ALTER COLUMN id SET DEFAULT nextval('public.stop_intent_id_seq'::regclass);

ALTER TABLE ONLY public.audit_event
    ADD CONSTRAINT audit_event_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.connection_lease
    ADD CONSTRAINT connection_lease_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.idempotency
    ADD CONSTRAINT idempotency_pkey PRIMARY KEY (tenant_id, key);

ALTER TABLE ONLY public.launch_ticket
    ADD CONSTRAINT launch_ticket_pkey PRIMARY KEY (ticket_hash);

ALTER TABLE ONLY public.outbox_intent
    ADD CONSTRAINT outbox_intent_pkey PRIMARY KEY (seq);

ALTER TABLE ONLY public.outbox_intent
    ADD CONSTRAINT outbox_intent_workspace_id_revision_key UNIQUE (workspace_id, revision);

ALTER TABLE ONLY public.platform_meta
    ADD CONSTRAINT platform_meta_pkey PRIMARY KEY (key);

ALTER TABLE ONLY public.principal_directory
    ADD CONSTRAINT principal_directory_pkey PRIMARY KEY (tenant_id, owner_ref);

ALTER TABLE ONLY public.quota_reservation
    ADD CONSTRAINT quota_reservation_pkey PRIMARY KEY (workspace_id);

ALTER TABLE ONLY public.rate_limit_window
    ADD CONSTRAINT rate_limit_window_pkey PRIMARY KEY (route, bucket_key, window_start);

ALTER TABLE ONLY public.retained_data
    ADD CONSTRAINT retained_data_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.retained_nonce_key
    ADD CONSTRAINT retained_nonce_key_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.schema_migrations
    ADD CONSTRAINT schema_migrations_pkey PRIMARY KEY (version);

ALTER TABLE ONLY public.session_file_transfer
    ADD CONSTRAINT session_file_transfer_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.sessions
    ADD CONSTRAINT sessions_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.stop_intent
    ADD CONSTRAINT stop_intent_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.stop_intent
    ADD CONSTRAINT stop_intent_workspace_id_runtime_generation_reason_key UNIQUE (workspace_id, runtime_generation, reason);

ALTER TABLE ONLY public.tenant_quota
    ADD CONSTRAINT tenant_quota_pkey PRIMARY KEY (tenant_id);

ALTER TABLE ONLY public.tenant_session_policy
    ADD CONSTRAINT tenant_session_policy_pkey PRIMARY KEY (tenant_id);

ALTER TABLE ONLY public.tenant_user_limit_default
    ADD CONSTRAINT tenant_user_limit_default_pkey PRIMARY KEY (tenant_id);

ALTER TABLE ONLY public.user_session_limit
    ADD CONSTRAINT user_session_limit_pkey PRIMARY KEY (tenant_id, owner_subject);

ALTER TABLE ONLY public.workspace_activity
    ADD CONSTRAINT workspace_activity_pkey PRIMARY KEY (workspace_id, runtime_generation);

ALTER TABLE ONLY public.workspace_revocation
    ADD CONSTRAINT workspace_revocation_pkey PRIMARY KEY (workspace_id, runtime_generation);

ALTER TABLE ONLY public.workspaces
    ADD CONSTRAINT workspaces_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.workspaces
    ADD CONSTRAINT workspaces_request_id_key UNIQUE (request_id);

CREATE INDEX audit_event_created_idx ON public.audit_event USING btree (created_at);

CREATE INDEX audit_event_tenant_action_idx ON public.audit_event USING btree (tenant, action, created_at DESC, id DESC);

CREATE INDEX audit_event_tenant_created_idx ON public.audit_event USING btree (tenant, created_at DESC, id DESC);

CREATE UNIQUE INDEX connection_lease_one_active ON public.connection_lease USING btree (workspace_id) WHERE (state = 'active'::text);

CREATE INDEX connection_lease_portal_session ON public.connection_lease USING btree (portal_session_digest);

CREATE INDEX connection_lease_principal_active ON public.connection_lease USING btree (tenant_id, principal_subject) WHERE (state = 'active'::text);

CREATE UNIQUE INDEX connection_lease_session_digest ON public.connection_lease USING btree (session_digest) WHERE ((session_digest IS NOT NULL) AND (state = 'active'::text));

CREATE INDEX connection_lease_session_digest_any ON public.connection_lease USING btree (session_digest) WHERE (session_digest IS NOT NULL);

CREATE INDEX connection_lease_workspace ON public.connection_lease USING btree (workspace_id);

CREATE INDEX launch_ticket_portal_session ON public.launch_ticket USING btree (portal_session_digest);

CREATE INDEX launch_ticket_principal_outstanding ON public.launch_ticket USING btree (tenant_id, principal_subject) WHERE ((consumed_at IS NULL) AND (revoked_at IS NULL));

CREATE INDEX launch_ticket_workspace ON public.launch_ticket USING btree (workspace_id);

CREATE INDEX outbox_intent_pending ON public.outbox_intent USING btree (workspace_id, revision) WHERE (dispatched_at IS NULL);

CREATE INDEX quota_reservation_held_tenant ON public.quota_reservation USING btree (tenant_id) WHERE (state = 'held'::text);

CREATE INDEX rate_limit_window_expiry ON public.rate_limit_window USING btree (window_start);

CREATE UNIQUE INDEX retained_data_consumer ON public.retained_data USING btree (consuming_workspace_id) WHERE (consuming_workspace_id IS NOT NULL);

CREATE INDEX retained_data_owner ON public.retained_data USING btree (tenant_id, owner_subject) WHERE (state <> 'Purged'::text);

CREATE UNIQUE INDEX retained_data_pvc ON public.retained_data USING btree (pvc_namespace, pvc_uid);

CREATE INDEX session_file_transfer_expiry_idx ON public.session_file_transfer USING btree (status, expires_at);

CREATE INDEX session_file_transfer_ws_idx ON public.session_file_transfer USING btree (workspace_id, created_at DESC);

CREATE INDEX sessions_expires_at ON public.sessions USING btree (expires_at) WHERE (expires_at IS NOT NULL);

CREATE INDEX sessions_issuer_subject ON public.sessions USING btree (issuer, subject);

CREATE INDEX stop_intent_undrained ON public.stop_intent USING btree (workspace_id) WHERE (drained_at IS NULL);

CREATE UNIQUE INDEX workspaces_owner_name ON public.workspaces USING btree (tenant_id, owner_subject, name) WHERE ((state = 'active'::text) AND (name <> ''::text));

ALTER TABLE ONLY public.connection_lease
    ADD CONSTRAINT connection_lease_workspace_id_fkey FOREIGN KEY (workspace_id) REFERENCES public.workspaces(id);

ALTER TABLE ONLY public.launch_ticket
    ADD CONSTRAINT launch_ticket_workspace_id_fkey FOREIGN KEY (workspace_id) REFERENCES public.workspaces(id);

ALTER TABLE ONLY public.outbox_intent
    ADD CONSTRAINT outbox_intent_workspace_id_fkey FOREIGN KEY (workspace_id) REFERENCES public.workspaces(id);

ALTER TABLE ONLY public.quota_reservation
    ADD CONSTRAINT quota_reservation_workspace_id_fkey FOREIGN KEY (workspace_id) REFERENCES public.workspaces(id);

ALTER TABLE ONLY public.retained_data
    ADD CONSTRAINT retained_data_consuming_workspace_id_fkey FOREIGN KEY (consuming_workspace_id) REFERENCES public.workspaces(id);

ALTER TABLE ONLY public.stop_intent
    ADD CONSTRAINT stop_intent_workspace_id_fkey FOREIGN KEY (workspace_id) REFERENCES public.workspaces(id);

ALTER TABLE ONLY public.workspace_activity
    ADD CONSTRAINT workspace_activity_workspace_id_fkey FOREIGN KEY (workspace_id) REFERENCES public.workspaces(id);

ALTER TABLE ONLY public.workspace_revocation
    ADD CONSTRAINT workspace_revocation_workspace_id_fkey FOREIGN KEY (workspace_id) REFERENCES public.workspaces(id);
