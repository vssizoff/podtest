package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

type Provider struct {
	Issuer, ClientID, ClientSecret, RedirectURL string
	discovery                                   *oidc.Provider
	verifier                                    *oidc.IDTokenVerifier
	oauth                                       oauth2.Config
	httpClient                                  *http.Client
}

func NewProvider(ctx context.Context, issuer, clientID, clientSecret, redirectURL string) (*Provider, error) {
	if issuer == "" || clientID == "" || clientSecret == "" {
		return nil, errors.New("OIDC issuer and client credentials are required")
	}
	dev := strings.EqualFold(os.Getenv("APP_ENV"), "development")
	if !dev {
		u, err := url.Parse(issuer)
		if err != nil || u.Scheme != "https" {
			return nil, errors.New("OIDC issuer must use HTTPS outside development")
		}
	}
	var client *http.Client
	if addr := os.Getenv("OIDC_DEV_DIAL_ADDR"); addr != "" {
		u, err := url.Parse(issuer)
		if err != nil || !dev || u.Scheme != "http" || u.Hostname() != "localhost" || u.Port() == "" || u.User != nil {
			return nil, errors.New("OIDC_DEV_DIAL_ADDR requires a local development issuer")
		}
		// Route server-side calls to Dex without changing the issuer or URL host.
		dialer := &net.Dialer{Timeout: 5 * time.Second}
		client = &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, target string) (net.Conn, error) {
				if target != u.Host {
					return nil, errors.New("unexpected development OIDC endpoint")
				}
				return dialer.DialContext(ctx, network, addr)
			},
		}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		ctx = oidc.ClientContext(ctx, client)
	}
	provider, err := oidc.NewProvider(ctx, issuer)
	if err != nil {
		return nil, err
	}
	return &Provider{Issuer: issuer, ClientID: clientID, ClientSecret: clientSecret, RedirectURL: redirectURL, discovery: provider, verifier: provider.Verifier(&oidc.Config{ClientID: clientID}), oauth: oauth2.Config{ClientID: clientID, ClientSecret: clientSecret, Endpoint: provider.Endpoint(), RedirectURL: redirectURL, Scopes: []string{oidc.ScopeOpenID, "profile", "email"}}, httpClient: client}, nil
}
func randomString(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

type Flow struct {
	State     string
	StateHash []byte
	Nonce     string
	Verifier  string
	Expires   time.Time
}

func (p *Provider) Begin() (Flow, string, error) {
	state, err := randomString(32)
	if err != nil {
		return Flow{}, "", err
	}
	nonce, err := randomString(32)
	if err != nil {
		return Flow{}, "", err
	}
	verifier, err := randomString(32)
	if err != nil {
		return Flow{}, "", err
	}
	challenge := oauth2.S256ChallengeFromVerifier(verifier)
	return Flow{State: state, StateHash: hash(state), Nonce: nonce, Verifier: verifier, Expires: time.Now().Add(10 * time.Minute)}, p.oauth.AuthCodeURL(state, oauth2.SetAuthURLParam("nonce", nonce), oauth2.SetAuthURLParam("code_challenge", challenge), oauth2.SetAuthURLParam("code_challenge_method", "S256")), nil
}
func hash(s string) []byte { h := sha256.Sum256([]byte(s)); return h[:] }

type Identity struct {
	Issuer    string
	Subject   string
	Name      string
	Email     string
	AvatarURL string
}

func (p *Provider) Exchange(ctx context.Context, code, verifier, nonce string) (Identity, error) {
	if p.httpClient != nil {
		ctx = oidc.ClientContext(ctx, p.httpClient)
	}
	token, err := p.oauth.Exchange(ctx, code, oauth2.VerifierOption(verifier))
	if err != nil {
		return Identity{}, err
	}
	raw, ok := token.Extra("id_token").(string)
	if !ok {
		return Identity{}, errors.New("OIDC response has no ID token")
	}
	id, err := p.verifier.Verify(ctx, raw)
	if err != nil {
		return Identity{}, err
	}
	if id.Issuer != p.Issuer || id.Nonce != nonce || nonce == "" {
		return Identity{}, errors.New("OIDC issuer or nonce mismatch")
	}
	var claims struct {
		Name   string `json:"name"`
		Email  string `json:"email"`
		Avatar string `json:"picture"`
	}
	if err := id.Claims(&claims); err != nil {
		return Identity{}, err
	}
	if id.Subject == "" {
		return Identity{}, errors.New("OIDC subject is missing")
	}
	return Identity{Issuer: id.Issuer, Subject: id.Subject, Name: claims.Name, Email: claims.Email, AvatarURL: claims.Avatar}, nil
}
