ALTER TABLE problem_revisions ADD COLUMN grader_script_artifact_id uuid REFERENCES artifacts(id);
