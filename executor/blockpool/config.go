package blockpool

import "time"

type Config struct {
	MaxBlockSize  uint64 // Maximum number of blocks
	BatchTimeout  time.Duration
	BatchCapacity int
}
