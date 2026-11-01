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
	r, found, err := s.store.GetRocket(ctx, id)
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
	return s.store.ListRockets(ctx, storage.ListOptions{SortBy: field, Desc: order == "desc", LaunchedOnly: true})
}

// Debug describes every channel's watermark and buffer.
func (s *Service) Debug(ctx context.Context) ([]model.ChannelInfo, error) {
	return s.store.Channels(ctx)
}
