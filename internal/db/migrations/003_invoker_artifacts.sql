ALTER TABLE judging_attempts ADD COLUMN process_epoch uuid,
 ADD COLUMN fencing_token bytea CHECK (octet_length(fencing_token) = 32);

CREATE TABLE invoker_upload_grants (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
 client_artifact_id uuid NOT NULL,
 attempt_id uuid NOT NULL,
 run_id uuid NOT NULL,
 kind text NOT NULL CHECK(kind IN ('build_log','runner_log','test_io','custom','metrics')),
 digest text NOT NULL CHECK(digest ~ '^[0-9a-f]{64}$'),
 size bigint NOT NULL CHECK(size BETWEEN 0 AND 268435456),
 completed_at timestamptz,
 UNIQUE(attempt_id,run_id,client_artifact_id),
 FOREIGN KEY(attempt_id,run_id) REFERENCES judging_attempts(id,run_id)
);
