// Copyright 2018 GRAIL, Inc. All rights reserved.
// Use of this source code is governed by the Apache 2.0
// license that can be found in the LICENSE file.

package bigslice_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"text/tabwriter"

	fuzz "github.com/google/gofuzz"
	"github.com/grailbio/base/log"
	"github.com/grailbio/bigmachine/rpc"
	"github.com/grailbio/bigmachine/testsystem"
	"github.com/grailbio/bigslice"
	"github.com/grailbio/bigslice/exec"
	"github.com/grailbio/bigslice/sliceio"
	"github.com/grailbio/bigslice/typecheck"
)

func init() {
	log.AddFlags() // so they can be used in tests
}

var (
	typeOfInt     = reflect.TypeOf(int(0))
	typeOfInt64   = reflect.TypeOf(int64(0))
	typeOfFloat64 = reflect.TypeOf(float64(0))
)

func sortColumns(columns []reflect.Value) {
	s := new(columnSlice)
	s.keys = columns[0].Interface().([]string)
	s.swappers = make([]func(i, j int), len(columns))
	for i := range columns {
		s.swappers[i] = reflect.Swapper(columns[i].Interface())
	}
	sort.Stable(s)
}

type columnSlice struct {
	keys     []string
	swappers []func(i, j int)
}

func (c columnSlice) Len() int           { return len(c.keys) }
func (c columnSlice) Less(i, j int) bool { return c.keys[i] < c.keys[j] }
func (c columnSlice) Swap(i, j int) {
	for _, swap := range c.swappers {
		swap(i, j)
	}
}

var executors = map[string]exec.Option{
	"Local":           exec.Local,
	"Bigmachine.Test": exec.Bigmachine(testsystem.New()),
}

func run(ctx context.Context, t *testing.T, slice bigslice.Slice) map[string]*sliceio.Scanner {
	results := make(map[string]*sliceio.Scanner)
	fn := bigslice.Func(func() bigslice.Slice { return slice })

	for name, opt := range executors {
		if testing.Short() && name != "Local" {
			continue
		}
		sess := exec.Start(opt)
		// TODO(marius): faster teardown in bigmachine so that we can call this here.
		// defer sess.Shutdown()
		res, err := sess.Run(ctx, fn)
		if err != nil {
			t.Errorf("executor %s error %v", name, err)
			continue
		}
		results[name] = res.Scan(ctx)
	}
	return results
}

func assertColumnsEqual(t *testing.T, sort bool, columns ...interface{}) {
	t.Helper()
	if len(columns)%2 != 0 {
		t.Fatal("must pass even number of columns")
	}
	numColumns := len(columns) / 2
	if numColumns < 1 {
		t.Fatal("must have at least one column to compare")
	}
	gotCols := make([]reflect.Value, numColumns)
	wantCols := make([]reflect.Value, numColumns)
	for i := range columns {
		j := i / 2
		if i%2 == 0 {
			gotCols[j] = reflect.ValueOf(columns[i])
			if gotCols[j].Kind() != reflect.Slice {
				t.Errorf("column %d of actual must be a slice", j)
				return
			}
			if j > 0 && gotCols[j].Len() != gotCols[j-1].Len() {
				t.Errorf("got %d, want %d columns in actual", gotCols[j].Len(), gotCols[j-1].Len())
				return
			}
		} else {
			// Problems with our expected columns are fatal, as that means that
			// the test itself is incorrectly constructed.
			wantCols[j] = reflect.ValueOf(columns[i])
			if wantCols[j].Kind() != reflect.Slice {
				t.Fatalf("column %d of expected must be a slice", j)
			}
			if j > 0 && wantCols[j].Len() != wantCols[j-1].Len() {
				t.Fatalf("got %d, want %d columns in expected", wantCols[j].Len(), wantCols[j-1].Len())
			}
		}
	}
	if sort {
		sortColumns(gotCols)
		sortColumns(wantCols)
	}

	switch got, want := gotCols[0].Len(), wantCols[0].Len(); {
	case got == want:
	case got < want:
		t.Errorf("short result: got %v, want %v", got, want)
		return
	case want < got:
		row := make([]string, len(gotCols))
		for i := range row {
			row[i] = fmt.Sprint(gotCols[i].Index(want).Interface())
		}
		// Show one row of extra values to help debug.
		t.Errorf("extra values: %v", strings.Join(row, ","))
	}

	// wantCols[0].Len() <= gotCols[0].Len() so we compare wantCols[0].Len()
	// rows.
	numRows := wantCols[0].Len()
	got := make([]interface{}, numColumns)
	want := make([]interface{}, numColumns)
	for i := 0; i < numColumns; i++ {
		got[i] = gotCols[i].Interface()
		want[i] = wantCols[i].Interface()
	}

	if !reflect.DeepEqual(got, want) {
		// Print full rows for small results. They are easier to interpret
		// than diffs.
		if numRows < 10 && numColumns < 10 {
			var (
				gotRows  = make([]string, numRows)
				wantRows = make([]string, numRows)
			)
			for i := range gotRows {
				var (
					got  = make([]string, numColumns)
					want = make([]string, numColumns)
				)
				for j := range got {
					got[j] = fmt.Sprint(gotCols[j].Index(i).Interface())
					want[j] = fmt.Sprint(wantCols[j].Index(i).Interface())
				}
				gotRows[i] = strings.Join(got, " ")
				wantRows[i] = strings.Join(want, " ")
			}
			t.Errorf("result mismatch:\ngot:\n%s\nwant:\n%s", strings.Join(gotRows, "\n"), strings.Join(wantRows, "\n"))
			return
		}

		// Print as columns
		var b bytes.Buffer
		var tw tabwriter.Writer
		tw.Init(&b, 4, 4, 1, ' ', 0)
		for i := 0; i < numRows; i++ {
			var diff bool
			row := make([]string, numColumns)
			for j := range row {
				got := gotCols[j].Index(i).Interface()
				want := wantCols[j].Index(i).Interface()
				if !reflect.DeepEqual(got, want) {
					diff = true
					row[j] = fmt.Sprintf("%v->%v", want, got)
				} else {
					row[j] = fmt.Sprint(got)
				}
			}
			if diff {
				fmt.Fprintf(&tw, "[%d] %s\n", i, strings.Join(row, "\t"))
			}
		}
		tw.Flush()
		t.Errorf("result mismatch:\n%s", b.String())
	}
}

func assertEqual(t *testing.T, slice bigslice.Slice, sort bool, expect ...interface{}) {
	if !testing.Short() {
		rpc.InjectFailures = true
		defer func() { rpc.InjectFailures = false }()
	}

	t.Helper()
	for name, s := range run(context.Background(), t, slice) {
		t.Run(name, func(t *testing.T) {
			args := make([]interface{}, len(expect))
			for i := range args {
				// Make this one larger to make sure we exhaust the scanner.
				v := reflect.ValueOf(expect[i])
				slice := reflect.MakeSlice(v.Type(), v.Len()+1, v.Len()+1)
				args[i] = slice.Interface()
			}
			n, ok := s.Scanv(context.Background(), args...)
			if ok {
				t.Errorf("%s: long read (%d)", name, n)
			}
			if err := s.Err(); err != nil {
				t.Errorf("%s: %v", name, err)
				return
			}
			for i := range args {
				args[i] = reflect.ValueOf(args[i]).Slice(0, n).Interface()
			}
			columns := make([]interface{}, len(expect)*2)
			for i := range expect {
				columns[i*2] = args[i]
				columns[i*2+1] = expect[i]
			}
			assertColumnsEqual(t, sort, columns...)
		})
	}
}

func expectTypeError(t *testing.T, message string, fn func()) {
	t.Helper()
	typecheck.TestCalldepth = 2
	_, file, line, ok := runtime.Caller(1)
	if !ok {
		t.Fatal("runtime.Caller error")
	}
	defer func() {
		t.Helper()
		typecheck.TestCalldepth = 0
		e := recover()
		if e == nil {
			t.Fatal("expected error")
		}
		err, ok := e.(*typecheck.Error)
		if !ok {
			t.Fatalf("expected typeError, got %T", e)
		}
		if got, want := err.File, file; got != want {
			t.Errorf("got %v, want %v", got, want)
		}
		if got, want := err.Line, line; got != want {
			t.Errorf("got %v, want %v", got, want)
		}
		if got, want := err.Err.Error(), message; got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	}()
	fn()
}

func TestConst(t *testing.T) {
	const N = 10000
	fz := fuzz.New()
	fz.NilChance(0)
	fz.NumElements(N, N)
	var (
		col1 []string
		col2 []int
	)
	fz.Fuzz(&col1)
	fz.Fuzz(&col2)
	for nshards := 1; nshards < 20; nshards++ {
		slice := bigslice.Const(nshards, col1, col2)
		assertEqual(t, slice, true, col1, col2)
	}
}

func TestConstError(t *testing.T) {
	expectTypeError(t, "const: invalid slice inputs", func() { bigslice.Const(1, 123) })
}

func TestReaderFunc(t *testing.T) {
	const (
		N      = 10000
		Nshard = 10
	)
	type state struct {
		*fuzz.Fuzzer
		total int
	}
	slice := bigslice.ReaderFunc(Nshard, func(shard int, state *state, strings []string, ints []int) (n int, err error) {
		// The input should be zerod by bigslice.
		var nnonzero int
		for i := range strings {
			if strings[i] != "" || ints[i] != 0 {
				nnonzero++
			}
		}
		if nnonzero > 0 {
			t.Errorf("%d (of %d) nonzero rows", nnonzero, len(strings))
		}
		if state.Fuzzer == nil {
			state.Fuzzer = fuzz.New()
		}
		state.NumElements(1, len(strings))
		var (
			fstrings []string
			fints    []int
		)
		state.Fuzz(&fstrings)
		state.Fuzz(&fints)
		n = copy(strings, fstrings)
		m := copy(ints, fints)
		if m < n {
			n = m
		}
		state.total += n
		if state.total >= N {
			return n - (state.total - N), sliceio.EOF
		}
		return n, nil
	})
	// Map everything to the same key so we can count them.
	slice = bigslice.Map(slice, func(s string, i int) (key string, count int) { return "", 1 })
	slice = bigslice.Fold(slice, func(a, e int) int { return a + e })
	assertEqual(t, slice, false, []string{""}, []int{N * Nshard})
}

func TestReaderFuncError(t *testing.T) {
	expectTypeError(t, "readerfunc: invalid reader function type func()", func() { bigslice.ReaderFunc(1, func() {}) })
	expectTypeError(t, "readerfunc: invalid reader function type string", func() { bigslice.ReaderFunc(1, "invalid") })
	expectTypeError(t, "readerfunc: invalid reader function type func(string, string, []int) (int, error)", func() { bigslice.ReaderFunc(1, func(shard string, state string, x []int) (int, error) { panic("") }) })
	expectTypeError(t, "readerfunc: function func(int, string, []int) error does not return (int, error)", func() { bigslice.ReaderFunc(1, func(shard int, state string, x []int) error { panic("") }) })
	expectTypeError(t, "readerfunc: invalid reader function type func(int, string) (int, error)", func() { bigslice.ReaderFunc(1, func(shard int, state string) (int, error) { panic("") }) })
}

const readerFuncForgetEOFMessage = "warning: reader func returned empty vector"

// TestReaderFuncForgetEOF runs a buggy ReaderFunc that never returns sliceio.EOF. We check that
// bigslice prints a warning.
func TestReaderFuncForgetEOF(t *testing.T) {
	var logOut bytes.Buffer
	log.SetOutput(&logOut)
	const N = 500
	slice := bigslice.ReaderFunc(1, func(_ int, state *int, _ []int) (int, error) {
		// Simulate an empty input. Users should return sliceio.EOF immediately, but some forget
		// and just return nil. Eventually return EOF so the test terminates.
		if *state >= N {
			return 0, sliceio.EOF
		}
		*state++
		return 0, nil
	})
	assertEqual(t, slice, false, []int{})
	if !strings.Contains(logOut.String(), readerFuncForgetEOFMessage) {
		t.Errorf("expected empty vector log message, got: %q", logOut.String())
	}
}

// TestReaderFuncNoForgetEOF complements TestReaderFuncForgetEOF, testing that no spurious log
// messages are written if reader funcs return non-empty vectors.
func TestReaderFuncNoForgetEOF(t *testing.T) {
	var logOut bytes.Buffer
	log.SetOutput(&logOut)
	const N = 500
	slice := bigslice.ReaderFunc(1, func(_ int, state *int, out []int) (int, error) {
		// Simulate an empty input. Users should return sliceio.EOF immediately, but some forget
		// and just return nil. Eventually return EOF so the test terminates.
		if *state >= N {
			return 0, sliceio.EOF
		}
		*state++
		return 1, nil
	})
	assertEqual(t, slice, false, make([]int, N))
	if strings.Contains(logOut.String(), readerFuncForgetEOFMessage) {
		t.Errorf("expected no empty vector log message, got: %q", logOut.String())
	}
}

// TestWriterFunc tests the basic functionality of WriterFunc, verifying that
// all data is passed to the write function, and all data is available in the
// resulting slice.
func TestWriterFunc(t *testing.T) {
	const (
		N      = 10000
		Nshard = 10
	)
	fz := fuzz.New()
	fz.NilChance(0)
	fz.NumElements(N, N)
	var (
		col1 []string
		col2 []int
	)
	fz.Fuzz(&col1)
	fz.Fuzz(&col2)

	slice := bigslice.Const(Nshard, col1, col2)

	type state struct {
		col1 []string
		col2 []int
		errs []error
	}
	var (
		writerMutex sync.Mutex
		// The states of the writers, by shard.
		writerStates []state
	)
	slice = bigslice.WriterFunc(slice,
		func(shard int, state *state, err error, col1 []string, col2 []int) error {
			state.col1 = append(state.col1, col1...)
			state.col2 = append(state.col2, col2...)
			state.errs = append(state.errs, err)
			if err != nil {
				writerMutex.Lock()
				defer writerMutex.Unlock()
				writerStates[shard] = *state
			}
			return nil
		})

	// We expect both the columns written by the writer func and the columns in
	// the resulting slice to match the input. We make a copy to avoid
	// disturbing the inputs, as we'll end up sorting these to compare them.
	wantCol1 := append([]string{}, col1...)
	wantCol2 := append([]int{}, col2...)

	ctx := context.Background()
	fn := bigslice.Func(func() bigslice.Slice { return slice })
	for name, opt := range executors {
		t.Run(name, func(t *testing.T) {
			// Each execution starts with a fresh state for the writer.
			writerStates = make([]state, Nshard)
			sess := exec.Start(opt)
			res, err := sess.Run(ctx, fn)
			if err != nil {
				t.Errorf("executor %s error %v", name, err)
				return
			}

			// Check the columns in the output slice.
			scanner := res.Scan(ctx)
			var (
				s       string
				i       int
				resCol1 []string
				resCol2 []int
			)
			for scanner.Scan(context.Background(), &s, &i) {
				resCol1 = append(resCol1, s)
				resCol2 = append(resCol2, i)
			}
			assertColumnsEqual(t, true, resCol1, wantCol1, resCol2, wantCol2)

			// Check the columns written by the writer func.
			var (
				writerCol1 []string
				writerCol2 []int
			)
			for _, state := range writerStates {
				writerCol1 = append(writerCol1, state.col1...)
				writerCol2 = append(writerCol2, state.col2...)
			}
			assertColumnsEqual(t, true, writerCol1, wantCol1, writerCol2, wantCol2)

			// Check that errors were passed as expected to the writer func.
			for shard, state := range writerStates {
				if len(state.errs) < 1 {
					t.Errorf("writer for shard %d did not get EOF", shard)
					continue
				}
				for i := 0; i < len(state.errs)-1; i++ {
					if state.errs[i] != nil {
						// Only the last error received should be non-nil.
						t.Errorf("got premature error")
						break
					}
				}
				if got, want := state.errs[len(state.errs)-1], sliceio.EOF; got != want {
					t.Errorf("got %v, want %v", got, want)
				}
			}
		})
	}
}

// TestWriterFuncBadFunc tests the type-checking of the writer func passed to
// WriterFunc.
func TestWriterFuncBadFunc(t *testing.T) {
	for _, c := range []struct {
		name    string
		message string
		f       interface{}
	}{
		{
			"String",
			"writerfunc: invalid writer function type string; must be func(shard int, state stateType, err error, col1 []string, col2 []int) error",
			"I'm not a function at all",
		},
		{
			"NoArguments",
			"writerfunc: invalid writer function type func(); must be func(shard int, state stateType, err error, col1 []string, col2 []int) error",
			func() {},
		},
		{
			"NonSliceColumn",
			"writerfunc: invalid writer function type func(int, int, error, string, []int) error; must be func(shard int, state stateType, err error, col1 []string, col2 []int) error",
			func(shard int, state int, err error, col1 string, col2 []int) error { panic("") },
		},
		{
			"NotEnoughColumns",
			"writerfunc: invalid writer function type func(int, int, error, []string) error; must be func(shard int, state stateType, err error, col1 []string, col2 []int) error",
			func(shard int, state int, err error, col1 []string) error { panic("") },
		},
		{
			"TooManyColumns",
			"writerfunc: invalid writer function type func(int, int, error, []string, []int, []int) error; must be func(shard int, state stateType, err error, col1 []string, col2 []int) error",
			func(shard int, state int, err error, col1 []string, col2 []int, col3 []int) error { panic("") },
		},
		{
			"StringShard",
			"writerfunc: invalid writer function type func(string, int, error, []string, []int) error; must be func(shard int, state stateType, err error, col1 []string, col2 []int) error",
			func(shard string, state int, err error, col1 []string, col2 []int) error { panic("") },
		},
		{
			"WrongColumnElementType",
			"writerfunc: invalid writer function type func(int, int, error, []string, []string) error; must be func(shard int, state stateType, err error, col1 []string, col2 []int) error",
			func(shard int, state int, err error, col1 []string, col2 []string) error { panic("") },
		},
		{
			"NoReturn",
			"writerfunc: invalid writer function type func(int, int, error, []string, []int); must return error",
			func(shard int, state int, err error, col1 []string, col2 []int) { panic("") },
		},
		{
			"ReturnInt",
			"writerfunc: invalid writer function type func(int, int, error, []string, []int) int; must return error",
			func(shard int, state int, err error, col1 []string, col2 []int) int { panic("") },
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			slice := bigslice.Const(1, []string{}, []int{})
			expectTypeError(t, c.message, func() { bigslice.WriterFunc(slice, c.f) })
		})
	}
}

// TestWriterFuncError tests the behavior of WriterFunc under various error
// conditions.
func TestWriterFuncError(t *testing.T) {
	assertWriterErr := func(t *testing.T, slice bigslice.Slice) {
		fn := bigslice.Func(func() bigslice.Slice { return slice })
		for name, opt := range executors {
			t.Run(name, func(t *testing.T) {
				sess := exec.Start(opt)
				_, err := sess.Run(context.Background(), fn)
				if err == nil {
					t.Errorf("expected error")
				} else {
					if got, want := err.Error(), "writerError"; !strings.Contains(got, want) {
						t.Errorf("got %v, want %v", got, want)
					}
				}
			})
		}
	}

	// The write function always returns an error, so we should see it.
	t.Run("WriteAlwaysErr", func(t *testing.T) {
		slice := bigslice.Const(2, []string{"a", "b", "c", "d"})
		slice = bigslice.WriterFunc(slice, func(shard int, state int, err error, col1 []string) error {
			return errors.New("writerError")
		})
		assertWriterErr(t, slice)
	})

	// The write function returns an error when it sees the EOF. We expect to
	// see the returned error, even though the underlying read succeeded
	// without error.
	t.Run("WriteErrOnEOF", func(t *testing.T) {
		slice := bigslice.Const(2, []string{"a", "b", "c", "d"})
		slice = bigslice.WriterFunc(slice, func(shard int, state int, err error, col1 []string) error {
			if err == sliceio.EOF {
				return errors.New("writerError")
			}
			return nil
		})
		assertWriterErr(t, slice)
	})
}

func TestMap(t *testing.T) {
	const N = 100000
	input := make([]int, N)
	output := make([]string, N)
	for i := range input {
		input[i] = i
		output[i] = fmt.Sprint(i)
	}
	slice := bigslice.Const(1, input)
	slice = bigslice.Map(slice, func(i int) string { return fmt.Sprint(i) })
	assertEqual(t, slice, false, output)
}

func TestMapError(t *testing.T) {
	input := bigslice.Const(1, []string{"x", "y"})
	expectTypeError(t, "map: invalid map function int", func() { bigslice.Map(input, 123) })
	expectTypeError(t, "map: function func(int) string does not match input slice type slice[1]string", func() { bigslice.Map(input, func(x int) string { return "" }) })
	expectTypeError(t, "map: function func(int, int) string does not match input slice type slice[1]string", func() { bigslice.Map(input, func(x, y int) string { return "" }) })
	expectTypeError(t, "map: need at least one output column", func() { bigslice.Map(input, func(x string) {}) })
}

func TestFilter(t *testing.T) {
	const N = 100000
	input := make([]int, N)
	output := make([]int, N/2)
	for i := range input {
		input[i] = i
		if i%2 == 0 {
			output[i/2] = i
		}
	}
	slice := bigslice.Const(N/1000, input)
	slice = bigslice.Filter(slice, func(i int) bool { return i%2 == 0 })
	assertEqual(t, slice, false, output)

	slice = bigslice.Const(1, input)
	slice = bigslice.Filter(slice, func(i int) bool { return false })
	assertEqual(t, slice, false, []int{})

	slice = bigslice.Const(1, input)
	slice = bigslice.Filter(slice, func(i int) bool {
		switch i {
		case N / 4, N / 2, 3 * N / 4:
			return true
		default:
			return false
		}
	})
	assertEqual(t, slice, false, []int{N / 4, N / 2, 3 * N / 4})
}

func TestFilterError(t *testing.T) {
	input := bigslice.Const(1, []string{"x", "y"})
	expectTypeError(t, "filter: invalid predicate function int", func() { bigslice.Filter(input, 123) })
	expectTypeError(t, "filter: function func(int) bool does not match input slice type slice[1]string", func() { bigslice.Filter(input, func(x int) bool { return false }) })
	expectTypeError(t, "filter: function func(int, int) string does not match input slice type slice[1]string", func() { bigslice.Filter(input, func(x, y int) string { return "" }) })
	expectTypeError(t, "filter: predicate must return a single boolean value", func() { bigslice.Filter(input, func(x string) {}) })
	expectTypeError(t, "filter: predicate must return a single boolean value", func() { bigslice.Filter(input, func(x string) int { return 0 }) })
	expectTypeError(t, "filter: predicate must return a single boolean value", func() { bigslice.Filter(input, func(x string) (bool, int) { return false, 0 }) })
}

func TestFlatmap(t *testing.T) {
	slice := bigslice.Const(2, []string{"x,x", "y,y,y", "z", "", "x"})
	slice = bigslice.Flatmap(slice, func(s string) []string {
		if s == "" {
			return nil
		}
		return strings.Split(s, ",")
	})
	assertEqual(t, slice, true, []string{"x", "x", "x", "y", "y", "y", "z"})

	// Multiple columns
	slice = bigslice.Flatmap(slice, func(s string) ([]string, []int) {
		return []string{s}, []int{len(s)}
	})
	assertEqual(t, slice, true,
		[]string{"x", "x", "x", "y", "y", "y", "z"},
		[]int{1, 1, 1, 1, 1, 1, 1},
	)

	// Filter everything
	slice = bigslice.Flatmap(slice, func(s string, i int) []string {
		return nil
	})
	assertEqual(t, slice, true, []string{})

	// Partial filter
	slice = bigslice.Const(1, []int{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10})
	slice = bigslice.Flatmap(slice, func(i int) []int {
		if i%2 == 0 {
			return []int{i}
		}
		return nil
	})
	assertEqual(t, slice, false, []int{0, 2, 4, 6, 8, 10})

	// Large slices
	input := make([]string, 1024*10)
	for i := range input {
		input[i] = fmt.Sprint(i)
	}
	slice = bigslice.Const(5, input)
	slice = bigslice.Flatmap(slice, func(s string) []string {
		switch s {
		case "1024":
			return []string{s}
		case "5000":
			return []string{s}
		default:
			return nil
		}
	})
	assertEqual(t, slice, true, []string{"1024", "5000"})
}

func TestFlatmapBuffered(t *testing.T) {
	zeros := make([]int, 1025)
	slice := bigslice.Const(1, []int{0})
	slice = bigslice.Flatmap(slice, func(i int) []int {
		return zeros
	})
	// Drive it manually:
	assertEqual(t, slice, false, zeros)
}

func TestFlatmapError(t *testing.T) {
	input := bigslice.Const(1, []int{1, 2, 3})
	expectTypeError(t, "flatmap: invalid flatmap function int", func() { bigslice.Flatmap(input, 123) })
	expectTypeError(t, "flatmap: flatmap function func(string) []int does not match input slice type slice[1]int", func() { bigslice.Flatmap(input, func(s string) []int { return nil }) })
	expectTypeError(t, "flatmap: flatmap function func(int) int is not vectorized", func() { bigslice.Flatmap(input, func(i int) int { return 0 }) })
	expectTypeError(t, "flatmap: flatmap function func(int, int) []int does not match input slice type slice[1]int", func() { bigslice.Flatmap(input, func(i, j int) []int { return nil }) })

}

func TestFold(t *testing.T) {
	const N = 10000
	fz := fuzz.New()
	fz.NilChance(0)
	fz.NumElements(N/2, N/2)
	var (
		keys   []string
		values []int
	)
	fz.Fuzz(&keys)
	fz.Fuzz(&values)
	keys = append(keys, keys...)
	values = append(values, values...)
	slice := bigslice.Const(N/1000, keys, values)
	slice = bigslice.Fold(slice, func(a, e int) int { return a + e })

	expect := make(map[string]int)
	for i, key := range keys {
		expect[key] += values[i]
	}
	var (
		expectKeys   []string
		expectValues []int
	)
	for key, value := range expect {
		expectKeys = append(expectKeys, key)
		expectValues = append(expectValues, value)
	}
	assertEqual(t, slice, true, expectKeys, expectValues)

	// Make sure we can partition other element types also.
	slice = bigslice.Const(N/1000, values, keys)
	slice = bigslice.Fold(slice, func(a int, e string) int { return a + len(e) })
	slice = bigslice.Map(slice, func(key, count int) (int, int) { return 0, count })
	slice = bigslice.Fold(slice, func(a, e int) int { return a + e })
	var totalSize int
	for _, key := range keys {
		totalSize += len(key)
	}
	assertEqual(t, slice, false, []int{0}, []int{totalSize})
}

func TestFoldError(t *testing.T) {
	input := bigslice.Const(1, []int{1, 2, 3})
	floatInput := bigslice.Map(input, func(x int) (float64, int) { return 0, 0 })
	intInput := bigslice.Map(input, func(x int) (int, int) { return 0, 0 })
	expectTypeError(t, "fold: key type float64 cannot be accumulated", func() { bigslice.Fold(floatInput, func(x int) int { return 0 }) })
	expectTypeError(t, "Fold can be applied only for slices with at least two columns; got 1", func() { bigslice.Fold(input, func(x int) int { return 0 }) })
	expectTypeError(t, "fold: expected func(acc, t2, t3, ..., tn), got func(int) int", func() { bigslice.Fold(intInput, func(x int) int { return 0 }) })
	expectTypeError(t, "fold: expected func(acc, t2, t3, ..., tn), got func(int, int) string", func() { bigslice.Fold(intInput, func(a, x int) string { return "" }) })
	expectTypeError(t, "fold: fold functions must return exactly one value", func() { bigslice.Fold(intInput, func(a, x int) (int, int) { return 0, 0 }) })
	expectTypeError(t, "fold: expected func(acc, t2, t3, ..., tn), got func(int, string) int", func() { bigslice.Fold(intInput, func(a int, x string) int { return 0 }) })
}

func TestHead(t *testing.T) {
	slice := bigslice.Head(bigslice.Const(2, []int{1, 2, 3, 4, 5, 6, 7, 8, 9, 0}), 2)
	assertEqual(t, slice, false, []int{1, 2, 7, 8})
}

func TestScan(t *testing.T) {
	const (
		N      = 10000
		Nshard = 10
	)
	input := make([]int, N)
	for i := range input {
		input[i] = i
	}
	var mu sync.Mutex
	output := make([]int, N)
	shards := make([]int, Nshard)
	slice := bigslice.Const(Nshard, input)
	slice = bigslice.Scan(slice, func(shard int, scan *sliceio.Scanner) error {
		mu.Lock()
		defer mu.Unlock()
		shards[shard]++
		var elem int
		ctx := context.Background()
		for scan.Scan(ctx, &elem) {
			output[elem]++
		}
		return scan.Err()
	})
	n := len(run(context.Background(), t, slice))
	for i, got := range output {
		if want := n; got != want {
			t.Errorf("wrong count for output %d, got %v, want %v", i, got, want)
		}
	}
	for i, got := range shards {
		if want := n; got != want {
			t.Errorf("wrong count for shard %d, got %v, want %v", i, got, want)
		}
	}
}

func TestPanic(t *testing.T) {
	slice := bigslice.Const(1, []int{1, 2, 3})
	slice = bigslice.Map(slice, func(i int) int {
		panic(i)
	})
	fn := bigslice.Func(func() bigslice.Slice { return slice })
	ctx := context.Background()
	for name, opt := range executors {
		sess := exec.Start(opt)
		// TODO(marius): faster teardown in bigmachine so that we can call this here.
		// defer sess.Shutdown()
		_, err := sess.Run(ctx, fn)
		if err == nil {
			t.Errorf("executor %s: expected error", name)
			continue
		}
		if msg := err.Error(); !strings.Contains(msg, "panic while evaluating slice") {
			t.Errorf("wrong error message %q", msg)
		}
	}
}
