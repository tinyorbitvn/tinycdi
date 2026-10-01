-- 008_retained_nonce_key.sql — shared MAC key for retained-data purge
-- confirmation nonces (design §5).
--
-- The purge nonce is stateless: nonce = payload || "." || HMAC-SHA256(key,
-- payload) where payload binds the record id, the caller, the record's
-- transition_seq epoch and the issue time. Every API replica must verify
-- nonces minted by any other replica, so the key lives in the DB: the
-- first writer generates 32 random bytes and every other reader shares
-- the single row. Rotating the key invalidates all outstanding nonces —
-- an acceptable, documented operational step (see
-- docs/runbooks/retained-data.md).

CREATE TABLE retained_nonce_key (
    id         boolean PRIMARY KEY DEFAULT true CHECK (id),  -- single row
    key        bytea NOT NULL CHECK (octet_length(key) >= 32),
    created_at timestamptz NOT NULL DEFAULT now()
);
