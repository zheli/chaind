// Copyright © 2022 Weald Technology Trading.
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package standard

import (
	"context"
	"strings"
	"time"

	"github.com/pkg/errors"
	"github.com/rs/zerolog"
	zerologger "github.com/rs/zerolog/log"
	"github.com/sasha-s/go-deadlock"
	"github.com/wealdtech/chaind/services/scheduler"
	"go.uber.org/atomic"
)

// module-wide log.
var log zerolog.Logger

// job contains control points for a job.
type job struct {
	// stateLock is required for active or finalised.
	stateLock deadlock.Mutex
	active    atomic.Bool
	finalised atomic.Bool
	periodic  bool
	cancelCh  chan struct{}
	runCh     chan struct{}
}

// Service is a scheduler service.  It uses additional per-job information to manage
// the state of each job, in an attempt to ensure additional robustness in the face
// of high concurrent load.
type Service struct {
	jobs      map[string]*job
	jobsMutex deadlock.RWMutex
}

// New creates a new scheduling service.
func New(ctx context.Context, params ...Parameter) (*Service, error) {
	parameters, err := parseAndCheckParameters(params...)
	if err != nil {
		return nil, errors.Wrap(err, "problem with parameters")
	}

	// Set logging.
	log = zerologger.With().Str("service", "scheduler").Str("impl", "advanced").Logger()
	if parameters.logLevel != log.GetLevel() {
		log = log.Level(parameters.logLevel)
	}

	if err := registerMetrics(ctx, parameters.monitor); err != nil {
		return nil, errors.New("failed to register metrics")
	}

	return &Service{
		jobs: make(map[string]*job),
	}, nil
}

// ScheduleJob schedules a one-off job for a given time.
// Note that if the parent context is cancelled the job wil not run.
func (s *Service) ScheduleJob(ctx context.Context,
	class string,
	name string,
	runtime time.Time,
	jobFunc scheduler.JobFunc,
	data interface{},
) error {
	if name == "" {
		return scheduler.ErrNoJobName
	}
	if jobFunc == nil {
		return scheduler.ErrNoJobFunc
	}

	s.jobsMutex.Lock()
	if _, exists := s.jobs[name]; exists {
		s.jobsMutex.Unlock()
		return scheduler.ErrJobAlreadyExists
	}

	job := &job{
		cancelCh: make(chan struct{}, 1),
		runCh:    make(chan struct{}, 1),
	}
	s.jobs[name] = job
	s.jobsMutex.Unlock()
	jobScheduled(class)

	log.Trace().Str("job", name).Time("scheduled", runtime).Msg("Scheduled job")
	go func() {
		select {
		case <-ctx.Done():
			log.Trace().Str("job", name).Time("scheduled", runtime).Msg("Parent context done; job not running")
			s.jobsMutex.Lock()
			delete(s.jobs, name)
			s.jobsMutex.Unlock()
			finaliseJob(job)
			jobCancelled(class)
		case <-job.cancelCh:
			log.Trace().Str("job", name).Time("scheduled", runtime).Msg("Cancel triggered; job not running")
			// If we receive this signal the job has already been deleted from the jobs list so no need to
			// do so again here.
			finaliseJob(job)
			jobCancelled(class)
		case <-job.runCh:
			log.Trace().Str("job", name).Time("scheduled", runtime).Msg("Run triggered; job running")
			// If we receive this signal the job has already been deleted from the jobs list so no need to
			// do so again here.
			jobStartedOnSignal(class)
			jobFunc(ctx, data)
			log.Trace().Str("job", name).Time("scheduled", runtime).Msg("Job complete")
			finaliseJob(job)
			job.active.Store(false)
		case <-time.After(time.Until(runtime)):
			// It is possible that the job is already active, so check that first before proceeding.
			if job.active.Load() {
				log.Trace().Str("job", name).Time("scheduled", runtime).Msg("Already running; job not running")
				break
			}
			s.jobsMutex.Lock()
			delete(s.jobs, name)
			s.jobsMutex.Unlock()
			log.Trace().Str("job", name).Time("scheduled", runtime).Msg("Timer triggered; job running")
			job.active.Store(true)
			jobStartedOnTimer(class)
			jobFunc(ctx, data)
			log.Trace().Str("job", name).Time("scheduled", runtime).Msg("Job complete")
			job.active.Store(false)
			finaliseJob(job)
		}
	}()

	return nil
}

// SchedulePeriodicJob schedules a job to run in a loop.
// The loop starts by calling runtimeFunc, which sets the time for the first run.
// Once the time as specified by runtimeFunc is met, jobFunc is called.
// Once jobFunc returns, go back to the beginning of the loop.
func (s *Service) SchedulePeriodicJob(ctx context.Context,
	class string,
	name string,
	runtimeFunc scheduler.RuntimeFunc,
	runtimeData interface{},
	jobFunc scheduler.JobFunc,
	jobData interface{},
) error {
	if name == "" {
		return scheduler.ErrNoJobName
	}
	if runtimeFunc == nil {
		return scheduler.ErrNoRuntimeFunc
	}
	if jobFunc == nil {
		return scheduler.ErrNoJobFunc
	}

	s.jobsMutex.Lock()
	if _, exists := s.jobs[name]; exists {
		s.jobsMutex.Unlock()
		return scheduler.ErrJobAlreadyExists
	}

	job := &job{
		cancelCh: make(chan struct{}, 1),
		runCh:    make(chan struct{}, 1),
		periodic: true,
	}
	s.jobs[name] = job
	s.jobsMutex.Unlock()
	jobScheduled(class)

	go func() {
		for {
			runtime, err := runtimeFunc(ctx, runtimeData)
			if errors.Is(err, scheduler.ErrNoMoreInstances) {
				log.Trace().Str("job", name).Msg("No more instances; period job stopping")
				s.jobsMutex.Lock()
				delete(s.jobs, name)
				s.jobsMutex.Unlock()
				finaliseJob(job)
				jobCancelled(class)
				return
			}
			if err != nil {
				log.Error().Str("job", name).Err(err).Msg("Failed to obtain runtime; periodic job stopping")
				s.jobsMutex.Lock()
				delete(s.jobs, name)
				s.jobsMutex.Unlock()
				finaliseJob(job)
				jobCancelled(class)
				return
			}
			log.Trace().Str("job", name).Time("scheduled", runtime).Msg("Scheduled job")
			select {
			case <-ctx.Done():
				log.Trace().Str("job", name).Time("scheduled", runtime).Msg("Parent context done; job not running")
				s.jobsMutex.Lock()
				delete(s.jobs, name)
				s.jobsMutex.Unlock()
				finaliseJob(job)
				jobCancelled(class)
				return
			case <-job.cancelCh:
				log.Trace().Str("job", name).Time("scheduled", runtime).Msg("Cancel triggered; job not running")
				finaliseJob(job)
				jobCancelled(class)
				return
			case <-job.runCh:
				log.Trace().Str("job", name).Time("scheduled", runtime).Msg("Run triggered; job running")
				jobStartedOnSignal(class)
				jobFunc(ctx, jobData)
				log.Trace().Str("job", name).Time("scheduled", runtime).Msg("Job complete")
				job.active.Store(false)
			case <-time.After(time.Until(runtime)):
				if job.active.Load() {
					log.Trace().Str("job", name).Time("scheduled", runtime).Msg("Already running; job not running")
					continue
				}
				job.active.Store(true)
				log.Trace().Str("job", name).Time("scheduled", runtime).Msg("Timer triggered; job running")
				jobStartedOnTimer(class)
				jobFunc(ctx, jobData)
				log.Trace().Str("job", name).Time("scheduled", runtime).Msg("Job complete")
				job.active.Store(false)
			}
		}
	}()

	return nil
}

// RunJob runs a named job immediately.
// If the job does not exist it will return an appropriate error.
func (s *Service) RunJob(ctx context.Context, name string) error {
	s.jobsMutex.Lock()
	job, exists := s.jobs[name]

	if !exists {
		s.jobsMutex.Unlock()
		return scheduler.ErrNoSuchJob
	}
	if !job.periodic {
		// Because this job only runs once we remove it from the jobs list immediately.
		delete(s.jobs, name)
	}
	s.jobsMutex.Unlock()

	return s.runJob(ctx, job)
}

// RunJobIfExists runs a job if it exists.
// This does not return an error if the job does not exist or is otherwise unable to run.
func (s *Service) RunJobIfExists(ctx context.Context, name string) {
	s.jobsMutex.Lock()
	job, exists := s.jobs[name]

	if !exists {
		s.jobsMutex.Unlock()
		return
	}
	if !job.periodic {
		// Because this job only runs once we remove it from the jobs list immediately.
		delete(s.jobs, name)
	}
	s.jobsMutex.Unlock()

	//nolint
	s.runJob(ctx, job)
}

// JobExists returns true if a job exists.
func (s *Service) JobExists(_ context.Context, name string) bool {
	s.jobsMutex.RLock()
	_, exists := s.jobs[name]
	s.jobsMutex.RUnlock()
	return exists
}

// ListJobs returns the names of all jobs.
func (s *Service) ListJobs(_ context.Context) []string {
	s.jobsMutex.RLock()
	names := make([]string, 0, len(s.jobs))
	for name := range s.jobs {
		names = append(names, name)
	}
	s.jobsMutex.RUnlock()

	return names
}

// CancelJob removes a named job.
// If the job does not exist it will return an appropriate error.
func (s *Service) CancelJob(_ context.Context, name string) error {
	s.jobsMutex.Lock()
	job, exists := s.jobs[name]
	if !exists {
		s.jobsMutex.Unlock()
		return scheduler.ErrNoSuchJob
	}
	delete(s.jobs, name)
	s.jobsMutex.Unlock()

	job.stateLock.Lock()
	if job.finalised.Load() {
		// Already marked to be cancelled.
		job.stateLock.Unlock()
		return nil
	}
	job.finalised.Store(true)
	job.cancelCh <- struct{}{}
	job.stateLock.Unlock()

	return nil
}

// CancelJobIfExists cancels a job that may or may not exist.
// If this is a period job then all future instances are cancelled.
func (s *Service) CancelJobIfExists(ctx context.Context, name string) {
	//nolint
	s.CancelJob(ctx, name)
}

// CancelJobs cancels all jobs with the given prefix.
// If the prefix matches a period job then all future instances are cancelled.
func (s *Service) CancelJobs(ctx context.Context, prefix string) {
	names := make([]string, 0)
	s.jobsMutex.Lock()
	for name := range s.jobs {
		if strings.HasPrefix(name, prefix) {
			names = append(names, name)
		}
	}
	s.jobsMutex.Unlock()

	for _, name := range names {
		// It is possible that the job has been removed whist we were iterating, so use the non-erroring version of cancel.
		s.CancelJobIfExists(ctx, name)
	}
}

// finaliseJob tidies up a job that is no longer in use.
func finaliseJob(job *job) {
	job.stateLock.Lock()
	job.finalised.Store(true)

	// Close the channels for the job to ensure that nothing is hanging on sending a message.
	close(job.cancelCh)
	close(job.runCh)

	job.stateLock.Unlock()
}

// runJob runs the given job.
// skipcq: RVV-B0001
func (*Service) runJob(_ context.Context, job *job) error {
	job.stateLock.Lock()
	if job.active.Load() {
		job.stateLock.Unlock()
		return scheduler.ErrJobRunning
	}
	if job.finalised.Load() {
		job.stateLock.Unlock()
		return scheduler.ErrJobFinalised
	}
	job.active.Store(true)
	job.runCh <- struct{}{}
	job.stateLock.Unlock()

	return nil
}
