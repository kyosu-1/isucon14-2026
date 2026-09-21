package main

import (
	"database/sql"
	"errors"
	"fmt"
	"net/http"

	"github.com/oklog/ulid/v2"
)

type chairPostChairsRequest struct {
	Name               string `json:"name"`
	Model              string `json:"model"`
	ChairRegisterToken string `json:"chair_register_token"`
}

type chairPostChairsResponse struct {
	ID      string `json:"id"`
	OwnerID string `json:"owner_id"`
}

func chairPostChairs(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	req := &chairPostChairsRequest{}
	if err := bindJSON(r, req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if req.Name == "" || req.Model == "" || req.ChairRegisterToken == "" {
		writeError(w, http.StatusBadRequest, errors.New("some of required fields(name, model, chair_register_token) are empty"))
		return
	}

	owner := &Owner{}
	if err := db.GetContext(ctx, owner, "SELECT * FROM owners WHERE chair_register_token = ?", req.ChairRegisterToken); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusUnauthorized, errors.New("invalid chair_register_token"))
			return
		}
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	chairID := ulid.Make().String()
	accessToken := secureRandomStr(32)

	_, err := db.ExecContext(
		ctx,
		"INSERT INTO chairs (id, owner_id, name, model, is_active, access_token) VALUES (?, ?, ?, ?, ?, ?)",
		chairID, owner.ID, req.Name, req.Model, false, accessToken,
	)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	st.addChair(&chairInfo{ID: chairID, OwnerID: owner.ID, Name: req.Name, Model: req.Model})

	http.SetCookie(w, &http.Cookie{
		Path:  "/",
		Name:  "chair_session",
		Value: accessToken,
	})

	writeJSON(w, http.StatusCreated, &chairPostChairsResponse{
		ID:      chairID,
		OwnerID: owner.ID,
	})
}

type postChairActivityRequest struct {
	IsActive bool `json:"is_active"`
}

func chairPostActivity(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	chair := ctx.Value("chair").(*Chair)

	req := &postChairActivityRequest{}
	if err := bindJSON(r, req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	_, err := db.ExecContext(ctx, "UPDATE chairs SET is_active = ? WHERE id = ?", req.IsActive, chair.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	st.setChairActive(chair.ID, req.IsActive)

	w.WriteHeader(http.StatusNoContent)
}

type chairPostCoordinateResponse struct {
	RecordedAt int64 `json:"recorded_at"`
}

func chairPostCoordinate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	req := &Coordinate{}
	if err := bindJSON(r, req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	chair := ctx.Value("chair").(*Chair)

	tx, err := db.Beginx()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	defer tx.Rollback()

	chairLocationID := ulid.Make().String()
	if _, err := tx.ExecContext(
		ctx,
		`INSERT INTO chair_locations (id, chair_id, latitude, longitude) VALUES (?, ?, ?, ?)`,
		chairLocationID, chair.ID, req.Latitude, req.Longitude,
	); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	location := &ChairLocation{}
	if err := tx.GetContext(ctx, location, `SELECT * FROM chair_locations WHERE id = ?`, chairLocationID); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	// 移動距離合計を差分で積み上げる。代入は左から評価されるので、
	// total_distance の式の latitude/longitude は更新前（=直前の座標）を指す。
	if _, err := tx.ExecContext(
		ctx,
		`INSERT INTO chair_distances (chair_id, total_distance, total_distance_updated_at, latitude, longitude) VALUES (?, 0, ?, ?, ?) AS new
		 ON DUPLICATE KEY UPDATE
		   total_distance = chair_distances.total_distance + ABS(chair_distances.latitude - new.latitude) + ABS(chair_distances.longitude - new.longitude),
		   total_distance_updated_at = new.total_distance_updated_at,
		   latitude = new.latitude,
		   longitude = new.longitude`,
		chair.ID, location.CreatedAt, req.Latitude, req.Longitude,
	); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	ride := &Ride{}
	var newStatus, newStatusID string
	if err := tx.GetContext(ctx, ride, `SELECT * FROM rides WHERE chair_id = ? ORDER BY updated_at DESC LIMIT 1`, chair.ID); err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
	} else {
		status, err := getLatestRideStatus(ctx, tx, ride.ID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		if status != "COMPLETED" && status != "CANCELED" {
			if req.Latitude == ride.PickupLatitude && req.Longitude == ride.PickupLongitude && status == "ENROUTE" {
				newStatus = "PICKUP"
				if newStatusID, err = insertRideStatus(ctx, tx, ride.ID, newStatus); err != nil {
					writeError(w, http.StatusInternalServerError, err)
					return
				}
			}

			if req.Latitude == ride.DestinationLatitude && req.Longitude == ride.DestinationLongitude && status == "CARRYING" {
				newStatus = "ARRIVED"
				if newStatusID, err = insertRideStatus(ctx, tx, ride.ID, newStatus); err != nil {
					writeError(w, http.StatusInternalServerError, err)
					return
				}
			}
		}
	}

	if err := tx.Commit(); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	st.setChairLocation(chair.ID, req.Latitude, req.Longitude)
	if newStatusID != "" {
		if err := st.addStatus(ctx, ride.ID, newStatusID, newStatus); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
	}

	writeJSON(w, http.StatusOK, &chairPostCoordinateResponse{
		RecordedAt: location.CreatedAt.UnixMilli(),
	})
}

type simpleUser struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type chairGetNotificationResponse struct {
	Data         *chairGetNotificationResponseData `json:"data"`
	RetryAfterMs int                               `json:"retry_after_ms"`
}

type chairGetNotificationResponseData struct {
	RideID                string     `json:"ride_id"`
	User                  simpleUser `json:"user"`
	PickupCoordinate      Coordinate `json:"pickup_coordinate"`
	DestinationCoordinate Coordinate `json:"destination_coordinate"`
	Status                string     `json:"status"`
}

func chairGetNotification(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	chair := ctx.Value("chair").(*Chair)

	// 椅子に最後に割り当てられたライドについて、まだ送っていない最古の状態（なければ最新）を返す。
	st.mu.Lock()
	ride := st.chairLatestRide[chair.ID]
	if ride == nil {
		st.mu.Unlock()
		writeJSON(w, http.StatusOK, &chairGetNotificationResponse{
			RetryAfterMs: notificationRetryAfterMs,
		})
		return
	}
	var sending *statusEntry
	for _, e := range ride.Statuses {
		if !e.ChairSent {
			sending = e
			break
		}
	}
	status := ride.latestStatus()
	if sending != nil {
		status = sending.Status
	}
	userName, ok := st.userNames[ride.UserID]
	data := &chairGetNotificationResponseData{
		RideID: ride.ID,
		User: simpleUser{
			ID:   ride.UserID,
			Name: userName,
		},
		PickupCoordinate: Coordinate{
			Latitude:  ride.PickupLatitude,
			Longitude: ride.PickupLongitude,
		},
		DestinationCoordinate: Coordinate{
			Latitude:  ride.DestinationLatitude,
			Longitude: ride.DestinationLongitude,
		},
		Status: status,
	}
	var sendingID string
	if sending != nil {
		sending.ChairSent = true
		sendingID = sending.ID
	}
	st.mu.Unlock()

	if !ok {
		user := &User{}
		if err := db.GetContext(ctx, user, "SELECT * FROM users WHERE id = ?", ride.UserID); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		data.User.Name = fmt.Sprintf("%s %s", user.Firstname, user.Lastname)
	}

	if sendingID != "" {
		// マッチングの「空き椅子」判定が chair_sent_at を見るので、DBにも記録する
		if _, err := db.ExecContext(ctx, `UPDATE ride_statuses SET chair_sent_at = CURRENT_TIMESTAMP(6) WHERE id = ?`, sendingID); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
	}

	writeJSON(w, http.StatusOK, &chairGetNotificationResponse{
		Data:         data,
		RetryAfterMs: notificationRetryAfterMs,
	})
}

type postChairRidesRideIDStatusRequest struct {
	Status string `json:"status"`
}

func chairPostRideStatus(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	rideID := r.PathValue("ride_id")

	chair := ctx.Value("chair").(*Chair)

	req := &postChairRidesRideIDStatusRequest{}
	if err := bindJSON(r, req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	tx, err := db.Beginx()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	defer tx.Rollback()

	ride := &Ride{}
	if err := tx.GetContext(ctx, ride, "SELECT * FROM rides WHERE id = ? FOR UPDATE", rideID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, errors.New("ride not found"))
			return
		}
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	if ride.ChairID.String != chair.ID {
		writeError(w, http.StatusBadRequest, errors.New("not assigned to this ride"))
		return
	}

	var newStatusID string
	switch req.Status {
	// Acknowledge the ride
	case "ENROUTE":
		if newStatusID, err = insertRideStatus(ctx, tx, ride.ID, "ENROUTE"); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
	// After Picking up user
	case "CARRYING":
		status, err := getLatestRideStatus(ctx, tx, ride.ID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		if status != "PICKUP" {
			writeError(w, http.StatusBadRequest, errors.New("chair has not arrived yet"))
			return
		}
		if newStatusID, err = insertRideStatus(ctx, tx, ride.ID, "CARRYING"); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
	default:
		writeError(w, http.StatusBadRequest, errors.New("invalid status"))
	}

	if err := tx.Commit(); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if newStatusID != "" {
		if err := st.addStatus(ctx, ride.ID, newStatusID, req.Status); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
	}

	w.WriteHeader(http.StatusNoContent)
}
