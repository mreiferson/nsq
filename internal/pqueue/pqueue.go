package pqueue

import (
	"golang.org/x/exp/constraints"
)

func Min[T constraints.Ordered](i, j T) bool {
	return i < j
}

type Item[T any, P constraints.Ordered] struct {
	Val      T
	Priority P
	Index    int
}

type PriorityQueue[T any, P constraints.Ordered] struct {
	items      []*Item[T, P]
	count      uint
	comparator func(l P, r P) bool
}

func New[T any, P constraints.Ordered](capacity int, comparator func(l P, r P) bool) *PriorityQueue[T, P] {
	return &PriorityQueue[T, P]{
		items:      make([]*Item[T, P], 0, capacity),
		comparator: comparator,
	}
}

func (pq *PriorityQueue[T, P]) Len() int {
	return len(pq.items)
}

func (pq *PriorityQueue[T, P]) Push(item *Item[T, P]) {
	n := len(pq.items)
	c := cap(pq.items)
	if n+1 > c {
		items := make([]*Item[T, P], n, c*2)
		copy(items, pq.items)
		pq.items = items
	}
	item.Index = n
	pq.items = append(pq.items, item)
	pq.up(n)
}

func (pq *PriorityQueue[T, P]) Pop() *Item[T, P] {
	n := len(pq.items)
	pq.swap(0, n-1)
	pq.down(0, n-1)
	return pq.pop()
}

func (pq *PriorityQueue[T, P]) Remove(i int) *Item[T, P] {
	n := len(pq.items)
	if i != n-1 {
		pq.swap(i, n-1)
		if !pq.down(i, n-1) {
			pq.up(i)
		}
	}
	return pq.pop()
}

func (pq *PriorityQueue[T, P]) Update(item *Item[T, P]) {
	if item.Index == -1 {
		return
	}
	n := len(pq.items)
	i := item.Index
	if !pq.down(i, n) {
		pq.up(i)
	}
}

func (pq *PriorityQueue[T, P]) PeekAndShift(comp func(p P) bool) *Item[T, P] {
	if len(pq.items) == 0 {
		return nil
	}

	item := pq.items[0]
	if comp(item.Priority) {
		return nil
	}
	pq.Remove(0)

	return item
}

func (pq *PriorityQueue[T, P]) less(i, j int) bool {
	return pq.comparator(pq.items[i].Priority, pq.items[j].Priority)
}

func (pq *PriorityQueue[T, P]) swap(i, j int) {
	pq.items[i], pq.items[j] = pq.items[j], pq.items[i]
}

func (pq *PriorityQueue[T, P]) pop() *Item[T, P] {
	n := len(pq.items)
	c := cap(pq.items)
	if n < (c/2) && c > 25 {
		items := make([]*Item[T, P], n, c/2)
		copy(items, pq.items)
		pq.items = items
	}
	item := pq.items[n-1]
	item.Index = -1
	pq.items[n-1] = nil
	pq.items = pq.items[0 : n-1]
	return item
}

func (pq *PriorityQueue[T, P]) up(j int) {
	for {
		i := (j - 1) / 2 // parent
		if i == j || !pq.less(j, i) {
			break
		}
		pq.swap(i, j)
		j = i
	}
}

func (pq *PriorityQueue[T, P]) down(i0, n int) bool {
	i := i0
	for {
		j1 := 2*i + 1
		if j1 >= n || j1 < 0 { // j1 < 0 after int overflow
			break
		}
		j := j1 // left child
		if j2 := j1 + 1; j2 < n && pq.less(j2, j1) {
			j = j2 // = 2*i + 2  // right child
		}
		if !pq.less(j, i) {
			break
		}
		pq.swap(i, j)
		i = j
	}
	return i > i0
}
