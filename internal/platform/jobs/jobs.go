// Package jobs builds the River background job client.
package jobs

import (
	"log/slog"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
)

// NewClient builds a River client with the default queue (10 workers) and
// the given periodic jobs (scheduled once the client is started).
func NewClient(pool *pgxpool.Pool, workers *river.Workers, logger *slog.Logger, periodic ...*river.PeriodicJob) (*river.Client[pgx.Tx], error) {
	return river.NewClient(riverpgxv5.New(pool), &river.Config{
		Queues:       map[string]river.QueueConfig{river.QueueDefault: {MaxWorkers: 10}},
		Workers:      workers,
		PeriodicJobs: periodic,
		Logger:       logger,
	})
}
