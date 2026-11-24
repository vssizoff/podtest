package authorization

import (
	"context"
	"errors"
	"fmt"
)

var ErrUnavailable = errors.New("authorization service unavailable")

type Tuple struct {
	User     string
	Relation string
	Object   string
}
type Client interface {
	Check(context.Context, string, string, string) (bool, error)
	BatchCheck(context.Context, []Tuple, string) ([]bool, error)
	Write(context.Context, []Tuple, []Tuple) error
}
type Manager struct{ client Client }

func NewManager(client Client) *Manager { return &Manager{client: client} }
func (m *Manager) Check(ctx context.Context, user, permission, object string) (bool, error) {
	if m.client == nil {
		return false, ErrUnavailable
	}
	ok, err := m.client.Check(ctx, user, permission, object)
	if err != nil {
		return false, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	return ok, nil
}
func (m *Manager) Require(ctx context.Context, user, permission, object string) error {
	ok, err := m.Check(ctx, user, permission, object)
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("permission denied")
	}
	return nil
}
func (m *Manager) BatchCheck(ctx context.Context, checks []Tuple, permission string) ([]bool, error) {
	if m.client == nil {
		return nil, ErrUnavailable
	}
	allowed, err := m.client.BatchCheck(ctx, checks, permission)
	if err != nil {
		return nil, fmt.Errorf("OpenFGA batch check: %w", err)
	}
	if len(allowed) != len(checks) {
		return nil, errors.New("OpenFGA returned incomplete batch")
	}
	return allowed, nil
}
func (m *Manager) Sync(ctx context.Context, writes, deletes []Tuple) error {
	if m.client == nil {
		return ErrUnavailable
	}
	if err := m.client.Write(ctx, writes, deletes); err != nil {
		return fmt.Errorf("OpenFGA tuple write: %w", err)
	}
	return nil
}
