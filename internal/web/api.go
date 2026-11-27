package web

import (
	"context"
	stdjson "encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"podtest/internal/artifact"
	"podtest/internal/auth"
	"podtest/internal/authorization"
	"podtest/internal/catalog"
	"podtest/internal/grader"
	"podtest/internal/judging"
	"podtest/internal/material"
	problems "podtest/internal/problem"
	"podtest/internal/statement"
)

type contextKey int

const userKey contextKey = 1

type api struct {
	pool        *pgxpool.Pool
	sessions    *auth.Sessions
	catalog     catalog.Manager
	submissions *judging.Submissions
	renderer    *statement.Renderer
	artifacts   *artifact.Store
}
type Error struct {
	Error struct {
		Code      string `json:"code"`
		Message   string `json:"message"`
		RequestID string `json:"request_id,omitempty"`
	} `json:"error"`
}

func Router(pool *pgxpool.Pool, sessions *auth.Sessions, authz *authorization.Manager, store *artifact.Store) http.Handler {
	a := &api{pool: pool, sessions: sessions, catalog: catalog.Manager{Pool: pool, Auth: authz}, submissions: &judging.Submissions{Pool: pool, Artifacts: store}, renderer: statement.NewRenderer(), artifacts: store}
	r := chi.NewRouter()
	r.Use(middleware.RequestID, middleware.Recoverer, securityHeaders, requestLog)
	r.Route("/auth", func(r chi.Router) {
		r.Get("/login", a.login)
		r.Get("/callback", a.callback)
		r.Get("/session", a.session)
		r.Post("/logout", a.logout)
	})
	r.Route("/api/v1", func(r chi.Router) {
		r.Get("/health/ready", a.ready)
		r.Get("/health/live", func(w http.ResponseWriter, _ *http.Request) { json(w, 200, map[string]string{"status": "ok"}) })
		r.Group(func(p chi.Router) {
			p.Use(a.protected)
			p.Get("/me", a.me)
			p.Get("/courses", a.list("course"))
			p.Post("/courses", a.create("course"))
			p.Get("/courses/{id}", a.get("course"))
			p.Patch("/courses/{id}", a.update("course"))
			p.Delete("/courses/{id}", a.archive("course"))
			p.Get("/courses/{id}/contests", a.courseContests)
			p.Post("/courses/{id}/contests", a.courseAddContest)
			p.Delete("/courses/{id}/contests/{contestId}", a.courseRemoveContest)
			p.Get("/courses/{id}/members", a.listRoles("course"))
			p.Put("/courses/{id}/members/{userId}/{role}", a.role("course", false))
			p.Delete("/courses/{id}/members/{userId}/{role}", a.role("course", true))
			p.Get("/contests", a.list("contest"))
			p.Post("/contests", a.create("contest"))
			p.Get("/contests/{id}", a.get("contest"))
			p.Patch("/contests/{id}", a.update("contest"))
			p.Delete("/contests/{id}", a.archive("contest"))
			p.Get("/contests/{id}/settings", a.getSettings)
			p.Patch("/contests/{id}/settings", a.setSettings)
			p.Post("/contests/{id}/problems/import/polygon", a.importContestPolygon)
			p.Get("/contests/{id}/participants", a.participants)
			p.Post("/contests/{id}/participants", a.registerParticipant)
			p.Delete("/contests/{id}/participants/{userId}", a.removeParticipant)
			p.Get("/contests/{id}/members", a.listRoles("contest"))
			p.Put("/contests/{id}/members/{userId}/{role}", a.role("contest", false))
			p.Delete("/contests/{id}/members/{userId}/{role}", a.role("contest", true))
			p.Get("/problems", a.list("problem"))
			p.Post("/problems", a.create("problem"))
			p.Get("/problems/{id}", a.get("problem"))
			p.Patch("/problems/{id}", a.update("problem"))
			p.Delete("/problems/{id}", a.archive("problem"))
			p.Post("/problems/import/polygon", a.polygonImport)
			p.With(a.resource("problem", "can_view")).Get("/problems/{id}/revisions", a.revisions)
			p.With(a.resource("problem", "can_edit")).Post("/problems/{id}/revisions", a.createRevision)
			p.With(a.resource("problem", "can_view")).Get("/problems/{id}/revisions/{revisionId}/statements", a.statements)
			p.Get("/contests/{id}/problems", a.contestProblems)
			p.Post("/contests/{id}/problems", a.attachProblem)
			p.Patch("/contests/{id}/problems/{problemId}", a.switchProblemRevision)
			p.Get("/contests/{id}/problems/{problemId}/revisions", a.pinnedProblemRevisions)
			p.Get("/contests/{id}/materials", a.contestMaterials)
			p.Get("/contests/{id}/materials/{materialId}/attachment", a.materialAttachment)
			p.Post("/materials", a.createMaterial)
			p.With(a.resource("material", "can_view")).Get("/materials/{id}", a.getMaterial)
			p.With(a.resource("material", "can_view")).Get("/materials/{id}/revisions", a.materialHistory)
			p.With(a.resource("material", "can_edit")).Post("/materials/{id}/revisions", a.createMaterialRevision)
			p.Post("/materials/render", a.renderPreview)
			p.Post("/contests/{id}/materials", a.attachMaterial)
			p.Post("/contests/{id}/problems/{problemId}/submissions", a.submit)
			p.Get("/contests/{id}/problems/{problemId}/submissions", a.taskSubmissions)
			p.Get("/contests/{id}/problems/{problemId}/submissions/me", a.mySubmissions)
			p.Get("/submissions", a.mySubmissionsList)
			p.Get("/submissions/{id}", a.getSubmission)
			p.Get("/submissions/{id}/attempts", a.attempts)
			p.Get("/submissions/{id}/source", a.submissionSource)
			p.Post("/submissions/{id}/rejudge", a.rejudge)
			p.Post("/submissions/{id}/attempts/{attemptId}/cancel", a.cancelAttempt)
			p.Get("/contests/{id}/scoreboard", a.scoreboard)
			p.Get("/contests/{id}/scoreboard/me", a.scoreboardMe)
			p.Get("/contests/{id}/score-publications", a.publications)
			p.Post("/contests/{id}/problems/{problemId}/grader-runs", a.graderRun)
			p.Get("/contests/{id}/grader-runs", a.graderRuns)
			p.Get("/grader-runs/{id}", a.getGraderRun)
			p.Post("/grader-runs/{id}/publish", a.publishGrader)
			p.Get("/contests/{id}/problems/{problemId}", a.task)
			p.Delete("/contests/{id}/problems/{problemId}", a.detachProblem)
			p.With(a.resource("problem", "can_edit")).Put("/problems/{id}/revisions/{revisionId}/statements/{locale}", a.setStatement)
			p.With(a.resource("problem", "can_edit")).Put("/problems/{id}/revisions/{revisionId}/languages/{code}", a.setLanguage)
			p.With(a.resource("problem", "can_edit")).Delete("/problems/{id}/revisions/{revisionId}/languages/{code}", a.setLanguage)
			p.With(a.resource("problem", "can_edit")).Put("/problems/{id}/revisions/{revisionId}/package", a.setPackage)
			p.With(a.resource("problem", "can_edit")).Get("/problems/{id}/revisions/{revisionId}/package/files", a.draftPackageFiles)
			p.With(a.resource("problem", "can_edit")).Patch("/problems/{id}/revisions/{revisionId}/package/files", a.editPackageFile)
			p.With(a.resource("problem", "can_edit")).Post("/problems/{id}/revisions/{revisionId}/validate", a.validateRevision)
			p.With(a.resource("problem", "can_edit")).Post("/problems/{id}/revisions/{revisionId}/publish", a.publishRevision)
			p.Get("/submissions/{id}/artifacts/{artifactId}", a.download)
			p.Get("/submissions/{id}/events", a.events)
		})
	})
	r.Get("/internal/grader/{id}/snapshot", a.graderSnapshot)
	r.Post("/internal/grader/{id}/result", a.graderResult)
	return r
}

func (a *api) resource(kind, permission string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if err := a.catalog.Require(r.Context(), a.principalID(r), permission, kind, chi.URLParam(r, "id")); err != nil {
				a.failure(w, r, err)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func (a *api) requireObject(w http.ResponseWriter, r *http.Request, permission, kind, id string) bool {
	if !uuidRE.MatchString(id) {
		problem(w, r, 422, "invalid_id", "expected a UUID")
		return false
	}
	if err := a.catalog.Require(r.Context(), a.principalID(r), permission, kind, id); err != nil {
		a.failure(w, r, err)
		return false
	}
	return true
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'self' 'unsafe-inline'; script-src 'self'; object-src 'none'; base-uri 'self'; frame-ancestors 'none'")
		w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		next.ServeHTTP(w, r)
	})
}
func requestLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		slog.Info("http request", "request_id", middleware.GetReqID(r.Context()), "method", r.Method, "path", r.URL.Path, "duration", time.Since(start))
	})
}
func (a *api) login(w http.ResponseWriter, r *http.Request) {
	if err := a.sessions.Begin(w, r); err != nil {
		problem(w, r, 500, "oidc_error", err.Error())
	}
}
func (a *api) callback(w http.ResponseWriter, r *http.Request) {
	if err := a.sessions.Callback(w, r); err != nil {
		problem(w, r, 401, "oidc_error", "identity provider authentication failed")
	}
}
func (a *api) session(w http.ResponseWriter, r *http.Request) {
	u, csrf, err := a.sessions.Current(r)
	if err != nil {
		json(w, 200, map[string]any{"authenticated": false})
		return
	}
	json(w, 200, map[string]any{"authenticated": true, "user": u, "csrf_token": csrf})
}
func (a *api) logout(w http.ResponseWriter, r *http.Request) {
	_, csrf, err := a.sessions.Current(r)
	if err != nil {
		problem(w, r, 401, "unauthorized", "session required")
		return
	}
	if !a.sessions.ValidCSRF(r, csrf) {
		problem(w, r, 403, "csrf_failed", "valid same-origin CSRF token required")
		return
	}
	if err = a.sessions.Logout(w, r); err != nil {
		problem(w, r, 500, "logout_failed", "could not revoke session")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
func (a *api) protected(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, csrf, err := a.sessions.Current(r)
		if err != nil {
			problem(w, r, 401, "unauthorized", "authentication required")
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		if r.Method != "GET" && r.Method != "HEAD" && r.Method != "OPTIONS" && !a.sessions.ValidCSRF(r, csrf) {
			problem(w, r, 403, "csrf_failed", "valid same-origin CSRF token required")
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), userKey, u)))
	})
}
func (a *api) principal(r *http.Request) auth.User { return r.Context().Value(userKey).(auth.User) }
func (a *api) ready(w http.ResponseWriter, r *http.Request) {
	if err := a.pool.Ping(r.Context()); err != nil {
		problem(w, r, 503, "not_ready", "database unavailable")
		return
	}
	json(w, 200, map[string]string{"status": "ready"})
}
func (a *api) me(w http.ResponseWriter, r *http.Request) {
	u := a.principal(r)
	json(w, 200, map[string]any{"id": u.ID, "display_name": u.Name, "email": u.Email, "avatar_url": u.Avatar})
}
func (a *api) list(typ string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		page, err := a.catalog.List(r.Context(), a.principal(r).ID, typ, r.URL.Query().Get("after"))
		if err != nil {
			a.failure(w, r, err)
			return
		}
		json(w, 200, page)
	}
}
func (a *api) create(typ string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var in catalog.Input
		if !decode(w, r, &in) {
			return
		}
		item, err := a.catalog.Create(r.Context(), a.principal(r).ID, typ, in)
		if err != nil {
			a.failure(w, r, err)
			return
		}
		json(w, 201, item)
	}
}
func (a *api) get(typ string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		item, err := a.catalog.Get(r.Context(), a.principal(r).ID, typ, chi.URLParam(r, "id"))
		if err != nil {
			a.failure(w, r, err)
			return
		}
		json(w, 200, item)
	}
}
func (a *api) update(typ string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var in catalog.Input
		if !decode(w, r, &in) {
			return
		}
		item, err := a.catalog.Update(r.Context(), a.principal(r).ID, typ, chi.URLParam(r, "id"), in)
		if err != nil {
			a.failure(w, r, err)
			return
		}
		json(w, 200, item)
	}
}
func (a *api) archive(typ string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := a.catalog.Archive(r.Context(), a.principal(r).ID, typ, chi.URLParam(r, "id")); err != nil {
			a.failure(w, r, err)
			return
		}
		w.WriteHeader(204)
	}
}
func (a *api) courseContests(w http.ResponseWriter, r *http.Request) {
	items, err := a.catalog.CourseContests(r.Context(), a.principal(r).ID, chi.URLParam(r, "id"))
	if err != nil {
		a.failure(w, r, err)
		return
	}
	json(w, 200, map[string]any{"items": items})
}
func (a *api) courseAddContest(w http.ResponseWriter, r *http.Request) {
	var in catalog.LinkInput
	if !decode(w, r, &in) {
		return
	}
	if err := a.catalog.Link(r.Context(), a.principal(r).ID, chi.URLParam(r, "id"), in.ContestID, in.Position, false); err != nil {
		a.failure(w, r, err)
		return
	}
	w.WriteHeader(204)
}
func (a *api) courseRemoveContest(w http.ResponseWriter, r *http.Request) {
	if err := a.catalog.Link(r.Context(), a.principal(r).ID, chi.URLParam(r, "id"), chi.URLParam(r, "contestId"), 0, true); err != nil {
		a.failure(w, r, err)
		return
	}
	w.WriteHeader(204)
}

type roleInput struct {
	Role string `json:"role"`
}

func (a *api) role(typ string, remove bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := a.catalog.Role(r.Context(), a.principal(r).ID, typ, chi.URLParam(r, "id"), chi.URLParam(r, "userId"), chi.URLParam(r, "role"), remove); err != nil {
			a.failure(w, r, err)
			return
		}
		w.WriteHeader(204)
	}
}
func (a *api) listRoles(typ string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		if err := a.catalog.Require(r.Context(), a.principal(r).ID, "can_manage", typ, id); err != nil {
			a.failure(w, r, err)
			return
		}
		query := `SELECT m.user_id,u.display_name,m.role FROM ` + typ + `_memberships m JOIN users u ON u.id=m.user_id WHERE m.` + typ + `_id=$1 ORDER BY u.display_name,m.role`
		rows, err := a.pool.Query(r.Context(), query, id)
		if err != nil {
			a.failure(w, r, err)
			return
		}
		defer rows.Close()
		out := []map[string]string{}
		for rows.Next() {
			var uid, name, role string
			if err := rows.Scan(&uid, &name, &role); err != nil {
				a.failure(w, r, err)
				return
			}
			out = append(out, map[string]string{"user_id": uid, "display_name": name, "role": role})
		}
		json(w, 200, map[string]any{"items": out})
	}
}
func (a *api) getSettings(w http.ResponseWriter, r *http.Request) {
	s, err := a.catalog.Settings(r.Context(), a.principal(r).ID, chi.URLParam(r, "id"), nil)
	if err != nil {
		a.failure(w, r, err)
		return
	}
	json(w, 200, s)
}
func (a *api) setSettings(w http.ResponseWriter, r *http.Request) {
	var s catalog.Settings
	if !decode(w, r, &s) {
		return
	}
	updated, err := a.catalog.Settings(r.Context(), a.principal(r).ID, chi.URLParam(r, "id"), &s)
	if err != nil {
		a.failure(w, r, err)
		return
	}
	json(w, 200, updated)
}
func (a *api) participants(w http.ResponseWriter, r *http.Request) {
	items, err := a.catalog.Participants(r.Context(), a.principal(r).ID, chi.URLParam(r, "id"))
	if err != nil {
		a.failure(w, r, err)
		return
	}
	json(w, 200, map[string]any{"items": items})
}

type participantInput struct {
	UserID string `json:"user_id"`
	Self   bool   `json:"self"`
}

func (a *api) registerParticipant(w http.ResponseWriter, r *http.Request) {
	var in participantInput
	if !decode(w, r, &in) {
		return
	}
	if err := a.catalog.Register(r.Context(), a.principal(r).ID, chi.URLParam(r, "id"), in.UserID, in.Self); err != nil {
		a.failure(w, r, err)
		return
	}
	w.WriteHeader(204)
}
func (a *api) removeParticipant(w http.ResponseWriter, r *http.Request) {
	if err := a.catalog.RemoveParticipant(r.Context(), a.principal(r).ID, chi.URLParam(r, "id"), chi.URLParam(r, "userId")); err != nil {
		a.failure(w, r, err)
		return
	}
	w.WriteHeader(204)
}
func (a *api) failure(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, catalog.ErrDenied):
		problem(w, r, 403, "forbidden", "permission denied")
	case errors.Is(err, authorization.ErrUnavailable):
		problem(w, r, 503, "authorization_unavailable", "authorization service unavailable")
	case errors.Is(err, problems.ErrNotDraft):
		problem(w, r, 409, "revision_not_draft", err.Error())
	case errors.Is(err, judging.ErrLimit):
		problem(w, r, 409, "submission_limit", err.Error())
	case errors.Is(err, judging.ErrExecutionUnavailable):
		problem(w, r, 409, "execution_unavailable", err.Error())
	case errors.Is(err, judging.ErrCancelUnavailable):
		problem(w, r, 409, "cancel_unavailable", err.Error())
	case errors.Is(err, judging.ErrInvalid):
		problem(w, r, 422, "submission_invalid", err.Error())
	case errors.Is(err, grader.ErrInvalid), errors.Is(err, material.ErrInvalid), errors.Is(err, catalog.ErrInvalid), errors.Is(err, catalog.ErrSettings), errors.Is(err, problems.ErrInvalid), errors.Is(err, problems.ErrArchive), errors.Is(err, problems.ErrMissingRunner), errors.Is(err, problems.ErrMissingPackage), errors.Is(err, problems.ErrIncomplete), errors.Is(err, problems.ErrReservedFile), errors.Is(err, problems.ErrUnsupported), errors.Is(err, statement.ErrInvalid):
		problem(w, r, 422, "validation_error", err.Error())
	case errors.Is(err, pgx.ErrNoRows):
		problem(w, r, 404, "not_found", "resource not found")
	default:
		slog.Error("request failed", "error", err, "request_id", middleware.GetReqID(r.Context()))
		problem(w, r, 500, "internal_error", "request failed")
	}
}

var uuidRE = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[1-8][0-9a-fA-F]{3}-[89aAbB][0-9a-fA-F]{3}-[0-9a-fA-F]{12}$`)

func decode(w http.ResponseWriter, r *http.Request, dst any) bool {
	if r.Body == nil {
		problem(w, r, 400, "invalid_json", "request body is required")
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	dec := stdjson.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		problem(w, r, 400, "invalid_json", "request body is invalid")
		return false
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		problem(w, r, 400, "invalid_json", "request body must contain one JSON value")
		return false
	}
	return true
}
func json(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = stdjson.NewEncoder(w).Encode(value)
}
func problem(w http.ResponseWriter, r *http.Request, status int, code, message string) {
	var e Error
	e.Error.Code = code
	e.Error.Message = message
	e.Error.RequestID = middleware.GetReqID(r.Context())
	json(w, status, e)
}
