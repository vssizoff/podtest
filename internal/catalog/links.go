package catalog

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"podtest/internal/authorization"
	"podtest/internal/db"
)

type LinkInput struct {
	ContestID string `json:"contest_id"`
	Position  int    `json:"position"`
}

func (m Manager) RemoveParticipant(ctx context.Context, actor, contest, user string) error {
	if err := m.Require(ctx, actor, "can_manage", "contest", contest); err != nil {
		return err
	}
	return db.Tx(ctx, m.Pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE contest_participants SET status='inactive' WHERE contest_id=$1 AND user_id=$2 AND status='active'`, contest, user); err != nil {
			return err
		}
		if err := authorization.Enqueue(ctx, tx, authorization.Tuple{User: "user:" + user, Relation: "participant", Object: "contest:" + contest}, true); err != nil {
			return err
		}
		return db.Audit(ctx, tx, actor, "contest.participant.remove", contest)
	})
}
func (m Manager) Link(ctx context.Context, user, course, contest string, position int, remove bool) error {
	if err := m.Require(ctx, user, "can_manage", "course", course); err != nil {
		return err
	}
	if err := m.Require(ctx, user, "can_manage", "contest", contest); err != nil {
		return err
	}
	return db.Tx(ctx, m.Pool, func(tx pgx.Tx) error {
		if remove {
			if _, err := tx.Exec(ctx, `DELETE FROM course_contests WHERE course_id=$1 AND contest_id=$2`, course, contest); err != nil {
				return err
			}
		} else {
			if _, err := tx.Exec(ctx, `INSERT INTO course_contests(course_id,contest_id,position) VALUES($1,$2,$3) ON CONFLICT(course_id,contest_id) DO UPDATE SET position=excluded.position`, course, contest, position); err != nil {
				return err
			}
		}
		return authorization.Enqueue(ctx, tx, authorization.Tuple{User: "course:" + course, Relation: "course", Object: "contest:" + contest}, remove)
	})
}
func (m Manager) CourseContests(ctx context.Context, user, course string) ([]Item, error) {
	if err := m.Require(ctx, user, "can_view", "course", course); err != nil {
		return nil, err
	}
	rows, err := m.Pool.Query(ctx, `SELECT c.id,c.title,c.description,c.created_at,c.status,c.starts_at,c.ends_at FROM contests c JOIN course_contests l ON l.contest_id=c.id WHERE l.course_id=$1 AND c.archived_at IS NULL ORDER BY l.position,c.id LIMIT 250`, course)
	if err != nil {
		return nil, err
	}
	items, err := pgx.CollectRows(rows, pgx.RowToStructByPos[record])
	if err != nil {
		return nil, err
	}
	out := []Item{}
	for _, item := range items {
		can, err := m.Allowed(ctx, user, "can_view", "contest", item.ID)
		if err != nil {
			return nil, err
		}
		if !can {
			continue
		}
		out = append(out, item.dto(false))
	}
	return out, nil
}

type Settings struct {
	Status               string     `json:"status"`
	StartsAt             *time.Time `json:"starts_at"`
	EndsAt               *time.Time `json:"ends_at"`
	SelfRegistration     bool       `json:"self_registration"`
	ScoreboardVisibility string     `json:"scoreboard_visibility"`
	MaxQueuedSubmissions *int       `json:"max_queued_submissions"`
}

var ErrSettings = errors.New("invalid contest settings")

func (m Manager) Settings(ctx context.Context, user, id string, in *Settings) (out Settings, err error) {
	permission := "can_view"
	if in != nil {
		permission = "can_manage"
	}
	if err = m.Require(ctx, user, permission, "contest", id); err != nil {
		return
	}
	if in == nil {
		err = m.Pool.QueryRow(ctx, `SELECT c.status,c.starts_at,c.ends_at,s.self_registration,s.scoreboard_visibility,s.max_queued_submissions FROM contests c JOIN contest_settings s ON s.contest_id=c.id WHERE c.id=$1 AND c.archived_at IS NULL`, id).Scan(&out.Status, &out.StartsAt, &out.EndsAt, &out.SelfRegistration, &out.ScoreboardVisibility, &out.MaxQueuedSubmissions)
		return
	}
	if (in.Status != "draft" && in.Status != "open" && in.Status != "closed") || (in.ScoreboardVisibility != "private" && in.ScoreboardVisibility != "contest") || (in.StartsAt != nil && in.EndsAt != nil && !in.EndsAt.After(*in.StartsAt)) || (in.MaxQueuedSubmissions != nil && *in.MaxQueuedSubmissions < 1) {
		return out, ErrSettings
	}
	err = db.Tx(ctx, m.Pool, func(tx pgx.Tx) error {
		res, err := tx.Exec(ctx, `UPDATE contests SET status=$2,starts_at=$3,ends_at=$4 WHERE id=$1 AND archived_at IS NULL`, id, in.Status, in.StartsAt, in.EndsAt)
		if err != nil {
			return err
		}
		if res.RowsAffected() != 1 {
			return pgx.ErrNoRows
		}
		if _, err = tx.Exec(ctx, `UPDATE contest_settings SET self_registration=$2,scoreboard_visibility=$3,max_queued_submissions=$4 WHERE contest_id=$1`, id, in.SelfRegistration, in.ScoreboardVisibility, in.MaxQueuedSubmissions); err != nil {
			return err
		}
		return db.Audit(ctx, tx, user, "contest.settings", id)
	})
	return *in, err
}

type Participant struct {
	ID          string `json:"id"`
	UserID      string `json:"user_id"`
	DisplayName string `json:"display_name"`
	Status      string `json:"status"`
}

func (m Manager) Participants(ctx context.Context, user, contest string) ([]Participant, error) {
	if err := m.Require(ctx, user, "can_manage", "contest", contest); err != nil {
		return nil, err
	}
	rows, err := m.Pool.Query(ctx, `SELECT p.id,p.user_id,u.display_name,p.status FROM contest_participants p JOIN users u ON u.id=p.user_id WHERE p.contest_id=$1 ORDER BY p.registered_at,p.id LIMIT 250`, contest)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[Participant])
}
func (m Manager) Register(ctx context.Context, actor, contest, user string, self bool) error {
	permission := "can_manage"
	if self {
		permission = "can_view"
		user = actor
	}
	if err := m.Require(ctx, actor, permission, "contest", contest); err != nil {
		return err
	}
	grant := authorization.Tuple{User: "user:" + user, Relation: "participant", Object: "contest:" + contest}
	var grantID int64
	err := db.Tx(ctx, m.Pool, func(tx pgx.Tx) error {
		var open, registration bool
		err := tx.QueryRow(ctx, `SELECT c.status='open' AND (c.starts_at IS NULL OR c.starts_at<=now()) AND (c.ends_at IS NULL OR c.ends_at>now()),s.self_registration FROM contests c JOIN contest_settings s ON s.contest_id=c.id WHERE c.id=$1 AND c.archived_at IS NULL FOR UPDATE OF c`, contest).Scan(&open, &registration)
		if err != nil {
			return err
		}
		if self && !(open && registration) {
			return ErrDenied
		}
		if _, err = tx.Exec(ctx, `INSERT INTO contest_participants(contest_id,user_id) VALUES($1,$2) ON CONFLICT(contest_id,user_id) DO UPDATE SET status='active'`, contest, user); err != nil {
			return err
		}
		if err = tx.QueryRow(ctx, `INSERT INTO authorization_outbox(subject,relation,object) VALUES($1,$2,$3) RETURNING id`, grant.User, grant.Relation, grant.Object).Scan(&grantID); err != nil {
			return err
		}
		return db.Audit(ctx, tx, actor, "contest.register", contest)
	})
	if err == nil {
		_ = m.Auth.SyncCreation(ctx, m.Pool, grant, grantID)
	}
	return err
}

func (m Manager) Role(ctx context.Context, actor, typ, id, user, role string, remove bool) error {
	if typ == "course" && (role != "manager" && role != "teacher" && role != "student" && role != "viewer") {
		return ErrSettings
	}
	if typ == "contest" && role != "viewer" && role != "manager" {
		return ErrSettings
	}
	if err := m.Require(ctx, actor, "can_manage", typ, id); err != nil {
		return err
	}
	if typ == "contest" && role == "manager" && actor == user && remove {
		return ErrSettings
	}
	return db.Tx(ctx, m.Pool, func(tx pgx.Tx) error {
		if typ == "contest" && role == "manager" && remove {
			var owner string
			if err := tx.QueryRow(ctx, `SELECT created_by FROM contests WHERE id=$1`, id).Scan(&owner); err != nil {
				return err
			}
			if owner == user {
				return ErrSettings
			}
		}
		table, col := typ+"_memberships", typ+"_id"
		query := `INSERT INTO ` + table + `(` + col + `,user_id,role) VALUES($1,$2,$3) ON CONFLICT DO NOTHING`
		if remove {
			query = `DELETE FROM ` + table + ` WHERE ` + col + `=$1 AND user_id=$2 AND role=$3`
		}
		if _, err := tx.Exec(ctx, query, id, user, role); err != nil {
			return err
		}
		if err := authorization.Enqueue(ctx, tx, authorization.Tuple{User: "user:" + user, Relation: role, Object: typ + ":" + id}, remove); err != nil {
			return err
		}
		return db.Audit(ctx, tx, actor, typ+".role", id)
	})
}
