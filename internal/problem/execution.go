package problem

// RunnableRevisionSQL is the current execution contract for a problem_revisions
// row aliased as r. Published diagnostic packages can still be read and attached.
const RunnableRevisionSQL = `(r.status='published' AND r.format_version=1
	AND (r.execution_format='standard' OR (r.execution_format='scripted_lua' AND r.runner_api_version=1))
	AND coalesce(r.manifest->>'package_format'='ustar_v1' AND r.manifest->'execution_ready'='true'::jsonb,false))`
