package main

import (
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"time"

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

	now := time.Now().UTC().Truncate(time.Microsecond)
	_, err := db.ExecContext(
		ctx,
		"INSERT INTO chairs (id, owner_id, name, model, is_active, access_token, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)",
		chairID, owner.ID, req.Name, req.Model, false, accessToken, now, now,
	)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	st.addChair(&chairInfo{ID: chairID, OwnerID: owner.ID, Name: req.Name, Model: req.Model, CreatedAt: now})

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

	// 椅子は座標更新の成功を確認するまで移動しないので、ここの応答時間がそのまま椅子の速さになる。
	// 座標と移動距離はメモリに記録し、DB(chair_distances)へは 200ms ごとにまとめて書く。
	// 状態遷移（乗車位置・目的地に到着）があるときだけ同期でトランザクションを張る。
	// 位置履歴(chair_locations)はどこからも読まないので書かない。
	now := time.Now().UTC().Truncate(time.Microsecond)
	st.setChairLocation(chair.ID, req.Latitude, req.Longitude, now)

	// 割り当て中のライドが乗車位置・目的地に着いたか（ライドと状態はメモリから）
	var rideID, newStatus string
	st.mu.Lock()
	if ride := st.chairLatestRide[chair.ID]; ride != nil {
		switch ride.latestStatus() {
		case "ENROUTE":
			if req.Latitude == ride.PickupLatitude && req.Longitude == ride.PickupLongitude {
				rideID, newStatus = ride.ID, "PICKUP"
			}
		case "CARRYING":
			if req.Latitude == ride.DestinationLatitude && req.Longitude == ride.DestinationLongitude {
				rideID, newStatus = ride.ID, "ARRIVED"
			}
		}
	}
	st.mu.Unlock()

	if newStatus != "" {
		tx, err := db.Beginx()
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		defer tx.Rollback()
		newStatusID, err := insertRideStatus(ctx, tx, rideID, newStatus)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		if err := tx.Commit(); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		if err := st.addStatus(ctx, rideID, newStatusID, newStatus); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
	}

	writeJSON(w, http.StatusOK, &chairPostCoordinateResponse{
		RecordedAt: now.UnixMilli(),
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
	if sending != nil {
		sending.ChairSent = true
		sending.ChairSentAt = time.Now()
		st.pendingChairSent = append(st.pendingChairSent, sending.ID)
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

	// 割り当てと現在の状態はメモリで判定する（マッチングはメモリ→DBの順に反映するので、
	// 通知を受け取った椅子がDBより先に来ても「割り当てられていない」にならない）
	st.mu.Lock()
	rs, err := st.rideLocked(ctx, rideID)
	var assignedChair, latest string
	if err == nil {
		assignedChair, latest = rs.ChairID, rs.latestStatus()
	}
	st.mu.Unlock()
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, errors.New("ride not found"))
			return
		}
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if assignedChair != chair.ID {
		writeError(w, http.StatusBadRequest, errors.New("not assigned to this ride"))
		return
	}

	switch req.Status {
	// Acknowledge the ride
	case "ENROUTE":
	// After Picking up user
	case "CARRYING":
		if latest != "PICKUP" {
			writeError(w, http.StatusBadRequest, errors.New("chair has not arrived yet"))
			return
		}
	default:
		writeError(w, http.StatusBadRequest, errors.New("invalid status"))
		return
	}

	tx, err := db.Beginx()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	defer tx.Rollback()
	newStatusID, err := insertRideStatus(ctx, tx, rideID, req.Status)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	if err := tx.Commit(); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if newStatusID != "" {
		if err := st.addStatus(ctx, rideID, newStatusID, req.Status); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
	}

	w.WriteHeader(http.StatusNoContent)
}
