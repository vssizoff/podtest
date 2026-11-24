package authorization

import (
	"context"
	"errors"
	"testing"
)

type failingClient struct{}

func (failingClient) Check(context.Context, string, string, string) (bool, error) {
	return false, errors.New("unavailable")
}
func (failingClient) BatchCheck(context.Context, []Tuple, string) ([]bool, error) {
	return nil, errors.New("unavailable")
}
func (failingClient) Write(context.Context, []Tuple, []Tuple) error { return errors.New("unavailable") }
func TestOpenFGAErrorFailsClosed(t *testing.T) {
	m := NewManager(failingClient{})
	allowed, err := m.Check(context.Background(), "user:1", "can_view", "contest:1")
	if err == nil || allowed {
		t.Fatal("OpenFGA errors must not allow access")
	}
}
