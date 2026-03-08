package app

import (
	"cmp"
	"fmt"
	"log"
	"slices"
	"strconv"
	"strings"
)

type FloatArray []float64

func (a *FloatArray) Get() interface{} { return []float64(*a) }

func (a *FloatArray) Set(param string) error {
	for _, s := range strings.Split(param, ",") {
		v, err := strconv.ParseFloat(s, 64)
		if err != nil {
			log.Fatalf("Could not parse: %s", s)
			return nil
		}
		*a = append(*a, v)
	}
	slices.SortFunc(*a, func(x, y float64) int { return cmp.Compare(y, x) })
	return nil
}

func (a *FloatArray) String() string {
	var s []string
	for _, v := range *a {
		s = append(s, fmt.Sprintf("%f", v))
	}
	return strings.Join(s, ",")
}
