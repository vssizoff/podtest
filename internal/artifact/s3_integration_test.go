package artifact

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestS3SignedArtifact(t *testing.T) {
	if os.Getenv("PODTEST_S3_ACCEPTANCE") != "1" {
		t.Skip("requires isolated S3 bucket")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	s, err := New(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Init(ctx); err != nil {
		t.Fatal(err)
	}
	body := "verified artifact\n"
	sum := sha256.Sum256([]byte(body))
	key := "invoker/" + uuid.NewString()
	put, err := s.PresignUpload(ctx, key, time.Minute, hex.EncodeToString(sum[:]), int64(len(body)))
	if err != nil {
		t.Fatal("could not sign PUT")
	}
	u, err := url.Parse(put)
	if err != nil || u.Query().Get("X-Amz-SignedHeaders") != "content-length;host;x-amz-checksum-sha256" {
		t.Fatal("wrong PUT signature scope")
	}
	request := func(put, payload, checksum string) int {
		t.Helper()
		r, err := http.NewRequestWithContext(ctx, http.MethodPut, put, strings.NewReader(payload))
		if err != nil {
			t.Fatal("could not build PUT")
		}
		r.Header.Set("X-Amz-Checksum-Sha256", checksum)
		resp, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal("PUT transport failed")
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}
	checksum := base64.StdEncoding.EncodeToString(sum[:])
	if code := request(put, body+"x", checksum); code != 403 {
		t.Fatalf("altered length: status %d", code)
	}
	if code := request(put, body, base64.StdEncoding.EncodeToString(make([]byte, 32))); code != 403 {
		t.Fatalf("altered checksum header: status %d", code)
	}
	if code := request(put, strings.Repeat("x", len(body)), checksum); code != 400 {
		t.Fatalf("altered payload: status %d", code)
	}
	if code := request(put, body, checksum); code != 200 {
		t.Fatalf("valid PUT: status %d", code)
	}
	get, err := s.PresignDownload(ctx, key, time.Minute)
	if err != nil {
		t.Fatal("could not sign GET")
	}
	r, err := http.NewRequestWithContext(ctx, http.MethodGet, get, nil)
	if err != nil {
		t.Fatal("could not build GET")
	}
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal("GET transport failed")
	}
	defer resp.Body.Close()
	got, err := io.ReadAll(io.LimitReader(resp.Body, 1024))
	if err != nil || resp.StatusCode != 200 || string(got) != body {
		t.Fatal("signed GET did not return verified bytes")
	}
	shortGrant, err := s.PresignUpload(ctx, key, time.Second, hex.EncodeToString(sum[:]), int64(len(body)))
	if err != nil {
		t.Fatal("could not sign short-lived PUT")
	}
	// SigV4 timestamps have second-level precision. Cross a full extra second.
	select {
	case <-time.After(2 * time.Second):
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if code := request(shortGrant, body, checksum); code != 403 {
		t.Fatalf("expired PUT: status %d", code)
	}
}
