package pqueue

import (
	"math/rand"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"testing"
)

func equal(t *testing.T, act, exp interface{}) {
	if !reflect.DeepEqual(exp, act) {
		_, file, line, _ := runtime.Caller(1)
		t.Logf("\033[31m%s:%d:\n\n\texp: %#v\n\n\tgot: %#v\033[39m\n\n",
			filepath.Base(file), line, exp, act)
		t.FailNow()
	}
}

func TestPriorityQueue(t *testing.T) {
	c := 100
	pq := New[int, int64](c, Min[int64])

	for i := 0; i < c+1; i++ {
		pq.Push(i, int64(i))
	}
	equal(t, pq.Len(), c+1)
	equal(t, cap(pq.items), c*2)

	for i := 0; i < c+1; i++ {
		val, priority, ok := pq.Pop()
		equal(t, ok, true)
		equal(t, val, i)
		equal(t, priority, int64(i))
	}
	equal(t, cap(pq.items), c/4)
}

func TestUnsortedInsert(t *testing.T) {
	c := 100
	pq := New[int, int64](c, Min[int64])
	ints := make([]int, 0, c)

	for i := 0; i < c; i++ {
		v := rand.Int()
		ints = append(ints, v)
		pq.Push(i, int64(v))
	}
	equal(t, pq.Len(), c)
	equal(t, cap(pq.items), c)

	sort.Ints(ints)
	max := int64(ints[len(ints)-1])

	for i := 0; i < c; i++ {
		_, priority, ok := pq.PeekAndShift(func(p int64) bool {
			return p > max
		})
		equal(t, ok, true)
		equal(t, priority, int64(ints[i]))
	}
}

func TestRemove(t *testing.T) {
	c := 100
	pq := New[string, int64](c, Min[int64])

	for i := 0; i < c; i++ {
		v := rand.Int()
		pq.Push("test", int64(v))
	}

	for i := 0; i < 10; i++ {
		pq.Remove(rand.Intn((c - 1) - i))
	}

	_, lastPriority, _ := pq.Pop()
	for i := 0; i < (c - 10 - 1); i++ {
		_, priority, ok := pq.Pop()
		equal(t, lastPriority < priority, true)
		equal(t, ok, true)
		lastPriority = priority
	}
}
