ALTER TABLE grader_runs ADD COLUMN log text NOT NULL DEFAULT '' CHECK (octet_length(log) <= 1048576);
CREATE TABLE grader_triggers (
    attempt_id uuid PRIMARY KEY REFERENCES judging_attempts,
    contest_problem_id uuid NOT NULL REFERENCES contest_problems,
    created_at timestamptz NOT NULL DEFAULT now()
);
