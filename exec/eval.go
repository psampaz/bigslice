// Copyright 2018 GRAIL, Inc. All rights reserved.
// Use of this source code is governed by the Apache 2.0
// license that can be found in the LICENSE file.

// Package exec implements compilation, evaluation, and execution of
// Bigslice slice operations.
package exec

import (
	"context"
	"net/http"

	"github.com/grailbio/base/log"
	"github.com/grailbio/base/status"
	"github.com/grailbio/bigslice"
	"github.com/grailbio/bigslice/internal/defaultsize"
	"github.com/grailbio/bigslice/sliceio"
)

var defaultChunksize = &defaultsize.Chunk

// Executor defines an interface used to provide implementations of
// task runners. An Executor is responsible for running single tasks,
// partitioning their outputs, and instantiating readers to retrieve the
// output of any given task.
type Executor interface {
	// Start starts the executor. It is called before evaluation has started
	// and after all funcs have been registered. Start need not return:
	// for example, the Bigmachine implementation of Executor uses
	// Start as an entry point for worker processes.
	Start(*Session) (shutdown func())

	// Run runs a task. The executor sets the state of the task as it
	// progresses. The task should enter in state TaskWaiting; by the
	// time Run returns the task state is >= TaskOk.
	Run(*Task)

	// Reader returns a locally accessible reader for the requested task.
	Reader(context.Context, *Task, int) sliceio.Reader

	// HandleDebug adds executor-specific debug handlers to the provided
	// http.ServeMux. This is used to serve diagnostic information relating
	// to the executor.
	HandleDebug(handler *http.ServeMux)
}

// Eval simultaneously evaluates a set of task graphs from the
// provided set of roots. Eval uses the provided executor to dispatch
// tasks when their dependencies have been satisfied. Eval returns on
// evaluation error or else when all roots are fully evaluated.
//
// TODO(marius): consider including the invocation in the task definitions
// themselves. This way, a task's name is entirely self contained and can
// be interpreted without an accompanying invocation.
// TODO(marius): we can often stream across shuffle boundaries. This would
// complicate scheduling, but may be worth doing.
func Eval(ctx context.Context, executor Executor, inv bigslice.Invocation, roots []*Task, group *status.Group) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	state := newState()
	for _, task := range roots {
		state.Enqueue(task)
	}
	var (
		donec   = make(chan *Task, 8)
		errc    = make(chan error)
		running int
	)
	for !state.Done() {
		group.Printf("tasks: runnable: %d", running)
		for !state.Done() && !state.Todo() {
			select {
			case err := <-errc:
				if err == nil {
					panic("nil err")
				}
				return err
			case task := <-donec:
				running--
				state.Return(task)
			}
		}

		// Mark each ready task as runnable and keep track of them.
		// The executor manages parallelism.
		for _, task := range state.Runnable() {
			task.Lock()
			if task.state == TaskLost {
				log.Printf("evaluator: resubmitting lost task %v", task)
				task.state = TaskInit
			}
			if task.state == TaskInit {
				task.state = TaskWaiting
				task.Status = group.Startf("%s(%x)", task.Name, inv.Index)
				go func(task *Task) {
					executor.Run(task)
					task.Status.Done()
				}(task)
			}
			running++
			go func(task *Task) {
				var err error
				for task.state < TaskOk && err == nil {
					err = task.Wait(ctx)
				}
				task.Unlock()
				if err != nil {
					errc <- err
				} else {
					donec <- task
				}
			}(task)
		}
	}
	return state.Err()
}

// State maintains state for the task graph being run by the
// evaluator. It maintains per-node waitlists so that it can
// efficiently traverse only the required portion of the task graph
// for each task update. When a task's waitlist has been cleared,
// state re-traverses the graph from that task. This reconciles task
// changes that have occurred between updates. This may cause a task
// to be re-queued, e.g., if a dependent task changed status from TaskOk
// to TaskLost. State does not watch for task changes for tasks that
// are ready, thus it won't aggressively recompute a lost task that
// is going to be needed by another task with a nonzero waitlist.
// This is only discovered once that waitlist is drained. The scheme
// could be more aggressive in this case, but these cases should be
// rare enough to not warrant the added complexity.
//
// In order to ensure that the state operates on consistent view of
// the task graph, waitlist decisions are memoized per toplevel call;
// it does not require locking subgraphs.
type state struct {
	// deps and counts maintains the task waitlist.
	deps   map[*Task]map[*Task]struct{}
	counts map[*Task]int

	// todo is the set of tasks that are scheduled to be run. They are
	// retrieved via the Runnable method.
	todo map[*Task]bool

	// pending is the set of tasks that have been scheduled but have not
	// yet been returned via Done.
	pending map[*Task]bool

	// wait stores memoized task waiting decisions (based on a single
	// atomic reading of task state), per round. This is what enables
	// state to maintain a consistent view of the task graph state.
	wait map[*Task]bool

	err error
}

// newState returns a newly allocated, empty state.
func newState() *state {
	return &state{
		deps:    make(map[*Task]map[*Task]struct{}),
		counts:  make(map[*Task]int),
		todo:    make(map[*Task]bool),
		pending: make(map[*Task]bool),
		wait:    make(map[*Task]bool),
	}
}

// Enqueue enqueues all ready tasks in the provided task graph,
// traversing only as much of it as necessary to schedule all
// currently runnable tasks in the graph. Enqueue maintains
// the waiting state for tasks so the correct (and minimal) task
// graphs can be efficiently enqueued on task completion.
func (s *state) Enqueue(task *Task) (wait bool) {
	if w, ok := s.wait[task]; ok {
		return w
	}
	switch task.State() {
	case TaskOk, TaskErr:
		wait = false
	case TaskWaiting, TaskRunning:
		s.schedule(task)
		wait = true
	case TaskInit, TaskLost:
		for _, dep := range task.Deps {
			for _, deptask := range dep.Tasks {
				if s.Enqueue(deptask) {
					s.add(deptask, task)
				}
			}
		}
		if s.ready(task) {
			s.schedule(task)
		}
		wait = true
	}
	s.wait[task] = wait
	return
}

// Return returns a pending task to state, recomputing the state view
// and scheduling follow-on tasks.
func (s *state) Return(task *Task) {
	if !s.pending[task] {
		panic("exec.Eval: done task " + task.Name.String() + ": not pending")
	}
	// Clear the wait map between each call since the state of tasks may
	// have changed between calls.
	s.wait = make(map[*Task]bool)
	delete(s.pending, task)
	switch task.State() {
	default:
		// We might be racing with another evaluator. Reschedule until
		// we get into an actionable state.
		s.schedule(task)
	case TaskErr:
		s.err = task.err
	case TaskOk:
		for _, task := range s.done(task) {
			s.Enqueue(task)
		}
	case TaskLost:
		// Re-enqueue immediately.
		s.Enqueue(task)
	}
}

// Runnable returns the current set of runnable tasks and
// resets the todo list. It is called by Eval to schedule a batch
// of tasks.
func (s *state) Runnable() (tasks []*Task) {
	if len(s.todo) == 0 {
		return
	}
	tasks = make([]*Task, 0, len(s.todo))
	for task := range s.todo {
		tasks = append(tasks, task)
		delete(s.todo, task)
		s.pending[task] = true
	}
	return
}

// Todo returns whether state has tasks to be scheduled.
func (s *state) Todo() bool {
	return len(s.todo) > 0
}

// Done returns whether evaluation is done. Evaluation is done when
// there remain no pending tasks, or tasks to be scheduled. Evaluation
// is also done if an error has occurred.
func (s *state) Done() bool {
	return s.err != nil || len(s.todo) == 0 && len(s.pending) == 0
}

// Err returns an error, if any, that occurred during evaluation.
func (s *state) Err() error {
	return s.err
}

// Schedule schedules the provided task. It is a no-op if
// the task has already been scheduled or is pending.
func (s *state) schedule(task *Task) {
	if s.pending[task] {
		return
	}
	s.todo[task] = true
}

// Add adds a dependency from the provided src to dst tasks.
func (s *state) add(src, dst *Task) {
	if s.deps[src] == nil {
		s.deps[src] = make(map[*Task]struct{})
	}
	if _, waiting := s.deps[src][dst]; !waiting {
		s.deps[src][dst] = struct{}{}
		s.counts[dst]++
	}
}

// Ready returns true if the provided task has no incoming
// dependencies.
func (s *state) ready(task *Task) bool {
	return s.counts[task] == 0
}

// Done marks the provided task as done, and returns the set
// of tasks that have consequently become ready for evaluation.
func (s *state) done(src *Task) (ready []*Task) {
	for dst := range s.deps[src] {
		s.counts[dst]--
		if s.counts[dst] == 0 {
			ready = append(ready, dst)
		}
	}
	s.deps[src] = nil
	return
}
