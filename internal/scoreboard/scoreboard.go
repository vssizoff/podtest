package scoreboard

import (
	"context"
	"errors"
	"math/big"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrDenied = errors.New("scoreboard unavailable")

type Manager struct{ Pool *pgxpool.Pool }
type Problem struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Position int    `json:"position"`
	MaxScore string `json:"max_score"`
}
type Participant struct {
	ID          string `json:"id"`
	UserID      string `json:"user_id"`
	DisplayName string `json:"display_name"`
}
type Row struct {
	ParticipantID string            `json:"participant_id"`
	Rank          int               `json:"rank"`
	Total         string            `json:"total"`
	Scores        map[string]string `json:"scores"`
}
type Board struct {
	ContestID    string        `json:"contest_id"`
	Problems     []Problem     `json:"problems"`
	Participants []Participant `json:"participants,omitempty"`
	Rows         []Row         `json:"rows"`
	CalculatedAt time.Time     `json:"calculated_at"`
}
type rawScore struct{ Participant, Problem, Value string }

func (m Manager) Get(ctx context.Context, user, contest string, manager bool) (Board, error) {
	var visibility string
	err := m.Pool.QueryRow(ctx, `SELECT coalesce(s.scoreboard_visibility,'private') FROM contests c LEFT JOIN contest_settings s ON s.contest_id=c.id WHERE c.id=$1 AND c.archived_at IS NULL`, contest).Scan(&visibility)
	if err != nil {
		return Board{}, err
	}
	if !manager && visibility == "private" && user == "" {
		return Board{}, ErrDenied
	}
	board := Board{ContestID: contest, Problems: []Problem{}, Participants: []Participant{}, Rows: []Row{}, CalculatedAt: time.Now().UTC()}
	rows, err := m.Pool.Query(ctx, `SELECT cp.id,p.title,cp.position,cp.max_score::text FROM contest_problems cp JOIN problems p ON p.id=cp.problem_id WHERE cp.contest_id=$1 AND cp.detached_at IS NULL ORDER BY cp.position,cp.id LIMIT 250`, contest)
	if err != nil {
		return board, err
	}
	board.Problems, err = pgx.CollectRows(rows, pgx.RowToStructByPos[Problem])
	if err != nil {
		return board, err
	}
	rows, err = m.Pool.Query(ctx, `SELECT p.id,p.user_id,u.display_name FROM contest_participants p JOIN users u ON u.id=p.user_id WHERE p.contest_id=$1 AND p.status='active' ORDER BY p.id LIMIT 5000`, contest)
	if err != nil {
		return board, err
	}
	board.Participants, err = pgx.CollectRows(rows, pgx.RowToStructByPos[Participant])
	if err != nil {
		return board, err
	}
	rows, err = m.Pool.Query(ctx, `SELECT v.participant_id,v.contest_problem_id,v.score::text FROM score_publication_values v JOIN contests c ON c.current_score_publication_id=v.publication_id WHERE c.id=$1 ORDER BY v.participant_id,v.contest_problem_id`, contest)
	if err != nil {
		return board, err
	}
	scores, err := pgx.CollectRows(rows, pgx.RowToStructByPos[rawScore])
	if err != nil {
		return board, err
	}
	byParticipant := map[string]map[string]string{}
	for _, s := range scores {
		if byParticipant[s.Participant] == nil {
			byParticipant[s.Participant] = map[string]string{}
		}
		byParticipant[s.Participant][s.Problem] = s.Value
	}
	rows, err = m.Pool.Query(ctx, `SELECT p.id,cp.id,coalesce(max(a.base_score),0)::text FROM contest_participants p CROSS JOIN contest_problems cp JOIN problem_revisions r ON r.id=cp.problem_revision_id LEFT JOIN submissions s ON s.participant_id=p.id AND s.contest_problem_id=cp.id LEFT JOIN LATERAL (SELECT status,base_score FROM judging_attempts WHERE submission_id=s.id ORDER BY attempt_number DESC LIMIT 1) a ON a.status='finished' WHERE p.contest_id=$1 AND p.status='active' AND cp.contest_id=$1 AND cp.detached_at IS NULL AND r.grader_mode IS NULL GROUP BY p.id,cp.id ORDER BY p.id,cp.id`, contest)
	if err != nil {
		return board, err
	}
	defaults, err := pgx.CollectRows(rows, pgx.RowToStructByPos[rawScore])
	if err != nil {
		return board, err
	}
	for _, s := range defaults {
		if byParticipant[s.Participant] == nil {
			byParticipant[s.Participant] = map[string]string{}
		}
		if _, published := byParticipant[s.Participant][s.Problem]; !published {
			byParticipant[s.Participant][s.Problem] = s.Value
		}
	}
	type ranked struct {
		row   Row
		sum   string
		value *big.Rat
	}
	all := []ranked{}
	for _, p := range board.Participants {
		r := Row{ParticipantID: p.ID, Scores: map[string]string{}}
		sum := new(big.Rat)
		for _, problem := range board.Problems {
			value := byParticipant[p.ID][problem.ID]
			if value == "" {
				continue
			}
			r.Scores[problem.ID] = value
			if n, ok := new(big.Rat).SetString(value); ok {
				sum.Add(sum, n)
			}
		}
		r.Total = sum.FloatString(6)
		all = append(all, ranked{r, r.Total, sum})
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].value.Cmp(all[j].value) > 0 })
	prev := ""
	rank := 0
	for i := range all {
		total := all[i].sum
		if total != prev {
			rank = i + 1
			prev = total
		}
		all[i].row.Rank = rank
		board.Rows = append(board.Rows, all[i].row)
	}
	if user != "" {
		own := []Participant{}
		ownRows := []Row{}
		for _, p := range board.Participants {
			if p.UserID == user {
				own = append(own, p)
				for _, row := range board.Rows {
					if row.ParticipantID == p.ID {
						ownRows = append(ownRows, row)
					}
				}
			}
		}
		board.Rows = ownRows
		board.Participants = own
	}
	return board, nil
}
