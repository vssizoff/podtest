package material

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"podtest/internal/artifact"
	"podtest/internal/authorization"
	"podtest/internal/db"
	"podtest/internal/statement"
)

var ErrInvalid = errors.New("invalid material revision")
var locale = regexp.MustCompile(`^[a-z]{2,3}(-[A-Za-z0-9]{2,8}){0,3}$`)

type Manager struct {
	Pool      *pgxpool.Pool
	Renderer  *statement.Renderer
	Artifacts *artifact.Store
	Auth      *authorization.Manager
}
type Input struct {
	Title       string `json:"title"`
	Description string `json:"description"`
}
type RevisionInput struct {
	Format string `json:"format"`
	Source string `json:"source"`
}
type Material struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	Description string `json:"description"`
}
type Revision struct {
	ID              string  `json:"id"`
	Revision        int     `json:"revision_number"`
	Format          string  `json:"format"`
	Source          string  `json:"source,omitempty"`
	RenderedHTML    string  `json:"rendered_html"`
	Digest          string  `json:"digest"`
	RendererVersion string  `json:"renderer_version"`
	AttachmentName  *string `json:"attachment_name,omitempty"`
}
type Attachment struct {
	MaterialID string `json:"material_id"`
	RevisionID string `json:"material_revision_id"`
	Position   int    `json:"position"`
	Visibility string `json:"visibility"`
}

func (m Manager) Create(ctx context.Context, user string, in Input) (Material, error) {
	in.Title = strings.TrimSpace(in.Title)
	if in.Title == "" || len([]rune(in.Title)) > 200 || len(in.Description) > 16000 {
		return Material{}, ErrInvalid
	}
	var item Material
	var grant authorization.Tuple
	var grantID int64
	err := db.Tx(ctx, m.Pool, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `INSERT INTO theory_materials(title,description,created_by) VALUES($1,$2,$3) RETURNING id,title,description`, in.Title, in.Description, user).Scan(&item.ID, &item.Title, &item.Description); err != nil {
			return err
		}
		grant = authorization.Tuple{User: "user:" + user, Relation: "owner", Object: "material:" + item.ID}
		if err := tx.QueryRow(ctx, `INSERT INTO authorization_outbox(subject,relation,object) VALUES ($1,$2,$3) RETURNING id`, grant.User, grant.Relation, grant.Object).Scan(&grantID); err != nil {
			return err
		}
		return db.Audit(ctx, tx, user, "material.create", item.ID)
	})
	if err == nil && m.Auth != nil {
		if syncErr := m.Auth.SyncCreation(ctx, m.Pool, grant, grantID); syncErr != nil {
			slog.Warn("initial material grant pending in outbox", "object", grant.Object, "error", syncErr)
		}
	}
	return item, err
}
func (m Manager) CreateRevision(ctx context.Context, user, id string, in RevisionInput, name string, data []byte) (Revision, error) {
	if (in.Format != "markdown" && in.Format != "latex") || strings.TrimSpace(in.Source) == "" {
		return Revision{}, ErrInvalid
	}
	rendered, err := m.Renderer.Render(ctx, in.Format, in.Source)
	if err != nil {
		return Revision{}, err
	}
	var out Revision
	out.Format = in.Format
	out.Source = in.Source
	out.RenderedHTML = rendered.HTML
	out.Digest = rendered.Digest
	out.RendererVersion = rendered.RendererVersion
	err = db.Tx(ctx, m.Pool, func(tx pgx.Tx) error {
		var lock string
		if err := tx.QueryRow(ctx, `SELECT id FROM theory_materials WHERE id=$1 AND archived_at IS NULL FOR UPDATE`, id).Scan(&lock); err != nil {
			return err
		}
		var attachment *string
		if name != "" {
			if len(name) > 255 || strings.ContainsAny(name, "/\\\r\n\x00") || len(data) == 0 || len(data) > 8<<20 {
				return ErrInvalid
			}
			ref, err := m.Artifacts.PutTx(ctx, tx, user, "material_attachment", "application/octet-stream", data)
			if err != nil {
				return err
			}
			attachment, out.AttachmentName = &ref.ID, &name
		}
		return tx.QueryRow(ctx, `INSERT INTO material_revisions(material_id,revision_number,format,source,digest,rendered_html,renderer_version,attachment_id,attachment_name) SELECT $1,coalesce(max(revision_number),0)+1,$2,$3,$4,$5,$6,$7,$8 FROM material_revisions WHERE material_id=$1 RETURNING id,revision_number`, id, in.Format, in.Source, out.Digest, out.RenderedHTML, out.RendererVersion, attachment, out.AttachmentName).Scan(&out.ID, &out.Revision)
	})
	return out, err
}
func (m Manager) Revision(ctx context.Context, rev string, includeSource bool) (Revision, error) {
	var out Revision
	err := m.Pool.QueryRow(ctx, `SELECT id,revision_number,format,source,rendered_html,digest,renderer_version FROM material_revisions WHERE id=$1`, rev).Scan(&out.ID, &out.Revision, &out.Format, &out.Source, &out.RenderedHTML, &out.Digest, &out.RendererVersion)
	if !includeSource {
		out.Source = ""
	}
	return out, err
}
func (m Manager) Attach(ctx context.Context, contest string, in Attachment) error {
	if in.Position < 0 || (in.Visibility != "contest" && in.Visibility != "managers") {
		return ErrInvalid
	}
	result, err := m.Pool.Exec(ctx, `INSERT INTO contest_materials(contest_id,material_id,material_revision_id,position,visibility) SELECT $1,r.material_id,r.id,$4,$5 FROM material_revisions r WHERE r.id=$3 AND r.material_id=$2 ON CONFLICT(contest_id,material_id) DO UPDATE SET material_revision_id=excluded.material_revision_id,position=excluded.position,visibility=excluded.visibility`, contest, in.MaterialID, in.RevisionID, in.Position, in.Visibility)
	if err == nil && result.RowsAffected() != 1 {
		return pgx.ErrNoRows
	}
	return err
}

func (m Manager) History(ctx context.Context, id string, includeSource bool) ([]Revision, error) {
	rows, err := m.Pool.Query(ctx, `SELECT id,revision_number,format,CASE WHEN $2 THEN source ELSE '' END,rendered_html,digest,renderer_version,attachment_name
		FROM material_revisions WHERE material_id=$1 ORDER BY revision_number DESC LIMIT 50`, id, includeSource)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[Revision])
}
func (m Manager) List(ctx context.Context, contest string) ([]RevisionAttachment, error) {
	rows, err := m.Pool.Query(ctx, `SELECT x.material_id,x.material_revision_id,x.position,x.visibility,m.title,r.format,r.rendered_html,r.digest,r.renderer_version,r.attachment_name FROM contest_materials x JOIN theory_materials m ON m.id=x.material_id JOIN material_revisions r ON r.id=x.material_revision_id WHERE x.contest_id=$1 ORDER BY x.position,x.material_id LIMIT 250`, contest)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, pgx.RowToStructByPos[RevisionAttachment])
	if out == nil {
		out = []RevisionAttachment{}
	}
	return out, err
}

type RevisionAttachment struct {
	MaterialID     string  `json:"material_id"`
	RevisionID     string  `json:"material_revision_id"`
	Position       int     `json:"position"`
	Visibility     string  `json:"visibility"`
	Title          string  `json:"title"`
	Format         string  `json:"format"`
	HTML           string  `json:"rendered_html"`
	Digest         string  `json:"digest"`
	Renderer       string  `json:"renderer_version"`
	AttachmentName *string `json:"attachment_name,omitempty"`
}

func ValidScore(value float64, max float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0) && value >= 0 && value <= max
}
