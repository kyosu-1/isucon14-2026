package main

import (
	"database/sql"
	"net/http"
	"sort"
	"time"
)

type matchingRide struct {
	ID                   string `db:"id"`
	PickupLatitude       int    `db:"pickup_latitude"`
	PickupLongitude      int    `db:"pickup_longitude"`
	DestinationLatitude  int    `db:"destination_latitude"`
	DestinationLongitude int    `db:"destination_longitude"`
}

type matchingChair struct {
	ID        string        `db:"id"`
	Speed     int           `db:"speed"`
	Latitude  sql.NullInt64 `db:"latitude"`
	Longitude sql.NullInt64 `db:"longitude"`
}

// このAPIをインスタンス内から一定間隔で叩かせることで、椅子とライドをマッチングさせる。
//
// 待っているライドを古い順に、空いている椅子のうち「乗車位置に着いて目的地まで運び終えるまでの時間」
// (= (椅子→乗車位置 + 乗車位置→目的地) / speed) が最短のものへ割り当てる。
// 1回の呼び出しで割り当てられるだけ割り当てる。
func internalGetMatching(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	rides := []matchingRide{}
	if err := db.SelectContext(ctx, &rides, `SELECT id, pickup_latitude, pickup_longitude, destination_latitude, destination_longitude FROM rides WHERE chair_id IS NULL ORDER BY created_at`); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if len(rides) == 0 {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// 空いている椅子 = 稼働中で、割り当て済みのライドがすべて「椅子に COMPLETED を通知済み」のもの。
	// COMPLETED を椅子が受け取る前に次のライドを通知するとクリティカルエラーになるので、
	// 元の実装（chair_sent_at が6件揃っているか）と同じく「通知済み」を基準にする。
	chairs := []matchingChair{}
	if err := db.SelectContext(ctx, &chairs, `
SELECT c.id, cm.speed, d.latitude, d.longitude
FROM chairs c
  JOIN chair_models cm ON cm.name = c.model
  LEFT JOIN chair_distances d ON d.chair_id = c.id
WHERE c.is_active = TRUE
  AND NOT EXISTS (
    SELECT 1 FROM rides r
    WHERE r.chair_id = c.id
      AND NOT EXISTS (SELECT 1 FROM ride_statuses rs WHERE rs.ride_id = r.id AND rs.status = 'COMPLETED' AND rs.chair_sent_at IS NOT NULL)
  )`); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if len(chairs) == 0 {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	// 同点のときに結果が毎回ぶれないよう、IDで並べておく
	sort.Slice(chairs, func(i, j int) bool { return chairs[i].ID < chairs[j].ID })

	used := make([]bool, len(chairs))
	for _, ride := range rides {
		best := -1
		var bestCost float64
		rideDistance := calculateDistance(ride.PickupLatitude, ride.PickupLongitude, ride.DestinationLatitude, ride.DestinationLongitude)
		for i, c := range chairs {
			if used[i] {
				continue
			}
			// 位置が一度も送られていない椅子は、どこにいるか分からないので最後の手段にする
			pickupDistance := 1 << 20
			if c.Latitude.Valid {
				pickupDistance = calculateDistance(int(c.Latitude.Int64), int(c.Longitude.Int64), ride.PickupLatitude, ride.PickupLongitude)
			}
			cost := float64(pickupDistance+rideDistance) / float64(c.Speed)
			if best < 0 || cost < bestCost {
				best, bestCost = i, cost
			}
		}
		if best < 0 {
			break
		}

		// updated_at はメモリにも同じ値を持ちたいので、DBの ON UPDATE に任せず明示する
		now := time.Now().UTC().Truncate(time.Microsecond)
		res, err := db.ExecContext(ctx, "UPDATE rides SET chair_id = ?, updated_at = ? WHERE id = ? AND chair_id IS NULL", chairs[best].ID, now, ride.ID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		if n, _ := res.RowsAffected(); n == 1 {
			used[best] = true
			if err := st.assignChair(ctx, ride.ID, chairs[best].ID, now); err != nil {
				writeError(w, http.StatusInternalServerError, err)
				return
			}
		}
	}

	w.WriteHeader(http.StatusNoContent)
}
