package catalog

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"podtest/internal/authorization"
	"podtest/internal/db"
)

type Manager struct {
	Pool *pgxpool.Pool
	Auth *authorization.Manager
}
type Item struct {
	ID          string     `json:"id"`
	Title       string     `json:"title"`
	Description string     `json:"description"`
	CreatedAt   time.Time  `json:"created_at"`
	CanManage   bool       `json:"can_manage"`
	Status      string     `json:"status,omitempty"`
	StartsAt    *time.Time `json:"starts_at,omitempty"`
	EndsAt      *time.Time `json:"ends_at,omitempty"`
}
type Input struct {
	Title       string `json:"title"`
	Description string `json:"description"`
}
type Page struct {
	Items []Item `json:"items"`
	Next  string `json:"next,omitempty"`
}
type kind struct{ table, role string }

var kinds = map[string]kind{"course": {"courses", "owner"}, "contest": {"contests", "manager"}, "problem": {"problems", "owner"}, "material": {"theory_materials", "owner"}}
var ErrInvalid = errors.New("title must contain 1–200 characters and description must be at most 16000 bytes")
var ErrDenied = errors.New("permission denied")

func (m Manager) Allowed(ctx context.Context, user, permission, typ, id string) (bool, error) {
	// Pending revocations deny access while PostgreSQL and OpenFGA converge.
	var pending bool
	if err := m.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT FROM authorization_outbox WHERE remove AND published_at IS NULL)`).Scan(&pending); err != nil {
		return false, err
	}
	if pending {
		return false, authorization.ErrUnavailable
	}
	return m.Auth.Check(ctx, "user:"+user, permission, typ+":"+id)
}
func (m Manager) Require(ctx context.Context, user, permission, typ, id string) error {
	ok, err := m.Allowed(ctx, user, permission, typ, id)
	if err != nil {
		return err
	}
	if !ok {
		return ErrDenied
	}
	return nil
}
func (m Manager) List(ctx context.Context, user, typ, after string) (Page, error) {
	k := kinds[typ]
	rows, err := m.Pool.Query(ctx, `SELECT `+itemColumns(typ)+` FROM `+k.table+` WHERE archived_at IS NULL AND (NULLIF($1,'')::uuid IS NULL OR id>NULLIF($1,'')::uuid) ORDER BY id LIMIT 25`, after)
	if err != nil {
		return Page{}, err
	}
	items, err := pgx.CollectRows(rows, pgx.RowToStructByPos[record])
	if err != nil {
		return Page{}, err
	}
	page := Page{Items: []Item{}}
	for _, item := range items {
		allowed, err := m.Allowed(ctx, user, "can_view", typ, item.ID)
		if err != nil {
			return Page{}, err
		}
		if !allowed {
			continue
		}
		manager, err := m.Allowed(ctx, user, "can_manage", typ, item.ID)
		if err != nil {
			return Page{}, err
		}
		page.Items = append(page.Items, item.dto(manager))
	}
	if len(items) == 25 {
		page.Next = items[len(items)-1].ID
	}
	return page, nil
}

type record struct {
	ID, Title, Description string
	CreatedAt              time.Time
	Status                 string
	StartsAt, EndsAt       *time.Time
}

func (r record) dto(manager bool) Item {
	return Item{r.ID, r.Title, r.Description, r.CreatedAt, manager, r.Status, r.StartsAt, r.EndsAt}
}

func itemColumns(typ string) string {
	if typ == "contest" {
		return "id,title,description,created_at,status,starts_at,ends_at"
	}
	return "id,title,description,created_at,''::text,NULL::timestamptz,NULL::timestamptz"
}
func (m Manager) Get(ctx context.Context, user, typ, id string) (Item, error) {
	if err := m.Require(ctx, user, "can_view", typ, id); err != nil {
		return Item{}, err
	}
	manager, err := m.Allowed(ctx, user, "can_manage", typ, id)
	if err != nil {
		return Item{}, err
	}
	rows, err := m.Pool.Query(ctx, `SELECT `+itemColumns(typ)+` FROM `+kinds[typ].table+` WHERE id=$1 AND archived_at IS NULL`, id)
	if err != nil {
		return Item{}, err
	}
	r, err := pgx.CollectOneRow(rows, pgx.RowToStructByPos[record])
	return r.dto(manager), err
}
func input(in Input) (Input, error) {
	in.Title = strings.TrimSpace(in.Title)
	if len([]rune(in.Title)) == 0 || len([]rune(in.Title)) > 200 || len(in.Description) > 16000 {
		return in, ErrInvalid
	}
	return in, nil
}
func (m Manager) Create(ctx context.Context, user, typ string, in Input) (Item, error) {
	in, err := input(in)
	if err != nil {
		return Item{}, err
	}
	k := kinds[typ]
	r := record{Title: in.Title, Description: in.Description}
	var grant authorization.Tuple
	var grantID int64
	err = db.Tx(ctx, m.Pool, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `INSERT INTO `+k.table+`(created_by,title,description) VALUES($1,$2,$3) RETURNING id,created_at`, user, in.Title, in.Description).Scan(&r.ID, &r.CreatedAt); err != nil {
			return err
		}
		switch typ {
		case "course", "contest":
			if _, err := tx.Exec(ctx, fmt.Sprintf(`INSERT INTO %s_memberships(%s_id,user_id,role) VALUES($1,$2,$3)`, typ, typ), r.ID, user, k.role); err != nil {
				return err
			}
		}
		if typ == "contest" {
			r.Status = "draft"
			if _, err := tx.Exec(ctx, `INSERT INTO contest_settings(contest_id) VALUES($1)`, r.ID); err != nil {
				return err
			}
		}
		grant = authorization.Tuple{User: "user:" + user, Relation: k.role, Object: typ + ":" + r.ID}
		return tx.QueryRow(ctx, `INSERT INTO authorization_outbox(subject,relation,object) VALUES ($1,$2,$3) RETURNING id`, grant.User, grant.Relation, grant.Object).Scan(&grantID)
	})
	if err == nil {
		// The committed item must not be reported as a failed create if FGA is down.
		if syncErr := m.Auth.SyncCreation(ctx, m.Pool, grant, grantID); syncErr != nil {
			slog.Warn("initial grant pending in outbox", "object", grant.Object, "error", syncErr)
		}
	}
	return r.dto(true), err
}
func (m Manager) Update(ctx context.Context, user, typ, id string, in Input) (Item, error) {
	if err := m.Require(ctx, user, "can_manage", typ, id); err != nil {
		return Item{}, err
	}
	in, err := input(in)
	if err != nil {
		return Item{}, err
	}
	rows, err := m.Pool.Query(ctx, `UPDATE `+kinds[typ].table+` SET title=$2,description=$3 WHERE id=$1 AND archived_at IS NULL RETURNING `+itemColumns(typ), id, in.Title, in.Description)
	if err != nil {
		return Item{}, err
	}
	r, err := pgx.CollectOneRow(rows, pgx.RowToStructByPos[record])
	return r.dto(true), err
}
func (m Manager) Archive(ctx context.Context, user, typ, id string) error {
	if err := m.Require(ctx, user, "can_manage", typ, id); err != nil {
		return err
	}
	return db.Tx(ctx, m.Pool, func(tx pgx.Tx) error {
		res, err := tx.Exec(ctx, `UPDATE `+kinds[typ].table+` SET archived_at=now() WHERE id=$1 AND archived_at IS NULL`, id)
		if err != nil {
			return err
		}
		if res.RowsAffected() != 1 {
			return pgx.ErrNoRows
		}
		return db.Audit(ctx, tx, user, typ+".archive", id)
	})
}
