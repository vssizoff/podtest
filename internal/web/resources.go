package web

import (
	"context"
	stdjson "encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"podtest/internal/catalog"
	"podtest/internal/grader"
	"podtest/internal/judging"
	"podtest/internal/material"
	problems "podtest/internal/problem"
	"podtest/internal/scoreboard"
)

func (a *api) revisionManager() problems.Manager {
	return problems.Manager{Pool: a.pool, Artifacts: a.artifacts, Renderer: a.renderer}
}
func (a *api) materialManager() material.Manager {
	return material.Manager{Pool: a.pool, Renderer: a.renderer}
}
func (a *api) submissionManager() judging.Submissions { return *a.submissions }
func (a *api) graderManager() grader.Manager          { return grader.Manager{Pool: a.pool} }
func (a *api) scoreboardManager() scoreboard.Manager  { return scoreboard.Manager{Pool: a.pool} }
func (a *api) principalID(r *http.Request) string     { return a.principal(r).ID }

func (a *api) polygonImport(w http.ResponseWriter, r *http.Request) {
	data, ok := readPolygon(w, r)
	if !ok {
		return
	}
	report, err := problems.InspectPolygon(data)
	if err != nil {
		problemErr(w, r, 422, "polygon_unsupported", err.Error())
		return
	}
	json(w, 200, report)
}

func readPolygon(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	const limit = problems.MaxPolygonBytes
	if r.ContentLength > limit+(64<<10) {
		problemErr(w, r, 413, "file_too_large", "archive exceeds 64 MiB")
		return nil, false
	}
	r.Body = http.MaxBytesReader(w, r.Body, limit+(64<<10))
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		problemErr(w, r, 400, "invalid_archive", "invalid multipart upload")
		return nil, false
	}
	defer r.MultipartForm.RemoveAll()
	f, _, err := r.FormFile("file")
	if err != nil {
		problemErr(w, r, 422, "invalid_archive", "file field is required")
		return nil, false
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil || len(data) > limit {
		problemErr(w, r, 413, "file_too_large", "archive exceeds 64 MiB")
		return nil, false
	}
	return data, true
}

func (a *api) importContestPolygon(w http.ResponseWriter, r *http.Request) {
	contest := chi.URLParam(r, "id")
	if err := a.catalog.Require(r.Context(), a.principalID(r), "can_manage", "contest", contest); err != nil {
		a.failure(w, r, err)
		return
	}
	data, ok := readPolygon(w, r)
	if !ok {
		return
	}
	out, err := a.revisionManager().ImportPolygon(r.Context(), a.catalog.Auth, a.principalID(r), contest, data)
	if errors.Is(err, problems.ErrArchive) || errors.Is(err, problems.ErrPolygon) {
		problemErr(w, r, 422, "polygon_unsupported", err.Error())
		return
	}
	if err != nil {
		a.failure(w, r, err)
		return
	}
	out.Report.Warnings = []string{"Full ZIP retained. Polygon conversion and execution require the future adapter and Invoker."}
	json(w, 201, out)
}

func (a *api) revisions(w http.ResponseWriter, r *http.Request) {
	items, err := a.revisionManager().List(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		a.failure(w, r, err)
		return
	}
	json(w, 200, map[string]any{"items": items})
}
func (a *api) createRevision(w http.ResponseWriter, r *http.Request) {
	var in problems.RevisionInput
	if !decode(w, r, &in) {
		return
	}
	if err := a.catalog.Require(r.Context(), a.principalID(r), "can_edit", "problem", chi.URLParam(r, "id")); err != nil {
		a.failure(w, r, err)
		return
	}
	item, err := a.revisionManager().Create(r.Context(), chi.URLParam(r, "id"), in)
	if err != nil {
		a.failure(w, r, err)
		return
	}
	json(w, 201, item)
}
func (a *api) statements(w http.ResponseWriter, r *http.Request) {
	items, err := a.revisionManager().Statements(r.Context(), chi.URLParam(r, "id"), chi.URLParam(r, "revisionId"))
	if err != nil {
		a.failure(w, r, err)
		return
	}
	editable, err := a.catalog.Allowed(r.Context(), a.principalID(r), "can_edit", "problem", chi.URLParam(r, "id"))
	if err != nil {
		a.failure(w, r, err)
		return
	}
	if !editable {
		for i := range items {
			items[i].Source = ""
		}
	}
	json(w, 200, map[string]any{"items": items})
}
func (a *api) setStatement(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Source string `json:"source"`
	}
	if !decode(w, r, &in) {
		return
	}
	if err := a.catalog.Require(r.Context(), a.principalID(r), "can_edit", "problem", chi.URLParam(r, "id")); err != nil {
		a.failure(w, r, err)
		return
	}
	item, err := a.revisionManager().SetStatement(r.Context(), chi.URLParam(r, "id"), chi.URLParam(r, "revisionId"), chi.URLParam(r, "locale"), in.Source)
	if err != nil {
		a.failure(w, r, err)
		return
	}
	json(w, 200, item)
}
func (a *api) setLanguage(w http.ResponseWriter, r *http.Request) {
	if err := a.catalog.Require(r.Context(), a.principalID(r), "can_edit", "problem", chi.URLParam(r, "id")); err != nil {
		a.failure(w, r, err)
		return
	}
	if err := a.revisionManager().SetLanguage(r.Context(), chi.URLParam(r, "id"), chi.URLParam(r, "revisionId"), chi.URLParam(r, "code")); err != nil {
		a.failure(w, r, err)
		return
	}
	w.WriteHeader(204)
}
func (a *api) setPackage(w http.ResponseWriter, r *http.Request) {
	const bodyLimit = problems.MaxPackageBytes + (64 << 10)
	if r.ContentLength > bodyLimit {
		problemErr(w, r, 413, "file_too_large", "archive exceeds 16 MiB")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, bodyLimit)
	f, _, err := r.FormFile("file")
	if r.MultipartForm != nil {
		defer r.MultipartForm.RemoveAll()
	}
	if err != nil {
		var limit *http.MaxBytesError
		if errors.As(err, &limit) {
			problemErr(w, r, 413, "file_too_large", "archive exceeds 16 MiB")
			return
		}
		problemErr(w, r, 422, "invalid_archive", "file field is required")
		return
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, problems.MaxPackageBytes+1))
	if err != nil {
		a.failure(w, r, err)
		return
	}
	if len(data) > problems.MaxPackageBytes {
		problemErr(w, r, 413, "file_too_large", "archive exceeds 16 MiB")
		return
	}
	if err = a.revisionManager().SetPackage(r.Context(), a.principalID(r), chi.URLParam(r, "id"), chi.URLParam(r, "revisionId"), data); err != nil {
		a.failure(w, r, err)
		return
	}
	w.WriteHeader(204)
}
func (a *api) validateRevision(w http.ResponseWriter, r *http.Request) {
	if err := a.catalog.Require(r.Context(), a.principalID(r), "can_edit", "problem", chi.URLParam(r, "id")); err != nil {
		a.failure(w, r, err)
		return
	}
	if err := a.revisionManager().Validate(r.Context(), chi.URLParam(r, "id"), chi.URLParam(r, "revisionId")); err != nil {
		a.failure(w, r, err)
		return
	}
	json(w, 200, map[string]any{"valid": true, "warnings": []string{}})
}
func (a *api) publishRevision(w http.ResponseWriter, r *http.Request) {
	if err := a.catalog.Require(r.Context(), a.principalID(r), "can_edit", "problem", chi.URLParam(r, "id")); err != nil {
		a.failure(w, r, err)
		return
	}
	if err := a.revisionManager().Publish(r.Context(), a.principalID(r), chi.URLParam(r, "id"), chi.URLParam(r, "revisionId")); err != nil {
		a.failure(w, r, err)
		return
	}
	w.WriteHeader(204)
}
func (a *api) contestProblems(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if err := a.catalog.Require(r.Context(), a.principalID(r), "can_view", "contest", id); err != nil {
		a.failure(w, r, err)
		return
	}
	items, err := a.revisionManager().ContestList(r.Context(), id)
	if err != nil {
		a.failure(w, r, err)
		return
	}
	json(w, 200, map[string]any{"items": items})
}
func (a *api) attachProblem(w http.ResponseWriter, r *http.Request) {
	var in problems.AttachInput
	if !decode(w, r, &in) {
		return
	}
	if err := a.catalog.Require(r.Context(), a.principalID(r), "can_manage", "contest", chi.URLParam(r, "id")); err != nil {
		a.failure(w, r, err)
		return
	}
	if !a.requireObject(w, r, "can_view", "problem", in.ProblemID) {
		return
	}
	id, err := a.revisionManager().Attach(r.Context(), a.principalID(r), chi.URLParam(r, "id"), in)
	if err != nil {
		a.failure(w, r, err)
		return
	}
	json(w, 201, map[string]string{"id": id})
}
func (a *api) detachProblem(w http.ResponseWriter, r *http.Request) {
	if err := a.catalog.Require(r.Context(), a.principalID(r), "can_manage", "contest", chi.URLParam(r, "id")); err != nil {
		a.failure(w, r, err)
		return
	}
	if err := a.revisionManager().Detach(r.Context(), a.principalID(r), chi.URLParam(r, "id"), chi.URLParam(r, "problemId")); err != nil {
		a.failure(w, r, err)
		return
	}
	w.WriteHeader(204)
}
func (a *api) switchProblemRevision(w http.ResponseWriter, r *http.Request) {
	if err := a.catalog.Require(r.Context(), a.principalID(r), "can_manage", "contest", chi.URLParam(r, "id")); err != nil {
		a.failure(w, r, err)
		return
	}
	var in struct {
		RevisionID string `json:"problem_revision_id"`
	}
	if !decode(w, r, &in) {
		return
	}
	if err := a.revisionManager().Switch(r.Context(), a.principalID(r), chi.URLParam(r, "id"), chi.URLParam(r, "problemId"), in.RevisionID); err != nil {
		a.failure(w, r, err)
		return
	}
	w.WriteHeader(204)
}
func (a *api) task(w http.ResponseWriter, r *http.Request) {
	contest, cp := chi.URLParam(r, "id"), chi.URLParam(r, "problemId")
	if err := a.catalog.Require(r.Context(), a.principalID(r), "can_view", "contest", contest); err != nil {
		a.failure(w, r, err)
		return
	}
	item, err := a.revisionManager().Task(r.Context(), contest, cp)
	if err != nil {
		a.failure(w, r, err)
		return
	}
	json(w, 200, item)
}

func (a *api) createMaterial(w http.ResponseWriter, r *http.Request) {
	var in material.Input
	if !decode(w, r, &in) {
		return
	}
	item, err := a.materialManager().Create(r.Context(), a.principalID(r), in)
	if err != nil {
		a.failure(w, r, err)
		return
	}
	json(w, 201, item)
}
func (a *api) getMaterial(w http.ResponseWriter, r *http.Request) {
	var item material.Material
	err := a.pool.QueryRow(r.Context(), `SELECT id,title,description FROM theory_materials WHERE id=$1 AND archived_at IS NULL`, chi.URLParam(r, "id")).Scan(&item.ID, &item.Title, &item.Description)
	if err != nil {
		a.failure(w, r, err)
		return
	}
	json(w, 200, item)
}
func (a *api) createMaterialRevision(w http.ResponseWriter, r *http.Request) {
	var in material.RevisionInput
	if !decode(w, r, &in) {
		return
	}
	if err := a.catalog.Require(r.Context(), a.principalID(r), "can_edit", "material", chi.URLParam(r, "id")); err != nil {
		a.failure(w, r, err)
		return
	}
	item, err := a.materialManager().CreateRevision(r.Context(), chi.URLParam(r, "id"), in)
	if err != nil {
		a.failure(w, r, err)
		return
	}
	json(w, 201, item)
}
func (a *api) renderPreview(w http.ResponseWriter, r *http.Request) {
	var in material.RevisionInput
	if !decode(w, r, &in) {
		return
	}
	item, err := a.renderer.Render(r.Context(), in.Format, in.Source)
	if err != nil {
		problemErr(w, r, 422, "render_error", err.Error())
		return
	}
	json(w, 200, item)
}
func (a *api) attachMaterial(w http.ResponseWriter, r *http.Request) {
	var in material.Attachment
	if !decode(w, r, &in) {
		return
	}
	if err := a.catalog.Require(r.Context(), a.principalID(r), "can_manage", "contest", chi.URLParam(r, "id")); err != nil {
		a.failure(w, r, err)
		return
	}
	if !a.requireObject(w, r, "can_view", "material", in.MaterialID) {
		return
	}
	if err := a.materialManager().Attach(r.Context(), chi.URLParam(r, "id"), in); err != nil {
		a.failure(w, r, err)
		return
	}
	w.WriteHeader(204)
}
func (a *api) contestMaterials(w http.ResponseWriter, r *http.Request) {
	if err := a.catalog.Require(r.Context(), a.principalID(r), "can_view", "contest", chi.URLParam(r, "id")); err != nil {
		a.failure(w, r, err)
		return
	}
	items, err := a.materialManager().List(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		a.failure(w, r, err)
		return
	}
	filtered := items[:0]
	for _, item := range items {
		if item.Visibility == "managers" {
			ok, e := a.catalog.Allowed(r.Context(), a.principalID(r), "can_manage", "contest", chi.URLParam(r, "id"))
			if e != nil {
				a.failure(w, r, e)
				return
			}
			if !ok {
				continue
			}
		}
		filtered = append(filtered, item)
	}
	json(w, 200, map[string]any{"items": filtered})
}

func (a *api) submit(w http.ResponseWriter, r *http.Request) {
	if r.ContentLength > 300<<10 {
		problemErr(w, r, 413, "source_too_large", "source exceeds 256 KiB")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 300<<10)
	if err := r.ParseMultipartForm(256 << 10); err != nil {
		problemErr(w, r, 400, "invalid_upload", "multipart source is required")
		return
	}
	defer r.MultipartForm.RemoveAll()
	f, _, err := r.FormFile("source")
	if err != nil {
		problemErr(w, r, 422, "invalid_upload", "source field is required")
		return
	}
	defer f.Close()
	source, err := io.ReadAll(io.LimitReader(f, (256<<10)+1))
	if err != nil || len(source) > 256<<10 {
		problemErr(w, r, 413, "source_too_large", "source exceeds 256 KiB")
		return
	}
	contest, cp := chi.URLParam(r, "id"), chi.URLParam(r, "problemId")
	// Submit checks that the task belongs to this contest and pins its revision.
	if !a.requireObject(w, r, "can_submit", "contest", contest) {
		return
	}
	if !uuidRE.MatchString(cp) {
		problemErr(w, r, 422, "invalid_id", "expected a UUID")
		return
	}
	item, err := a.submissionManager().Submit(r.Context(), a.principalID(r), contest, cp, r.FormValue("language"), source)
	if err != nil {
		a.failure(w, r, err)
		return
	}
	json(w, 202, item)
}
func (a *api) mySubmissions(w http.ResponseWriter, r *http.Request) {
	items, err := a.submissionManager().List(r.Context(), a.principalID(r), chi.URLParam(r, "id"), chi.URLParam(r, "problemId"))
	if err != nil {
		a.failure(w, r, err)
		return
	}
	json(w, 200, map[string]any{"items": items})
}
func (a *api) mySubmissionsList(w http.ResponseWriter, r *http.Request) {
	items, err := a.submissionManager().List(r.Context(), a.principalID(r), "", "")
	if err != nil {
		a.failure(w, r, err)
		return
	}
	json(w, 200, map[string]any{"items": items})
}

type visibility struct{ Policy string }

func (a *api) submissionAccess(ctx context.Context, user, id string) (judging.Submission, string, bool, error) {
	sub, err := a.submissionManager().Get(ctx, id)
	if err != nil {
		return sub, "", false, err
	}
	var owner, manager bool
	var policy string
	err = a.pool.QueryRow(ctx, `SELECT s.submitted_by=$2,cp.verdict_visibility FROM submissions s JOIN contest_problems cp ON cp.id=s.contest_problem_id WHERE s.id=$1`, id, user).Scan(&owner, &policy)
	if err != nil {
		return sub, "", false, err
	}
	manager, err = a.catalog.Allowed(ctx, user, "can_manage", "contest", sub.ContestID)
	if err != nil {
		return sub, "", false, err
	}
	if !owner && !manager {
		return sub, "", false, catalog.ErrDenied
	}
	if manager {
		policy = "manager"
	}
	return sub, policy, manager, nil
}
func (a *api) getSubmission(w http.ResponseWriter, r *http.Request) {
	sub, policy, _, err := a.submissionAccess(r.Context(), a.principalID(r), chi.URLParam(r, "id"))
	if err != nil {
		a.failure(w, r, err)
		return
	}
	attempts, err := a.submissionManager().Attempts(r.Context(), sub.ID, policy)
	if err != nil {
		a.failure(w, r, err)
		return
	}
	json(w, 200, map[string]any{"submission": sub, "attempts": attempts})
}
func (a *api) attempts(w http.ResponseWriter, r *http.Request) {
	sub, policy, _, err := a.submissionAccess(r.Context(), a.principalID(r), chi.URLParam(r, "id"))
	if err != nil {
		a.failure(w, r, err)
		return
	}
	items, err := a.submissionManager().Attempts(r.Context(), sub.ID, policy)
	if err != nil {
		a.failure(w, r, err)
		return
	}
	json(w, 200, map[string]any{"items": items})
}
func (a *api) rejudge(w http.ResponseWriter, r *http.Request) {
	sub, _, manager, err := a.submissionAccess(r.Context(), a.principalID(r), chi.URLParam(r, "id"))
	if err != nil {
		a.failure(w, r, err)
		return
	}
	if !manager {
		problemErr(w, r, 403, "forbidden", "manager permission required")
		return
	}
	var in struct {
		RevisionID string `json:"problem_revision_id"`
	}
	if r.Body != nil && r.ContentLength != 0 && !decode(w, r, &in) {
		return
	}
	attempt, err := a.submissionManager().Rejudge(r.Context(), a.principalID(r), sub.ID, in.RevisionID)
	if err != nil {
		a.failure(w, r, err)
		return
	}
	json(w, 202, map[string]string{"judging_attempt_id": attempt})
}
func (a *api) download(w http.ResponseWriter, r *http.Request) {
	sub, _, manager, err := a.submissionAccess(r.Context(), a.principalID(r), chi.URLParam(r, "id"))
	if err != nil {
		a.failure(w, r, err)
		return
	}
	artifactID := chi.URLParam(r, "artifactId")
	var allowed bool
	err = a.pool.QueryRow(r.Context(), `SELECT EXISTS(SELECT FROM submissions WHERE id=$3 AND source_artifact_id=$1 AND (submitted_by=$2 OR $4)) OR ($4 AND EXISTS(SELECT FROM attempt_artifacts x JOIN judging_attempts j ON j.id=x.attempt_id WHERE x.artifact_id=$1 AND j.submission_id=$3))`, artifactID, a.principalID(r), sub.ID, manager).Scan(&allowed)
	if err != nil || !allowed {
		a.failure(w, r, catalog.ErrDenied)
		return
	}
	reader, err := a.artifacts.Open(r.Context(), artifactID)
	if err != nil {
		a.failure(w, r, err)
		return
	}
	defer reader.Close()
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", "attachment")
	_, _ = io.Copy(w, reader)
}
func (a *api) events(w http.ResponseWriter, r *http.Request) {
	sub, _, _, err := a.submissionAccess(r.Context(), a.principalID(r), chi.URLParam(r, "id"))
	if err != nil {
		a.failure(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("X-Accel-Buffering", "no")
	flusher, ok := w.(http.Flusher)
	if !ok {
		return
	}
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	deadline := time.NewTimer(45 * time.Second)
	defer deadline.Stop()
	send := func() bool {
		user, _, e := a.sessions.Current(r)
		if e != nil {
			return false
		}
		_, policy, _, e := a.submissionAccess(r.Context(), user.ID, sub.ID)
		if e != nil {
			return false
		}
		attempts, e := a.submissionManager().Attempts(r.Context(), sub.ID, policy)
		if e == nil {
			data, _ := stdjson.Marshal(attempts)
			fmt.Fprintf(w, "event: submission\ndata: %s\n\n", data)
			flusher.Flush()
		}
		return e == nil
	}
	if !send() {
		return
	}
	for {
		select {
		case <-r.Context().Done():
			return
		case <-deadline.C:
			return
		case <-ticker.C:
			if !send() {
				return
			}
		}
	}
}

func (a *api) graderRun(w http.ResponseWriter, r *http.Request) {
	contest, cp := chi.URLParam(r, "id"), chi.URLParam(r, "problemId")
	if err := a.catalog.Require(r.Context(), a.principalID(r), "can_manage", "contest", contest); err != nil {
		a.failure(w, r, err)
		return
	}
	var in struct {
		Mode string `json:"mode"`
	}
	if !decode(w, r, &in) {
		return
	}
	run, _, err := a.graderManager().Create(r.Context(), a.principalID(r), contest, cp, "manual", in.Mode)
	if err != nil {
		a.failure(w, r, err)
		return
	}
	json(w, 202, run)
}
func (a *api) graderSnapshot(w http.ResponseWriter, r *http.Request) {
	run, scope, err := a.graderManager().Snapshot(r.Context(), chi.URLParam(r, "id"), bearer(r))
	if err != nil {
		problemErr(w, r, 403, "invalid_capability", "run capability invalid or expired")
		return
	}
	json(w, 200, map[string]any{"run": run, "scope": scope})
}
func (a *api) graderResult(w http.ResponseWriter, r *http.Request) {
	run, _, err := a.graderManager().Snapshot(r.Context(), chi.URLParam(r, "id"), bearer(r))
	if err != nil {
		problemErr(w, r, 403, "invalid_capability", "run capability invalid or expired")
		return
	}
	var payload grader.ScorePayload
	if !decode(w, r, &payload) {
		return
	}
	if err = a.graderManager().Submit(r.Context(), run.ID, bearer(r), payload); err != nil {
		a.failure(w, r, err)
		return
	}
	w.WriteHeader(204)
}
func (a *api) graderRuns(w http.ResponseWriter, r *http.Request) {
	contest := chi.URLParam(r, "id")
	if err := a.catalog.Require(r.Context(), a.principalID(r), "can_manage", "contest", contest); err != nil {
		a.failure(w, r, err)
		return
	}
	rows, err := a.pool.Query(r.Context(), `SELECT id,contest_id,contest_problem_id,snapshot_id,snapshot_digest,mode,status FROM grader_runs WHERE contest_id=$1 ORDER BY created_at DESC LIMIT 100`, contest)
	if err != nil {
		a.failure(w, r, err)
		return
	}
	items, err := pgx.CollectRows(rows, pgx.RowToStructByPos[grader.Run])
	if err != nil {
		a.failure(w, r, err)
		return
	}
	json(w, 200, map[string]any{"items": items})
}
func (a *api) getGraderRun(w http.ResponseWriter, r *http.Request) {
	var item grader.Run
	err := a.pool.QueryRow(r.Context(), `SELECT id,contest_id,contest_problem_id,snapshot_id,snapshot_digest,mode,status FROM grader_runs WHERE id=$1`, chi.URLParam(r, "id")).Scan(&item.ID, &item.ContestID, &item.ProblemID, &item.SnapshotID, &item.SnapshotDigest, &item.Mode, &item.Status)
	if err != nil {
		a.failure(w, r, err)
		return
	}
	if err = a.catalog.Require(r.Context(), a.principalID(r), "can_manage", "contest", item.ContestID); err != nil {
		a.failure(w, r, err)
		return
	}
	json(w, 200, item)
}
func (a *api) publishGrader(w http.ResponseWriter, r *http.Request) {
	var contest string
	if err := a.pool.QueryRow(r.Context(), `SELECT contest_id FROM grader_runs WHERE id=$1`, chi.URLParam(r, "id")).Scan(&contest); err != nil {
		a.failure(w, r, err)
		return
	}
	if err := a.catalog.Require(r.Context(), a.principalID(r), "can_manage", "contest", contest); err != nil {
		a.failure(w, r, err)
		return
	}
	publication, err := a.graderManager().Publish(r.Context(), a.principalID(r), chi.URLParam(r, "id"))
	if err != nil {
		a.failure(w, r, err)
		return
	}
	json(w, 200, map[string]string{"score_publication_id": publication})
}

func (a *api) scoreboard(w http.ResponseWriter, r *http.Request) {
	contest := chi.URLParam(r, "id")
	if !a.requireObject(w, r, "can_view", "contest", contest) {
		return
	}
	manager, err := a.catalog.Allowed(r.Context(), a.principalID(r), "can_manage", "contest", contest)
	if err != nil {
		a.failure(w, r, err)
		return
	}
	board, err := a.scoreboardManager().Get(r.Context(), "", contest, manager)
	if err != nil {
		if errors.Is(err, scoreboard.ErrDenied) {
			a.failure(w, r, catalog.ErrDenied)
		} else {
			a.failure(w, r, err)
		}
		return
	}
	json(w, 200, board)
}
func (a *api) scoreboardMe(w http.ResponseWriter, r *http.Request) {
	contest := chi.URLParam(r, "id")
	if !a.requireObject(w, r, "can_view", "contest", contest) {
		return
	}
	board, err := a.scoreboardManager().Get(r.Context(), a.principalID(r), contest, false)
	if err != nil {
		a.failure(w, r, err)
		return
	}
	json(w, 200, board)
}
func (a *api) publications(w http.ResponseWriter, r *http.Request) {
	contest := chi.URLParam(r, "id")
	if err := a.catalog.Require(r.Context(), a.principalID(r), "can_manage", "contest", contest); err != nil {
		a.failure(w, r, err)
		return
	}
	rows, err := a.pool.Query(r.Context(), `SELECT id,grader_run_id,status,created_at FROM score_publications WHERE contest_id=$1 ORDER BY created_at DESC LIMIT 100`, contest)
	if err != nil {
		a.failure(w, r, err)
		return
	}
	items, err := pgx.CollectRows(rows, pgx.RowToStructByPos[struct {
		ID     string
		RunID  string
		Status string
		At     time.Time
	}])
	if err != nil {
		a.failure(w, r, err)
		return
	}
	json(w, 200, map[string]any{"items": items})
}

func bearer(r *http.Request) string {
	v := r.Header.Get("Authorization")
	var token string
	if _, err := fmt.Sscanf(v, "Bearer %s", &token); err != nil {
		return ""
	}
	return token
}
func problemErr(w http.ResponseWriter, r *http.Request, status int, code, message string) {
	problem(w, r, status, code, message)
}
