package service

import (
	"cmp"
	"context"
	"fmt"
	"slices"

	"rockets-service/internal/model"
	"rockets-service/internal/storage"
)

// GetRocket returns one rocket. Unknown rockets and rockets that have not
// launched yet are ErrNotFound: without RocketLaunched there is no type,
// mission or speed to show.
func (s *Service) GetRocket(ctx context.Context, id string) (model.Rocket, error) {
	var r model.Rocket
	var found bool
	err := s.store.InTx(ctx, func(rockets storage.RocketTable, _ storage.PendingTable) error {
		var err error
		r, found, err = rockets.Get(id)
		return err
	})
	if err != nil {
		return model.Rocket{}, err
	}
	if !found || !r.Launched() {
		return model.Rocket{}, ErrNotFound
	}
	return r, nil
}

// Defaults for ListRockets.
const (
	DefaultSort  = storage.SortLaunchedAt
	DefaultOrder = "asc"
)

// ListRockets returns all launched rockets ordered by sortBy ("" means
// DefaultSort) and order ("asc" or "desc", "" means DefaultOrder). Ties are
// broken by id ascending. Unknown values wrap ErrInvalidSort.
func (s *Service) ListRockets(ctx context.Context, sortBy, order string) ([]model.Rocket, error) {
	field := storage.SortField(cmp.Or(sortBy, string(DefaultSort)))
	if !slices.Contains(storage.SortFields, field) {
		return nil, fmt.Errorf("%w: unknown sort field %q (want one of %v)", ErrInvalidSort, field, storage.SortFields)
	}
	order = cmp.Or(order, DefaultOrder)
	if order != "asc" && order != "desc" {
		return nil, fmt.Errorf("%w: unknown order %q (want asc or desc)", ErrInvalidSort, order)
	}
	opts := storage.ListOptions{SortBy: field, Desc: order == "desc", LaunchedOnly: true}

	var list []model.Rocket
	err := s.store.InTx(ctx, func(rockets storage.RocketTable, _ storage.PendingTable) error {
		var err error
		list, err = rockets.List(opts)
		return err
	})
	return list, err
}

// Debug describes every channel's progress and buffer.
func (s *Service) Debug(ctx context.Context) ([]model.ChannelInfo, error) {
	var infos []model.ChannelInfo
	err := s.store.InTx(ctx, func(_ storage.RocketTable, pending storage.PendingTable) error {
		var err error
		infos, err = pending.Summary()
		return err
	})
	return infos, err
}
