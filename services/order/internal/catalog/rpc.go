package catalog

import (
	"context"
	"errors"
	"log/slog"

	"connectrpc.com/connect"
	"github.com/google/uuid"

	orderv1 "github.com/Yulian302/deliverit/gen/deliverit/order/v1"
	"github.com/Yulian302/deliverit/gen/deliverit/order/v1/orderv1connect"
)

// Handler serves the catalog part of OrderService over Connect-RPC. It only
// translates: protobuf ⇄ model, and errors → Connect codes. The rules live in
// Service.
//
// OrderService is served by two packages (catalog and ordering), each with
// its own Handler. Embedding Unimplemented… makes each Handler a complete
// OrderServiceHandler on its own (so tests can serve it alone); Order's main
// embeds both, and Go picks each package's real method over the embedded
// stub because it is shallower.
type Handler struct {
	orderv1connect.UnimplementedOrderServiceHandler
	svc *Service
	log *slog.Logger
}

var _ orderv1connect.OrderServiceHandler = (*Handler)(nil)

// NewHandler returns a Handler; log receives the internal errors that are
// hidden from callers.
func NewHandler(svc *Service, log *slog.Logger) *Handler {
	return &Handler{svc: svc, log: log}
}

// ListRestaurants implements orderv1connect.OrderServiceHandler.
func (h *Handler) ListRestaurants(ctx context.Context, req *orderv1.ListRestaurantsRequest) (*orderv1.ListRestaurantsResponse, error) {
	rs, err := h.svc.ListNearby(ctx, NearbyQuery{
		Center:   Point{Lat: req.GetLat(), Lng: req.GetLng()},
		RadiusKm: req.GetRadiusKm(),
		Limit:    int(req.GetLimit()),
	})
	if err != nil {
		return nil, h.toConnectError(ctx, err)
	}

	resp := &orderv1.ListRestaurantsResponse{Restaurants: make([]*orderv1.Restaurant, 0, len(rs))}
	for _, r := range rs {
		resp.Restaurants = append(resp.Restaurants, &orderv1.Restaurant{
			Id:          r.ID.String(),
			Name:        r.Name,
			Lat:         r.Location.Lat,
			Lng:         r.Location.Lng,
			PrepMinutes: int32(r.PrepMinutes),
			DistanceKm:  r.DistanceKm,
		})
	}
	return resp, nil
}

// GetMenu implements orderv1connect.OrderServiceHandler.
func (h *Handler) GetMenu(ctx context.Context, req *orderv1.GetMenuRequest) (*orderv1.GetMenuResponse, error) {
	id, err := uuid.Parse(req.GetRestaurantId())
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("restaurant_id must be a UUID"))
	}
	r, items, err := h.svc.Menu(ctx, id)
	if err != nil {
		return nil, h.toConnectError(ctx, err)
	}
	resp := &orderv1.GetMenuResponse{
		Restaurant: &orderv1.Restaurant{
			Id:          r.ID.String(),
			Name:        r.Name,
			Lat:         r.Location.Lat,
			Lng:         r.Location.Lng,
			PrepMinutes: int32(r.PrepMinutes),
		},
		Items:    make([]*orderv1.MenuItem, 0, len(items)),
		Currency: Currency,
	}
	for _, it := range items {
		resp.Items = append(resp.Items, &orderv1.MenuItem{
			Id:         it.ID.String(),
			Name:       it.Name,
			PriceMinor: it.PriceMinor,
			Available:  it.Available,
		})
	}
	return resp, nil
}

// toConnectError decides what the caller learns about a failure.
//
//   - Bad input: INVALID_ARGUMENT with our own validation message (safe to
//     show: we wrote it, and it tells the caller what to fix).
//   - The caller's deadline ran out or it hung up: DEADLINE_EXCEEDED or
//     CANCELED. That's the caller's timeout, not our bug, so not an error log.
//   - Anything else (DB down, bad SQL...): INTERNAL with a generic message.
//     The real error is logged here and never sent: it can reveal schema,
//     hostnames or query text.
func (h *Handler) toConnectError(ctx context.Context, err error) error {
	switch {
	case errors.Is(err, ErrInvalidArgument):
		return connect.NewError(connect.CodeInvalidArgument, err)
	case errors.Is(err, ErrNotFound):
		return connect.NewError(connect.CodeNotFound, errors.New("restaurant not found"))
	case errors.Is(err, context.DeadlineExceeded):
		return connect.NewError(connect.CodeDeadlineExceeded, errors.New("deadline exceeded"))
	case errors.Is(err, context.Canceled):
		return connect.NewError(connect.CodeCanceled, errors.New("request canceled"))
	default:
		h.log.ErrorContext(ctx, "catalog rpc failed", slog.Any("error", err))
		return connect.NewError(connect.CodeInternal, errors.New("internal error"))
	}
}
