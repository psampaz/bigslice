// Copyright 2018 GRAIL, Inc. All rights reserved.
// Use of this source code is governed by the Apache 2.0
// license that can be found in the LICENSE file.

package exec

import (
	"bufio"
	"context"
	"encoding/gob"
	"fmt"
	"io"
	"io/ioutil"
	"math/rand"
	"net/http"
	"runtime"
	"runtime/debug"
	"sync"
	"time"

	"github.com/grailbio/base/backgroundcontext"
	"github.com/grailbio/base/errors"
	"github.com/grailbio/base/limitbuf"
	"github.com/grailbio/base/limiter"
	"github.com/grailbio/base/log"
	"github.com/grailbio/base/retry"
	"github.com/grailbio/base/status"
	"github.com/grailbio/base/sync/ctxsync"
	"github.com/grailbio/base/sync/once"
	"github.com/grailbio/bigmachine"
	"github.com/grailbio/bigslice"
	"github.com/grailbio/bigslice/frame"
	"github.com/grailbio/bigslice/sliceio"
	"github.com/grailbio/bigslice/stats"
	"golang.org/x/sync/errgroup"
)

const BigmachineStatusGroup = "bigmachine"

func init() {
	gob.Register(invocationRef{})
}

const (
	// StatsPollInterval is the period at which task statistics are polled.
	statsPollInterval = 10 * time.Second

	// StatTimeout is the maximum amount of time allowed to retrieve
	// machine stats, per iteration.
	statTimeout = 5 * time.Second
)

// RetryPolicy is the default retry policy used for machine calls.
var retryPolicy = retry.Backoff(time.Second, 5*time.Second, 1.5)

// FatalErr is used to match fatal errors.
var fatalErr = errors.E(errors.Fatal)

// DoShuffleReaders determines whether reader tasks should be
// shuffled in order to avoid potential thundering herd issues.
// This should only be used in testing when deterministic ordering
// matters.
//
// TODO(marius): make this a session option instead.
var DoShuffleReaders = true

func init() {
	gob.Register(&worker{})
}

// TODO(marius): clean up flag registration, etc. vis-a-vis bigmachine.
// e.g., perhaps we can register flags in a bigmachine flagset that gets
// parsed together, so that we don't litter the process with global flags.

// BigmachineExecutor is an executor that runs individual tasks on
// bigmachine machines.
type bigmachineExecutor struct {
	system bigmachine.System
	params []bigmachine.Param

	sess *Session
	b    *bigmachine.B

	status *status.Group

	mu sync.Mutex

	locations map[*Task]*sliceMachine
	stats     map[string]stats.Values

	// Invocations and invocationDeps are used to track dependencies
	// between invocations so that we can execute arbitrary graphs of
	// slices on bigmachine workers. Note that this requires that we
	// hold on to the invocations, which is somewhat unfortunate, but
	// I don't see a clean way around it.
	invocations    map[uint64]bigslice.Invocation
	invocationDeps map[uint64]map[uint64]bool

	// Worker is the (configured) worker service to instantiate on
	// allocated machines.
	worker *worker

	// Managers is the set of machine machine managers used by this
	// executor. Even managers use the session's maxload, and will
	// share task load on a single machine. Odd managers are used
	// for exclusive tasks.
	//
	// Thus manager selection proceeds as follows: the default manager
	// is managers[0]. Func-exclusive tasks use managers[invocation*2].
	//
	// If the task is marked as exclusive, then one is added to their
	// manager index.
	managers []*machineManager
}

func newBigmachineExecutor(system bigmachine.System, params ...bigmachine.Param) *bigmachineExecutor {
	return &bigmachineExecutor{system: system, params: params}
}

// Start starts registers the bigslice worker with bigmachine and then
// starts the bigmachine.
//
// TODO(marius): provide fine-grained fault tolerance.
func (b *bigmachineExecutor) Start(sess *Session) (shutdown func()) {
	b.sess = sess
	b.b = bigmachine.Start(b.system)
	b.locations = make(map[*Task]*sliceMachine)
	b.stats = make(map[string]stats.Values)
	if status := sess.Status(); status != nil {
		b.status = status.Group(BigmachineStatusGroup)
	}
	b.invocations = make(map[uint64]bigslice.Invocation)
	b.invocationDeps = make(map[uint64]map[uint64]bool)
	b.worker = &worker{
		MachineCombiners: sess.machineCombiners,
	}

	return b.b.Shutdown
}

func (b *bigmachineExecutor) manager(i int) *machineManager {
	b.mu.Lock()
	defer b.mu.Unlock()
	for i >= len(b.managers) {
		b.managers = append(b.managers, nil)
	}
	if b.managers[i] == nil {
		maxLoad := b.sess.MaxLoad()
		if i%2 == 1 {
			// In this case, the maxLoad will be adjusted to the smallest
			// feasible value; i.e., one task may run on each machine.
			maxLoad = 0
		}
		b.managers[i] = newMachineManager(b.b, b.params, b.status, b.sess.Parallelism(), maxLoad, b.worker)
		go b.managers[i].Do(backgroundcontext.Get())
	}
	return b.managers[i]
}

type invocationRef struct{ Index uint64 }

func (b *bigmachineExecutor) compile(ctx context.Context, m *sliceMachine, inv bigslice.Invocation) error {
	// Substitute Result arguments for an invocation ref and record the
	// dependency.
	b.mu.Lock()
	for i, arg := range inv.Args {
		result, ok := arg.(*Result)
		if !ok {
			continue
		}
		inv.Args[i] = invocationRef{result.inv.Index}
		if _, ok := b.invocations[result.inv.Index]; !ok {
			b.mu.Unlock()
			return fmt.Errorf("invalid result invocation %x", result.inv.Index)
		}
		if b.invocationDeps[inv.Index] == nil {
			b.invocationDeps[inv.Index] = make(map[uint64]bool)
		}
		b.invocationDeps[inv.Index][result.inv.Index] = true
	}
	b.invocations[inv.Index] = inv

	// Now traverse the invocation graph bottom-up, making sure
	// everything on the machine is compiled. We produce a valid order,
	// but we don't capture opportunities for parallel compilations.
	// TODO(marius): allow for parallel compilation as some users are
	// performing expensive computations inside of bigslice.Funcs.
	var (
		todo        = []uint64{inv.Index}
		invocations []bigslice.Invocation
	)
	for len(todo) > 0 {
		var i uint64
		i, todo = todo[0], todo[1:]
		invocations = append(invocations, b.invocations[i])
		for j := range b.invocationDeps[i] {
			todo = append(todo, j)
		}
	}
	b.mu.Unlock()

	for i := len(invocations) - 1; i >= 0; i-- {
		err := m.Compiles.Do(invocations[i].Index, func() error {
			inv := invocations[i]
			// Flatten these into lists so that we don't capture further
			// structure by JSON encoding down the line. We also truncate them
			// so that, e.g., huge lists of arguments don't make it into the trace.
			args := make([]string, len(inv.Args))
			for i := range args {
				args[i] = truncatef(inv.Args[i])
			}
			b.sess.tracer.Event(m, inv, "B", "location", inv.Location, "args", args)
			err := m.RetryCall(ctx, "Worker.Compile", inv, nil)
			if err != nil {
				b.sess.tracer.Event(m, inv, "E", "error", err)
			} else {
				b.sess.tracer.Event(m, inv, "E")
			}
			return err
		})
		if err != nil {
			return err
		}
	}
	return nil
}

func (b *bigmachineExecutor) commit(ctx context.Context, m *sliceMachine, key string) error {
	return m.Commits.Do(key, func() error {
		log.Printf("committing key %v on worker %v", key, m.Addr)
		return m.RetryCall(ctx, "Worker.CommitCombiner", TaskName{Op: key}, nil)
	})
}

func (b *bigmachineExecutor) Run(task *Task) {
	task.Status.Print("waiting for a machine")

	// Use the default/shared cluster unless the func is exclusive.
	var cluster int
	if task.Invocation.Exclusive {
		cluster = int(task.Invocation.Index) * 2
	}
	if task.Pragma.Exclusive() {
		cluster++
	}
	var (
		mgr            = b.manager(cluster)
		ctx            = backgroundcontext.Get()
		offerc, cancel = mgr.Offer(int(task.Invocation.Index))
		m              *sliceMachine
	)
	select {
	case <-ctx.Done():
		task.Error(ctx.Err())
		cancel()
		return
	case m = <-offerc:
	}
	numTasks := m.Stats.Int("tasks")
	numTasks.Add(1)
	m.UpdateStatus()
	defer func() {
		numTasks.Add(-1)
		m.UpdateStatus()
	}()

	// Make sure that the invocation has been compiled on the selected
	// machine.
compile:
	for {
		err := b.compile(ctx, m, task.Invocation)
		switch {
		case err == nil:
			break compile
		case ctx.Err() == nil && (err == context.Canceled || err == context.DeadlineExceeded):
			// In this case, we've caught a context error from a prior
			// invocation. We're going to try to run it again. Note that this
			// is racy: the behavior remains correct but may imply additional
			// data transfer. C'est la vie.
			m.Compiles.Forget(task.Invocation.Index)
		case errors.Is(errors.Net, err), errors.Is(errors.Unavailable, err), errors.IsTemporary(err):
			// Compilations don't involve invoking user code, nor does it
			// involve dependencies other than potentially uploading data from
			// the driver node, so we can relax our usual assumptions and mark
			// the task as lost. This will cause it to be rescheduled and compilation
			// will be retried.
			task.Status.Printf("task lost while compiling bigslice.Func: %v", err)
			task.Set(TaskLost)
			m.Done(err)
			return
		default:
			task.Errorf("failed to compile invocation on machine %s: %v", m.Addr, err)
			m.Done(err)
			return
		}
	}

	// Populate the run request. Include the locations of all dependent
	// outputs so that the receiving worker can read from them.
	req := taskRunRequest{
		Name:       task.Name,
		Invocation: task.Invocation.Index,
	}
	machineIndices := make(map[string]int)
	g, _ := errgroup.WithContext(ctx)
	for _, dep := range task.Deps {
		for i := 0; i < dep.NumTask(); i++ {
			deptask := dep.Task(i)
			depm := b.location(deptask)
			if depm == nil {
				// TODO(marius): make this a separate state, or a separate
				// error type?
				task.Errorf("task %v has no location", deptask)
				m.Done(nil)
				return
			}
			j, ok := machineIndices[depm.Addr]
			if !ok {
				j = len(machineIndices)
				machineIndices[depm.Addr] = j
				req.Machines = append(req.Machines, depm.Addr)
			}
			req.Locations = append(req.Locations, j)
			key := dep.CombineKey
			if key == "" {
				continue
			}
			// Make sure that the result is committed.
			g.Go(func() error { return b.commit(ctx, depm, key) })
		}
	}

	task.Status.Print(m.Addr)
	if err := g.Wait(); err != nil {
		task.Errorf("failed to commit combiner: %v", err)
		return
	}

	// While we're running, also update task stats directly into the tasks's status.
	// TODO(marius): also aggregate stats across all tasks.
	ctx, ctxcancel := context.WithCancel(ctx)
	defer ctxcancel()

	b.sess.tracer.Event(m, task, "B")
	task.Set(TaskRunning)
	var reply taskRunReply
	err := m.RetryCall(ctx, "Worker.Run", req, &reply)
	m.Done(err)
	switch {
	case err == nil:
		b.sess.tracer.Event(m, task, "E")
		b.setLocation(task, m)
		task.Set(TaskOk)
		m.Assign(task)
	case ctx.Err() != nil:
		b.sess.tracer.Event(m, task, "E", "error", ctx.Err())
		task.Error(err)
	case errors.Match(fatalErr, err) && !errors.Is(errors.Unavailable, err):
		// We assume errors.Unavailable indicates that a worker machine, either
		// directly or transitively, is unavailable, so we consider the task
		// lost and able to rescheduled.

		// TODO(jcharumilind): Make this precise. As it is, this may be a false
		// positive, as user code can produce Unavailable errors that should be
		// fatal. In practice, this does not seem to happen. All cases I've
		// seen have been machine unavailability from which bigslice should try
		// to recover.
		b.sess.tracer.Event(m, task, "E", "error", err, "error_type", "fatal")
		// Fatal errors aren't retryable.
		task.Error(err)
	default:
		// Everything else we consider as the task being lost. It'll get
		// resubmitted by the evaluator.
		b.sess.tracer.Event(m, task, "E", "error", err, "error_type", "lost")
		task.Status.Printf("lost task during task evaluation: %v", err)
		task.Set(TaskLost)
	}
}

func (b *bigmachineExecutor) Reader(ctx context.Context, task *Task, partition int) sliceio.Reader {
	m := b.location(task)
	if m == nil {
		return sliceio.ErrReader(errors.E(errors.NotExist, fmt.Sprintf("task %s", task.Name)))
	}
	if task.CombineKey != "" {
		return sliceio.ErrReader(fmt.Errorf("read %s: cannot read tasks with combine keys", task.Name))
	}
	// TODO(marius): access the store here, too, in case it's a shared one (e.g., s3)
	return &machineReader{
		Machine:       m.Machine,
		TaskPartition: taskPartition{task.Name, partition},
	}
}

func (b *bigmachineExecutor) HandleDebug(handler *http.ServeMux) {
	b.b.HandleDebug(handler)
}

// Location returns the machine on which the results of the provided
// task resides.
func (b *bigmachineExecutor) location(task *Task) *sliceMachine {
	b.mu.Lock()
	m := b.locations[task]
	b.mu.Unlock()
	return m
}

func (b *bigmachineExecutor) setLocation(task *Task, m *sliceMachine) {
	b.mu.Lock()
	b.locations[task] = m
	b.mu.Unlock()
}

type combinerState int

const (
	combinerNone combinerState = iota
	combinerWriting
	combinerCommitted
	combinerError
	combinerIdle
	// States > combinerIdle are reference counts.
)

// A worker is the bigmachine service that runs individual tasks and serves
// the results of previous runs. Currently all output is buffered in memory.
type worker struct {
	// MachineCombiners determines whether to use the MachineCombiners
	// compilation option.
	MachineCombiners bool

	b     *bigmachine.B
	store Store

	mu       sync.Mutex
	cond     *ctxsync.Cond
	compiles once.Map
	tasks    map[uint64]map[TaskName]*Task
	slices   map[uint64]bigslice.Slice
	stats    *stats.Map

	// CombinerStates and combiners are used to manage shared combine
	// buffers.
	combinerStates map[TaskName]combinerState
	combiners      map[TaskName][]chan *combiner

	commitLimiter *limiter.Limiter
}

func (w *worker) Init(b *bigmachine.B) error {
	w.cond = ctxsync.NewCond(&w.mu)
	w.tasks = make(map[uint64]map[TaskName]*Task)
	w.slices = make(map[uint64]bigslice.Slice)
	w.combiners = make(map[TaskName][]chan *combiner)
	w.combinerStates = make(map[TaskName]combinerState)
	w.b = b
	dir, err := ioutil.TempDir("", "bigslice")
	if err != nil {
		return err
	}
	w.store = &fileStore{Prefix: dir + "/"}
	w.stats = stats.NewMap()
	// Set up a limiter to limit the number of concurrent commits
	// that are allowed to happen in the worker.
	//
	// TODO(marius): we should treat commits like tasks and apply
	// load balancing/limiting instead.
	w.commitLimiter = limiter.New()
	procs := b.System().Maxprocs()
	if procs == 0 {
		procs = runtime.GOMAXPROCS(0)
	}
	w.commitLimiter.Release(procs)
	return nil
}

// FuncLocations produces a slice of strings that describe the locations of
// Func creation. It is used to verify that this process is working from an
// identical Func registry. See bigslice.FuncLocations for more information.
func (w *worker) FuncLocations(ctx context.Context, _ struct{}, locs *[]string) error {
	*locs = bigslice.FuncLocations()
	return nil
}

// Compile compiles an invocation on the worker and stores the
// resulting tasks. Compile is idempotent: it will compile each
// invocation at most once.
func (w *worker) Compile(ctx context.Context, inv bigslice.Invocation, _ *struct{}) (err error) {
	defer func() {
		if e := recover(); e != nil {
			err = fmt.Errorf("invocation panic! %v", e)
			err = errors.E(errors.Fatal, err)
		}
	}()
	return w.compiles.Do(inv.Index, func() error {
		// Substitute invocation refs for the results of the invocation.
		// The executor must ensure that all references have been compiled.
		for i, arg := range inv.Args {
			ref, ok := arg.(invocationRef)
			if !ok {
				continue
			}
			w.mu.Lock()
			inv.Args[i], ok = w.slices[ref.Index]
			w.mu.Unlock()
			if !ok {
				return fmt.Errorf("worker.Compile: invalid invocation reference %x", ref.Index)
			}
		}
		slice := inv.Invoke()
		tasks, err := compile(slice, inv, w.MachineCombiners)
		if err != nil {
			return err
		}
		all := make(map[*Task]bool)
		for _, task := range tasks {
			task.all(all)
		}
		named := make(map[TaskName]*Task)
		for task := range all {
			named[task.Name] = task
		}
		w.mu.Lock()
		w.tasks[inv.Index] = named
		w.slices[inv.Index] = &Result{Slice: slice, tasks: tasks}
		w.mu.Unlock()
		return nil
	})
}

// TaskRunRequest contains all data required to run an individual task.
type taskRunRequest struct {
	// Invocation is the invocation from which the task was compiled.
	Invocation uint64

	// Name is the name of the task compiled from Invocation.
	Name TaskName

	// Machines stores the set of machines indexed in Locations.
	Machines []string

	// Locations indexes machine locations for task outputs. Locations
	// stores the machine index of each task dependency. We rely on the
	// fact that the task graph is identical to all viewers: locations
	// are stored in the order of task dependencies.
	Locations []int
}

func (r *taskRunRequest) location(taskIndex int) string {
	return r.Machines[r.Locations[taskIndex]]
}

type taskRunReply struct{} // nothing here yet

// Run runs an individual task as described in the request. Run
// returns a nil error when the task was successfully run and its
// output deposited in a local buffer.
func (w *worker) Run(ctx context.Context, req taskRunRequest, reply *taskRunReply) (err error) {
	recordsOut := w.stats.Int("write")
	w.mu.Lock()
	named := w.tasks[req.Invocation]
	w.mu.Unlock()
	if named == nil {
		return errors.E(errors.Fatal, fmt.Errorf("invocation %x not compiled", req.Invocation))
	}
	task := named[req.Name]
	if task == nil {
		return errors.E(errors.Fatal, fmt.Errorf("task %s not found", req.Name))
	}

	task.Lock()
	switch task.state {
	case TaskLost:
		log.Printf("Worker.Run: %s: reviving LOST task", task.Name)
	case TaskErr:
		log.Printf("Worker.Run: %s: reviving FAILED task", task.Name)
	case TaskInit:
	default:
		for task.state <= TaskRunning {
			log.Printf("runtask: %s already running. Waiting for it to finish.", task.Name)
			err = task.Wait(ctx)
			if err != nil {
				break
			}
		}
		task.Unlock()
		if e := task.Err(); e != nil {
			err = e
		}
		return err
	}
	task.state = TaskRunning
	task.Unlock()
	defer func() {
		if e := recover(); e != nil {
			stack := debug.Stack()
			err = fmt.Errorf("panic while evaluating slice: %v\n%s", e, string(stack))
			err = errors.E(err, errors.Fatal)
		}
		if err != nil {
			log.Printf("task %s error: %v", req.Name, err)
			task.Error(errors.Recover(err))
		} else {
			task.Set(TaskOk)
		}
	}()

	// Gather inputs from the bigmachine cluster, dialing machines
	// as necessary.
	var (
		totalRecordsIn *stats.Int
		recordsIn      *stats.Int
	)
	if len(task.Deps) > 0 {
		totalRecordsIn = w.stats.Int("inrecords")
		recordsIn = w.stats.Int("read")
	}
	var (
		in        = make([]sliceio.Reader, 0, len(task.Deps))
		taskIndex int
	)
	for _, dep := range task.Deps {
		// If the dependency has a combine key, they are combined on the
		// machine, and we de-dup the dependencies.
		//
		// The caller of has already ensured that the combiner buffers
		// are committed on the machines.
		if dep.CombineKey != "" {
			locations := make(map[string]bool)
			for i := 0; i < dep.NumTask(); i++ {
				addr := req.location(taskIndex)
				taskIndex++
				// We only read the first combine key for each location.
				//
				// TODO(marius): compute some non-overlapping intersection of
				// combine keys instead, so that we can handle error recovery
				// properly. In particular, in the case of error recovery, we
				// have to create new combiner keys so that they aren't written
				// into previous combiner buffers. This suggests that combiner
				// keys should be assigned by the executor, and not during
				// compile time.
				if locations[addr] {
					continue
				}
				locations[addr] = true
			}
			for addr := range locations {
				machine, err := w.b.Dial(ctx, addr)
				if err != nil {
					return err
				}
				r := &machineReader{
					Machine:       machine,
					TaskPartition: taskPartition{TaskName{Op: dep.CombineKey}, dep.Partition},
				}
				in = append(in, &statsReader{r, recordsIn})
				defer r.Close()
			}
		} else {
			reader := new(multiReader)
			reader.q = make([]sliceio.Reader, dep.NumTask())
		Tasks:
			for j := 0; j < dep.NumTask(); j++ {
				deptask := dep.Task(j)
				// If we have it locally, or if we're using a shared backend store
				// (e.g., S3), then read it directly.
				info, err := w.store.Stat(ctx, deptask.Name, dep.Partition)
				if err == nil {
					rc, err := w.store.Open(ctx, deptask.Name, dep.Partition, 0)
					if err == nil {
						defer rc.Close()
						reader.q[j] = sliceio.NewDecodingReader(rc)
						totalRecordsIn.Add(info.Records)
						taskIndex++
						continue Tasks
					}
				}
				// Find the location of the task.
				addr := req.location(taskIndex)
				taskIndex++
				machine, err := w.b.Dial(ctx, addr)
				if err != nil {
					return err
				}
				tp := taskPartition{deptask.Name, dep.Partition}
				if err := machine.Call(ctx, "Worker.Stat", tp, &info); err != nil {
					return err
				}
				r := &machineReader{
					Machine:       machine,
					TaskPartition: tp,
				}
				reader.q[j] = &statsReader{r, recordsIn}
				totalRecordsIn.Add(info.Records)
				defer r.Close()
			}
			// We shuffle the tasks here so that we don't encounter
			// "thundering herd" issues were partitions are read sequentially
			// from the same (ordered) list of machines.
			//
			// TODO(marius): possibly we should perform proper load balancing
			// here
			if DoShuffleReaders {
				rand.Shuffle(len(reader.q), func(i, j int) { reader.q[i], reader.q[j] = reader.q[j], reader.q[i] })
			}
			if dep.Expand {
				in = append(in, reader.q...)
			} else {
				in = append(in, reader)
			}
		}
	}

	// If we have a combiner, then we partition globally for the machine
	// into common combiners.
	if task.Combiner != nil {
		return w.runCombine(ctx, task, task.Do(in))
	}

	// Stream partition output directly to the underlying store, but
	// through a buffer because the column encoder can make small
	// writes.
	//
	// TODO(marius): switch to using a monotasks-like arrangement
	// instead once we also have memory management, in order to control
	// buffer growth.
	type partition struct {
		wc  writeCommitter
		buf *bufio.Writer
		*sliceio.Encoder
	}
	partitions := make([]*partition, task.NumPartition)
	for p := range partitions {
		wc, err := w.store.Create(ctx, task.Name, p)
		if err != nil {
			return err
		}
		// TODO(marius): pool the writers so we can reuse them.
		part := new(partition)
		part.wc = wc
		part.buf = bufio.NewWriter(wc)
		part.Encoder = sliceio.NewEncoder(part.buf)
		partitions[p] = part
	}
	defer func() {
		for _, part := range partitions {
			if part == nil {
				continue
			}
			part.wc.Discard(ctx)
		}
	}()
	out := task.Do(in)
	count := make([]int64, task.NumPartition)
	switch {
	case task.NumOut() == 0:
		// If there are no output columns, just drive the computation.
		_, err := out.Read(ctx, frame.Empty)
		if err == sliceio.EOF {
			err = nil
		}
		return err
	case task.NumPartition > 1:
		var psize = *defaultChunksize / 100
		var (
			partitionv = make([]frame.Frame, task.NumPartition)
			lens       = make([]int, task.NumPartition)
		)
		for i := range partitionv {
			partitionv[i] = frame.Make(task, psize, psize)
		}
		in := frame.Make(task, *defaultChunksize, *defaultChunksize)
		for {
			n, err := out.Read(ctx, in)
			if err != nil && err != sliceio.EOF {
				return err
			}
			for i := 0; i < n; i++ {
				p := int(in.Hash(i)) % task.NumPartition
				j := lens[p]
				frame.Copy(partitionv[p].Slice(j, j+1), in.Slice(i, i+1))
				lens[p]++
				count[p]++
				// Flush when we fill up.
				if lens[p] == psize {
					if err := partitions[p].Encode(partitionv[p]); err != nil {
						return err
					}
					lens[p] = 0
				}
			}
			recordsOut.Add(int64(n))
			if err == sliceio.EOF {
				break
			}
		}
		// Flush remaining data.
		for p, n := range lens {
			if n == 0 {
				continue
			}
			if err := partitions[p].Encode(partitionv[p].Slice(0, n)); err != nil {
				return err
			}
		}
	default:
		in := frame.Make(task, *defaultChunksize, *defaultChunksize)
		for {
			n, err := out.Read(ctx, in)
			if err != nil && err != sliceio.EOF {
				return err
			}
			if err := partitions[0].Encode(in.Slice(0, n)); err != nil {
				return err
			}
			recordsOut.Add(int64(n))
			count[0] += int64(n)
			if err == sliceio.EOF {
				break
			}
		}
	}

	for i, part := range partitions {
		if err := part.buf.Flush(); err != nil {
			return err
		}
		partitions[i] = nil
		if err := part.wc.Commit(ctx, count[i]); err != nil {
			return err
		}
	}
	partitions = nil
	return nil
}

func (w *worker) runCombine(ctx context.Context, task *Task, in sliceio.Reader) (err error) {
	combineKey := task.Name
	if task.CombineKey != "" {
		combineKey = TaskName{Op: task.CombineKey}
	}
	w.mu.Lock()
	switch w.combinerStates[combineKey] {
	case combinerWriting, combinerCommitted, combinerError:
		w.mu.Unlock()
		return fmt.Errorf("combine key %s already committed", combineKey)
	case combinerNone:
		combiners := make([]chan *combiner, task.NumPartition)
		for i := range combiners {
			comb, err := newCombiner(task, fmt.Sprintf("%s%d", combineKey, i), *task.Combiner, *defaultChunksize*100)
			if err != nil {
				w.mu.Unlock()
				for j := 0; j < i; j++ {
					if err := (<-combiners[j]).Discard(); err != nil {
						log.Error.Printf("error discarding combiner: %v", err)
					}
				}
				return err
			}
			combiners[i] = make(chan *combiner, 1)
			combiners[i] <- comb
		}
		w.combiners[combineKey] = combiners
		w.combinerStates[combineKey] = combinerIdle
	}
	w.combinerStates[combineKey]++
	combiners := w.combiners[combineKey]
	w.mu.Unlock()

	defer func() {
		w.mu.Lock()
		w.combinerStates[combineKey]--
		w.mu.Unlock()
		if task.CombineKey == "" {
			err = w.CommitCombiner(ctx, combineKey, nil)
		}
	}()

	recordsOut := w.stats.Int("write")
	// Now perform the partition-combine operation. We maintain a
	// per-task combine buffer for each partition. When this buffer
	// reaches half of its capacity, we attempt to combine up to 3/4ths
	// of its least frequent keys into the shared combine buffer. This
	// arrangement permits hot keys to be combined primarily in the task
	// buffer, while spilling less frequent keys into the per machine
	// buffer. (The local buffer is purely in memory, and has a fixed
	// capacity; the machine buffer spills to disk when it reaches a
	// preconfigured threshold.)
	var (
		partitionCombiner = make([]*combiningFrame, task.NumPartition)
		out               = frame.Make(task, *defaultChunksize, *defaultChunksize)
	)
	for i := range partitionCombiner {
		partitionCombiner[i] = makeCombiningFrame(task, *task.Combiner, 8, 1)
	}
	for {
		n, err := in.Read(ctx, out)
		if err != nil && err != sliceio.EOF {
			return err
		}
		for i := 0; i < n; i++ {
			p := int(out.Hash(i)) % task.NumPartition
			pcomb := partitionCombiner[p]
			pcomb.Combine(out.Slice(i, i+1))

			len, cap := pcomb.Len(), pcomb.Cap()
			if len <= cap/2 {
				continue
			}
			var combiner *combiner
			if len >= 8 {
				combiner = <-combiners[p]
			} else {
				select {
				case combiner = <-combiners[p]:
				default:
					continue
				}
			}

			flushed := pcomb.Compact()
			err := combiner.Combine(ctx, flushed)
			combiners[p] <- combiner
			if err != nil {
				return err
			}
		}
		recordsOut.Add(int64(n))
		if err == sliceio.EOF {
			break
		}
	}
	// Flush the remainder.
	for p, comb := range partitionCombiner {
		combiner := <-combiners[p]
		err := combiner.Combine(ctx, comb.Compact())
		combiners[p] <- combiner
		if err != nil {
			return err
		}
	}
	return nil
}

func (w *worker) Stats(ctx context.Context, _ struct{}, values *stats.Values) error {
	w.stats.AddAll(*values)
	return nil
}

// TaskPartition names a partition of a task.
type taskPartition struct {
	// Name is the name of the task whose output is to be read.
	Name TaskName
	// Partition is the partition number to read.
	Partition int
}

// Stat returns the SliceInfo for a slice.
func (w *worker) Stat(ctx context.Context, tp taskPartition, info *sliceInfo) (err error) {
	*info, err = w.store.Stat(ctx, tp.Name, tp.Partition)
	return
}

// CommitCombiner commits the current combiner buffer with the
// provided key. After successful return, its results are available via
// Read.
func (w *worker) CommitCombiner(ctx context.Context, key TaskName, _ *struct{}) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	for {
		switch w.combinerStates[key] {
		case combinerNone:
			return fmt.Errorf("invalid combiner key %s", key)
		case combinerWriting:
			if err := w.cond.Wait(ctx); err != nil {
				return err
			}
		case combinerCommitted:
			return nil
		case combinerError:
			return errors.E("error while writing combiner")
		case combinerIdle:
			w.combinerStates[key] = combinerWriting
			go w.writeCombiner(key)
		default:
			return fmt.Errorf("combiner key %s busy", key)
		}
	}
}

func (w *worker) writeCombiner(key TaskName) {
	g, ctx := errgroup.WithContext(backgroundcontext.Get())
	w.mu.Lock()
	defer w.mu.Unlock()
	for part := range w.combiners[key] {
		part, combiner := part, <-w.combiners[key][part]
		g.Go(func() error {
			w.commitLimiter.Acquire(ctx, 1)
			defer w.commitLimiter.Release(1)
			wc, err := w.store.Create(ctx, key, part)
			if err != nil {
				return err
			}
			buf := bufio.NewWriter(wc)
			enc := sliceio.NewEncoder(buf)
			n, err := combiner.WriteTo(ctx, enc)
			if err != nil {
				wc.Discard(ctx)
				return err
			}
			if err := buf.Flush(); err != nil {
				wc.Discard(ctx)
				return err
			}
			return wc.Commit(ctx, n)
		})
	}
	w.mu.Unlock()
	err := g.Wait()
	w.mu.Lock()
	w.combiners[key] = nil
	if err == nil {
		w.combinerStates[key] = combinerCommitted
	} else {
		log.Error.Printf("failed to write combine buffer %s: %v", key, err)
		w.combinerStates[key] = combinerError
	}
	w.cond.Broadcast()
}

// Read reads a slice.
//
// TODO(marius): should we flush combined outputs explicitly?
func (w *worker) Read(ctx context.Context, req readRequest, rc *io.ReadCloser) (err error) {
	*rc, err = w.store.Open(ctx, req.Name, req.Partition, req.Offset)
	return
}

// readRequest is the request payload for Worker.Run
type readRequest struct {
	// Name is the name of the task whose output is to be read.
	Name TaskName
	// Partition is the partition number to read.
	Partition int
	// Offset is the start offset of the read
	Offset int64
}

// openerAt opens an io.ReadCloser at a given offset. This is used to
// reestablish io.ReadClosers when they are lost due to potentially recoverable
// errors.
type openerAt interface {
	// Open returns a new io.ReadCloser.
	OpenAt(ctx context.Context, offset int64) (io.ReadCloser, error)
}

// machineTaskPartition is a task partition on a specific machine. It implements
// the openerAt interface to provide an io.ReadCloser to read the task data.
type machineTaskPartition struct {
	// machine is the machine from which task data is read.
	machine *bigmachine.Machine
	// taskPartition is the task and partition that should be read.
	taskPartition taskPartition
}

func (m machineTaskPartition) OpenAt(ctx context.Context, offset int64) (reader io.ReadCloser, err error) {
	err = m.machine.RetryCall(ctx, "Worker.Read",
		readRequest{m.taskPartition.Name, m.taskPartition.Partition, offset}, &reader)
	return
}

// retryReader implements an io.ReadCloser that is backed by an openerAt. If it
// encounters an error, it retries by using the openerAt to reopen a new
// io.ReadCloser.
type retryReader struct {
	ctx context.Context
	// name is used for descriptive logging.
	name string
	// openerAt is used to open and reopen the backing io.ReadCloser.
	openerAt openerAt

	err     error
	reader  io.ReadCloser
	bytes   int64
	retries int
}

func newRetryReader(ctx context.Context, name string, openerAt openerAt) *retryReader {
	return &retryReader{
		ctx:      ctx,
		name:     name,
		openerAt: openerAt,
	}
}

func (r *retryReader) Read(data []byte) (int, error) {
	for {
		if r.err != nil {
			return 0, r.err
		}
		if r.reader == nil {
			if r.retries > 0 {
				log.Debug.Printf("reader %s: retrying(%d) from offset %d",
					r.name, r.retries, r.bytes)
			}
			r.reader, r.err = r.openerAt.OpenAt(r.ctx, r.bytes)
			if r.err != nil {
				return 0, r.err
			}
		}
		n, err := r.reader.Read(data)
		if err == nil || err == io.EOF {
			r.retries = 0
			r.err = err
			r.bytes += int64(n)
			return n, err
		}
		// Here, we blindly retry regardless of error kind/severity.
		// This allows us to retry on errors such as aws-sdk or io.UnexpectedEOF.
		// The subsequent call to Worker.Read will detect any permanent
		// errors in any case.
		log.Error.Printf("reader %s: error(retry %d) at %d bytes: %v",
			r.name, r.retries, r.bytes, err)
		r.reader.Close()
		r.reader = nil
		r.retries++
		if r.err = retry.Wait(r.ctx, retryPolicy, r.retries); r.err != nil {
			return 0, r.err
		}
	}
}

func (r *retryReader) Close() error {
	if r.reader == nil {
		return nil
	}
	err := r.reader.Close()
	r.reader = nil
	return err
}

// MachineReader reads a taskPartition from a machine. It issues the
// (streaming) read RPC on the first call to Read so that data are
// not buffered unnecessarily. MachineReaders close themselves after
// they have been read to completion; they should otherwise be closed
// if they are not read to completion.
type machineReader struct {
	// Machine is the machine from which task data is read.
	Machine *bigmachine.Machine
	// TaskPartition is the task and partition that should be read.
	TaskPartition taskPartition

	reader sliceio.Reader
	rpc    *retryReader
}

func newMachineReader(machine *bigmachine.Machine, partition taskPartition) *machineReader {
	m := &machineReader{
		Machine:       machine,
		TaskPartition: partition,
	}
	return m
}

func (m *machineReader) Read(ctx context.Context, f frame.Frame) (int, error) {
	if m.rpc == nil {
		name := fmt.Sprintf("Worker.Read %s:%s:%d",
			m.Machine.Addr, m.TaskPartition.Name, m.TaskPartition.Partition)
		openerAt := machineTaskPartition{
			machine:       m.Machine,
			taskPartition: m.TaskPartition,
		}
		m.rpc = newRetryReader(ctx, name, openerAt)
		m.reader = sliceio.NewDecodingReader(m.rpc)
	}
	n, err := m.reader.Read(ctx, f)
	return n, err
}

func (m *machineReader) Close() error {
	if m.rpc != nil {
		return m.rpc.Close()
	}
	return nil
}

type statsReader struct {
	reader  sliceio.Reader
	numRead *stats.Int
}

func (s *statsReader) Read(ctx context.Context, f frame.Frame) (n int, err error) {
	n, err = s.reader.Read(ctx, f)
	s.numRead.Add(int64(n))
	return
}

func truncatef(v interface{}) string {
	b := limitbuf.NewLogger(512)
	fmt.Fprint(b, v)
	return b.String()
}
