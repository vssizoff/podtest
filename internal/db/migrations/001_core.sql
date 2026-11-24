CREATE TABLE users (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(), display_name text NOT NULL, email text NOT NULL DEFAULT '',
 avatar_url text NOT NULL DEFAULT '', status text NOT NULL DEFAULT 'active' CHECK(status IN ('active','disabled')),
 created_at timestamptz NOT NULL DEFAULT now(), updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE user_identities (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(), user_id uuid NOT NULL REFERENCES users,
 issuer text NOT NULL, subject text NOT NULL, created_at timestamptz NOT NULL DEFAULT now(), UNIQUE(issuer,subject)
);
CREATE TABLE auth_sessions (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(), session_token_hash bytea NOT NULL UNIQUE,
 user_id uuid NOT NULL REFERENCES users, created_at timestamptz NOT NULL DEFAULT now(),
 expires_at timestamptz NOT NULL, last_seen_at timestamptz NOT NULL DEFAULT now(), revoked_at timestamptz
);
CREATE TABLE oidc_flows (
 state_hash bytea PRIMARY KEY, verifier text NOT NULL, nonce text NOT NULL, expires_at timestamptz NOT NULL
);
CREATE TABLE courses (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(), title text NOT NULL, description text NOT NULL DEFAULT '',
 created_by uuid NOT NULL REFERENCES users, created_at timestamptz NOT NULL DEFAULT now(), archived_at timestamptz
);
CREATE TABLE contests (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(), title text NOT NULL, description text NOT NULL DEFAULT '',
 created_by uuid NOT NULL REFERENCES users, created_at timestamptz NOT NULL DEFAULT now(), archived_at timestamptz,
 status text NOT NULL DEFAULT 'draft' CHECK(status IN ('draft','open','closed')),
 starts_at timestamptz, ends_at timestamptz, CHECK(ends_at > starts_at)
);
CREATE TABLE course_contests (
 course_id uuid REFERENCES courses, contest_id uuid REFERENCES contests, position int NOT NULL CHECK(position>=0),
 added_at timestamptz NOT NULL DEFAULT now(), PRIMARY KEY(course_id,contest_id)
);
CREATE INDEX ON course_contests(course_id,position,contest_id);
CREATE INDEX ON course_contests(contest_id);
CREATE TABLE contest_settings (
 contest_id uuid PRIMARY KEY REFERENCES contests, self_registration boolean NOT NULL DEFAULT false,
 scoreboard_visibility text NOT NULL DEFAULT 'private' CHECK(scoreboard_visibility IN ('private','contest')),
 max_queued_submissions int CHECK(max_queued_submissions>0)
);
CREATE TABLE contest_participants (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(), contest_id uuid NOT NULL REFERENCES contests, user_id uuid NOT NULL REFERENCES users,
 status text NOT NULL DEFAULT 'active' CHECK(status IN ('active','inactive')), registered_at timestamptz NOT NULL DEFAULT now(),
 UNIQUE(contest_id,user_id), UNIQUE(id,contest_id,user_id), UNIQUE(id,contest_id)
);
CREATE TABLE course_memberships (
 course_id uuid REFERENCES courses, user_id uuid REFERENCES users,
 role text CHECK(role IN ('owner','manager','teacher','student','viewer')), PRIMARY KEY(course_id,user_id,role)
);
CREATE TABLE contest_memberships (
 contest_id uuid REFERENCES contests, user_id uuid REFERENCES users,
 role text CHECK(role IN ('manager','viewer')), PRIMARY KEY(contest_id,user_id,role)
);
CREATE TABLE artifacts (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(), storage_key text NOT NULL UNIQUE,
 digest text NOT NULL CHECK(digest ~ '^[0-9a-f]{64}$'), size bigint NOT NULL CHECK(size>=0),
 media_type text NOT NULL, kind text NOT NULL, created_by uuid REFERENCES users, created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX ON artifacts(digest);
CREATE TABLE problems (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(), title text NOT NULL, description text NOT NULL DEFAULT '',
 created_by uuid NOT NULL REFERENCES users, created_at timestamptz NOT NULL DEFAULT now(), archived_at timestamptz
);
CREATE TABLE problem_revisions (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(), problem_id uuid NOT NULL REFERENCES problems,
 revision_number int NOT NULL CHECK(revision_number>0), execution_format text NOT NULL CHECK(execution_format IN ('standard','scripted_lua')),
 format_version int NOT NULL DEFAULT 1 CHECK(format_version>0), runner_api_version int NOT NULL DEFAULT 1 CHECK(runner_api_version>0),
 manifest jsonb NOT NULL DEFAULT '{}', source_artifact_id uuid REFERENCES artifacts, bundle_artifact_id uuid REFERENCES artifacts,
 digest text CHECK(digest ~ '^[0-9a-f]{64}$'), status text NOT NULL DEFAULT 'draft' CHECK(status IN ('draft','published')),
 grader_mode text CHECK(grader_mode IN ('manual','automatic_submission')), created_at timestamptz NOT NULL DEFAULT now(), published_at timestamptz,
 UNIQUE(problem_id,revision_number), UNIQUE(id,problem_id),
 CHECK(status='draft' OR (published_at IS NOT NULL AND digest IS NOT NULL AND bundle_artifact_id IS NOT NULL))
);
CREATE TABLE problem_statements (
 revision_id uuid REFERENCES problem_revisions, locale text, source text NOT NULL, rendered_html text NOT NULL,
 renderer_version text NOT NULL, digest text NOT NULL, PRIMARY KEY(revision_id,locale)
);
CREATE TABLE languages (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(), code text NOT NULL UNIQUE, title text NOT NULL,
 builtin boolean NOT NULL DEFAULT false, created_by uuid REFERENCES users
);
CREATE TABLE problem_revision_languages (
 revision_id uuid REFERENCES problem_revisions, language_id uuid REFERENCES languages,
 build_command text NOT NULL DEFAULT '', run_command text NOT NULL DEFAULT '', containerfile_artifact_id uuid REFERENCES artifacts,
 PRIMARY KEY(revision_id,language_id)
);
CREATE TABLE contest_problems (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(), contest_id uuid NOT NULL REFERENCES contests,
 problem_id uuid NOT NULL REFERENCES problems, problem_revision_id uuid NOT NULL,
 position int NOT NULL CHECK(position>=0), max_score numeric(18,6) NOT NULL CHECK(max_score BETWEEN 0 AND 999999999999),
 submission_limit int CHECK(submission_limit>0), verdict_visibility text NOT NULL DEFAULT 'summary' CHECK(verdict_visibility IN ('hidden','summary','tests')),
 detached_at timestamptz, FOREIGN KEY(problem_revision_id,problem_id) REFERENCES problem_revisions(id,problem_id),
 UNIQUE(id,contest_id), UNIQUE(id,problem_id)
);
CREATE INDEX ON contest_problems(contest_id,position,id) WHERE detached_at IS NULL;
CREATE INDEX ON contest_problems(problem_revision_id);
CREATE TABLE theory_materials (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(), title text NOT NULL, description text NOT NULL DEFAULT '',
 created_by uuid NOT NULL REFERENCES users, created_at timestamptz NOT NULL DEFAULT now(), archived_at timestamptz
);
CREATE TABLE material_revisions (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(), material_id uuid NOT NULL REFERENCES theory_materials,
 revision_number int NOT NULL CHECK(revision_number>0), format text NOT NULL CHECK(format IN ('markdown','latex')),
 source text NOT NULL, digest text NOT NULL, rendered_html text NOT NULL, rendered_artifact_id uuid REFERENCES artifacts,
 renderer_version text NOT NULL, status text NOT NULL DEFAULT 'rendered' CHECK(status IN ('rendered','failed')),
 created_at timestamptz NOT NULL DEFAULT now(), UNIQUE(material_id,revision_number), UNIQUE(id,material_id)
);
CREATE TABLE contest_materials (
 contest_id uuid REFERENCES contests, material_id uuid REFERENCES theory_materials, material_revision_id uuid NOT NULL,
 position int NOT NULL CHECK(position>=0), visibility text NOT NULL CHECK(visibility IN ('contest','managers')),
 PRIMARY KEY(contest_id,material_id), FOREIGN KEY(material_revision_id,material_id) REFERENCES material_revisions(id,material_id)
);
CREATE INDEX ON contest_materials(contest_id,position,material_id);
CREATE TABLE submissions (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(), contest_problem_id uuid NOT NULL, participant_id uuid NOT NULL,
 contest_id uuid NOT NULL, problem_id uuid NOT NULL, submitted_by uuid NOT NULL REFERENCES users, problem_revision_id uuid NOT NULL,
 language_id uuid NOT NULL, source_artifact_id uuid NOT NULL REFERENCES artifacts, digest text NOT NULL,
 submitted_at timestamptz NOT NULL DEFAULT now(),
 FOREIGN KEY(contest_problem_id,contest_id) REFERENCES contest_problems(id,contest_id),
 FOREIGN KEY(contest_problem_id,problem_id) REFERENCES contest_problems(id,problem_id),
 FOREIGN KEY(participant_id,contest_id,submitted_by) REFERENCES contest_participants(id,contest_id,user_id),
 FOREIGN KEY(problem_revision_id,problem_id) REFERENCES problem_revisions(id,problem_id),
  FOREIGN KEY(problem_revision_id,language_id) REFERENCES problem_revision_languages(revision_id,language_id),
 UNIQUE(id,participant_id), UNIQUE(id,contest_problem_id)
);
CREATE INDEX ON submissions(participant_id,contest_problem_id,submitted_at DESC);
CREATE INDEX ON submissions(contest_problem_id,submitted_at DESC,id);
CREATE TABLE invokers (
 id uuid PRIMARY KEY, last_heartbeat timestamptz NOT NULL DEFAULT now(), protocol_version int NOT NULL,
 capabilities text[] NOT NULL, capacity int NOT NULL CHECK(capacity>0), load int NOT NULL CHECK(load>=0 AND load<=capacity)
);
CREATE TABLE judging_attempts (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(), submission_id uuid NOT NULL REFERENCES submissions,
 attempt_number int NOT NULL CHECK(attempt_number>0), target_problem_revision_id uuid NOT NULL REFERENCES problem_revisions,
 run_id uuid NOT NULL UNIQUE DEFAULT gen_random_uuid(), status text NOT NULL CHECK(status IN ('waiting','queued','assigned','running','recovery','finished','error','timeout','cancelled')),
 verdict text, base_score numeric(18,6) CHECK(base_score BETWEEN 0 AND 999999999999),
 trigger_actor uuid REFERENCES users, trigger_reason text NOT NULL, retry_of uuid REFERENCES judging_attempts,
 invoker_id uuid REFERENCES invokers, lease_until timestamptz, max_score numeric(18,6) NOT NULL CHECK(max_score BETWEEN 0 AND 999999999999),
 last_sequence bigint NOT NULL DEFAULT 0 CHECK(last_sequence>=0), created_at timestamptz NOT NULL DEFAULT now(), started_at timestamptz, finished_at timestamptz,
 UNIQUE(submission_id,attempt_number), UNIQUE(id,submission_id), UNIQUE(id,run_id)
);
CREATE INDEX ON judging_attempts(status,created_at);
CREATE INDEX ON judging_attempts(invoker_id,status,lease_until);
CREATE TABLE test_results (
 attempt_id uuid REFERENCES judging_attempts, test_id text, verdict text NOT NULL,
 score numeric(18,6) CHECK(score BETWEEN 0 AND 999999999999), checker_metadata jsonb NOT NULL DEFAULT '{}', PRIMARY KEY(attempt_id,test_id)
);
CREATE TABLE subgroup_results (
 attempt_id uuid REFERENCES judging_attempts, subgroup_id text,
 score numeric(18,6) NOT NULL CHECK(score BETWEEN 0 AND 999999999999), PRIMARY KEY(attempt_id,subgroup_id)
);
CREATE TABLE queue_entries (
 attempt_id uuid PRIMARY KEY REFERENCES judging_attempts, contest_id uuid NOT NULL REFERENCES contests,
 participant_id uuid NOT NULL, status text NOT NULL CHECK(status IN ('waiting','queued','assigned','done')),
 created_at timestamptz NOT NULL DEFAULT now(), FOREIGN KEY(participant_id,contest_id) REFERENCES contest_participants(id,contest_id)
);
CREATE INDEX ON queue_entries(status,created_at,attempt_id);
CREATE INDEX ON queue_entries(contest_id,participant_id,status);
CREATE TABLE judging_events (
 event_id uuid PRIMARY KEY, attempt_id uuid NOT NULL, run_id uuid NOT NULL,
 sequence bigint NOT NULL CHECK(sequence>0), schema_version int NOT NULL CHECK(schema_version>0),
 kind text NOT NULL, payload jsonb NOT NULL, occurred_at timestamptz NOT NULL, received_at timestamptz NOT NULL DEFAULT now(),
 UNIQUE(attempt_id,sequence), FOREIGN KEY(attempt_id,run_id) REFERENCES judging_attempts(id,run_id)
);
CREATE TABLE outbox_events (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(), topic text NOT NULL, dedup_key text NOT NULL UNIQUE, payload jsonb NOT NULL,
 created_at timestamptz NOT NULL DEFAULT now(), published_at timestamptz
);
CREATE INDEX ON outbox_events(created_at,id) WHERE published_at IS NULL;
CREATE TABLE inbox_events (
 consumer text, event_id uuid, received_at timestamptz NOT NULL DEFAULT now(), PRIMARY KEY(consumer,event_id)
);
CREATE TABLE attempt_artifacts (
 attempt_id uuid REFERENCES judging_attempts, artifact_id uuid REFERENCES artifacts, kind text NOT NULL,
 test_id text, PRIMARY KEY(attempt_id,artifact_id)
);
CREATE TABLE grader_runs (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(), contest_id uuid NOT NULL REFERENCES contests, contest_problem_id uuid NOT NULL,
 problem_revision_id uuid NOT NULL REFERENCES problem_revisions,
 trigger_type text NOT NULL CHECK(trigger_type IN ('automatic_submission','manual')), trigger_actor uuid REFERENCES users,
 mode text NOT NULL CHECK(mode IN ('preview','publish')), status text NOT NULL CHECK(status IN ('pending','running','completed','failed','cancelled')),
 snapshot_id uuid NOT NULL DEFAULT gen_random_uuid() UNIQUE, snapshot_digest text NOT NULL, max_score numeric(18,6) NOT NULL,
 capability_hash bytea UNIQUE, capability_expires_at timestamptz, created_at timestamptz NOT NULL DEFAULT now(), finished_at timestamptz,
 FOREIGN KEY(contest_problem_id,contest_id) REFERENCES contest_problems(id,contest_id), UNIQUE(id,contest_problem_id)
);
CREATE INDEX ON grader_runs(contest_id,status,created_at DESC);
CREATE TABLE grader_participants (
 grader_run_id uuid REFERENCES grader_runs, participant_id uuid REFERENCES contest_participants, PRIMARY KEY(grader_run_id,participant_id)
);
CREATE TABLE grader_inputs (
 grader_run_id uuid REFERENCES grader_runs, participant_id uuid NOT NULL, submission_id uuid NOT NULL, judging_attempt_id uuid,
 PRIMARY KEY(grader_run_id,submission_id), FOREIGN KEY(grader_run_id,participant_id) REFERENCES grader_participants,
 FOREIGN KEY(submission_id,participant_id) REFERENCES submissions(id,participant_id),
 FOREIGN KEY(judging_attempt_id,submission_id) REFERENCES judging_attempts(id,submission_id)
);
CREATE TABLE grader_scores (
 grader_run_id uuid REFERENCES grader_runs, participant_id uuid NOT NULL, contest_problem_id uuid NOT NULL,
 score numeric(18,6) NOT NULL CHECK(score BETWEEN 0 AND 999999999999), metadata jsonb NOT NULL DEFAULT '{}',
 PRIMARY KEY(grader_run_id,participant_id), FOREIGN KEY(grader_run_id,participant_id) REFERENCES grader_participants,
 FOREIGN KEY(grader_run_id,contest_problem_id) REFERENCES grader_runs(id,contest_problem_id)
);
CREATE TABLE score_publications (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(), contest_id uuid NOT NULL REFERENCES contests,
 grader_run_id uuid NOT NULL UNIQUE REFERENCES grader_runs, created_by uuid REFERENCES users,
 status text NOT NULL DEFAULT 'published' CHECK(status='published'), created_at timestamptz NOT NULL DEFAULT now(), UNIQUE(id,contest_id)
);
CREATE INDEX ON score_publications(contest_id,status,created_at DESC);
CREATE TABLE score_publication_values (
 publication_id uuid REFERENCES score_publications, participant_id uuid REFERENCES contest_participants,
 contest_problem_id uuid REFERENCES contest_problems, score numeric(18,6) NOT NULL CHECK(score BETWEEN 0 AND 999999999999),
 PRIMARY KEY(publication_id,participant_id,contest_problem_id)
);
ALTER TABLE contests ADD COLUMN current_score_publication_id uuid;
ALTER TABLE contests ADD FOREIGN KEY(current_score_publication_id,id) REFERENCES score_publications(id,contest_id);
CREATE INDEX ON contests(current_score_publication_id);
CREATE TABLE authorization_outbox (
 id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY, subject text NOT NULL, relation text NOT NULL, object text NOT NULL,
 remove boolean NOT NULL DEFAULT false, created_at timestamptz NOT NULL DEFAULT now(), published_at timestamptz,
 attempts int NOT NULL DEFAULT 0, next_try_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX ON authorization_outbox(next_try_at,id) WHERE published_at IS NULL;
CREATE TABLE audit_logs (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(), actor_id uuid REFERENCES users, action text NOT NULL,
 object_id uuid NOT NULL, details jsonb NOT NULL DEFAULT '{}', created_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE system_settings (key text PRIMARY KEY, value jsonb NOT NULL);

CREATE FUNCTION immutable_record() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN RAISE EXCEPTION 'immutable record in %', TG_TABLE_NAME USING ERRCODE='23514'; END $$;
CREATE TRIGGER immutable_submission BEFORE UPDATE OR DELETE ON submissions FOR EACH ROW EXECUTE FUNCTION immutable_record();
CREATE TRIGGER immutable_material_revision BEFORE UPDATE OR DELETE ON material_revisions FOR EACH ROW EXECUTE FUNCTION immutable_record();
CREATE TRIGGER immutable_grader_inputs BEFORE UPDATE OR DELETE ON grader_inputs FOR EACH ROW EXECUTE FUNCTION immutable_record();
CREATE TRIGGER immutable_grader_participants BEFORE UPDATE OR DELETE ON grader_participants FOR EACH ROW EXECUTE FUNCTION immutable_record();
CREATE TRIGGER immutable_grader_scores BEFORE UPDATE OR DELETE ON grader_scores FOR EACH ROW EXECUTE FUNCTION immutable_record();
CREATE TRIGGER immutable_score_publication BEFORE UPDATE OR DELETE ON score_publications FOR EACH ROW EXECUTE FUNCTION immutable_record();
CREATE TRIGGER immutable_score_values BEFORE UPDATE OR DELETE ON score_publication_values FOR EACH ROW EXECUTE FUNCTION immutable_record();
CREATE TRIGGER immutable_published_revision BEFORE UPDATE OR DELETE ON problem_revisions FOR EACH ROW
 WHEN (OLD.status='published') EXECUTE FUNCTION immutable_record();
CREATE TRIGGER immutable_terminal_attempt BEFORE UPDATE OR DELETE ON judging_attempts FOR EACH ROW
 WHEN (OLD.status IN ('finished','error','timeout','cancelled')) EXECUTE FUNCTION immutable_record();
CREATE FUNCTION protect_revision_child() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE old_id uuid; new_id uuid;
BEGIN
 IF TG_OP <> 'INSERT' THEN old_id := OLD.revision_id; END IF;
 IF TG_OP <> 'DELETE' THEN new_id := NEW.revision_id; END IF;
 PERFORM id FROM problem_revisions WHERE id IN (old_id,new_id) ORDER BY id FOR UPDATE;
 IF EXISTS(SELECT FROM problem_revisions WHERE id IN (old_id,new_id) AND status='published') THEN
   RAISE EXCEPTION 'published revision contents are immutable' USING ERRCODE='23514';
 END IF;
 IF TG_OP='DELETE' THEN RETURN OLD; END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER protect_statements BEFORE INSERT OR UPDATE OR DELETE ON problem_statements FOR EACH ROW EXECUTE FUNCTION protect_revision_child();
CREATE TRIGGER protect_revision_languages BEFORE INSERT OR UPDATE OR DELETE ON problem_revision_languages FOR EACH ROW EXECUTE FUNCTION protect_revision_child();
CREATE FUNCTION require_published_revision() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF NOT EXISTS(SELECT FROM problem_revisions WHERE id=NEW.problem_revision_id AND status='published') THEN
   RAISE EXCEPTION 'contest requires a published revision' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER pin_published_revision BEFORE INSERT OR UPDATE OF problem_revision_id ON contest_problems
 FOR EACH ROW EXECUTE FUNCTION require_published_revision();
CREATE FUNCTION require_contest_problem_revision() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF NOT EXISTS(SELECT FROM contest_problems WHERE id=NEW.contest_problem_id AND contest_id=NEW.contest_id AND problem_id=NEW.problem_id AND problem_revision_id=NEW.problem_revision_id AND detached_at IS NULL FOR SHARE) THEN
  RAISE EXCEPTION 'submission must match contest problem pinned revision' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER validate_submission_problem BEFORE INSERT ON submissions FOR EACH ROW EXECUTE FUNCTION require_contest_problem_revision();
CREATE FUNCTION validate_revision_publish() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF NEW.status='published' AND (TG_OP='INSERT' OR OLD.status='draft') THEN
  IF NOT EXISTS(SELECT FROM problem_statements WHERE revision_id=NEW.id) OR NOT EXISTS(SELECT FROM problem_revision_languages WHERE revision_id=NEW.id) THEN
   RAISE EXCEPTION 'revision needs a statement and supported language' USING ERRCODE='23514';
  END IF;
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER validate_revision_publish BEFORE INSERT OR UPDATE ON problem_revisions FOR EACH ROW EXECUTE FUNCTION validate_revision_publish();
