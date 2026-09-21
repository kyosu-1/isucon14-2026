SET CHARACTER_SET_CLIENT = utf8mb4;
SET CHARACTER_SET_CONNECTION = utf8mb4;

USE isuride;

-- 初期データの chair_locations から chair_distances を作る（元の owner/chairs のクエリと同じ計算）
INSERT INTO chair_distances (chair_id, total_distance, total_distance_updated_at, latitude, longitude)
SELECT chair_id, SUM(IFNULL(distance, 0)), MAX(created_at), 0, 0
FROM (SELECT chair_id,
             created_at,
             ABS(latitude - LAG(latitude) OVER w) + ABS(longitude - LAG(longitude) OVER w) AS distance
      FROM chair_locations
      WINDOW w AS (PARTITION BY chair_id ORDER BY created_at)) tmp
GROUP BY chair_id;

UPDATE chair_distances d
  JOIN (SELECT chair_id, latitude, longitude,
               ROW_NUMBER() OVER (PARTITION BY chair_id ORDER BY created_at DESC) AS rn
        FROM chair_locations) l ON l.chair_id = d.chair_id AND l.rn = 1
SET d.latitude = l.latitude, d.longitude = l.longitude;

-- rides.status = ride_statuses の最新（updated_at は ON UPDATE で書き換わらないよう明示的に据え置く）
UPDATE rides r
  JOIN (SELECT ride_id, status,
               ROW_NUMBER() OVER (PARTITION BY ride_id ORDER BY created_at DESC) AS rn
        FROM ride_statuses) s ON s.ride_id = r.id AND s.rn = 1
SET r.status = s.status, r.updated_at = r.updated_at;
