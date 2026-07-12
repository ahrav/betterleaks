package main

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math/rand"
	"strconv"
)

type scheduledBlock struct {
	Index      int
	SequenceID int
	Candidates []string
}

func buildSchedule(candidates []candidate, blocks int, seed int64, stratumID, phase string) ([]scheduledBlock, error) {
	cycleLength := scheduleCycleLength(len(candidates))
	if phase != "calibration" && blocks%cycleLength != 0 {
		return nil, fmt.Errorf("blocks %d is not divisible by schedule cycle %d", blocks, cycleLength)
	}
	rows := williamsRows(len(candidates))
	ids := make([]string, len(candidates))
	for i, candidate := range candidates {
		ids[i] = candidate.ID
	}
	random := rand.New(rand.NewSource(derivedSeed(seed, stratumID)))
	result := make([]scheduledBlock, 0, blocks)
	for len(result) < blocks {
		rowOrder := random.Perm(len(rows))
		mapping := random.Perm(len(ids))
		for _, rowIndex := range rowOrder {
			if len(result) == blocks {
				break
			}
			row := rows[rowIndex]
			order := make([]string, len(row))
			for position, candidateIndex := range row {
				order[position] = ids[mapping[candidateIndex]]
			}
			result = append(result, scheduledBlock{
				Index: len(result), SequenceID: rowIndex, Candidates: order,
			})
		}
	}
	return result, nil
}

func williamsRows(count int) [][]int {
	if count == 1 {
		return [][]int{{0}}
	}
	base := make([]int, count)
	for position := range count {
		switch {
		case position == 0:
			base[position] = 0
		case position%2 == 1:
			base[position] = (position + 1) / 2
		default:
			base[position] = count - position/2
		}
	}
	rows := make([][]int, 0, scheduleCycleLength(count))
	for shift := range count {
		row := make([]int, count)
		for position, value := range base {
			row[position] = (value + shift) % count
		}
		rows = append(rows, row)
	}
	if count%2 == 1 {
		original := len(rows)
		for i := range original {
			reversed := make([]int, count)
			for position := range count {
				reversed[position] = rows[i][count-1-position]
			}
			rows = append(rows, reversed)
		}
	}
	return rows
}

func scheduleCycleLength(candidateCount int) int {
	if candidateCount < 1 {
		return 0
	}
	if candidateCount%2 == 1 && candidateCount > 1 {
		return 2 * candidateCount
	}
	return candidateCount
}

// scheduleSHA256 binds the plan to the exact seeded sequence, not merely to
// aggregate position and carryover balance. IDs cannot contain tabs or
// newlines, so the canonical record is unambiguous and easy to reproduce in
// the standalone analyzer.
func scheduleSHA256(schedule []scheduledBlock) string {
	digest := sha256.New()
	for _, block := range schedule {
		_, _ = digest.Write([]byte{'\t'})
		_, _ = digest.Write([]byte(strconv.Itoa(block.Index)))
		_, _ = digest.Write([]byte{'\t'})
		_, _ = digest.Write([]byte(strconv.Itoa(block.SequenceID)))
		for _, candidate := range block.Candidates {
			_, _ = digest.Write([]byte{'\t'})
			_, _ = digest.Write([]byte(candidate))
		}
		_, _ = digest.Write([]byte{'\n'})
	}
	return hex.EncodeToString(digest.Sum(nil))
}

func derivedSeed(seed int64, label string) int64 {
	hash := sha256.New()
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], uint64(seed))
	_, _ = hash.Write(encoded[:])
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write([]byte(label))
	sum := hash.Sum(nil)
	return int64(binary.BigEndian.Uint64(sum[:8]))
}
