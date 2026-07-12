//go:build linux

package main

import (
	"fmt"
	"reflect"
	"testing"
)

func TestWilliamsRowsBalancePositionAndCarryover(t *testing.T) {
	for _, count := range []int{2, 3, 4, 5} {
		t.Run(fmt.Sprintf("candidates-%d", count), func(t *testing.T) {
			rows := williamsRows(count)
			if len(rows) != scheduleCycleLength(count) {
				t.Fatalf("got %d rows, want %d", len(rows), scheduleCycleLength(count))
			}
			positions := make([][]int, count)
			carryover := make([][]int, count)
			for i := range count {
				positions[i] = make([]int, count)
				carryover[i] = make([]int, count)
			}
			for _, row := range rows {
				if len(row) != count {
					t.Fatalf("short row: %v", row)
				}
				seen := make(map[int]bool, count)
				for position, candidate := range row {
					if candidate < 0 || candidate >= count || seen[candidate] {
						t.Fatalf("row is not a permutation: %v", row)
					}
					seen[candidate] = true
					positions[candidate][position]++
					if position > 0 {
						carryover[row[position-1]][candidate]++
					}
				}
			}
			positionWant := len(rows) / count
			carryoverWant := len(rows) / count
			for candidate := range count {
				for position := range count {
					if positions[candidate][position] != positionWant {
						t.Fatalf("candidate %d position %d count %d, want %d", candidate, position, positions[candidate][position], positionWant)
					}
					if candidate != position && carryover[candidate][position] != carryoverWant {
						t.Fatalf("carryover %d->%d count %d, want %d", candidate, position, carryover[candidate][position], carryoverWant)
					}
				}
			}
		})
	}
}

func TestBuildScheduleIsSeededAndComplete(t *testing.T) {
	candidates := []candidate{{ID: "a"}, {ID: "b"}, {ID: "c"}}
	first, err := buildSchedule(candidates, 12, 42, "large-files", "confirmation")
	if err != nil {
		t.Fatal(err)
	}
	second, err := buildSchedule(candidates, 12, 42, "large-files", "confirmation")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatal("same seed did not reproduce schedule")
	}
	different, err := buildSchedule(candidates, 12, 43, "large-files", "confirmation")
	if err != nil {
		t.Fatal(err)
	}
	if reflect.DeepEqual(first, different) {
		t.Fatal("different seeds produced identical schedule")
	}
	for index, block := range first {
		if block.Index != index || len(block.Candidates) != len(candidates) {
			t.Fatalf("malformed block: %#v", block)
		}
	}
	if _, err := buildSchedule(candidates, 5, 42, "large-files", "confirmation"); err == nil {
		t.Fatal("incomplete balanced cycle was accepted")
	}
}

func TestCalibrationScheduleTruncatesRandomizedCycle(t *testing.T) {
	candidates := []candidate{{ID: "a"}, {ID: "b"}, {ID: "c"}, {ID: "d"}, {ID: "e"}}
	blocks, err := buildSchedule(candidates, 2, 42, "calibration", "calibration")
	if err != nil {
		t.Fatal(err)
	}
	if len(blocks) != 2 {
		t.Fatalf("got %d calibration blocks, want exactly 2", len(blocks))
	}
	for _, block := range blocks {
		if len(block.Candidates) != len(candidates) {
			t.Fatalf("truncated a block instead of the cycle: %#v", block)
		}
	}
}
