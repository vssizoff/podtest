package problem

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"podtest/internal/artifact"
	"podtest/internal/authorization"
	"podtest/internal/db"
	"podtest/internal/statement"
)

var ErrInvalid = errors.New("invalid revision configuration")
var ErrNotDraft = errors.New("revision is not a draft")
var ErrUnsupported = errors.New("unsupported revision execution format")
var ErrMissingRunner = errors.New("ZIP must contain a non-empty runner.lua at its root")
var ErrMissingPackage = errors.New("upload an execution package before publishing")
var ErrIncomplete = errors.New("save a statement and enable at least one language before publishing")
var ErrReservedFile = errors.New("podtest-manifest.json is reserved; remove it from the uploaded ZIP")

const MaxPackageBytes = 16 << 20

var localePattern = regexp.MustCompile(`^[a-z]{2,3}(-[A-Za-z0-9]{2,8}){0,3}$`)

type Manager struct {
	Pool      *pgxpool.Pool
	Artifacts *artifact.Store
	Renderer  *statement.Renderer
}
type Revision struct {
	ID        string  `json:"id"`
	Number    int     `json:"revision_number"`
	Format    string  `json:"execution_format"`
	Status    string  `json:"status"`
	Digest    *string `json:"digest"`
	OlderPins int     `json:"older_pins"`
}
type RevisionInput struct {
	Format     string `json:"execution_format"`
	CloneFrom  string `json:"clone_from"`
	GraderMode string `json:"grader_mode"`
}
type Language struct {
	Code  string `json:"code"`
	Title string `json:"title"`
}
type Statement struct {
	Locale string `json:"locale"`
	Source string `json:"source,omitempty"`
	statement.Document
}

func (m Manager) List(ctx context.Context, id string) ([]Revision, error) {
	rows, err := m.Pool.Query(ctx, `SELECT r.id,r.revision_number,r.execution_format,r.status,r.digest,(SELECT count(*)::int FROM contest_problems cp JOIN problem_revisions old ON old.id=cp.problem_revision_id WHERE cp.problem_id=r.problem_id AND cp.detached_at IS NULL AND old.revision_number<r.revision_number) FROM problem_revisions r WHERE problem_id=$1 ORDER BY revision_number DESC LIMIT 250`, id)
	if err != nil {
		return nil, err
	}
	items, err := pgx.CollectRows(rows, pgx.RowToStructByPos[Revision])
	if items == nil {
		items = []Revision{}
	}
	return items, err
}
func (m Manager) Create(ctx context.Context, id string, in RevisionInput) (out Revision, err error) {
	if (in.Format != "standard" && in.Format != "scripted_lua") || (in.GraderMode != "" && in.GraderMode != "manual") {
		return out, ErrInvalid
	}
	out.Format = in.Format
	out.Status = "draft"
	err = db.Tx(ctx, m.Pool, func(tx pgx.Tx) error {
		var lock string
		if err := tx.QueryRow(ctx, `SELECT id FROM problems WHERE id=$1 AND archived_at IS NULL FOR UPDATE`, id).Scan(&lock); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `INSERT INTO problem_revisions(problem_id,revision_number,execution_format,grader_mode) SELECT $1,coalesce(max(revision_number),0)+1,$2,NULLIF($3,'') FROM problem_revisions WHERE problem_id=$1 RETURNING id,revision_number`, id, in.Format, in.GraderMode).Scan(&out.ID, &out.Number); err != nil {
			return err
		}
		if in.CloneFrom != "" {
			res, err := tx.Exec(ctx, `UPDATE problem_revisions d SET source_artifact_id=s.source_artifact_id FROM problem_revisions s WHERE d.id=$1 AND s.id=$2 AND s.problem_id=$3 AND s.execution_format=d.execution_format`, out.ID, in.CloneFrom, id)
			if err != nil {
				return err
			}
			if res.RowsAffected() == 0 {
				return ErrInvalid
			}
			if _, err := tx.Exec(ctx, `INSERT INTO problem_statements SELECT $1,locale,source,rendered_html,renderer_version,digest FROM problem_statements WHERE revision_id=$2`, out.ID, in.CloneFrom); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `INSERT INTO problem_revision_languages SELECT $1,language_id,build_command,run_command,containerfile_artifact_id FROM problem_revision_languages WHERE revision_id=$2`, out.ID, in.CloneFrom); err != nil {
				return err
			}
		}
		return nil
	})
	return
}
func draft(ctx context.Context, tx pgx.Tx, problem, rev string) error {
	var status string
	err := tx.QueryRow(ctx, `SELECT status FROM problem_revisions WHERE id=$1 AND problem_id=$2 FOR UPDATE`, rev, problem).Scan(&status)
	if err != nil {
		return err
	}
	if status != "draft" {
		return ErrNotDraft
	}
	return nil
}
func (m Manager) Statements(ctx context.Context, problem, rev string) ([]Statement, error) {
	rows, err := m.Pool.Query(ctx, `SELECT s.locale,s.source,s.rendered_html,s.digest,s.renderer_version FROM problem_statements s JOIN problem_revisions r ON r.id=s.revision_id WHERE r.id=$1 AND r.problem_id=$2 ORDER BY s.locale`, rev, problem)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Statement{}
	for rows.Next() {
		var s Statement
		if err = rows.Scan(&s.Locale, &s.Source, &s.HTML, &s.Digest, &s.RendererVersion); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}
func (m Manager) SetStatement(ctx context.Context, problem, rev, locale, source string) (Statement, error) {
	if !localePattern.MatchString(locale) || strings.TrimSpace(source) == "" {
		return Statement{}, ErrInvalid
	}
	rendered, err := m.Renderer.Render(ctx, "markdown", source)
	if err != nil {
		return Statement{}, err
	}
	err = db.Tx(ctx, m.Pool, func(tx pgx.Tx) error {
		if err := draft(ctx, tx, problem, rev); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO problem_statements(revision_id,locale,source,rendered_html,renderer_version,digest) VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT(revision_id,locale) DO UPDATE SET source=excluded.source,rendered_html=excluded.rendered_html,renderer_version=excluded.renderer_version,digest=excluded.digest`, rev, locale, source, rendered.HTML, rendered.RendererVersion, rendered.Digest)
		return err
	})
	return Statement{locale, source, rendered}, err
}
func (m Manager) SetLanguage(ctx context.Context, problem, rev, code string) error {
	return db.Tx(ctx, m.Pool, func(tx pgx.Tx) error {
		if err := draft(ctx, tx, problem, rev); err != nil {
			return err
		}
		res, err := tx.Exec(ctx, `INSERT INTO problem_revision_languages(revision_id,language_id) SELECT $1,id FROM languages WHERE code=$2 AND builtin ON CONFLICT DO NOTHING`, rev, code)
		if err != nil {
			return err
		}
		if res.RowsAffected() == 0 {
			var exists bool
			if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT FROM languages WHERE code=$1 AND builtin)`, code).Scan(&exists); err != nil {
				return err
			}
			if !exists {
				return ErrInvalid
			}
		}
		return nil
	})
}
func (m Manager) SetPackage(ctx context.Context, user, problem, rev string, data []byte) error {
	var format string
	if err := m.Pool.QueryRow(ctx, `SELECT execution_format FROM problem_revisions WHERE id=$1 AND problem_id=$2 AND status='draft'`, rev, problem).Scan(&format); err != nil {
		return err
	}
	files, err := Files(data)
	if err != nil {
		return err
	}
	switch format {
	case "standard":
		if _, err := validateStandard(files); err != nil {
			return err
		}
	case "scripted_lua":
		if len(files["runner.lua"]) == 0 {
			return ErrMissingRunner
		}
	default:
		return ErrUnsupported
	}
	if _, exists := files["podtest-manifest.json"]; exists {
		return ErrReservedFile
	}
	ref, err := m.Artifacts.Put(ctx, user, "problem-import", "application/zip", data)
	if err != nil {
		return err
	}
	return db.Tx(ctx, m.Pool, func(tx pgx.Tx) error {
		if err := draft(ctx, tx, problem, rev); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE problem_revisions SET source_artifact_id=$2 WHERE id=$1`, rev, ref.ID)
		return err
	})
}

type revisionManifest struct {
	Format         string            `json:"execution_format"`
	FormatVersion  int               `json:"format_version"`
	RunnerVersion  int               `json:"runner_api_version"`
	Source         string            `json:"source_sha256"`
	Statements     map[string]string `json:"statements"`
	Languages      []string          `json:"languages"`
	GraderMode     *string           `json:"grader_mode"`
	PackageFormat  string            `json:"package_format"`
	ExecutionReady bool              `json:"execution_ready"`
}

func (m Manager) Validate(ctx context.Context, problem, rev string) error {
	return db.Tx(ctx, m.Pool, func(tx pgx.Tx) error {
		meta, source, err := manifest(ctx, tx, problem, rev)
		if err != nil || meta.Format != "standard" {
			return err
		}
		_, err = m.packageFiles(ctx, source, &meta)
		return err
	})
}
func manifest(ctx context.Context, tx pgx.Tx, problem, rev string) (revisionManifest, string, error) {
	out := revisionManifest{Statements: map[string]string{}, PackageFormat: "ustar_v1"}
	var sourceID *string
	var sourceDigest *string
	if err := draft(ctx, tx, problem, rev); err != nil {
		return out, "", err
	}
	err := tx.QueryRow(ctx, `SELECT r.execution_format,r.format_version,r.runner_api_version,r.source_artifact_id,a.digest,r.grader_mode FROM problem_revisions r LEFT JOIN artifacts a ON a.id=r.source_artifact_id WHERE r.id=$1`, rev).Scan(&out.Format, &out.FormatVersion, &out.RunnerVersion, &sourceID, &sourceDigest, &out.GraderMode)
	if err != nil {
		return out, "", err
	}
	if out.Format != "scripted_lua" && out.Format != "standard" {
		return out, "", ErrUnsupported
	}
	if out.Format == "standard" {
		out.RunnerVersion = 0
	}
	if sourceID == nil || sourceDigest == nil {
		return out, "", ErrMissingPackage
	}
	out.Source = *sourceDigest
	rows, err := tx.Query(ctx, `SELECT locale,digest FROM problem_statements WHERE revision_id=$1`, rev)
	if err != nil {
		return out, "", err
	}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			rows.Close()
			return out, "", err
		}
		out.Statements[k] = v
	}
	rows.Close()
	if rows.Err() != nil {
		return out, "", rows.Err()
	}
	rows, err = tx.Query(ctx, `SELECT l.code FROM problem_revision_languages x JOIN languages l ON l.id=x.language_id WHERE x.revision_id=$1 ORDER BY l.code`, rev)
	if err != nil {
		return out, "", err
	}
	out.Languages, err = pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return out, "", err
	}
	if len(out.Statements) == 0 || len(out.Languages) == 0 {
		return out, "", ErrIncomplete
	}
	return out, *sourceID, nil
}
func (m Manager) Publish(ctx context.Context, user, problem, rev string) error {
	return db.Tx(ctx, m.Pool, func(tx pgx.Tx) error {
		manifest, source, err := manifest(ctx, tx, problem, rev)
		if err != nil {
			return err
		}
		files, err := m.packageFiles(ctx, source, &manifest)
		if err != nil {
			return err
		}
		data, err := json.Marshal(manifest)
		if err != nil {
			return err
		}
		files["podtest-manifest.json"] = data
		bundle, err := buildRevisionBundle(manifest, files)
		if err != nil {
			return err
		}
		ref, err := m.Artifacts.PutTx(ctx, tx, user, "revision-bundle", "application/x-tar", bundle)
		if err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `UPDATE problem_revisions SET bundle_artifact_id=$2,digest=$3,manifest=$4,status='published',published_at=now() WHERE id=$1`, rev, ref.ID, ref.Digest, data); err != nil {
			return err
		}
		return db.Audit(ctx, tx, user, "revision.publish", rev)
	})
}

func (m Manager) packageFiles(ctx context.Context, source string, meta *revisionManifest) (map[string][]byte, error) {
	original, err := m.Artifacts.Read(ctx, source, MaxPackageBytes)
	if err != nil {
		return nil, err
	}
	if fmt.Sprintf("%x", sha256.Sum256(original)) != meta.Source {
		return nil, errors.New("revision source digest mismatch")
	}
	files, err := Files(original)
	if err != nil {
		return nil, err
	}
	if _, exists := files["podtest-manifest.json"]; exists {
		return nil, ErrReservedFile
	}
	if meta.Format == "standard" {
		language, err := validateStandard(files)
		if err != nil {
			return nil, err
		}
		if len(meta.Languages) != 1 || meta.Languages[0] != language {
			return nil, fmt.Errorf("%w: enable only the language declared in standard.json", ErrInvalid)
		}
		meta.ExecutionReady = true
	}
	return files, nil
}

func buildRevisionBundle(manifest revisionManifest, files map[string][]byte) ([]byte, error) {
	if manifest.Format == "scripted_lua" && len(files["runner.lua"]) == 0 {
		return nil, ErrMissingRunner
	}
	normalized := make([]artifact.BundleFile, 0, len(files))
	for name, data := range files {
		normalized = append(normalized, artifact.BundleFile{Path: name, Data: data})
	}
	bundle, _, err := artifact.BuildBundle(manifest.Format, uint32(manifest.FormatVersion), uint32(manifest.RunnerVersion), normalized)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrArchive, err)
	}
	return bundle, nil
}

type ContestProblem struct {
	ID                string  `json:"id"`
	ProblemID         string  `json:"problem_id"`
	RevisionID        string  `json:"problem_revision_id"`
	Title             string  `json:"title"`
	Position          int     `json:"position"`
	MaxScore          string  `json:"max_score"`
	VerdictVisibility string  `json:"verdict_visibility"`
	GraderMode        *string `json:"grader_mode"`
}
type AttachInput struct {
	ProblemID         string `json:"problem_id"`
	RevisionID        string `json:"problem_revision_id"`
	Position          int    `json:"position"`
	MaxScore          string `json:"max_score"`
	SubmissionLimit   *int   `json:"submission_limit"`
	VerdictVisibility string `json:"verdict_visibility"`
}

func (m Manager) ContestList(ctx context.Context, contest string) ([]ContestProblem, error) {
	rows, err := m.Pool.Query(ctx, `SELECT cp.id,cp.problem_id,cp.problem_revision_id,p.title,cp.position,cp.max_score::text,cp.verdict_visibility,r.grader_mode FROM contest_problems cp JOIN problems p ON p.id=cp.problem_id JOIN problem_revisions r ON r.id=cp.problem_revision_id WHERE cp.contest_id=$1 AND cp.detached_at IS NULL ORDER BY cp.position,cp.id LIMIT 250`, contest)
	if err != nil {
		return nil, err
	}
	items, err := pgx.CollectRows(rows, pgx.RowToStructByPos[ContestProblem])
	if items == nil {
		items = []ContestProblem{}
	}
	return items, err
}
func (m Manager) Attach(ctx context.Context, user, contest string, in AttachInput) (id string, err error) {
	if in.Position < 0 || !scoreRE.MatchString(in.MaxScore) || (in.SubmissionLimit != nil && *in.SubmissionLimit < 1) || (in.VerdictVisibility != "hidden" && in.VerdictVisibility != "summary" && in.VerdictVisibility != "tests") {
		return "", ErrInvalid
	}
	err = db.Tx(ctx, m.Pool, func(tx pgx.Tx) error {
		var lock string
		if err := tx.QueryRow(ctx, `SELECT id FROM contests WHERE id=$1 AND archived_at IS NULL FOR UPDATE`, contest).Scan(&lock); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `INSERT INTO contest_problems(contest_id,problem_id,problem_revision_id,position,max_score,submission_limit,verdict_visibility) SELECT $1,problem_id,id,$4,$5::numeric,$6,$7 FROM problem_revisions WHERE id=$3 AND problem_id=$2 AND status='published' RETURNING id`, contest, in.ProblemID, in.RevisionID, in.Position, in.MaxScore, in.SubmissionLimit, in.VerdictVisibility).Scan(&id); err != nil {
			return err
		}
		for _, t := range []authorization.Tuple{{User: "contest:" + contest, Relation: "contest", Object: "contest_problem:" + id}, {User: "problem:" + in.ProblemID, Relation: "problem", Object: "contest_problem:" + id}} {
			if err := authorization.Enqueue(ctx, tx, t, false); err != nil {
				return err
			}
		}
		return db.Audit(ctx, tx, user, "contest.problem.attach", id)
	})
	return
}

var scoreRE = regexp.MustCompile(`^[0-9]{1,12}(\.[0-9]{1,6})?$`)

func (m Manager) Switch(ctx context.Context, user, contest, cp, revision string) error {
	return db.Tx(ctx, m.Pool, func(tx pgx.Tx) error {
		var id string
		if err := tx.QueryRow(ctx, `UPDATE contest_problems cp SET problem_revision_id=r.id FROM problem_revisions r WHERE cp.id=$1 AND cp.contest_id=$2 AND cp.detached_at IS NULL AND r.id=$3 AND r.problem_id=cp.problem_id AND r.status='published' RETURNING cp.id`, cp, contest, revision).Scan(&id); err != nil {
			return err
		}
		return db.Audit(ctx, tx, user, "contest.problem.switch_revision", id)
	})
}
func (m Manager) Detach(ctx context.Context, user, contest, cp string) error {
	return db.Tx(ctx, m.Pool, func(tx pgx.Tx) error {
		res, err := tx.Exec(ctx, `UPDATE contest_problems SET detached_at=now() WHERE contest_id=$1 AND id=$2 AND detached_at IS NULL`, contest, cp)
		if err != nil {
			return err
		}
		if res.RowsAffected() != 1 {
			return pgx.ErrNoRows
		}
		return db.Audit(ctx, tx, user, "contest.problem.detach", cp)
	})
}

type Task struct {
	ID         string      `json:"id"`
	Title      string      `json:"title"`
	RevisionID string      `json:"problem_revision_id"`
	Statements []Statement `json:"statements"`
	Languages  []Language  `json:"languages"`
}

func (m Manager) Task(ctx context.Context, contest, cp string) (t Task, err error) {
	var problem string
	err = m.Pool.QueryRow(ctx, `SELECT cp.id,p.title,cp.problem_revision_id,p.id FROM contest_problems cp JOIN problems p ON p.id=cp.problem_id JOIN contests c ON c.id=cp.contest_id WHERE cp.id=$1 AND cp.contest_id=$2 AND cp.detached_at IS NULL AND c.archived_at IS NULL`, cp, contest).Scan(&t.ID, &t.Title, &t.RevisionID, &problem)
	if err != nil {
		return
	}
	if t.Statements, err = m.Statements(ctx, problem, t.RevisionID); err != nil {
		return
	}
	for i := range t.Statements {
		t.Statements[i].Source = ""
	}
	rows, err := m.Pool.Query(ctx, `SELECT l.code,l.title FROM problem_revision_languages x JOIN languages l ON l.id=x.language_id WHERE x.revision_id=$1 ORDER BY l.code`, t.RevisionID)
	if err != nil {
		return t, err
	}
	t.Languages, err = pgx.CollectRows(rows, pgx.RowToStructByPos[Language])
	return
}
