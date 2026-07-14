package store

import (
	"context"
	"log"
)

// JobHandler processes a single job end-to-end (extract → chunk → write to vault).
type JobHandler func(job Job)

// StartWorkers spins up n workers reading from a buffered queue and returns the
// send side for producers (the HTTP layer). The buffer lets /ingest accept and
// return without blocking unless the system is badly backed up.
func StartWorkers(ctx context.Context, n int, handle JobHandler) chan<- Job {
	queue := make(chan Job, 128)
	for i := 0; i < n; i++ {
		go func(workerID int) {
			for {
				select {
				case <-ctx.Done():
					return
				case job := <-queue:
					runJob(workerID, job, handle)
				}
			}
		}(i)
	}
	return queue
}

// runJob isolates each job so a panic in one handler kills only that job.
func runJob(workerID int, job Job, handle JobHandler) {
	defer func() {
		if rec := recover(); rec != nil {
			log.Printf("worker %d: job %s panicked: %v", workerID, job.ID, rec)
			_ = UpdateJobStatus(job.ID, "misfiled", "internal error")
		}
	}()
	handle(job)
}
