package api

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	orderv1 "github.com/Yulian302/deliverit/gen/deliverit/order/v1"
	"github.com/Yulian302/deliverit/gen/deliverit/order/v1/orderv1connect"
)

// NewRestaurantsHandler serves GET /v1/restaurants?near=lat,lng[&radiusKm=][&limit=]
// by asking Order for nearby restaurants.
//
// Validation is split on purpose: the Gateway only checks that the query
// *parses* (two numbers in near, an integer limit...). Range rules (lat within
// ±90, radius at most 20 km...) live only in Order, which answers
// InvalidArgument; that becomes the same 400. One place for the rules, so
// they can't drift apart between services.
func NewRestaurantsHandler(order orderv1connect.OrderServiceClient, log *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req, err := parseRestaurantsQuery(r.URL.Query())
		if err != nil {
			WriteProblem(w, http.StatusBadRequest, CodeInvalidArgument, err.Error())
			return
		}

		resp, err := order.ListRestaurants(r.Context(), req)
		if err != nil {
			writeUpstreamError(w, r, log, err)
			return
		}

		out := restaurantsResponse{Restaurants: make([]restaurantJSON, 0, len(resp.GetRestaurants()))}
		for _, x := range resp.GetRestaurants() {
			out.Restaurants = append(out.Restaurants, restaurantJSON{
				ID:          x.GetId(),
				Name:        x.GetName(),
				Lat:         x.GetLat(),
				Lng:         x.GetLng(),
				PrepMinutes: x.GetPrepMinutes(),
				DistanceKm:  x.GetDistanceKm(),
			})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out) // the client may be gone; nothing useful to do on error
	})
}

// The public JSON shape. Separate from the protobuf types on purpose: the
// internal RPC contract can change without changing what browsers see.
type restaurantsResponse struct {
	Restaurants []restaurantJSON `json:"restaurants"` // always an array, never null
}

type restaurantJSON struct {
	ID          string  `json:"id"`
	Name        string  `json:"name"`
	Lat         float64 `json:"lat"`
	Lng         float64 `json:"lng"`
	PrepMinutes int32   `json:"prepMinutes"`
	DistanceKm  float64 `json:"distanceKm"`
}

// parseRestaurantsQuery checks the query's format only. Its error messages
// are shown to the caller, so they are fixed texts: no echo of raw input.
func parseRestaurantsQuery(q url.Values) (*orderv1.ListRestaurantsRequest, error) {
	near := q.Get("near")
	if near == "" {
		return nil, errors.New("near is required, as near=lat,lng")
	}
	parts := strings.Split(near, ",")
	if len(parts) != 2 {
		return nil, errors.New("near must be two numbers: near=lat,lng")
	}
	lat, errLat := strconv.ParseFloat(strings.TrimSpace(parts[0]), 64)
	lng, errLng := strconv.ParseFloat(strings.TrimSpace(parts[1]), 64)
	if errLat != nil || errLng != nil {
		return nil, errors.New("near must be two numbers: near=lat,lng")
	}
	req := &orderv1.ListRestaurantsRequest{Lat: lat, Lng: lng}

	if s := q.Get("radiusKm"); s != "" {
		radius, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return nil, errors.New("radiusKm must be a number")
		}
		req.RadiusKm = radius
	}
	if s := q.Get("limit"); s != "" {
		limit, err := strconv.ParseInt(s, 10, 32) // int32: the RPC field's size
		if err != nil {
			return nil, errors.New("limit must be a whole number")
		}
		req.Limit = int32(limit)
	}
	return req, nil
}
