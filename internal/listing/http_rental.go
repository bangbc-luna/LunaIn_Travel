package listing

import (
	"net/http"

	"github.com/google/uuid"

	"github.com/bangbc-luna/lunain-travel/internal/platform/httpx"
)

// rentalRoutes registers the self-drive rental routes: store, vehicle models
// and pickup points. Each one answers ErrNotRentalListing for a listing of another
// service type.
func (s *Service) rentalRoutes(mux httpx.Router, withID func(h func(w http.ResponseWriter, r *http.Request, partnerID, userID, id uuid.UUID)) http.Handler) {
	reply := func(w http.ResponseWriter, r *http.Request, status int, v any, err error) {
		if err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		httpx.WriteJSON(w, status, v)
	}

	mux.Handle("GET /v1/partner/listings/{id}/rental", withID(func(w http.ResponseWriter, r *http.Request, partnerID, _, id uuid.UUID) {
		d, err := s.GetRental(r.Context(), partnerID, id)
		reply(w, r, http.StatusOK, d, err)
	}))

	mux.Handle("PUT /v1/partner/listings/{id}/rental-store", withID(func(w http.ResponseWriter, r *http.Request, partnerID, userID, id uuid.UUID) {
		var in RentalStoreInput
		if err := httpx.DecodeJSON(r, &in); err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		st, err := s.SetRentalStore(r.Context(), partnerID, userID, id, in)
		reply(w, r, http.StatusOK, st, err)
	}))

	mux.Handle("POST /v1/partner/listings/{id}/rental-pickup-points", withID(func(w http.ResponseWriter, r *http.Request, partnerID, userID, id uuid.UUID) {
		var in PickupPointInput
		if err := httpx.DecodeJSON(r, &in); err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		pp, err := s.CreatePickupPoint(r.Context(), partnerID, userID, id, in)
		reply(w, r, http.StatusCreated, pp, err)
	}))

	mux.Handle("PATCH /v1/partner/listings/{id}/rental-pickup-points/{pointID}", withID(func(w http.ResponseWriter, r *http.Request, partnerID, userID, id uuid.UUID) {
		pointID, err := uuid.Parse(r.PathValue("pointID"))
		if err != nil {
			httpx.WriteError(w, r, httpx.ErrNotFound)
			return
		}
		var in PickupPointPatch
		if err := httpx.DecodeJSON(r, &in); err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		pp, err := s.UpdatePickupPoint(r.Context(), partnerID, userID, id, pointID, in)
		reply(w, r, http.StatusOK, pp, err)
	}))

	mux.Handle("POST /v1/partner/listings/{id}/rental-models", withID(func(w http.ResponseWriter, r *http.Request, partnerID, userID, id uuid.UUID) {
		var in RentalModelInput
		if err := httpx.DecodeJSON(r, &in); err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		m, err := s.CreateRentalModel(r.Context(), partnerID, userID, id, in)
		reply(w, r, http.StatusCreated, m, err)
	}))

	mux.Handle("PATCH /v1/partner/listings/{id}/rental-models/{modelID}", withID(func(w http.ResponseWriter, r *http.Request, partnerID, userID, id uuid.UUID) {
		modelID, err := uuid.Parse(r.PathValue("modelID"))
		if err != nil {
			httpx.WriteError(w, r, httpx.ErrNotFound)
			return
		}
		var in RentalModelPatch
		if err := httpx.DecodeJSON(r, &in); err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		m, err := s.UpdateRentalModel(r.Context(), partnerID, userID, id, modelID, in)
		reply(w, r, http.StatusOK, m, err)
	}))
}
