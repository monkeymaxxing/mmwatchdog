package daemon

import (
	"time"

	"mmwatchdog/internal/store"
)

// pruneMeasurementHistory is the retention tick of the node's measurement
// history: it removes the readings that are older than
// keep_measurement_history_for.
func (d *Daemon) pruneMeasurementHistory(now time.Time) {
	d.mu.Lock()
	keep := d.config.KeepMeasurementHistoryFor.Value()
	d.mu.Unlock()
	d.reportStorage(store.MeasurementHistoryRetention, d.history.Prune(now, keep))
}

// trimMeasurementHistories takes one step toward a lower size limit, for the
// node's measurement history and on a hub for that of the nodes
// (MeasurementHistory.TrimStep). The daemon calls it several times a second,
// and it costs nothing while both files are within their limits. A step that
// fails is a storage problem until a later step succeeds.
func (d *Daemon) trimMeasurementHistories() {
	if shrinking, e := d.history.TrimStep(); shrinking || e != nil {
		d.reportStorage(store.MeasurementHistoryRetention, e)
	}
	if d.hub != nil {
		if shrinking, e := d.hub.Readings.TrimStep(); shrinking || e != nil {
			d.hub.Mu.Lock()
			d.hub.Storage.Report(store.MeasurementHistoryOfNodesRetention, e)
			d.hub.Mu.Unlock()
		}
	}
}

// measurementHistoryStatus is the node's measurement history for status.
// The caller holds d.mu.
func (d *Daemon) measurementHistoryStatus(now time.Time) store.MeasurementHistoryStatus {
	keep, limit := d.config.KeepMeasurementHistoryFor, d.config.MeasurementHistorySizeLimit
	reach := d.history.Reach(now, keep.Value())
	s := store.MeasurementHistoryStatus{KeepMeasurementHistoryFor: keep, MeasurementHistorySizeLimit: limit, CutShortByMeasurementHistorySizeLimit: reach.CutShortBySizeLimit}
	if !reach.Oldest.IsZero() {
		s.OldestReadingAt, s.NewestReadingAt = &reach.Oldest, &reach.Newest
	}
	return s
}
