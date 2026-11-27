package api

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/google/uuid"

	orderv1 "github.com/Yulian302/deliverit/gen/deliverit/order/v1"
	"github.com/Yulian302/deliverit/gen/deliverit/order/v1/orderv1connect"
)

// NewMenuHandler serves GET /v1/restaurants/{id}/menu (public).
func NewMenuHandler(order orderv1connect.OrderServiceClient, log *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if _, err := uuid.Parse(id); err != nil {
			WriteProblem(w, http.StatusBadRequest, CodeInvalidArgument, "restaurant id must be a UUID")
			return
		}
		resp, err := order.GetMenu(r.Context(), &orderv1.GetMenuRequest{RestaurantId: id})
		if err != nil {
			writeUpstreamError(w, r, log, err)
			return
		}
		out := menuResponse{
			Restaurant: menuRestaurantJSON{ID: resp.GetRestaurant().GetId(), Name: resp.GetRestaurant().GetName()},
			Currency:   resp.GetCurrency(),
			Items:      make([]menuItemJSON, 0, len(resp.GetItems())),
		}
		for _, it := range resp.GetItems() {
			out.Items = append(out.Items, menuItemJSON{
				ID: it.GetId(), Name: it.GetName(), PriceMinor: it.GetPriceMinor(), Available: it.GetAvailable(),
			})
		}
		writeJSON(w, http.StatusOK, out)
	})
}

type menuResponse struct {
	Restaurant menuRestaurantJSON `json:"restaurant"`
	Currency   string             `json:"currency"`
	Items      []menuItemJSON     `json:"items"` // always an array, never null
}

type menuRestaurantJSON struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type menuItemJSON struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	PriceMinor int64  `json:"priceMinor"`
	Available  bool   `json:"available"`
}

// writeJSON writes v as the JSON response body with status.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v) // the client may be gone; nothing useful to do on error
}
