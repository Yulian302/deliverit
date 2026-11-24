package catalog

import (
	"context"
	"errors"
	"log/slog"

	"connectrpc.com/connect"

	orderv1 "github.com/Yulian302/deliverit/gen/deliverit/order/v1"
	"github.com/Yulian302/deliverit/gen/deliverit/order/v1/orderv1connect"
)

// Handler serves the catalog part of OrderService over Connect-RPC. It only
// translates: protobuf ⇄ model, and errors → Connect codes. The rules live in
// Service.
type Handler struct {
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
	case errors.Is(err, context.DeadlineExceeded):
		return connect.NewError(connect.CodeDeadlineExceeded, errors.New("deadline exceeded"))
	case errors.Is(err, context.Canceled):
		return connect.NewError(connect.CodeCanceled, errors.New("request canceled"))
	default:
		h.log.ErrorContext(ctx, "list restaurants failed", slog.Any("error", err))
		return connect.NewError(connect.CodeInternal, errors.New("internal error"))
	}
}
