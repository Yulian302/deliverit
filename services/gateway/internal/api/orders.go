package api

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	orderv1 "github.com/Yulian302/deliverit/gen/deliverit/order/v1"
	"github.com/Yulian302/deliverit/gen/deliverit/order/v1/orderv1connect"
)

// maxOrderBody bounds POST /v1/orders; a 20-line order is about 2 KiB.
const maxOrderBody = 16 << 10

// idempotencyKeyPattern matches Order's rule; checking it here too turns a
// bad key into a 400 without a round trip.
var idempotencyKeyPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// NewPlaceOrderHandler serves POST /v1/orders. Mount it behind
// RequireRole(customer). The Idempotency-Key header is required; repeating
// the request with the same key returns the original order (202 again, with
// Idempotent-Replayed: true) and never creates a second one.
func NewPlaceOrderHandler(order orderv1connect.OrderServiceClient, log *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("Idempotency-Key")
		if !idempotencyKeyPattern.MatchString(key) {
			WriteProblem(w, http.StatusBadRequest, CodeInvalidArgument,
				"Idempotency-Key header is required: 1-64 characters of A-Z, a-z, 0-9, _ or -")
			return
		}
		var body placeOrderBody
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxOrderBody))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&body); err != nil || dec.Decode(&struct{}{}) != io.EOF {
			// Fixed text: never echo what the client sent.
			WriteProblem(w, http.StatusBadRequest, CodeInvalidArgument,
				`body must be {"restaurantId", "items": [{"menuItemId", "quantity"}], "delivery": {"lat", "lng", "address"}}`)
			return
		}

		req := &orderv1.PlaceOrderRequest{
			IdempotencyKey: key,
			RestaurantId:   body.RestaurantID,
			Lines:          make([]*orderv1.OrderLine, 0, len(body.Items)),
		}
		for _, it := range body.Items {
			req.Lines = append(req.Lines, &orderv1.OrderLine{MenuItemId: it.MenuItemID, Quantity: it.Quantity})
		}
		if d := body.Delivery; d != nil {
			req.Delivery = &orderv1.DeliveryLocation{Lat: d.Lat, Lng: d.Lng, Address: d.Address}
		}

		resp, err := order.PlaceOrder(r.Context(), req)
		if err != nil {
			writeUpstreamError(w, r, log, err)
			return
		}
		o := orderFromProto(resp.GetOrder())
		w.Header().Set("Location", "/v1/orders/"+o.ID)
		if resp.GetReplayed() {
			w.Header().Set("Idempotent-Replayed", "true")
		}
		// 202: the order exists, but payment is still to happen.
		writeJSON(w, http.StatusAccepted, o)
	})
}

// NewGetOrderHandler serves GET /v1/orders/{id}. Mount it behind
// RequireRole(); Order decides who may see which order.
func NewGetOrderHandler(order orderv1connect.OrderServiceClient, log *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp, err := order.GetOrder(r.Context(), &orderv1.GetOrderRequest{OrderId: r.PathValue("id")})
		if err != nil {
			writeUpstreamError(w, r, log, err)
			return
		}
		writeJSON(w, http.StatusOK, orderFromProto(resp.GetOrder()))
	})
}

type placeOrderBody struct {
	RestaurantID string          `json:"restaurantId"`
	Items        []orderLineBody `json:"items"`
	Delivery     *deliveryJSON   `json:"delivery"`
}

type orderLineBody struct {
	MenuItemID string `json:"menuItemId"`
	Quantity   int32  `json:"quantity"`
}

type deliveryJSON struct {
	Lat     float64 `json:"lat"`
	Lng     float64 `json:"lng"`
	Address string  `json:"address"`
}

// orderJSON is the public order shape. Separate from the protobuf types on
// purpose, like restaurantJSON.
type orderJSON struct {
	ID           string          `json:"id"`
	CustomerID   string          `json:"customerId"`
	RestaurantID string          `json:"restaurantId"`
	Status       string          `json:"status"`
	Items        []orderItemJSON `json:"items"`
	TotalMinor   int64           `json:"totalMinor"`
	Currency     string          `json:"currency"`
	Delivery     deliveryJSON    `json:"delivery"`
	CreatedAt    string          `json:"createdAt"`
	Version      int32           `json:"version"`
}

type orderItemJSON struct {
	MenuItemID     string `json:"menuItemId"`
	Name           string `json:"name"`
	UnitPriceMinor int64  `json:"unitPriceMinor"`
	Quantity       int32  `json:"quantity"`
}

func orderFromProto(o *orderv1.Order) orderJSON {
	out := orderJSON{
		ID:           o.GetId(),
		CustomerID:   o.GetCustomerId(),
		RestaurantID: o.GetRestaurantId(),
		// ORDER_STATUS_PENDING_PAYMENT → PENDING_PAYMENT, the SPEC's name.
		Status:     strings.TrimPrefix(o.GetStatus().String(), "ORDER_STATUS_"),
		Items:      make([]orderItemJSON, 0, len(o.GetItems())),
		TotalMinor: o.GetTotalMinor(),
		Currency:   o.GetCurrency(),
		Delivery: deliveryJSON{
			Lat: o.GetDelivery().GetLat(), Lng: o.GetDelivery().GetLng(), Address: o.GetDelivery().GetAddress(),
		},
		CreatedAt: o.GetCreatedAt().AsTime().UTC().Format(time.RFC3339Nano),
		Version:   o.GetVersion(),
	}
	for _, it := range o.GetItems() {
		out.Items = append(out.Items, orderItemJSON{
			MenuItemID: it.GetMenuItemId(), Name: it.GetName(), UnitPriceMinor: it.GetUnitPriceMinor(), Quantity: it.GetQuantity(),
		})
	}
	return out
}
