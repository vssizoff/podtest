package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"net/url"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"podtest/internal/artifact"
	pb "podtest/internal/invokerpb"
	"podtest/internal/judging"
)

func serveInvokerArtifacts(ctx context.Context, pool *pgxpool.Pool, store *artifact.Store) error {
	endpoint, err := url.Parse(os.Getenv("S3_URL"))
	if err != nil || endpoint.Scheme != "https" || endpoint.User != nil || endpoint.Path != "" || endpoint.RawQuery != "" || endpoint.Fragment != "" {
		return errors.New("invoker-artifacts requires an HTTPS S3 origin in S3_URL")
	}
	addr := os.Getenv("INVOKER_ARTIFACTS_ADDR")
	if addr == "" {
		return errors.New("INVOKER_ARTIFACTS_ADDR required")
	}
	cert, err := tls.LoadX509KeyPair(os.Getenv("INVOKER_TLS_CERT"), os.Getenv("INVOKER_TLS_KEY"))
	if err != nil {
		return err
	}
	pem, err := os.ReadFile(os.Getenv("INVOKER_CLIENT_CA"))
	if err != nil {
		return err
	}
	ca := x509.NewCertPool()
	if !ca.AppendCertsFromPEM(pem) {
		return errors.New("invalid Invoker client CA")
	}
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	defer listener.Close()
	server := grpc.NewServer(grpc.Creds(credentials.NewTLS(&tls.Config{
		MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{cert}, ClientCAs: ca, ClientAuth: tls.RequireAndVerifyClientCert,
	})), grpc.MaxRecvMsgSize(64<<10))
	pb.RegisterControlServer(server, &judging.ArtifactControl{Pool: pool, Store: store})
	stop := context.AfterFunc(ctx, server.Stop)
	defer stop()
	return server.Serve(listener)
}
