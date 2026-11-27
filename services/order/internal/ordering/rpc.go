package ordering

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"google.golang.org/protobuf/types/known/timestamppb"

	orderv1 "github.com/Yulian302/deliverit/gen/deliverit/order/v1"
	"github.com/Yulian302/deliverit/gen/deliverit/order/v1/orderv1connect"
	"github.com/Yulian302/deliverit/platform/identity"
)

// Handler serves the ordering part of OrderService over Connect-RPC. It only
// translates: protobuf ⇄ model, and errors → Connect codes. The caller comes
// from platform/identity (the Gateway sends it). Embedding Unimplemented…
// makes it a complete OrderServiceHandler on its own (see catalog.Handler).
type Handler struct {
	orderv1connect.UnimplementedOrderServiceHandler
	svc *Service
	log *slog.Logger
}

// NewHandler returns a Handler; log receives the internal errors that are
// hidden from callers.
func NewHandler(svc *Service, log *slog.Logger) *Handler {
	return &Handler{svc: svc, log: log}
}

// PlaceOrder implements orderv1connect.OrderServiceHandler.
func (h *Handler) PlaceOrder(ctx context.Context, req *orderv1.PlaceOrderRequest) (*orderv1.PlaceOrderResponse, error) {
	caller, ok := identity.FromContext(ctx)
	if !ok {
		return nil, h.toConnectError(ctx, ErrUnauthenticated)
	}
	in, err := placeInputFromProto(req)
	if err != nil {
		return nil, h.toConnectError(ctx, err)
	}
	o, replayed, err := h.svc.Place(ctx, caller, in)
	if err != nil {
		return nil, h.toConnectError(ctx, err)
	}
	return &orderv1.PlaceOrderResponse{Order: orderToProto(o), Replayed: replayed}, nil
}

// GetOrder implements orderv1connect.OrderServiceHandler.
func (h *Handler) GetOrder(ctx context.Context, req *orderv1.GetOrderRequest) (*orderv1.GetOrderResponse, error) {
	caller, ok := identity.FromContext(ctx)
	if !ok {
		return nil, h.toConnectError(ctx, ErrUnauthenticated)
	}
	id, err := uuid.Parse(req.GetOrderId())
	if err != nil {
		// A malformed ID can't name a visible order: same answer as a missing one.
		return nil, h.toConnectError(ctx, ErrNotFound)
	}
	o, err := h.svc.Get(ctx, caller, id)
	if err != nil {
		return nil, h.toConnectError(ctx, err)
	}
	return &orderv1.GetOrderResponse{Order: orderToProto(o)}, nil
}

func placeInputFromProto(req *orderv1.PlaceOrderRequest) (PlaceInput, error) {
	restaurantID, err := uuid.Parse(req.GetRestaurantId())
	if err != nil {
		return PlaceInput{}, invalid("restaurant_id must be a UUID")
	}
	if req.GetDelivery() == nil {
		return PlaceInput{}, invalid("delivery is required")
	}
	lines := make([]Line, 0, len(req.GetLines()))
	for i, l := range req.GetLines() {
		id, err := uuid.Parse(l.GetMenuItemId())
		if err != nil {
			return PlaceInput{}, invalid("line %d: menu_item_id must be a UUID", i+1)
		}
		lines = append(lines, Line{MenuItemID: id, Quantity: int(l.GetQuantity())})
	}
	d := req.GetDelivery()
	return PlaceInput{
		IdempotencyKey: req.GetIdempotencyKey(),
		RestaurantID:   restaurantID,
		Lines:          lines,
		Delivery:       Delivery{Lat: d.GetLat(), Lng: d.GetLng(), Address: d.GetAddress()},
	}, nil
}

var statusToProto = map[Status]orderv1.OrderStatus{
	StatusPendingPayment:     orderv1.OrderStatus_ORDER_STATUS_PENDING_PAYMENT,
	StatusAwaitingRestaurant: orderv1.OrderStatus_ORDER_STATUS_AWAITING_RESTAURANT,
	StatusPreparing:          orderv1.OrderStatus_ORDER_STATUS_PREPARING,
	StatusReady:              orderv1.OrderStatus_ORDER_STATUS_READY,
	StatusPickedUp:           orderv1.OrderStatus_ORDER_STATUS_PICKED_UP,
	StatusDelivered:          orderv1.OrderStatus_ORDER_STATUS_DELIVERED,
	StatusCancelled:          orderv1.OrderStatus_ORDER_STATUS_CANCELLED,
	StatusFailed:             orderv1.OrderStatus_ORDER_STATUS_FAILED,
}

func orderToProto(o Order) *orderv1.Order {
	out := &orderv1.Order{
		Id:           o.ID.String(),
		CustomerId:   o.CustomerID.String(),
		RestaurantId: o.RestaurantID.String(),
		Status:       statusToProto[o.Status], // unknown → UNSPECIFIED
		Items:        make([]*orderv1.OrderItem, 0, len(o.Items)),
		TotalMinor:   o.TotalMinor,
		Currency:     o.Currency,
		Delivery:     &orderv1.DeliveryLocation{Lat: o.Delivery.Lat, Lng: o.Delivery.Lng, Address: o.Delivery.Address},
		CreatedAt:    timestamppb.New(o.CreatedAt),
		Version:      int32(o.Version),
	}
	for _, it := range o.Items {
		out.Items = append(out.Items, &orderv1.OrderItem{
			MenuItemId:     it.MenuItemID.String(),
			Name:           it.Name,
			UnitPriceMinor: it.UnitPriceMinor,
			Quantity:       int32(it.Quantity),
		})
	}
	return out
}

// toConnectError decides what the caller learns about a failure (same policy
// as catalog): our own validation messages are safe to show; ctx errors are
// the caller's timeout; anything else is logged and answered as a generic
// INTERNAL, because it can reveal schema, hosts or query text.
func (h *Handler) toConnectError(ctx context.Context, err error) error {
	switch {
	case errors.Is(err, ErrInvalidArgument):
		return connect.NewError(connect.CodeInvalidArgument, err)
	case errors.Is(err, ErrNotFound):
		return connect.NewError(connect.CodeNotFound, errors.New("order not found"))
	case errors.Is(err, ErrKeyReused):
		return connect.NewError(connect.CodeAlreadyExists, ErrKeyReused)
	case errors.Is(err, ErrUnauthenticated):
		return connect.NewError(connect.CodeUnauthenticated, errors.New("no caller identity"))
	case errors.Is(err, ErrPermissionDenied):
		return connect.NewError(connect.CodePermissionDenied, err)
	case errors.Is(err, context.DeadlineExceeded):
		return connect.NewError(connect.CodeDeadlineExceeded, errors.New("deadline exceeded"))
	case errors.Is(err, context.Canceled):
		return connect.NewError(connect.CodeCanceled, errors.New("request canceled"))
	default:
		h.log.ErrorContext(ctx, "ordering rpc failed", slog.Any("error", err))
		return connect.NewError(connect.CodeInternal, fmt.Errorf("internal error"))
	}
}
