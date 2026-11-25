UPDATE outbox_events e
SET payload = jsonb_build_object(
    'schema_version', 1,
    'job_id', j.id,
    'run_id', j.run_id,
    'judging_attempt_id', j.id,
    'submission_id', s.id,
    'contest_id', s.contest_id,
    'contest_problem_id', s.contest_problem_id,
    'task_id', s.problem_id,
    'task_revision_id', r.id,
    'execution_format', r.execution_format,
    'execution_format_version', r.format_version,
    'created_at', to_char(j.created_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US"Z"')
)
FROM judging_attempts j
JOIN submissions s ON s.id = j.submission_id
JOIN problem_revisions r ON r.id = j.target_problem_revision_id
WHERE e.topic = 'podtest.judging.jobs.v1'
  AND e.dedup_key = j.id::text
  AND e.published_at IS NULL
  AND NOT (e.payload ? 'task_revision_id');
