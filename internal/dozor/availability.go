package dozor

import (
	"context"
	"sort"
	"time"
)

const availabilityRetention = 24 * time.Hour
const streamTimeout = 20 * time.Second

type AvailabilityInterval struct {
	Start int64  `json:"start"`
	End   int64  `json:"end"`
	State string `json:"state"`
}

type CameraAvailability struct {
	CameraID  string                 `json:"camera_id"`
	Name      string                 `json:"name"`
	Intervals []AvailabilityInterval `json:"intervals"`
}

type AvailabilityHistory struct {
	Start   int64                `json:"start"`
	End     int64                `json:"end"`
	Cameras []CameraAvailability `json:"cameras"`
}

type StreamSignal struct {
	CameraID string `json:"camera_id"`
	Online   bool   `json:"online"`
	At       int64  `json:"at"`
}

// Only observed intervals are stored. A stopped Dozor leaves a gap (unknown),
// rather than reporting that a camera stayed online throughout a restart.
func (s *Store) SaveAvailability(camera string, interval AvailabilityInterval) error {
	_, err := s.DB.Exec(`INSERT INTO availability VALUES(?,?,?,?)
 ON CONFLICT(camera,start) DO UPDATE SET end=excluded.end,state=excluded.state`, camera, interval.Start, interval.End, interval.State)
	return err
}

func (s *Store) PruneAvailability(now time.Time) error {
	cutoff := now.Add(-availabilityRetention).UnixMilli()
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec("DELETE FROM availability WHERE end<=?", cutoff); err != nil {
		return err
	}
	if _, err = tx.Exec("UPDATE availability SET start=? WHERE start<?", cutoff, cutoff); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) Availability(cameras []Camera, camera string, now time.Time) (AvailabilityHistory, error) {
	h := AvailabilityHistory{Start: now.Add(-availabilityRetention).UnixMilli(), End: now.UnixMilli(), Cameras: []CameraAvailability{}}
	for _, cam := range cameras {
		if camera != "" && cam.ID != camera {
			continue
		}
		row := CameraAvailability{CameraID: cam.ID, Name: cam.Name, Intervals: []AvailabilityInterval{}}
		rows, err := s.DB.Query("SELECT start,end,state FROM availability WHERE camera=? AND end>? AND start<? ORDER BY start", cam.ID, h.Start, h.End)
		if err != nil {
			return h, err
		}
		for rows.Next() {
			var interval AvailabilityInterval
			if err = rows.Scan(&interval.Start, &interval.End, &interval.State); err != nil {
				rows.Close()
				return h, err
			}
			interval.Start = max(interval.Start, h.Start)
			interval.End = min(interval.End, h.End)
			row.Intervals = append(row.Intervals, interval)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return h, err
		}
		h.Cameras = append(h.Cameras, row)
	}
	return h, nil
}

func (r *Runtime) initStreams(cameras []Camera, now time.Time) error {
	r.streams = make(map[string]AvailabilityInterval)
	r.streamSignals = make(chan StreamSignal, 512)
	for _, cam := range cameras {
		state := "offline"
		if !cam.Enabled {
			state = "disabled"
		}
		v := AvailabilityInterval{Start: now.UnixMilli(), End: now.UnixMilli(), State: state}
		r.streams[cam.ID] = v
		r.Engine.SetOnline(cam.ID, false)
		if err := r.Store.SaveAvailability(cam.ID, v); err != nil {
			return err
		}
	}
	return r.Store.PruneAvailability(now)
}

func (r *Runtime) setStream(ctx context.Context, camera string, online bool, at int64) error {
	prev, exists := r.streams[camera]
	if !exists || prev.State == "disabled" || at < prev.Start {
		return nil
	}
	state := "offline"
	if online {
		state = "online"
	}
	if prev.State == state {
		return nil
	}
	if !online {
		// The last file no longer needs a following segment to prove rotation.
		// Scan before closing the event, including when its completion hook was lost.
		if err := scanCameraSegments(ctx, r.Store, r.Engine.Bins.FFprobe, camera, at, r.Engine.AddSegment); err != nil {
			return err
		}
		if err := r.Engine.Disconnect(camera, time.UnixMilli(at)); err != nil {
			return err
		}
	}
	prev.End = at
	if err := r.Store.SaveAvailability(camera, prev); err != nil {
		return err
	}
	v := AvailabilityInterval{Start: at, End: at, State: state}
	if err := r.Store.SaveAvailability(camera, v); err != nil {
		return err
	}
	r.streams[camera] = v
	r.Engine.SetOnline(camera, online)
	return nil
}

func (r *Runtime) tickStreams(ctx context.Context, now time.Time) error {
	if err := r.Store.PruneAvailability(now); err != nil {
		return err
	}
	for camera, state := range r.streams {
		state.Start = max(state.Start, now.Add(-availabilityRetention).UnixMilli())
		r.streams[camera] = state
	}
	var signals []StreamSignal
	for len(r.streamSignals) > 0 {
		signals = append(signals, <-r.streamSignals)
	}
	sort.SliceStable(signals, func(i, j int) bool { return signals[i].At < signals[j].At })
	for _, v := range signals {
		if err := r.setStream(ctx, v.CameraID, v.Online, min(v.At, now.UnixMilli())); err != nil {
			return err
		}
	}
	for camera, state := range r.streams {
		if state.State != "disabled" {
			var start, end int64
			if err := r.Store.DB.QueryRow("SELECT COALESCE(MAX(start),0),COALESCE(MAX(end),0) FROM segments WHERE camera=?", camera).Scan(&start, &end); err != nil {
				return err
			}
			alive := r.mediaSince.Load() > 0
			online := state.State == "online"
			if online && (!alive || now.UnixMilli()-max(state.Start, end) > streamTimeout.Milliseconds()) {
				if err := r.setStream(ctx, camera, false, now.UnixMilli()); err != nil {
					return err
				}
			} else if !online && alive && start >= state.Start && end > now.Add(-streamTimeout).UnixMilli() {
				// A fresh recording also recovers a lost online hook; an old tail
				// delivered after disconnection must never bring the camera online.
				if err := r.setStream(ctx, camera, true, start); err != nil {
					return err
				}
			}
		}
		state = r.streams[camera]
		state.End = now.UnixMilli()
		if err := r.Store.SaveAvailability(camera, state); err != nil {
			return err
		}
		r.streams[camera] = state
	}
	return nil
}

func (r *Runtime) reconcileSegments(ctx context.Context) error {
	if err := scanSegments(ctx, r.Store, r.Engine.Bins.FFprobe, r.mediaSince.Load() > 0, r.Engine.AddSegment); err != nil {
		return err
	}
	for camera, state := range r.streams {
		if state.State == "offline" {
			// Include the tail of the disconnected stream. A newer file could
			// belong to a reconnect whose online hook was lost and still be open.
			if err := scanCameraSegments(ctx, r.Store, r.Engine.Bins.FFprobe, camera, state.Start, r.Engine.AddSegment); err != nil {
				return err
			}
		}
	}
	return nil
}

// Configuration reload stops MediaMTX before the worker can consume its hooks.
// Finalize cameras explicitly disabled/deleted by the user after the recorder
// has closed its files. Unrelated settings reloads keep their recovery behavior.
func (r *Runtime) finishDisabledStreams(ctx context.Context, cameras []Camera, now time.Time) error {
	enabled := make(map[string]bool)
	for _, camera := range cameras {
		enabled[camera.ID] = camera.Enabled
	}
	changed := false
	for camera, state := range r.streams {
		if !enabled[camera] && state.State == "online" {
			if err := r.setStream(ctx, camera, false, now.UnixMilli()); err != nil {
				return err
			}
			changed = true
		}
	}
	if !changed {
		return nil
	}
	for camera := range r.streams {
		r.Engine.SetOnline(camera, false)
	}
	return r.Engine.Tick(ctx, now)
}
