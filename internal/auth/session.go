package auth

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"net/http"
	"net/url"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"podtest/internal/db"
)

type User struct {
	ID     string `json:"id"`
	Name   string `json:"display_name"`
	Email  string `json:"email"`
	Avatar string `json:"avatar_url"`
}

type Sessions struct {
	pool     *pgxpool.Pool
	provider *Provider
	origin   string
	secure   bool
}

func NewSessions(pool *pgxpool.Pool, provider *Provider, origin string, development bool) (*Sessions, error) {
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil || (u.Scheme != "https" && !(development && u.Scheme == "http" && (u.Hostname() == "localhost" || u.Hostname() == "127.0.0.1"))) {
		return nil, errors.New("PUBLIC_URL must be an HTTPS origin or a local development HTTP origin")
	}
	return &Sessions{pool: pool, provider: provider, origin: origin, secure: u.Scheme == "https"}, nil
}

func (s *Sessions) cookie(w http.ResponseWriter, name, value string, age int) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: value, MaxAge: age, Path: "/", HttpOnly: true, Secure: s.secure, SameSite: http.SameSiteLaxMode})
}

func (s *Sessions) Begin(w http.ResponseWriter, r *http.Request) error {
	flow, redirect, err := s.provider.Begin()
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(r.Context(), `INSERT INTO oidc_flows(state_hash,verifier,nonce,expires_at) VALUES ($1,$2,$3,$4)`, flow.StateHash, flow.Verifier, flow.Nonce, flow.Expires)
	if err != nil {
		return err
	}
	s.cookie(w, "podtest_flow", flow.State, 600)
	http.Redirect(w, r, redirect, http.StatusFound)
	return nil
}

func (s *Sessions) Callback(w http.ResponseWriter, r *http.Request) error {
	q := r.URL.Query()
	c, err := r.Cookie("podtest_flow")
	if err != nil || q.Get("state") == "" || subtle.ConstantTimeCompare([]byte(c.Value), []byte(q.Get("state"))) != 1 {
		return errors.New("invalid OAuth state")
	}
	s.cookie(w, "podtest_flow", "", -1)
	var verifier, nonce string
	err = s.pool.QueryRow(r.Context(), `DELETE FROM oidc_flows WHERE state_hash=$1 AND expires_at>now() RETURNING verifier,nonce`, hash(c.Value)).Scan(&verifier, &nonce)
	if err != nil {
		return err
	}
	if q.Get("error") != "" || q.Get("code") == "" {
		return errors.New("authorization declined")
	}
	identity, err := s.provider.Exchange(r.Context(), q.Get("code"), verifier, nonce)
	if err != nil {
		return err
	}
	raw, err := randomString(32)
	if err != nil {
		return err
	}
	err = db.Tx(r.Context(), s.pool, func(tx pgx.Tx) error {
		id, err := upsertIdentity(r.Context(), tx, identity)
		if err != nil {
			return err
		}
		if old, err := r.Cookie("podtest_session"); err == nil {
			if _, err = tx.Exec(r.Context(), `UPDATE auth_sessions SET revoked_at=now() WHERE session_token_hash=$1 AND revoked_at IS NULL`, hash(old.Value)); err != nil {
				return err
			}
		}
		_, err = tx.Exec(r.Context(), `INSERT INTO auth_sessions(session_token_hash,user_id,expires_at) VALUES($1,$2,now()+interval '12 hours')`, hash(raw), id)
		return err
	})
	if err != nil {
		return err
	}
	s.cookie(w, "podtest_session", raw, int((12 * time.Hour).Seconds()))
	http.Redirect(w, r, "/", http.StatusSeeOther)
	return nil
}

func upsertIdentity(ctx context.Context, tx pgx.Tx, i Identity) (string, error) {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended(jsonb_build_array($1::text,$2::text)::text,0))`, i.Issuer, i.Subject); err != nil {
		return "", err
	}
	var id, status string
	err := tx.QueryRow(ctx, `SELECT u.id,u.status FROM user_identities i JOIN users u ON u.id=i.user_id WHERE issuer=$1 AND subject=$2`, i.Issuer, i.Subject).Scan(&id, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		if err = tx.QueryRow(ctx, `INSERT INTO users(display_name,email,avatar_url) VALUES($1,$2,$3) RETURNING id`, i.Name, i.Email, i.AvatarURL).Scan(&id); err != nil {
			return "", err
		}
		_, err = tx.Exec(ctx, `INSERT INTO user_identities(user_id,issuer,subject) VALUES($1,$2,$3)`, id, i.Issuer, i.Subject)
		return id, err
	}
	if err != nil {
		return "", err
	}
	if status != "active" {
		return "", errors.New("account disabled")
	}
	_, err = tx.Exec(ctx, `UPDATE users SET display_name=$2,email=$3,avatar_url=$4,updated_at=now() WHERE id=$1`, id, i.Name, i.Email, i.AvatarURL)
	return id, err
}

func (s *Sessions) Current(r *http.Request) (User, string, error) {
	c, err := r.Cookie("podtest_session")
	if err != nil {
		return User{}, "", err
	}
	var user User
	err = s.pool.QueryRow(r.Context(), `SELECT u.id,u.display_name,u.email,u.avatar_url FROM auth_sessions s JOIN users u ON u.id=s.user_id WHERE s.session_token_hash=$1 AND s.expires_at>now() AND s.revoked_at IS NULL AND u.status='active'`, hash(c.Value)).Scan(&user.ID, &user.Name, &user.Email, &user.Avatar)
	if err != nil {
		return User{}, "", err
	}
	token := sha256.Sum256([]byte("csrf-v1:" + c.Value))
	return user, base64.RawURLEncoding.EncodeToString(token[:]), nil
}

func (s *Sessions) ValidCSRF(r *http.Request, token string) bool {
	return token != "" && r.Header.Get("Origin") == s.origin && subtle.ConstantTimeCompare([]byte(r.Header.Get("X-CSRF-Token")), []byte(token)) == 1
}

func (s *Sessions) Logout(w http.ResponseWriter, r *http.Request) error {
	c, err := r.Cookie("podtest_session")
	if err != nil {
		return err
	}
	if _, err = s.pool.Exec(r.Context(), `UPDATE auth_sessions SET revoked_at=now() WHERE session_token_hash=$1`, hash(c.Value)); err != nil {
		return err
	}
	s.cookie(w, "podtest_session", "", -1)
	return nil
}
