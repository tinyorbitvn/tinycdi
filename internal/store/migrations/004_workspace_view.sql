-- 004_workspace_view.sql — extend workspaces with the public view fields the
-- API serves (name, resolved template snapshot, policies, lifecycle
-- projection) plus the fencing counters the dispatcher snapshots into each
-- outbox intent payload.
--
-- desired_state/runtime_generation are bumped inside the same transaction as
-- AppendIntent so the intent payload always carries an atomic snapshot.

ALTER TABLE workspaces
    ADD COLUMN name               text        NOT NULL DEFAULT '',
    ADD COLUMN owner_issuer       text        NOT NULL DEFAULT '',
    ADD COLUMN owner_sub          text        NOT NULL DEFAULT '',
    ADD COLUMN template           jsonb,
    ADD COLUMN data_policy        text        NOT NULL DEFAULT 'Ephemeral'
                                  CHECK (data_policy IN ('Ephemeral', 'Retain')),
    ADD COLUMN desired_state      text        NOT NULL DEFAULT 'Stopped'
                                  CHECK (desired_state IN ('Running', 'Stopped')),
    ADD COLUMN runtime_generation bigint      NOT NULL DEFAULT 0
                                  CHECK (runtime_generation >= 0),
    ADD COLUMN phase              text        NOT NULL DEFAULT 'Pending',
    ADD COLUMN failure_reason     text,
    ADD COLUMN retained_data_ref  text,
    ADD COLUMN updated_at         timestamptz NOT NULL DEFAULT now();

-- Display name is unique per owner among live workspaces.
CREATE UNIQUE INDEX workspaces_owner_name
    ON workspaces (tenant_id, owner_subject, name)
    WHERE state = 'active' AND name <> '';
