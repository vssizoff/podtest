package problem

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"html"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"podtest/internal/authorization"
	"podtest/internal/db"
)

const MaxPolygonBytes = 64 << 20

type PolygonImport struct {
	ProblemID        string        `json:"problem_id"`
	RevisionID       string        `json:"revision_id"`
	ContestProblemID string        `json:"contest_problem_id"`
	Report           PolygonReport `json:"report"`
}

func (m Manager) ImportPolygon(ctx context.Context, auth *authorization.Manager, user, contest string, data []byte) (out PolygonImport, err error) {
	files, err := polygonFiles(data)
	if err != nil {
		return out, err
	}
	out.Report, err = inspectPolygonFiles(files)
	if err != nil {
		return out, err
	}
	title := strings.TrimSpace(out.Report.Title)
	if title == "" || utf8.RuneCountInString(title) > 200 {
		return out, ErrInvalid
	}
	normalized, languages, reason := polygonTestlib(files)
	out.Report.Compatible = reason == ""
	if reason != "" {
		out.Report.Warnings = append(out.Report.Warnings, reason)
	}
	meta := revisionManifest{Format: "standard", FormatVersion: 1, PackageFormat: "polygon_full_zip"}
	var bundle []byte
	if out.Report.Compatible {
		meta.PackageFormat, meta.ExecutionReady, meta.Languages = "ustar_v1", true, languages
		bundle, err = buildRevisionBundle(meta, normalized)
		if err != nil {
			return out, err
		}
	}
	meta.Source = fmt.Sprintf("%x", sha256.Sum256(data))
	manifest, err := json.Marshal(meta)
	if err != nil {
		return out, err
	}
	var grant authorization.Tuple
	var grantID int64
	err = db.Tx(ctx, m.Pool, func(tx pgx.Tx) error {
		var locked string
		if err := tx.QueryRow(ctx, `SELECT id FROM contests WHERE id=$1 AND archived_at IS NULL FOR UPDATE`, contest).Scan(&locked); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `INSERT INTO problems(created_by,title) VALUES($1,$2) RETURNING id`, user, title).Scan(&out.ProblemID); err != nil {
			return err
		}
		ref, err := m.Artifacts.PutTx(ctx, tx, user, "problem-import", "application/zip", data)
		if err != nil {
			return err
		}
		if err = tx.QueryRow(ctx, `INSERT INTO problem_revisions(problem_id,revision_number,execution_format,source_artifact_id) VALUES($1,1,'standard',$2) RETURNING id`, out.ProblemID, ref.ID).Scan(&out.RevisionID); err != nil {
			return err
		}
		if tag, err := tx.Exec(ctx, `INSERT INTO problem_revision_languages(revision_id,language_id) SELECT $1,id FROM languages WHERE builtin AND (NOT $2::boolean OR code=ANY($3::text[]))`, out.RevisionID, out.Report.Compatible, languages); err != nil {
			return err
		} else if out.Report.Compatible && tag.RowsAffected() != int64(len(languages)) {
			return fmt.Errorf("Polygon execution languages are not registered")
		}
		source, locale := "The original Polygon package is retained for inspection. This revision is not executable: "+reason, "en"
		if out.Report.Compatible {
			source, locale = polygonPlainStatement(files, title)
			ref, err = m.Artifacts.PutTx(ctx, tx, user, "revision-bundle", "application/x-tar", bundle)
			if err != nil {
				return err
			}
		}
		rendered := "<p>" + strings.ReplaceAll(html.EscapeString(source), "\n", "<br>") + "</p>"
		if _, err = tx.Exec(ctx, `INSERT INTO problem_statements(revision_id,locale,source,rendered_html,renderer_version,digest) VALUES($1,$2,$3,$4,'polygon-text-v1',$5)`, out.RevisionID, locale, source, rendered, fmt.Sprintf("%x", sha256.Sum256([]byte(rendered)))); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `UPDATE problem_revisions SET bundle_artifact_id=$2,digest=$3,manifest=$4,status='published',published_at=now() WHERE id=$1`, out.RevisionID, ref.ID, ref.Digest, manifest); err != nil {
			return err
		}
		if err = tx.QueryRow(ctx, `INSERT INTO contest_problems(contest_id,problem_id,problem_revision_id,position,max_score) SELECT $1,$2,$3,COALESCE(max(position)+1,0),100 FROM contest_problems WHERE contest_id=$1 AND detached_at IS NULL RETURNING id`, contest, out.ProblemID, out.RevisionID).Scan(&out.ContestProblemID); err != nil {
			return err
		}
		grant = authorization.Tuple{User: "user:" + user, Relation: "owner", Object: "problem:" + out.ProblemID}
		if err = tx.QueryRow(ctx, `INSERT INTO authorization_outbox(subject,relation,object) VALUES($1,$2,$3) RETURNING id`, grant.User, grant.Relation, grant.Object).Scan(&grantID); err != nil {
			return err
		}
		for _, t := range []authorization.Tuple{{User: "contest:" + contest, Relation: "contest", Object: "contest_problem:" + out.ContestProblemID}, {User: "problem:" + out.ProblemID, Relation: "problem", Object: "contest_problem:" + out.ContestProblemID}} {
			if err = authorization.Enqueue(ctx, tx, t, false); err != nil {
				return err
			}
		}
		return db.Audit(ctx, tx, user, "contest.problem.polygon_import", out.ContestProblemID)
	})
	if err == nil {
		_ = auth.SyncCreation(ctx, m.Pool, grant, grantID)
	}
	return out, err
}

func polygonPlainStatement(files map[string][]byte, title string) (string, string) {
	for _, lang := range []struct{ name, locale string }{{"russian", "ru"}, {"english", "en"}} {
		parts := []string{title}
		for _, name := range []string{"legend", "input", "output", "notes"} {
			data := files["statement-sections/"+lang.name+"/"+name+".tex"]
			if len(data) > 0 && len(data) <= 64<<10 && utf8.Valid(data) {
				parts = append(parts, strings.TrimSpace(string(data)))
			}
		}
		if len(parts) > 1 {
			return strings.Join(parts, "\n\n"), lang.locale
		}
	}
	return title, "en"
}
