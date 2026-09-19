package main

import (
	"encoding/base64"
	"encoding/binary"
	"testing"
	"time"
)

func TestBoundedBalancedSchedule(t *testing.T) {
	counts := map[string]int{}
	for i := 0; i < maxAttempts; i++ {
		proxy, effort := schedule(i)
		if proxy < 1 || proxy > 2 {
			t.Fatal("unexpected proxy")
		}
		if i%2 == 0 && effort != "high" || i%2 == 1 && effort != "medium" {
			t.Fatal("effort did not alternate")
		}
		counts[string(rune('0'+proxy))+effort]++
	}
	if maxAttempts != 100 || len(counts) != 4 {
		t.Fatal("incorrect cap or combinations")
	}
	for _, n := range counts {
		if n != 25 {
			t.Fatal("unbalanced schedule")
		}
	}
}
func TestNormal292RequiresShapeAndFreshness(t *testing.T) {
	now := time.Unix(1790000000, 0)
	makeValue := func(blocks int, issued time.Time) string {
		raw := make([]byte, 57+16*blocks)
		raw[0] = 0x80
		binary.BigEndian.PutUint64(raw[1:9], uint64(issued.Unix()))
		return base64.URLEncoding.EncodeToString(raw)
	}
	if !valid292(makeValue(10, now), now) {
		t.Fatal("fresh 292 rejected")
	}
	for _, value := range []string{makeValue(10, now.Add(-time.Hour)), makeValue(10, now.Add(6*time.Minute)), makeValue(12, now), "bad"} {
		if valid292(value, now) {
			t.Fatal("invalid candidate accepted")
		}
	}
}
