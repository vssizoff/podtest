package authorization

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"podtest/internal/db"
)

//go:embed model.fga
var modelDSL string

type HTTPClient struct{ URL, Token, Store, Model, StateFile string }
type modelState struct{ Store, Model string }

var client = &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

func (c *HTTPClient) call(ctx context.Context, path string, in, out any) error {
	body, err := json.Marshal(in)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", strings.TrimRight(c.URL, "/")+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	res, err := client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return fmt.Errorf("OpenFGA HTTP %d", res.StatusCode)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(res.Body).Decode(out)
}
func (c *HTTPClient) state() (modelState, error) {
	if c.Store != "" && c.Model != "" {
		return modelState{c.Store, c.Model}, nil
	}
	var s modelState
	b, err := os.ReadFile(c.StateFile)
	if err != nil {
		return s, err
	}
	err = json.Unmarshal(b, &s)
	if s.Store == "" || s.Model == "" {
		return s, ErrUnavailable
	}
	return s, err
}
func (c *HTTPClient) Check(ctx context.Context, user, rel, obj string) (bool, error) {
	s, err := c.state()
	if err != nil {
		return false, err
	}
	var r struct{ Allowed bool }
	err = c.call(ctx, "/stores/"+s.Store+"/check", map[string]any{"authorization_model_id": s.Model, "tuple_key": key(Tuple{user, rel, obj}), "consistency": "HIGHER_CONSISTENCY"}, &r)
	return r.Allowed, err
}
func (c *HTTPClient) BatchCheck(ctx context.Context, tuples []Tuple, permission string) ([]bool, error) {
	result := make([]bool, len(tuples))
	for i, t := range tuples {
		r, err := c.Check(ctx, t.User, permission, t.Object)
		if err != nil {
			return nil, err
		}
		result[i] = r
	}
	return result, nil
}
func key(t Tuple) map[string]string {
	return map[string]string{"user": t.User, "relation": t.Relation, "object": t.Object}
}
func (c *HTTPClient) Write(ctx context.Context, writes, deletes []Tuple) error {
	s, err := c.state()
	if err != nil {
		return err
	}
	in := map[string]any{"authorization_model_id": s.Model}
	for name, tuples := range map[string][]Tuple{"writes": writes, "deletes": deletes} {
		if len(tuples) == 0 {
			continue
		}
		keys := make([]map[string]string, 0, len(tuples))
		for _, t := range tuples {
			keys = append(keys, key(t))
		}
		op := map[string]any{"tuple_keys": keys}
		if name == "writes" {
			op["on_duplicate"] = "ignore"
		} else {
			op["on_missing"] = "ignore"
		}
		in[name] = op
	}
	return c.call(ctx, "/stores/"+s.Store+"/write", in, nil)
}
func Enqueue(ctx context.Context, tx pgx.Tx, tuple Tuple, remove bool) error {
	_, err := tx.Exec(ctx, `INSERT INTO authorization_outbox(subject,relation,object,remove) VALUES ($1,$2,$3,$4)`, tuple.User, tuple.Relation, tuple.Object, remove)
	return err
}

// SyncCreation gives a newly created object's owner immediate access without
// relying on the outbox polling interval. The durable entry remains on failure.
func (m *Manager) SyncCreation(ctx context.Context, pool *pgxpool.Pool, t Tuple, id int64) error {
	return db.Tx(ctx, pool, func(tx pgx.Tx) error {
		var pending bool
		if err := tx.QueryRow(ctx, `SELECT e.published_at IS NULL AND NOT EXISTS (SELECT FROM authorization_outbox p WHERE p.id<e.id AND p.published_at IS NULL AND (p.subject,p.relation,p.object)=(e.subject,e.relation,e.object)) FROM authorization_outbox e WHERE e.id=$1 FOR UPDATE OF e`, id).Scan(&pending); err != nil || !pending {
			return err
		}
		if err := m.Sync(ctx, []Tuple{t}, nil); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE authorization_outbox SET published_at=now() WHERE id=$1`, id)
		return err
	})
}
func RunOutbox(ctx context.Context, pool *pgxpool.Pool, c *HTTPClient) error {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		if err := syncOne(ctx, pool, c); err != nil && ctx.Err() == nil {
			fmt.Fprintln(os.Stderr, "authorization outbox:", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}
func syncOne(ctx context.Context, pool *pgxpool.Pool, c *HTTPClient) error {
	return db.Tx(ctx, pool, func(tx pgx.Tx) error {
		var id int64
		var t Tuple
		var remove bool
		err := tx.QueryRow(ctx, `SELECT e.id,e.subject,e.relation,e.object,e.remove FROM authorization_outbox e WHERE e.published_at IS NULL AND e.next_try_at<=now() AND NOT EXISTS (SELECT FROM authorization_outbox p WHERE p.id<e.id AND p.published_at IS NULL AND (p.subject,p.relation,p.object)=(e.subject,e.relation,e.object)) ORDER BY e.id LIMIT 1 FOR UPDATE SKIP LOCKED`).Scan(&id, &t.User, &t.Relation, &t.Object, &remove)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		var writes, deletes []Tuple
		if remove {
			deletes = []Tuple{t}
		} else {
			writes = []Tuple{t}
		}
		if err = c.Write(ctx, writes, deletes); err != nil {
			fmt.Fprintln(os.Stderr, "OpenFGA sync:", err)
			_, err = tx.Exec(ctx, `UPDATE authorization_outbox SET attempts=attempts+1,next_try_at=now()+interval '1 second'*least(60,2^least(attempts,6)) WHERE id=$1`, id)
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE authorization_outbox SET published_at=now() WHERE id=$1`, id)
		return err
	})
}

// Load installs the checked-in model. A state file records the model and store IDs for development.
func Load(ctx context.Context, base, token, store string) error {
	file := os.Getenv("FGA_STATE_FILE")
	if file == "" {
		return errors.New("FGA_STATE_FILE required")
	}
	c := &HTTPClient{URL: base, Token: token, StateFile: file}
	if store == "" {
		if old, err := c.state(); err == nil {
			store = old.Store
		}
	}
	if store == "" {
		var out struct{ ID string }
		if err := c.call(ctx, "/stores", map[string]string{"name": "podtest"}, &out); err != nil {
			return err
		}
		store = out.ID
	}
	var out struct {
		ID string `json:"authorization_model_id"`
	}
	if err := c.call(ctx, "/stores/"+store+"/authorization-models", compileModel(), &out); err != nil {
		return err
	}
	b, err := json.Marshal(modelState{store, out.ID})
	if err != nil {
		return err
	}
	if err = os.WriteFile(file+".tmp", b, 0644); err != nil {
		return err
	}
	return os.Rename(file+".tmp", file)
}

// compileModel handles the restricted DSL constructs used in model.fga.
func compileModel() map[string]any {
	types := []map[string]any{}
	var relations, meta map[string]any
	for _, line := range strings.Split(modelDSL, "\n") {
		line = strings.TrimSpace(line)
		if kind, ok := strings.CutPrefix(line, "type "); ok {
			relations, meta = map[string]any{}, map[string]any{}
			types = append(types, map[string]any{"type": kind, "relations": relations, "metadata": map[string]any{"relations": meta}})
			continue
		}
		if text, ok := strings.CutPrefix(line, "define "); ok {
			name, expr, _ := strings.Cut(text, ": ")
			relations[name] = expression(expr)
			if m := direct.FindStringSubmatch(expr); m != nil {
				meta[name] = map[string]any{"directly_related_user_types": []map[string]string{{"type": m[1]}}}
			}
		}
	}
	return map[string]any{"schema_version": "1.1", "type_definitions": types}
}

var direct = regexp.MustCompile(`^\[([a-z_]+)\]`)

func expression(expr string) map[string]any {
	for _, operator := range []struct{ sep, name string }{{" or ", "union"}, {" and ", "intersection"}} {
		if parts := strings.Split(expr, operator.sep); len(parts) > 1 {
			children := []map[string]any{}
			for _, p := range parts {
				children = append(children, expression(p))
			}
			return map[string]any{operator.name: map[string]any{"child": children}}
		}
	}
	if direct.MatchString(expr) {
		return map[string]any{"this": map[string]any{}}
	}
	if computed, tuple, ok := strings.Cut(expr, " from "); ok {
		return map[string]any{"tupleToUserset": map[string]any{"tupleset": map[string]string{"relation": tuple}, "computedUserset": map[string]string{"relation": computed}}}
	}
	return map[string]any{"computedUserset": map[string]string{"relation": expr}}
}
