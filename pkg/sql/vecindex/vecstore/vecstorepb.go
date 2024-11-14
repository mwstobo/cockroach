// Copyright 2024 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package vecstore

import (
	"bytes"
	"fmt"
	"math"

	_ "github.com/gogo/protobuf/gogoproto"
)

// String returns a human-readable representation of the index stats.
func (s *IndexStats) String() string {
	var buf bytes.Buffer
	buf.WriteString(fmt.Sprintf("%d levels, %d partitions, %.2f vectors/partition.\n",
		len(s.CVStats)+1, s.NumPartitions, s.VectorsPerPartition))
	buf.WriteString("CV stats:\n")
	for i, cvstats := range s.CVStats {
		stdev := math.Sqrt(cvstats.Variance)
		buf.WriteString(fmt.Sprintf("  level %d - mean: %.4f, stdev: %.4f\n",
			i+2, cvstats.Mean, stdev))
	}
	return buf.String()
}

// Equal returns true if this key has the same partition key and primary key as
// the given key.
func (k ChildKey) Equal(other ChildKey) bool {
	return k.PartitionKey == other.PartitionKey && bytes.Equal(k.PrimaryKey, other.PrimaryKey)
}

// Compare returns an integer comparing two child keys. The result is zero if
// the two are equal, -1 if this key is less than the other, and +1 if this key
// is greater than the other.
func (k ChildKey) Compare(other ChildKey) int {
	if k.PrimaryKey != nil {
		return bytes.Compare(k.PrimaryKey, other.PrimaryKey)
	}
	if k.PartitionKey < other.PartitionKey {
		return -1
	} else if k.PartitionKey > other.PartitionKey {
		return 1
	}
	return 0
}
